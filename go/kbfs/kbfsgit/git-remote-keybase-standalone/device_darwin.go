//go:build darwin

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/keybase/client/go/libkb"
)

func existingDeviceSecretStore() (libkb.SecretStoreAll, error) {
	// Use the official native backend directly. Its retrieval is read-only and
	// does not fall back to storing a secret in a plaintext file.
	return libkb.KeychainSecretStore{}, nil
}

func standaloneDeviceSecretStore(root string) (libkb.SecretStoreAll, error) {
	digest := sha256.Sum256([]byte(root))
	return libkb.KeychainSecretStore{ServiceName: fmt.Sprintf("keybase-git-standalone-%x", digest[:])}, nil
}

func lockIdentityCache(ctx context.Context, root string) (func() error, error) {
	file, err := os.OpenFile(filepath.Join(root, "helper.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Keep the lock file in place: unlinking it after unlocking could
			// let two processes acquire locks on different inodes.
			return file.Close, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}
