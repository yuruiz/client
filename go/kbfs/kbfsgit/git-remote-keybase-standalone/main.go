// Copyright 2026 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

// A daemonless remote helper. Install this binary as git-remote-keybase to use
// existing keybase:// remotes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/keybase/client/go/kbfs/kbfsgit"
	"github.com/keybase/client/go/kbfs/libkbfs"
)

// Set by release builds; the Keybase version is reported separately.
var buildVersion = "dev"

func run(ctx context.Context, args []string, input io.Reader, output, errput io.Writer) error {
	flags := flag.NewFlagSet("git-remote-keybase", flag.ContinueOnError)
	flags.SetOutput(errput)
	username := flags.String("username", os.Getenv("KEYBASE_GIT_USERNAME"), "Keybase username")
	modeDefault := os.Getenv("KEYBASE_GIT_AUTH_MODE")
	if modeDefault == "" {
		modeDefault = "auto"
	}
	authMode := flags.String("auth-mode", modeDefault, "auto, existing, device or oneshot")
	keybaseHome := flags.String("keybase-home", os.Getenv("KEYBASE_GIT_KEYBASE_HOME"), "home used by the official Keybase client (default: normal Keybase home)")
	cacheDir := flags.String("cache-dir", os.Getenv("KEYBASE_GIT_CACHE_DIR"), "parent directory for this helper's persistent identity caches")
	stateDir := flags.String("state-dir", os.Getenv("KEYBASE_GIT_STATE_DIR"), "parent directory for this helper's dedicated devices")
	setup := flags.Bool("setup", false, "authorize a dedicated device once using a Paper Key")
	deviceName := flags.String("device-name", "", "public device name for --setup")
	paperFD := flags.Int("paper-key-fd", -1, "read a paper key from an inherited descriptor >= 3 (default: terminal prompt)")
	version := flags.Bool("version", false, "print the Keybase build version")
	diagnostics := flags.Bool("diagnostics", false, "report initialization timing and goroutine stacks if a repository request takes over 30 seconds")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *version {
		_, err := fmt.Fprintf(output, "git-remote-keybase-standalone %s (Keybase %s)\n", buildVersion, libkbfs.VersionString())
		return err
	}
	if *authMode != "auto" && *authMode != "existing" && *authMode != "device" && *authMode != "oneshot" {
		return errors.New("auth-mode must be auto, existing, device or oneshot")
	}
	// Git invokes the helper without flags, so the descriptor can also be
	// selected through the environment. The secret itself is never an argument.
	if *paperFD == -1 {
		if fd := os.Getenv("KEYBASE_GIT_PAPER_KEY_FD"); fd != "" {
			var err error
			*paperFD, err = strconv.Atoi(fd)
			if err != nil {
				return errors.New("KEYBASE_GIT_PAPER_KEY_FD must be a descriptor number")
			}
		}
	}
	if *paperFD != -1 && *paperFD < 3 {
		return errors.New("paper-key-fd must be >= 3; stdin/stdout are reserved for Git")
	}
	if !*setup && *authMode != "oneshot" && *paperFD != -1 {
		return errors.New("paper-key-fd is only valid with --setup or --auth-mode=oneshot; device authentication never asks for a Paper Key")
	}
	identity := identityOptions{
		username: strings.ToLower(strings.TrimSpace(*username)), mode: *authMode,
		paperFD: *paperFD, keybaseHome: *keybaseHome, cacheDir: *cacheDir, stateDir: *stateDir,
	}
	positional := flags.Args()
	if *setup {
		if len(positional) != 0 || (*authMode != "auto" && *authMode != "device") || strings.TrimSpace(*deviceName) == "" {
			return errors.New("usage: git-remote-keybase --setup --username USER --device-name NAME [--paper-key-fd FD] (no remote argument)")
		}
		return setupDevice(ctx, identity, strings.TrimSpace(*deviceName), errput)
	}
	if *deviceName != "" {
		return errors.New("device-name is only valid with --setup")
	}
	// Some Git LFS versions pass the transfer-agent invocation as one argument.
	if len(positional) == 1 && strings.HasPrefix(positional[0], "lfs ") {
		positional = strings.Fields(positional[0])
	}
	lfs := len(positional) > 0 && positional[0] == "lfs"
	if lfs {
		positional = positional[1:]
	}
	if len(positional) < 1 || len(positional) > 2 {
		return errors.New("usage: git-remote-keybase [--username USER] [--auth-mode auto|existing|device|oneshot] [lfs] <remote> [keybase://<type>/<folder>/<repo>]")
	}
	remote, repo := positional[0], positional[0]
	if len(positional) == 2 {
		repo = positional[1]
	}
	if !strings.HasPrefix(repo, "keybase://") {
		return errors.New("expected a keybase:// repository URL")
	}
	_, folder, name, err := kbfsgit.ParseRepo(repo)
	if err != nil {
		return err
	}
	if folder == "" || name == "" || folder == "." || folder == ".." || name == "." || name == ".." {
		return errors.New("repository URL must contain a folder and repository name")
	}
	options := kbfsgit.StartOptions{
		Remote: remote, Repo: repo, GitDir: filepath.FromSlash(os.Getenv("GIT_DIR")), LFS: lfs,
	}
	var diagnosticTimer *time.Timer
	var diagnosticDone chan struct{}
	defer func() {
		if diagnosticTimer != nil && !diagnosticTimer.Stop() {
			<-diagnosticDone
		}
	}()
	return kbfsgit.StartOnDemand(ctx, options, input, output, errput,
		func(ctx context.Context) (libkbfs.Config, func() error, error) {
			if identity.username == "" {
				username, err := gitConfiguredUsername(ctx)
				if err != nil {
					return nil, nil, err
				}
				identity.username = username
			}
			started := time.Now()
			if *diagnostics {
				_, _ = fmt.Fprintln(errput, "diagnostics: starting identity and KBFS initialization")
				diagnosticDone = make(chan struct{})
				diagnosticTimer = time.AfterFunc(30*time.Second, func() {
					defer close(diagnosticDone)
					_, _ = fmt.Fprintln(errput, "diagnostics: request still active after 30s; goroutine stacks (no argument values)")
					// debug=1 emits symbolic stacks rather than argument values.
					_ = pprof.Lookup("goroutine").WriteTo(errput, 1)
				})
			}
			config, cleanup, err := initialize(ctx, identity, lfs, errput)
			if *diagnostics {
				_, _ = fmt.Fprintf(errput, "diagnostics: initialization returned after %s (error=%v)\n",
					time.Since(started).Round(time.Millisecond), err)
			}
			return config, cleanup, err
		})
}

func gitConfiguredUsername(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "git", "config", "--get", "keybase.username").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil // No value configured.
		}
		return "", fmt.Errorf("reading git config keybase.username: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(string(output))), nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "git-remote-keybase: %v\n", err)
		os.Exit(1)
	}
}
