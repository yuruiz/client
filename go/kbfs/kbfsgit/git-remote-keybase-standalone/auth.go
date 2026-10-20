package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/keybase/go-crypto/ssh/terminal"
)

func readPaperKey(ctx context.Context, fd int, errput io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var secret []byte
	if fd >= 3 {
		file := os.NewFile(uintptr(fd), "paper-key")
		if file == nil {
			return "", errors.New("invalid paper-key descriptor")
		}
		defer closeSecretInput(ctx, file)
		var err error
		secret, err = readSecret(ctx, func() ([]byte, error) {
			return io.ReadAll(io.LimitReader(file, 4097))
		})
		if err != nil {
			return "", fmt.Errorf("reading paper-key descriptor: %w", err)
		}
		if len(secret) > 4096 {
			clear(secret)
			return "", errors.New("paper key exceeds 4096 bytes")
		}
	} else {
		path := "/dev/tty"
		if runtime.GOOS == "windows" {
			path = "CONIN$"
		}
		tty, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return "", errors.New("no terminal available; provide a paper key using KEYBASE_GIT_PAPER_KEY_FD")
		}
		defer closeSecretInput(ctx, tty)
		state, err := terminal.GetState(int(tty.Fd()))
		if err != nil {
			return "", err
		}
		// Restore echo even if cancellation returns before ReadPassword does.
		defer terminal.Restore(int(tty.Fd()), state)
		if _, err := fmt.Fprint(errput, "Keybase paper key: "); err != nil {
			return "", err
		}
		secret, err = readSecret(ctx, func() ([]byte, error) {
			return terminal.ReadPassword(int(tty.Fd()))
		})
		_, _ = fmt.Fprintln(errput)
		if err != nil {
			return "", fmt.Errorf("reading paper key: %w", err)
		}
	}
	defer clear(secret)
	key := strings.TrimSpace(string(secret))
	if key == "" {
		return "", errors.New("paper key is empty")
	}
	return key, nil
}

func closeSecretInput(ctx context.Context, file *os.File) {
	if ctx.Err() != nil {
		// On macOS, closing a terminal with a blocked read can itself block.
		// Echo has already been restored; let process exit finish releasing
		// the descriptor rather than holding up cancellation.
		go func() { _ = file.Close() }()
		return
	}
	_ = file.Close()
}

// Inherited descriptors need not be pollable; Close alone cannot reliably
// interrupt their reads. The command can exit on cancellation while a read
// unwinds. A late result is wiped instead of delivered to an abandoned caller.
func readSecret(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	type result struct {
		secret []byte
		err    error
	}
	done := make(chan result)
	go func() {
		secret, err := read()
		select {
		case done <- result{secret: secret, err: err}:
		case <-ctx.Done():
			clear(secret)
		}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			clear(r.secret)
			return nil, r.err
		}
		return r.secret, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
