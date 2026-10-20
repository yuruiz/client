package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/externals"
	"github.com/keybase/client/go/kbconst"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
)

type identityOptions struct {
	username    string
	mode        string
	paperFD     int
	keybaseHome string
	cacheDir    string
	stateDir    string
}

type credentialSource struct {
	libkb.NullConfiguration
	home string
}

func (c credentialSource) GetHome() string { return c.home }
func (credentialSource) GetRunMode() (kbconst.RunMode, error) {
	return kbconst.ProductionRunMode, nil
}
func (credentialSource) GetUseRootConfigFile() (bool, bool) { return false, true }

type borrowedCommandLine struct {
	isolatedCommandLine
	keyringTemplate string
}

func (c borrowedCommandLine) GetSecretKeyringTemplate() string { return c.keyringTemplate }

// Only override the selected identity in memory. Never switch the desktop
// client's current_user on disk or expose its ConfigWriter to the helper.
type selectedConfig struct {
	libkb.ConfigReader
	user *libkb.UserConfig
}

func (c selectedConfig) GetUID() keybase1.UID                      { return c.user.GetUID() }
func (c selectedConfig) GetDeviceID() keybase1.DeviceID            { return c.user.GetDeviceID() }
func (c selectedConfig) GetUsername() libkb.NormalizedUsername     { return c.user.GetUsername() }
func (c selectedConfig) GetUserConfig() (*libkb.UserConfig, error) { return c.user, nil }
func (c selectedConfig) GetPassphraseState() *keybase1.PassphraseState {
	return c.user.GetPassphraseState()
}

var errReadOnlyCredentials = errors.New("the standalone helper cannot modify the official client's credentials")
var errNoExistingDevice = errors.New("no provisioned Keybase device")

type readOnlySecretStore struct {
	libkb.SecretStoreAll
}

func (readOnlySecretStore) StoreSecret(libkb.MetaContext, libkb.NormalizedUsername, libkb.LKSecFullSecret) error {
	return errReadOnlyCredentials
}
func (readOnlySecretStore) ClearSecret(libkb.MetaContext, libkb.NormalizedUsername) error {
	return errReadOnlyCredentials
}
func (readOnlySecretStore) SetOptions(libkb.MetaContext, *libkb.SecretStoreOptions) {}

func closeIdentity(g *libkb.GlobalContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clearErr := g.ActiveDevice.Clear()
	return errors.Join(clearErr, g.Shutdown(libkb.NewMetaContext(ctx, g)))
}

func openExistingIdentity(ctx context.Context, options identityOptions,
	storeFactory func() (libkb.SecretStoreAll, error),
) (g *libkb.GlobalContext, cleanup func() error, retErr error) {
	g = externals.NewGlobalContextInit()
	ownedG := g
	var unlock func() error
	cleanup = func() error {
		err := closeIdentity(ownedG)
		if unlock != nil {
			err = errors.Join(err, unlock())
		}
		return err
	}
	success := false
	closeOnFailure := cleanup
	defer func() {
		if !success {
			retErr = errors.Join(retErr, closeOnFailure())
			cleanup = nil
		}
	}()

	source := libkb.NewEnv(credentialSource{home: options.keybaseHome}, nil, g.GetLog)
	configPath := source.GetConfigFilename()
	// Loading this file does not call ConfigReload (which can write GUI config).
	config := libkb.NewJSONConfigFile(g, configPath)
	if err := config.Load(false); err != nil {
		return nil, nil, fmt.Errorf("reading existing Keybase configuration: %w", err)
	}
	if err := config.Check(); err != nil {
		return nil, nil, err
	}
	source.SetConfig(config, nil)
	user, err := config.GetUserConfigForUsername(libkb.NewNormalizedUsername(options.username))
	if err != nil || user == nil || user.GetUID().IsNil() || user.GetDeviceID().IsNil() || user.IsOneshot() {
		return nil, nil, fmt.Errorf("%w for %s in %s", errNoExistingDevice, options.username, configPath)
	}
	root, err := identityCacheDir(options.cacheDir, configPath, user)
	if err != nil {
		return nil, nil, err
	}
	unlock, err = lockIdentityCache(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	g.SetCommandLine(borrowedCommandLine{
		isolatedCommandLine: isolatedCommandLine{root: root},
		keyringTemplate:     source.GetSecretKeyringTemplate(),
	})
	g.Env.SetConfig(selectedConfig{ConfigReader: config, user: user}, nil)
	if err := g.ConfigureUsage(libkb.Usage{KbKeyring: true, API: true}); err != nil {
		return nil, nil, err
	}
	installIdentityLibraries(g)
	store, err := storeFactory()
	if err != nil {
		return nil, nil, err
	}
	g.SetSecretStore(readOnlySecretStore{SecretStoreAll: store})
	success = true
	return g, cleanup, nil
}

func identityCacheDir(base, configPath string, user *libkb.UserConfig) (string, error) {
	if base == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(dir, "keybase-git-standalone")
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	configPath, err = filepath.EvalSymlinks(configPath)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(configPath + "\x00" + user.GetUID().String() + "\x00" + user.GetDeviceID().String()))
	root, err := filepath.Abs(filepath.Join(base, "identities", fmt.Sprintf("%x", digest[:])))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("identity cache must be a private directory (0700): %s", root)
	}
	return filepath.EvalSymlinks(root)
}

func authenticateExistingIdentity(ctx context.Context, g *libkb.GlobalContext) error {
	m := libkb.NewMetaContext(ctx, g)
	// Refresh public device state before using cached private keys. In particular,
	// never let a cached UPAK hide a revoked device or a reset account.
	_, _, err := g.GetUPAKLoader().LoadV2(libkb.NewLoadUserArgWithMetaContext(m).
		WithUID(g.Env.GetUID()).WithPublicKeyOptional().WithForcePoll(true))
	if err != nil {
		return fmt.Errorf("checking existing device: %w", err)
	}
	if _, err := libkb.BootstrapActiveDeviceFromConfig(m, true); err != nil {
		return fmt.Errorf("unlocking existing Keybase device: %w (unlock the system keychain or restore the device's credentials; no Paper Key fallback was attempted)", err)
	}
	if !g.ActiveDevice.Valid() {
		return errors.New("existing device credentials did not establish a valid identity")
	}
	return nil
}

func openOneshotIdentity(ctx context.Context, options identityOptions, errput io.Writer) (
	g *libkb.GlobalContext, cleanup func() error, retErr error,
) {
	paperKey, err := readPaperKey(ctx, options.paperFD, errput)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.MkdirTemp("", "keybase-git-standalone-")
	if err != nil {
		return nil, nil, err
	}
	g = externals.NewGlobalContextInit()
	ownedG := g
	cleanup = func() error { return errors.Join(closeIdentity(ownedG), os.RemoveAll(root)) }
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
	err = engine.RunEngine2(libkb.NewMetaContext(ctx, g),
		engine.NewLoginOneshot(g, keybase1.LoginOneshotArg{Username: options.username, PaperKey: paperKey}))
	paperKey = ""
	if err != nil {
		return nil, nil, fmt.Errorf("paper-key authentication: %w", err)
	}
	if !g.ActiveDevice.Valid() {
		return nil, nil, errors.New("paper-key authentication did not establish a valid session")
	}
	success = true
	return g, cleanup, nil
}
