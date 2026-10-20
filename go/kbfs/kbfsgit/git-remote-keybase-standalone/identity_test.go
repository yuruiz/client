//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keybase/client/go/externals"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/stretchr/testify/require"
)

type fixtureDeviceLoader struct {
	libkb.UPAKLoader
	user keybase1.UserPlusKeysV2AllIncarnations
}

func (l fixtureDeviceLoader) LoadV2(libkb.LoadUserArg) (*keybase1.UserPlusKeysV2AllIncarnations, *libkb.User, error) {
	copy := l.user.DeepCopy()
	return &copy, nil, nil
}

func makeExistingDeviceFixture(t *testing.T) (identityOptions, *libkb.SecretStoreMem, keybase1.UserPlusKeysV2AllIncarnations, string, string) {
	t.Helper()
	home := t.TempDir()
	// Never let this fixture fall through to the real desktop configuration.
	configPath := filepath.Join(home, "config.json")
	keyringTemplate := filepath.Join(home, "secretkeys.%u.mpack")
	t.Setenv("KEYBASE_CONFIG_FILE", configPath)
	t.Setenv("KEYBASE_SECRET_KEYRING_TEMPLATE", keyringTemplate)
	options := identityOptions{username: "alice", mode: "existing", keybaseHome: home, cacheDir: t.TempDir(), paperFD: -1}
	uid := keybase1.MakeTestUID(1)
	deviceID, err := libkb.NewDeviceID()
	require.NoError(t, err)
	user := libkb.NewUserConfig(uid, "alice", []byte("test-salt"), deviceID)
	other := libkb.NewUserConfig(keybase1.MakeTestUID(2), "other", nil, deviceID)
	configBytes, err := json.Marshal(map[string]any{
		"current_user": "other", "users": map[string]any{"alice": user, "other": other},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, configBytes, 0o600))
	g := externals.NewGlobalContextInit()
	defer closeIdentity(g)
	g.SetCommandLine(credentialSource{home: home})
	require.NoError(t, g.ConfigureConfig())
	signing, err := libkb.GenerateNaclSigningKeyPair()
	require.NoError(t, err)
	encryption, err := libkb.GenerateNaclDHKeyPair()
	require.NoError(t, err)
	clientHalf, err := libkb.NewLKSecClientHalfFromBytes(bytes.Repeat([]byte{42}, libkb.LKSecLen))
	require.NoError(t, err)
	lks := libkb.NewLKSecWithClientHalf(clientHalf, 1, uid)
	require.NoError(t, lks.GenerateServerHalf())
	m := libkb.NewMetaContextBackground(g)
	lks.SetFullSecret(m)
	ring := libkb.NewSKBKeyringFile(g, "alice")
	for _, key := range []libkb.GenericKey{signing, encryption} {
		skb, err := lks.ToSKB(m, key)
		require.NoError(t, err)
		require.NoError(t, ring.PushAndSave(skb))
	}
	store := libkb.NewSecretStoreMem()
	require.NoError(t, store.StoreSecret(m, "alice", lks.FullSecret()))
	signingKID := signing.GetKID()
	fixture := keybase1.UserPlusKeysV2AllIncarnations{Current: keybase1.UserPlusKeysV2{
		Uid: uid, Username: "alice", EldestSeqno: 1, Status: keybase1.StatusCode_SCOk,
		DeviceKeys: map[keybase1.KID]keybase1.PublicKeyV2NaCl{
			signingKID:          {Base: keybase1.PublicKeyV2Base{Kid: signingKID, IsSibkey: true}, DeviceID: deviceID, DeviceDescription: "test desktop"},
			encryption.GetKID(): {Base: keybase1.PublicKeyV2Base{Kid: encryption.GetKID()}, Parent: &signingKID, DeviceID: deviceID},
		},
	}}
	return options, store, fixture, configPath, strings.ReplaceAll(keyringTemplate, "%u", "alice")
}

func TestExistingDeviceRestartsWithoutPaperKeyAndKeepsAuditCache(t *testing.T) {
	options, store, fixture, configPath, keyringPath := makeExistingDeviceFixture(t)
	configBefore, err := os.ReadFile(configPath)
	require.NoError(t, err)
	keysBefore, err := os.ReadFile(keyringPath)
	require.NoError(t, err)
	var firstCache string
	for attempt := range 2 {
		g, cleanup, err := openExistingIdentity(context.Background(), options, func() (libkb.SecretStoreAll, error) { return store, nil })
		require.NoError(t, err)
		func() {
			defer func() { require.NoError(t, cleanup()) }()
			require.Nil(t, g.Env.GetConfigWriter())
			require.Equal(t, fixture.Current.Uid, g.Env.GetUID())
			require.Equal(t, libkb.NewNormalizedUsername("alice"), g.Env.GetUsername())
			require.Equal(t, keyringPath, g.SKBFilenameForUser("alice"))
			cache := g.Env.GetDbFilename()
			require.NotEqual(t, filepath.Join(options.keybaseHome, "identity.leveldb"), cache)
			if attempt == 0 {
				firstCache = cache
			} else {
				require.Equal(t, firstCache, cache)
			}
			g.SetUPAKLoader(fixtureDeviceLoader{UPAKLoader: g.GetUPAKLoader(), user: fixture})
			require.NoError(t, authenticateExistingIdentity(context.Background(), g))
			require.True(t, g.ActiveDevice.Valid())
			// Exercise real encrypted SKB loading/unlocking, not just a cached
			// ActiveDevice. Every loop starts a new GlobalContext.
			key, err := g.ActiveDevice.SigningKey()
			require.NoError(t, err)
			require.True(t, fixture.Current.DeviceKeys[key.GetKID()].Base.IsSibkey)
			cacheKey := libkb.DbKey{Typ: libkb.DBTeamAuditor, Key: "helper-test"}
			if attempt == 0 {
				require.NoError(t, g.LocalDb.PutObj(cacheKey, nil, "verified-history"))
			} else {
				var history string
				found, err := g.LocalDb.GetInto(&history, cacheKey)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, "verified-history", history)
			}
			m := libkb.NewMetaContextBackground(g)
			require.ErrorIs(t, g.SecretStore().ClearSecret(m, "alice"), errReadOnlyCredentials)
		}()
		require.False(t, g.ActiveDevice.Valid())
	}
	configAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	keysAfter, err := os.ReadFile(keyringPath)
	require.NoError(t, err)
	require.Equal(t, configBefore, configAfter)
	require.Equal(t, keysBefore, keysAfter)
	require.DirExists(t, filepath.Dir(firstCache))
}

func TestExistingDeviceRejectsRevocationAndMissingUnlockSecret(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing secret", true: "revoked device"}[revoked], func(t *testing.T) {
			options, store, fixture, _, _ := makeExistingDeviceFixture(t)
			if revoked {
				for kid, key := range fixture.Current.DeviceKeys {
					if key.Base.IsSibkey {
						key.Base.Revocation = &keybase1.SignatureMetadata{}
						fixture.Current.DeviceKeys[kid] = key
					}
				}
			} else {
				store = libkb.NewSecretStoreMem()
			}
			g, cleanup, err := openExistingIdentity(context.Background(), options, func() (libkb.SecretStoreAll, error) { return store, nil })
			require.NoError(t, err)
			defer cleanup()
			g.SetUPAKLoader(fixtureDeviceLoader{UPAKLoader: g.GetUPAKLoader(), user: fixture})
			require.Error(t, authenticateExistingIdentity(context.Background(), g))
			require.False(t, g.ActiveDevice.Valid())
		})
	}
}

func TestIdentityCacheLockCancellationAndRelease(t *testing.T) {
	root := t.TempDir()
	release, err := lockIdentityCache(context.Background(), root)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = lockIdentityCache(ctx, root)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, release())
	release, err = lockIdentityCache(context.Background(), root)
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestStandaloneDevicePersistsAndSetupDoesNotRegisterAgain(t *testing.T) {
	options, store, fixture, configPath, keyringPath := makeExistingDeviceFixture(t)
	options.mode, options.stateDir = "device", t.TempDir()
	root, err := standaloneStateDir(options)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0o700))
	configBytes, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, json.Unmarshal(configBytes, &config))
	config["current_user"] = "alice"
	configBytes, err = json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), configBytes, 0o600))
	keys, err := os.ReadFile(keyringPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "secretkeys.alice.mpack"), keys, 0o600))
	for range 2 {
		g, cleanup, err := openStandaloneIdentity(context.Background(), options, false,
			func(string) (libkb.SecretStoreAll, error) { return store, nil })
		require.NoError(t, err)
		g.SetUPAKLoader(fixtureDeviceLoader{UPAKLoader: g.GetUPAKLoader(), user: fixture})
		require.NoError(t, authenticateExistingIdentity(context.Background(), g))
		require.True(t, g.ActiveDevice.Valid())
		require.NoError(t, cleanup())
		require.False(t, g.ActiveDevice.Valid())
		require.FileExists(t, filepath.Join(root, "config.json"))
		require.FileExists(t, filepath.Join(root, "secretkeys.alice.mpack"))
	}
	var errput bytes.Buffer
	err = run(context.Background(), []string{
		"--setup", "--username", "alice", "--device-name", "another device",
		"--state-dir", options.stateDir, "--paper-key-fd", "12345",
	}, strings.NewReader(""), io.Discard, &errput)
	require.ErrorContains(t, err, "already has a device")
	require.NotContains(t, errput.String(), "Keybase paper key:")
}

func TestStandaloneKeychainNamespaceIsSeparate(t *testing.T) {
	existing, err := existingDeviceSecretStore()
	require.NoError(t, err)
	require.Empty(t, existing.(libkb.KeychainSecretStore).ServiceName)
	first, err := standaloneDeviceSecretStore("/first")
	require.NoError(t, err)
	second, err := standaloneDeviceSecretStore("/second")
	require.NoError(t, err)
	firstName := first.(libkb.KeychainSecretStore).ServiceName
	secondName := second.(libkb.KeychainSecretStore).ServiceName
	require.Contains(t, firstName, "keybase-git-standalone-")
	require.NotEqual(t, firstName, secondName)
}

func TestExistingModeNeverFallsBackToPaperKey(t *testing.T) {
	t.Setenv("KEYBASE_GIT_AUTH_MODE", "")
	t.Setenv("KEYBASE_GIT_PAPER_KEY_FD", "")
	t.Setenv("KEYBASE_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-config.json"))
	cache := filepath.Join(t.TempDir(), "cache")
	var output, errput bytes.Buffer
	err := run(context.Background(), []string{"--username", "alice", "--cache-dir", cache, "origin", "keybase://private/alice/test"},
		strings.NewReader("list\n"), &output, &errput)
	require.ErrorContains(t, err, "no provisioned Keybase device")
	require.NotContains(t, errput.String(), "Keybase paper key:")
	require.Empty(t, output.String())
	_, err = os.Stat(cache)
	require.True(t, os.IsNotExist(err))
	require.NoError(t, run(context.Background(), []string{"--username", "alice", "origin", "keybase://private/alice/test"},
		strings.NewReader("capabilities\n\n"), io.Discard, &errput))
}
