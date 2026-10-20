// Copyright 2026 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package service

import (
	"context"
	"errors"

	"github.com/keybase/client/go/chat/globals"
	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/offline"
	"github.com/keybase/client/go/protocol/keybase1"
)

// KBFSInProcessClients exposes the official handlers needed by a short-lived
// KBFS client. Calls execute directly; no service, listener or RPC connection is
// started. The caller owns the authenticated GlobalContext and its shutdown.
type KBFSInProcessClients struct {
	Identify keybase1.IdentifyInterface
	User     keybase1.UserInterface
	Teams    keybase1.TeamsInterface
	Merkle   keybase1.MerkleInterface
	Session  keybase1.SessionInterface
	Favorite keybase1.FavoriteInterface
	KBFS     keybase1.KbfsInterface
	Mount    keybase1.KbfsMountInterface
	Git      keybase1.GitInterface
	KVStore  keybase1.KvstoreInterface
	Crypto   keybase1.CryptoInterface
}

// NewKBFSInProcessClients builds handlers without running Service.Run or
// starting Gregor, chat, background login, or a local RPC server.
func NewKBFSInProcessClients(g *libkb.GlobalContext) KBFSInProcessClients {
	s := &Service{offlineRPCCache: offline.NewRPCCache(g)}
	identify := NewIdentifyHandler(nil, g, s)
	identify.localUIs = func() libkb.UIs {
		var breaks *keybase1.IdentifyTrackBreaks
		return libkb.UIs{
			LogUI: g.Log,
			IdentifyUI: &inProcessIdentifyUI{
				LoopbackIdentifyUI: engine.NewLoopbackIdentifyUI(g, &breaks),
			},
		}
	}
	return KBFSInProcessClients{
		Identify: identify,
		User:     NewUserHandler(nil, g, nil, s),
		Teams:    NewTeamsHandler(nil, 0, globals.NewContext(g, nil), s),
		Merkle:   newMerkleHandler(nil, g),
		Session:  NewSessionHandler(nil, g),
		Favorite: &inProcessFavorite{NewFavoriteHandler(nil, g), identify.localUIs},
		KBFS:     &inProcessKBFS{NewKBFSHandler(nil, g, nil, s)},
		Mount:    NewKBFSMountHandler(nil, g),
		Git:      NewGitHandler(nil, g),
		KVStore:  NewKVStoreHandler(nil, g),
		Crypto:   &inProcessCrypto{NewCryptoHandler(g)},
	}
}

type inProcessIdentifyUI struct {
	*engine.LoopbackIdentifyUI
}

type inProcessKBFS struct {
	*KBFSHandler
}

func (h *inProcessKBFS) FSEvent(_ context.Context, arg keybase1.FSNotification) error {
	// Single-operation clients have no chat conversations to rekey.
	h.G().NotifyRouter.HandleFSActivity(arg)
	return nil
}

func (i *inProcessIdentifyUI) Confirm(_ libkb.MetaContext,
	outcome *keybase1.IdentifyOutcome,
) (keybase1.ConfirmResult, error) {
	// Never accept a changed identity on behalf of a noninteractive Git caller.
	if outcome.NumTrackFailures > 0 || outcome.NumProofFailures > 0 ||
		outcome.NumRevoked > 0 || len(outcome.Revoked) > 0 {
		return keybase1.ConfirmResult{}, errors.New("identity changed; review the user's proofs before retrying")
	}
	return keybase1.ConfirmResult{IdentityConfirmed: true}, nil
}

type inProcessFavorite struct {
	*FavoriteHandler
	uis func() libkb.UIs
}

func (h *inProcessFavorite) FavoriteAdd(ctx context.Context, arg keybase1.FavoriteAddArg) error {
	eng := engine.NewFavoriteAdd(h.G(), &arg)
	return engine.RunEngine2(libkb.NewMetaContext(ctx, h.G()).WithUIs(h.uis()), eng)
}

type inProcessCrypto struct {
	*CryptoHandler
}

func (c *inProcessCrypto) UnboxBytes32Any(ctx context.Context,
	arg keybase1.UnboxBytes32AnyArg,
) (keybase1.UnboxAnyRes, error) {
	key, err := c.G().ActiveDevice.EncryptionKey()
	if err != nil {
		return keybase1.UnboxAnyRes{}, err
	}
	matches := false
	for _, bundle := range arg.Bundles {
		if bundle.Kid.Equal(key.GetKID()) {
			matches = true
			break
		}
	}
	if !matches {
		// The active device is the only key source. Do not fall back to
		// loading device keyrings or prompting for another paper device.
		return keybase1.UnboxAnyRes{}, libkb.NoSecretKeyError{}
	}
	return engine.UnboxBytes32Any(libkb.NewMetaContext(ctx, c.G()),
		func() libkb.SecretUI {
			return errorSecretUI{reason: "the active device cannot decrypt this folder"}
		}, arg)
}
