package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/externals"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
)

var errNoStandaloneDevice = errors.New("no standalone device configured; run --setup --username USER --device-name NAME once")

func standaloneStateDir(options identityOptions) (string, error) {
	if !libkb.CheckUsername.F(options.username) {
		return "", errors.New("a valid --username or KEYBASE_GIT_USERNAME is required")
	}
	base := options.stateDir
	if base == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(dir, "keybase-git-standalone")
	}
	return filepath.Abs(filepath.Join(base, "devices", options.username))
}

func openStandaloneIdentity(ctx context.Context, options identityOptions, create bool,
	storeFactory func(string) (libkb.SecretStoreAll, error),
) (g *libkb.GlobalContext, cleanup func() error, retErr error) {
	root, err := standaloneStateDir(options)
	if err != nil {
		return nil, nil, err
	}
	if !create {
		if _, err := os.Stat(filepath.Join(root, "config.json")); err != nil {
			if os.IsNotExist(err) {
				return nil, nil, errNoStandaloneDevice
			}
			return nil, nil, err
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, nil, errors.New("standalone device directory must have permissions 0700")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := lockIdentityCache(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	g = externals.NewGlobalContextInit()
	ownedG := g
	cleanup = func() error { return errors.Join(closeIdentity(ownedG), unlock()) }
	success := false
	closeOnFailure := cleanup
	defer func() {
		if !success {
			retErr = errors.Join(retErr, closeOnFailure())
			cleanup = nil
		}
	}()
	if err := configureIdentity(g, root); err != nil {
		return nil, nil, err
	}
	if err := g.ConfigureKeyring(); err != nil {
		return nil, nil, err
	}
	store, err := storeFactory(root)
	if err != nil {
		return nil, nil, err
	}
	g.SetSecretStore(store)
	if !create && g.Env.GetUsername().String() != options.username {
		return nil, nil, errors.New("standalone device configuration belongs to a different user")
	}
	success = true
	return g, cleanup, nil
}

func setupDevice(ctx context.Context, options identityOptions, deviceName string, errput io.Writer) (retErr error) {
	g, cleanup, err := openStandaloneIdentity(ctx, options, true, standaloneDeviceSecretStore)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	username := libkb.NewNormalizedUsername(options.username)
	user, _ := g.Env.GetConfig().GetUserConfigForUsername(username)
	if user != nil && !user.GetDeviceID().IsNil() {
		return errors.New("this helper already has a device; run Git normally instead of registering another device")
	}
	m := libkb.NewMetaContext(ctx, g)
	// Check that the OS can retain the unlock secret before registering keys.
	if err := g.SecretStore().PrimeSecretStores(m); err != nil {
		return fmt.Errorf("system keychain is unavailable: %w", err)
	}
	paperKey, err := readPaperKey(ctx, options.paperFD, errput)
	if err != nil {
		return err
	}
	ui := setupUI{}
	m = m.WithUIs(libkb.UIs{
		LogUI: g.Log, SecretUI: ui, LoginUI: ui, ProvisionUI: ui, GPGUI: ui,
	})
	login := engine.NewLogin(g, keybase1.DeviceTypeV2_DESKTOP, options.username, keybase1.ClientType_CLI)
	login.PaperKey, login.DeviceName = paperKey, deviceName
	err = engine.RunEngine2(m, login)
	login.PaperKey, paperKey = "", ""
	if err != nil {
		return err
	}
	if !g.ActiveDevice.Valid() {
		return errors.New("device setup did not establish a valid identity")
	}
	// The official login flow treats secret-store write errors as warnings.
	// Verify the native backend, bypassing its in-memory write-through cache.
	root, err := standaloneStateDir(options)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	store, err := standaloneDeviceSecretStore(root)
	if err != nil {
		return err
	}
	persisted, err := store.RetrieveSecret(m, username)
	if err != nil || persisted.IsNil() {
		return fmt.Errorf("device registered but its unlock secret was not retained by the system keychain; keep the state directory for recovery: %v", err)
	}
	expected, err := g.SecretStore().RetrieveSecret(m, username)
	if err != nil || !persisted.Equal(expected) {
		return errors.New("device registered but the persisted unlock secret does not match; keep the state directory for recovery")
	}
	_, err = fmt.Fprintln(errput, "Standalone device configured. Future Git operations use its encrypted keys and system keychain; no Paper Key is needed.")
	return err
}

// Provisioning is fully specified by PaperKey and DeviceName. Refuse any
// unexpected recovery, account reset, GPG or interactive provisioning path.
type setupUI struct{}

var errUnexpectedSetupPrompt = errors.New("unexpected interactive provisioning step; no recovery or account reset was authorized")

func (setupUI) GetPassphrase(keybase1.GUIEntryArg, *keybase1.SecretEntryArg) (keybase1.GetPassphraseRes, error) {
	return keybase1.GetPassphraseRes{}, errUnexpectedSetupPrompt
}
func (setupUI) GetEmailOrUsername(context.Context, int) (string, error) {
	return "", errUnexpectedSetupPrompt
}
func (setupUI) PromptRevokePaperKeys(context.Context, keybase1.PromptRevokePaperKeysArg) (bool, error) {
	return false, errUnexpectedSetupPrompt
}
func (setupUI) DisplayPaperKeyPhrase(context.Context, keybase1.DisplayPaperKeyPhraseArg) error {
	return errUnexpectedSetupPrompt
}
func (setupUI) DisplayPrimaryPaperKey(context.Context, keybase1.DisplayPrimaryPaperKeyArg) error {
	return errUnexpectedSetupPrompt
}
func (setupUI) PromptResetAccount(context.Context, keybase1.PromptResetAccountArg) (keybase1.ResetPromptResponse, error) {
	return 0, errUnexpectedSetupPrompt
}
func (setupUI) DisplayResetProgress(context.Context, keybase1.DisplayResetProgressArg) error {
	return errUnexpectedSetupPrompt
}
func (setupUI) ExplainDeviceRecovery(context.Context, keybase1.ExplainDeviceRecoveryArg) error {
	return errUnexpectedSetupPrompt
}
func (setupUI) PromptPassphraseRecovery(context.Context, keybase1.PromptPassphraseRecoveryArg) (bool, error) {
	return false, errUnexpectedSetupPrompt
}
func (setupUI) ChooseDeviceToRecoverWith(context.Context, keybase1.ChooseDeviceToRecoverWithArg) (keybase1.DeviceID, error) {
	return "", errUnexpectedSetupPrompt
}
func (setupUI) DisplayResetMessage(context.Context, keybase1.DisplayResetMessageArg) error {
	return errUnexpectedSetupPrompt
}
func (setupUI) ChooseProvisioningMethod(context.Context, keybase1.ChooseProvisioningMethodArg) (keybase1.ProvisionMethod, error) {
	return 0, errUnexpectedSetupPrompt
}
func (setupUI) ChooseGPGMethod(context.Context, keybase1.ChooseGPGMethodArg) (keybase1.GPGMethod, error) {
	return 0, errUnexpectedSetupPrompt
}
func (setupUI) SwitchToGPGSignOK(context.Context, keybase1.SwitchToGPGSignOKArg) (bool, error) {
	return false, errUnexpectedSetupPrompt
}
func (setupUI) ChooseDevice(context.Context, keybase1.ChooseDeviceArg) (keybase1.DeviceID, error) {
	return "", errUnexpectedSetupPrompt
}
func (setupUI) ChooseDeviceType(context.Context, keybase1.ChooseDeviceTypeArg) (keybase1.DeviceType, error) {
	return 0, errUnexpectedSetupPrompt
}
func (setupUI) DisplayAndPromptSecret(context.Context, keybase1.DisplayAndPromptSecretArg) (keybase1.SecretResponse, error) {
	return keybase1.SecretResponse{}, errUnexpectedSetupPrompt
}
func (setupUI) DisplaySecretExchanged(context.Context, int) error { return errUnexpectedSetupPrompt }
func (setupUI) PromptNewDeviceName(context.Context, keybase1.PromptNewDeviceNameArg) (string, error) {
	return "", errUnexpectedSetupPrompt
}
func (setupUI) ProvisioneeSuccess(context.Context, keybase1.ProvisioneeSuccessArg) error { return nil }
func (setupUI) ProvisionerSuccess(context.Context, keybase1.ProvisionerSuccessArg) error {
	return errUnexpectedSetupPrompt
}
func (setupUI) WantToAddGPGKey(context.Context, int) (bool, error) {
	return false, errUnexpectedSetupPrompt
}
func (setupUI) ConfirmDuplicateKeyChosen(context.Context, int) (bool, error) {
	return false, errUnexpectedSetupPrompt
}
func (setupUI) ConfirmImportSecretToExistingKey(context.Context, int) (bool, error) {
	return false, errUnexpectedSetupPrompt
}
func (setupUI) SelectKeyAndPushOption(context.Context, keybase1.SelectKeyAndPushOptionArg) (keybase1.SelectKeyRes, error) {
	return keybase1.SelectKeyRes{}, errUnexpectedSetupPrompt
}
func (setupUI) SelectKey(context.Context, keybase1.SelectKeyArg) (string, error) {
	return "", errUnexpectedSetupPrompt
}
func (setupUI) Sign(context.Context, keybase1.SignArg) (string, error) {
	return "", errUnexpectedSetupPrompt
}
func (setupUI) GetTTY(context.Context) (string, error) { return "", errUnexpectedSetupPrompt }
