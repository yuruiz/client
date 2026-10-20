package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"github.com/keybase/client/go/kbcrypto"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/nacl/box"
)

func TestKBFSInProcessCrypto(t *testing.T) {
	g := libkb.NewGlobalContextInit()
	m := libkb.NewMetaContext(context.Background(), g)
	defer g.Shutdown(m)
	signing, err := libkb.GenerateNaclSigningKeyPair()
	require.NoError(t, err)
	encryption, err := libkb.GenerateNaclDHKeyPair()
	require.NoError(t, err)
	g.ActiveDevice = libkb.NewProvisionalActiveDevice(m, keybase1.UserVersion{
		Uid: keybase1.MakeTestUID(1),
	}, "", signing, encryption, "test paper key", libkb.KeychainModeNone)
	// No API, UI router, transport, or local service is configured.
	client := NewKBFSInProcessClients(g).Crypto
	message := []byte("git authentication")
	sig, err := client.SignED25519(m.Ctx(), keybase1.SignED25519Arg{Msg: message})
	require.NoError(t, err)
	require.True(t, kbcrypto.NaclSigningKeyPublic(sig.PublicKey).Verify(message, kbcrypto.NaclSignature(sig.Sig)))
	_, err = client.SignED25519ForKBFS(m.Ctx(), keybase1.SignED25519ForKBFSArg{Msg: message})
	require.NoError(t, err)

	peerPub, peerPrivate, err := box.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var nonce [24]byte
	_, err = rand.Read(nonce[:])
	require.NoError(t, err)
	plain := bytes.Repeat([]byte{42}, 32)
	ciphertext := box.Seal(nil, plain, &nonce, (*[32]byte)(&encryption.Public), peerPrivate)
	var encrypted keybase1.EncryptedBytes32
	copy(encrypted[:], ciphertext)
	arg := keybase1.UnboxBytes32Arg{
		EncryptedBytes32: encrypted, Nonce: keybase1.BoxNonce(nonce), PeersPublicKey: keybase1.BoxPublicKey(*peerPub),
	}
	opened, err := client.UnboxBytes32(m.Ctx(), arg)
	require.NoError(t, err)
	require.Equal(t, plain, opened[:])
	bundle := keybase1.CiphertextBundle{
		Kid: encryption.GetKID(), Ciphertext: encrypted, Nonce: arg.Nonce, PublicKey: arg.PeersPublicKey,
	}
	anyResult, err := client.UnboxBytes32Any(m.Ctx(), keybase1.UnboxBytes32AnyArg{
		Bundles: []keybase1.CiphertextBundle{bundle},
	})
	require.NoError(t, err)
	require.Equal(t, plain, anyResult.Plaintext[:])
	bundle.Kid = signing.GetKID()
	_, err = client.UnboxBytes32Any(m.Ctx(), keybase1.UnboxBytes32AnyArg{
		Bundles: []keybase1.CiphertextBundle{bundle}, PromptPaper: true,
	})
	require.Error(t, err, "a missing key must not access a local keyring, API or UI")
	arg.EncryptedBytes32[0] ^= 1
	_, err = client.UnboxBytes32(m.Ctx(), arg)
	require.Error(t, err)
	require.NoError(t, g.ActiveDevice.Clear())
	_, err = client.SignED25519(m.Ctx(), keybase1.SignED25519Arg{Msg: message})
	require.Error(t, err)
}

func TestKBFSInProcessIdentityFailuresAreNotConfirmed(t *testing.T) {
	ui := &inProcessIdentifyUI{}
	for _, outcome := range []keybase1.IdentifyOutcome{
		{NumTrackFailures: 1}, {NumProofFailures: 1}, {NumRevoked: 1},
	} {
		confirmation, err := ui.Confirm(libkb.MetaContext{}, &outcome)
		require.Error(t, err)
		require.False(t, confirmation.IdentityConfirmed)
	}
	confirmation, err := ui.Confirm(libkb.MetaContext{}, &keybase1.IdentifyOutcome{})
	require.NoError(t, err)
	require.True(t, confirmation.IdentityConfirmed)
}
