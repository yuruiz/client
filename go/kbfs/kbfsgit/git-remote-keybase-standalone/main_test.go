package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keybase/client/go/externals"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/stretchr/testify/require"
)

func TestRunNegotiationWithoutCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("KEYBASE_GIT_USERNAME", "")
	t.Setenv("KEYBASE_GIT_PAPER_KEY_FD", "")
	t.Setenv("KEYBASE_SERVER_URI", "http://127.0.0.1:1")
	for _, tc := range []struct {
		args          []string
		input, output string
	}{
		{[]string{"origin", "keybase://private/alice/test"}, "capabilities\noption progress false\n\n", "fetch\npush\noption\n\nok\n"},
		{[]string{"--diagnostics", "origin", "keybase://private/alice/test"}, "capabilities\n\n", "fetch\npush\noption\n\n"},
		{[]string{"keybase://public/alice/test"}, "capabilities\n\n", "fetch\npush\noption\n\n"},
		{[]string{"lfs", "origin", "keybase://team/example/test"}, "{\"event\":\"init\"}\n{\"event\":\"terminate\"}\n", "{}\n"},
		{[]string{"lfs origin keybase://team/example/test"}, "{\"event\":\"init\"}\n{\"event\":\"terminate\"}\n", "{}\n"},
	} {
		var output, errput bytes.Buffer
		require.NoError(t, run(context.Background(), tc.args, strings.NewReader(tc.input), &output, &errput))
		require.Equal(t, tc.output, output.String())
		require.Empty(t, errput.String())
	}
	var output, errput bytes.Buffer
	err := run(context.Background(), []string{"origin", "keybase://private/alice/test"},
		strings.NewReader("list\n"), &output, &errput)
	require.ErrorContains(t, err, "KEYBASE_GIT_USERNAME")
	require.Empty(t, output.String())
}

func TestGitUsernameConfigurationIsUsedOnlyForRequests(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("KEYBASE_GIT_USERNAME", "")
	t.Setenv("KEYBASE_GIT_AUTH_MODE", "auto")
	t.Setenv("KEYBASE_GIT_PAPER_KEY_FD", "")
	t.Setenv("KEYBASE_GIT_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("KEYBASE_CONFIG_FILE", filepath.Join(root, "missing.json"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	require.NoError(t, exec.Command("git", "config", "--global", "keybase.username", "alice").Run())
	var output, errput bytes.Buffer
	err := run(context.Background(), []string{"origin", "keybase://private/alice/test"},
		strings.NewReader("list\n"), &output, &errput)
	require.ErrorContains(t, err, "no provisioned Keybase device for alice")
	require.Empty(t, output.String())
	require.NotContains(t, errput.String(), "Keybase paper key:")
	// An invalid Git config must not affect capability negotiation.
	require.NoError(t, os.WriteFile(filepath.Join(root, "gitconfig"), []byte("[invalid\n"), 0o600))
	require.NoError(t, run(context.Background(), []string{"origin", "keybase://private/alice/test"},
		strings.NewReader("capabilities\n\n"), io.Discard, io.Discard))
}

func TestReadPaperKeyDescriptor(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{" example paper key \n", true}, {" \n", false}, {strings.Repeat("x", 4097), false},
	} {
		file, err := os.CreateTemp(t.TempDir(), "key")
		require.NoError(t, err)
		_, err = io.WriteString(file, tc.value)
		require.NoError(t, err)
		_, err = file.Seek(0, io.SeekStart)
		require.NoError(t, err)
		var prompts bytes.Buffer
		secret, err := readPaperKey(context.Background(), int(file.Fd()), &prompts)
		if tc.ok {
			require.NoError(t, err)
			require.Equal(t, strings.TrimSpace(tc.value), secret)
		} else {
			require.Error(t, err)
			require.Empty(t, secret)
		}
		require.Empty(t, prompts.String())
		_ = file.Close()
	}
}

func TestIsolatedIdentityConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KEYBASE_CONFIG_FILE", "/must-not-read-desktop-config")
	t.Setenv("KEYBASE_DB_FILE", "/must-not-open-desktop-db")
	t.Setenv("KEYBASE_CHAT_DB_FILE", "/must-not-open-chat-db")
	g := externals.NewGlobalContextInit()
	defer g.Shutdown(libkb.NewMetaContextBackground(g))
	require.NoError(t, configureIdentity(g, root))
	for _, path := range []string{g.Env.GetConfigFilename(), g.Env.GetDbFilename(),
		g.Env.GetChatDbFilename(), g.Env.GetSessionFilename()} {
		require.True(t, strings.HasPrefix(path, root+string(os.PathSeparator)), path)
	}
	require.Nil(t, g.SecretStore())
	require.False(t, g.ActiveDevice.Valid())
	// This is the same pre-login lookup that crashed with a nil UIDMapper.
	// The official hardcoded mapping keeps the regression test offline.
	username, err := g.GetUPAKLoader().LookupUsername(context.Background(),
		keybase1.UID("dbb165b7879fe7b1174df73bed0b9500"))
	require.NoError(t, err)
	require.Equal(t, libkb.NewNormalizedUsername("max"), username)
}

func TestInitializeFailureCleansUp(t *testing.T) {
	// An invalid API URI fails library configuration without making a request.
	t.Setenv("KEYBASE_SERVER_URI", "://invalid")
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	key, err := os.CreateTemp(t.TempDir(), "test-key")
	require.NoError(t, err)
	defer key.Close()
	_, err = io.WriteString(key, "not a real paper key")
	require.NoError(t, err)
	_, err = key.Seek(0, io.SeekStart)
	require.NoError(t, err)
	config, cleanup, err := initialize(context.Background(), identityOptions{
		username: "testuser", mode: "oneshot", paperFD: int(key.Fd()),
	}, false, io.Discard)
	require.Error(t, err)
	require.Nil(t, config)
	require.Nil(t, cleanup, "failed initialization already releases its resources")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "failed initialization must remove its temporary home")
}

func TestLivePublicIdentityLookup(t *testing.T) {
	username := os.Getenv("KEYBASE_GIT_LIVE_TEST_USERNAME")
	if username == "" {
		t.Skip("set KEYBASE_GIT_LIVE_TEST_USERNAME to check public identity loading against Keybase")
	}
	g := externals.NewGlobalContextInit()
	defer g.Shutdown(libkb.NewMetaContextBackground(g))
	require.NoError(t, configureIdentity(g, t.TempDir()))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	user, _, err := g.GetUPAKLoader().LoadV2(
		libkb.NewLoadUserArgWithMetaContext(libkb.NewMetaContext(ctx, g)).WithName(username))
	require.NoError(t, err)
	require.Equal(t, username, user.Current.Username)
	require.False(t, user.Current.Uid.IsNil())
	require.False(t, g.ActiveDevice.Valid(), "public identity lookup must not log in")
}

func TestLocalServiceAccessIsRejected(t *testing.T) {
	c := &noLocalServiceContext{}
	require.ErrorIs(t, c.CheckService(), errLocalService)
	_, _, _, err := c.GetSocket(false)
	require.ErrorIs(t, err, errLocalService)
	_, _, _, err = c.GetKBFSSocket(false)
	require.ErrorIs(t, err, errLocalService)
	_, err = c.BindToKBFSSocket()
	require.ErrorIs(t, err, errLocalService)
}

func TestReadSecretCancellationDoesNotWaitForReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := readSecret(ctx, func() ([]byte, error) {
			close(started)
			<-release
			defer close(readDone)
			return []byte("late paper key"), nil
		})
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("credential read ignored cancellation")
	}
	close(release)
	<-readDone
}
