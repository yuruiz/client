//go:build !darwin

package main

import (
	"context"
	"errors"

	"github.com/keybase/client/go/libkb"
)

var errExistingDevicePlatform = errors.New("existing-device authentication currently requires macOS Keychain; use --auth-mode=oneshot on this platform")

func existingDeviceSecretStore() (libkb.SecretStoreAll, error) {
	return nil, errExistingDevicePlatform
}

func standaloneDeviceSecretStore(string) (libkb.SecretStoreAll, error) {
	return nil, errExistingDevicePlatform
}

func lockIdentityCache(context.Context, string) (func() error, error) {
	return nil, errExistingDevicePlatform
}
