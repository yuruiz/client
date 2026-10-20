package kbfsgit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keybase/client/go/kbfs/data"
	"github.com/keybase/client/go/kbfs/libfs"
	"github.com/keybase/client/go/kbfs/libgit"
	"github.com/keybase/client/go/kbfs/libkbfs"
	"github.com/keybase/client/go/kbfs/tlf"
	"github.com/keybase/client/go/kbfs/tlfhandle"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/stretchr/testify/require"
)

func TestOnDemandNegotiationDoesNotInitialize(t *testing.T) {
	for _, tc := range []struct {
		name, input, output string
		lfs                 bool
	}{
		{"EOF", "", "", false},
		{"quit", "\n", "", false},
		{"capabilities", "capabilities\n\n", "fetch\npush\noption\n\n", false},
		{"options", "option verbosity 0\noption progress false\noption cloning true\noption depth 1\n\n", "ok\nok\nok\nunsupported\n", false},
		{"LFS", "{\"event\":\"init\"}\n{\"event\":\"terminate\"}\n", "{}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := StartOnDemand(context.Background(), StartOptions{
				Repo: "keybase://private/alice/test", LFS: tc.lfs,
			}, strings.NewReader(tc.input), &output, &bytes.Buffer{},
				func(context.Context) (libkbfs.Config, func() error, error) {
					t.Fatal("protocol negotiation must not authenticate")
					return nil, nil, nil
				})
			require.NoError(t, err)
			require.Equal(t, tc.output, output.String())
		})
	}
}

func TestOnDemandLFSUpload(t *testing.T) {
	ctx, config, journalRoot := initConfigForRunner(t)
	defer os.RemoveAll(journalRoot)
	defer libkbfs.CheckConfigAndShutdown(ctx, t, config)
	h, err := tlfhandle.ParseHandle(ctx, config.KBPKI(), config.MDOps(), config, "user1", tlf.Private)
	require.NoError(t, err)
	_, err = libgit.CreateRepoAndID(ctx, config, h, "test")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "lfs.bin")
	require.NoError(t, os.WriteFile(path, []byte("large object"), 0o600))
	const oid = "standalone-lfs-test"
	request, err := json.Marshal(lfsRequest{Event: gitLFSUploadEvent, Oid: oid, Size: 12, Path: path})
	require.NoError(t, err)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		_, _ = io.WriteString(writer, "{\"event\":\"init\"}\n"+string(request)+"\n{\"event\":\"terminate\"}\n")
	}()
	var output bytes.Buffer
	calls := 0
	err = StartOnDemand(context.Background(), StartOptions{
		Remote: "origin", Repo: "keybase://private/user1/test", LFS: true,
	}, reader, &output, testErrput{t}, func(context.Context) (libkbfs.Config, func() error, error) {
		calls++
		require.Equal(t, "{}\n", output.String(), "LFS init must finish before authentication")
		return config, nil, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Contains(t, output.String(), `{"event":"complete","oid":"`+oid+`"}`)
	fs, err := libfs.NewFS(ctx, config, h, data.MasterBranch,
		fmt.Sprintf("%s/test/%s", kbfsRepoDir, libgit.LFSSubdir), "", keybase1.MDPriorityGit)
	require.NoError(t, err)
	file, err := fs.Open(oid)
	require.NoError(t, err)
	defer file.Close()
	contents, err := io.ReadAll(file)
	require.NoError(t, err)
	require.Equal(t, "large object", string(contents))
}

func TestOnDemandCompatiblePushAndClone(t *testing.T) {
	ctx, config, journalRoot := initConfigForRunner(t)
	defer os.RemoveAll(journalRoot)
	defer libkbfs.CheckConfigAndShutdown(ctx, t, config)
	h, err := tlfhandle.ParseHandle(ctx, config.KBPKI(), config.MDOps(), config, "user1", tlf.Private)
	require.NoError(t, err)
	_, err = libgit.CreateRepoAndID(ctx, config, h, "test")
	require.NoError(t, err)

	source := t.TempDir()
	makeLocalRepoWithOneFile(t, source, "hello.txt", "compatible encrypted git\n", "")
	gitExec(t, filepath.Join(source, ".git"), source, "branch", "-M", "master")
	run := func(gitDir, commands string) string {
		t.Helper()
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		go func() { _, _ = io.WriteString(writer, commands) }()
		var output bytes.Buffer
		initialized, closed := 0, 0
		err := StartOnDemand(context.Background(), StartOptions{
			Remote: "origin", Repo: "keybase://private/user1/test", GitDir: filepath.Join(gitDir, ".git"),
		}, reader, &output, testErrput{t},
			func(context.Context) (libkbfs.Config, func() error, error) {
				initialized++
				return config, func() error { closed++; return nil }, nil
			})
		require.NoError(t, err)
		require.Equal(t, 1, initialized)
		require.Equal(t, 1, closed)
		return output.String()
	}
	require.Contains(t, run(source, "option verbosity 0\npush refs/heads/master:refs/heads/master\n\n\n"), "ok refs/heads/master\n")
	// Read the new helper's push through the original official runner.
	testListAndGetHeads(ctx, t, config, source, []string{"refs/heads/master", "HEAD"})
	// Update through the official runner, then clone through the new entry.
	addOneFileToRepo(t, source, "second.txt", "official runner\n")
	testPush(ctx, t, config, source, "refs/heads/master:refs/heads/master")
	heads := testListAndGetHeads(ctx, t, config, source, []string{"refs/heads/master", "HEAD"})
	dest := t.TempDir()
	gitExec(t, filepath.Join(dest, ".git"), dest, "init")
	response := run(dest, fmt.Sprintf("option cloning true\noption verbosity 0\nfetch %s refs/heads/master\n\n\n", heads[0]))
	require.Equal(t, "ok\nok\n\n", response)
	gitExec(t, filepath.Join(dest, ".git"), dest, "checkout", heads[0])
	contents, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, "compatible encrypted git\n", string(contents))
	contents, err = os.ReadFile(filepath.Join(dest, "second.txt"))
	require.NoError(t, err)
	require.Equal(t, "official runner\n", string(contents))
}

func TestOnDemandOperationInitializesAndCleansUpFailure(t *testing.T) {
	authErr := errors.New("authentication failed")
	closeErr := errors.New("cleanup failed")
	for _, command := range []string{"list\n", "list for-push\n", "fetch hash refs/heads/main\n", "push HEAD:refs/heads/main\n"} {
		t.Run(strings.TrimSpace(command), func(t *testing.T) {
			calls, closed := 0, 0
			err := StartOnDemand(context.Background(), StartOptions{
				Repo: "keybase://team/example/test",
			}, strings.NewReader("capabilities\n"+command), &bytes.Buffer{}, &bytes.Buffer{},
				func(context.Context) (libkbfs.Config, func() error, error) {
					calls++
					return nil, func() error { closed++; return closeErr }, authErr
				})
			require.ErrorIs(t, err, authErr)
			require.ErrorIs(t, err, closeErr)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, closed)
		})
	}
}

func TestOnDemandRejectsInvalidCommandsBeforeAuthentication(t *testing.T) {
	for _, command := range []string{"unknown\n", "list typo\n", "push\n", "fetch\n", "option verbosity x\n", "capabilities extra\n", "list"} {
		t.Run(command, func(t *testing.T) {
			err := StartOnDemand(context.Background(), StartOptions{
				Repo: "keybase://public/alice/test",
			}, strings.NewReader(command), &bytes.Buffer{}, &bytes.Buffer{},
				func(context.Context) (libkbfs.Config, func() error, error) {
					t.Fatal("invalid commands must not authenticate")
					return nil, nil, nil
				})
			require.Error(t, err)
		})
	}
}
