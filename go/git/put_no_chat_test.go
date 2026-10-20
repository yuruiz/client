package git

import (
	"context"
	"testing"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/stretchr/testify/require"
)

func TestSendChatWithoutChatRuntime(t *testing.T) {
	g := libkb.NewGlobalContext()
	// No API or ChatHelper: metadata publication must not resolve chat settings.
	require.NoError(t, sendChat(libkb.NewMetaContext(context.Background(), g), "", keybase1.PutGitMetadataArg{
		Folder: keybase1.FolderHandle{FolderType: keybase1.FolderType_TEAM},
	}))
}
