package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/keybase/client/go/kbconst"
	"github.com/keybase/client/go/kbfs/env"
	"github.com/keybase/client/go/kbfs/idutil"
	"github.com/keybase/client/go/kbfs/libgit"
	"github.com/keybase/client/go/kbfs/libkbfs"
	"github.com/keybase/client/go/kbfs/tlf"
	"github.com/keybase/client/go/kbfs/tlfhandle"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/client/go/service"
	"github.com/keybase/client/go/teams"
	"github.com/keybase/client/go/uidmap"
	"github.com/keybase/go-framed-msgpack-rpc/rpc"
)

// A separate home prevents the helper from reading, locking or modifying the
// desktop client's databases, config, sessions and device keyring.
type isolatedCommandLine struct {
	libkb.NullConfiguration
	root string
}

func (c isolatedCommandLine) GetHome() string { return c.root }
func (c isolatedCommandLine) GetRunMode() (kbconst.RunMode, error) {
	return kbconst.ProductionRunMode, nil
}
func (c isolatedCommandLine) GetDebug() (bool, bool)       { return false, true }
func (c isolatedCommandLine) GetConfigFilename() string    { return filepath.Join(c.root, "config.json") }
func (c isolatedCommandLine) GetGUIConfigFilename() string { return filepath.Join(c.root, "gui.json") }
func (c isolatedCommandLine) GetDbFilename() string        { return filepath.Join(c.root, "identity.leveldb") }
func (c isolatedCommandLine) GetChatDbFilename() string    { return filepath.Join(c.root, "chat.leveldb") }
func (c isolatedCommandLine) GetSecretKeyringTemplate() string {
	return filepath.Join(c.root, libkb.SecretKeyringTemplate)
}
func (c isolatedCommandLine) GetSessionFilename() string {
	return filepath.Join(c.root, "session.json")
}
func (c isolatedCommandLine) GetDeviceCloneStateFilename() string {
	return filepath.Join(c.root, "device-clone.json")
}
func (c isolatedCommandLine) GetUseRootConfigFile() (bool, bool) { return false, true }

func configureIdentity(g *libkb.GlobalContext, root string) error {
	g.SetCommandLine(isolatedCommandLine{root: root})
	// Keep the default stderr logger: Init already starts identity maintenance
	// goroutines that can use it. Do not reconfigure it concurrently, initialize
	// an OS secret store, or redirect the helper's stderr.
	if err := g.ConfigureUsage(libkb.Usage{Config: true, API: true}); err != nil {
		return err
	}
	installIdentityLibraries(g)
	return nil
}

func installIdentityLibraries(g *libkb.GlobalContext) {
	// Match keybase/main.go's configOtherLibraries. CachedUPAKLoader uses
	// UIDMapper even before login, when resolving the supplied username.
	g.SetUIDMapper(uidmap.NewUIDMap(g.Env.GetUIDMapFullNameCacheSize()))
	g.SetServiceSummaryMapper(uidmap.NewServiceSummaryMap(1000))
	g.ConnectionManager = libkb.NewConnectionManager()
	g.NotifyRouter = libkb.NewNotifyRouter(g)
	g.UIRouter = service.NewUIRouter(g)
	teams.ServiceInit(g)
}

func initialize(ctx context.Context, options identityOptions, lfs bool, errput io.Writer) (
	config libkbfs.Config, cleanup func() error, retErr error,
) {
	if options.username == "" {
		return nil, nil, errors.New("set git config keybase.username, KEYBASE_GIT_USERNAME or --username before accessing a repository")
	}
	if !libkb.CheckUsername.F(options.username) {
		return nil, nil, errors.New("invalid Keybase username")
	}
	var g *libkb.GlobalContext
	var closeSession func() error
	var err error
	switch options.mode {
	case "auto":
		// Explicit setup selects the dedicated device for future invocations.
		// Until then, reuse the official client's device without provisioning.
		g, closeSession, err = openStandaloneIdentity(ctx, options, false, standaloneDeviceSecretStore)
		if errors.Is(err, errNoStandaloneDevice) {
			standaloneErr := err
			g, closeSession, err = openExistingIdentity(ctx, options, existingDeviceSecretStore)
			if err != nil {
				err = errors.Join(standaloneErr, err)
			}
		}
	case "existing":
		g, closeSession, err = openExistingIdentity(ctx, options, existingDeviceSecretStore)
	case "oneshot":
		g, closeSession, err = openOneshotIdentity(ctx, options, errput)
	case "device":
		g, closeSession, err = openStandaloneIdentity(ctx, options, false, standaloneDeviceSecretStore)
	default:
		return nil, nil, errors.New("auth-mode must be auto, existing, device or oneshot")
	}
	if err != nil {
		return nil, nil, err
	}
	var operationRoot string
	cleanup = func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var configErr, removeErr error
		if config != nil {
			configErr = config.Shutdown(shutdownCtx)
		}
		sessionErr := closeSession()
		if operationRoot != "" {
			removeErr = os.RemoveAll(operationRoot)
		}
		return errors.Join(configErr, sessionErr, removeErr)
	}
	initialized := false
	shutdown := cleanup
	defer func() {
		if !initialized {
			retErr = errors.Join(retErr, shutdown())
			cleanup = nil
		}
	}()
	if options.mode != "oneshot" {
		if err := authenticateExistingIdentity(ctx, g); err != nil {
			return nil, nil, err
		}
	}
	operationRoot, err = os.MkdirTemp("", "keybase-git-operation-")
	if err != nil {
		return nil, nil, err
	}
	kbCtx := &noLocalServiceContext{KBFSContext: env.NewContextFromGlobalContext(g)}
	params, _, err := libgit.Params(kbCtx, operationRoot, nil)
	if err != nil {
		return nil, nil, err
	}
	// Git's journal is per-operation; identity and audit caches survive in
	// existing-device mode. No cache is served by the desktop KBFS process.
	params.DiskCacheMode = libkbfs.DiskCacheModeOff
	params.Debug, params.LogToFile = false, false
	params.LogFileConfig.Path = ""
	params.SkipSignalHandling = true
	if lfs {
		params.TLFJournalBackgroundWorkStatus = libkbfs.TLFJournalBackgroundWorkEnabled
	}
	factory := inProcessFactory{clients: service.NewKBFSInProcessClients(g)}
	config, err = libkbfs.InitWithLogPrefix(ctx, kbCtx, params, factory, nil, g.Log, "git")
	if err != nil {
		return nil, nil, err
	}
	config.SetDefaultBlockType(keybase1.BlockType_GIT)
	initialized = true
	return config, cleanup, nil
}

var errLocalService = errors.New("local Keybase services are disabled in the standalone helper")

type noLocalServiceContext struct {
	*env.KBFSContext
}

func (*noLocalServiceContext) CheckService() error { return errLocalService }
func (*noLocalServiceContext) GetSocket(bool) (net.Conn, rpc.Transporter, bool, error) {
	return nil, nil, false, errLocalService
}
func (*noLocalServiceContext) GetKBFSSocket(bool) (net.Conn, rpc.Transporter, bool, error) {
	return nil, nil, false, errLocalService
}
func (*noLocalServiceContext) BindToKBFSSocket() (net.Listener, error) {
	return nil, errLocalService
}

type inProcessFactory struct {
	clients service.KBFSInProcessClients
}

var _ libkbfs.KeybaseServiceCn = inProcessFactory{}

func (f inProcessFactory) NewKeybaseService(config libkbfs.Config,
	_ libkbfs.InitParams, ctx libkbfs.Context, log logger.Logger,
) (libkbfs.KeybaseService, error) {
	c := f.clients
	base := libkbfs.NewKeybaseServiceBase(config, ctx, log)
	base.FillClients(c.Identify, c.User, c.Teams, c.Merkle, c.Session,
		c.Favorite, c.KBFS, c.Mount, c.Git, c.KVStore)
	return &inProcessKeybase{KeybaseServiceBase: base, session: c.Session}, nil
}

func (f inProcessFactory) NewCrypto(config libkbfs.Config,
	_ libkbfs.InitParams, _ libkbfs.Context, _ logger.Logger,
) (libkbfs.Crypto, error) {
	return libkbfs.NewCryptoClient(config, f.clients.Crypto, f.clients.Teams), nil
}

func (f inProcessFactory) NewChat(_ libkbfs.Config,
	_ libkbfs.InitParams, _ libkbfs.Context, _ logger.Logger,
) (libkbfs.Chat, error) {
	return disabledChat{}, nil
}

type inProcessKeybase struct {
	*libkbfs.KeybaseServiceBase
	session keybase1.SessionInterface
}

// There is only one identity per helper invocation. Avoid the long-running
// client's login hooks (home-folder warmups and background auth refreshes).
func (k *inProcessKeybase) CurrentSession(ctx context.Context, id int) (idutil.SessionInfo, error) {
	session, err := k.session.CurrentSession(ctx, id)
	if err != nil {
		return idutil.SessionInfo{}, err
	}
	return idutil.SessionInfoFromProtocol(session)
}

func (*inProcessKeybase) Shutdown() {}

type disabledChat struct{}

var errChatDisabled = errors.New("chat is unavailable in the standalone Git helper")

func (disabledChat) GetConversationID(context.Context, tlf.CanonicalName, tlf.Type, string, chat1.TopicType) (chat1.ConversationID, error) {
	return nil, errChatDisabled
}
func (disabledChat) SendTextMessage(context.Context, tlf.CanonicalName, tlf.Type, chat1.ConversationID, string) error {
	return errChatDisabled
}
func (disabledChat) GetGroupedInbox(context.Context, chat1.TopicType, int) ([]*tlfhandle.Handle, error) {
	return nil, errChatDisabled
}
func (disabledChat) GetChannels(context.Context, tlf.CanonicalName, tlf.Type, chat1.TopicType) ([]chat1.ConversationID, []string, error) {
	return nil, nil, errChatDisabled
}
func (disabledChat) ReadChannel(context.Context, chat1.ConversationID, []byte) ([]string, []byte, error) {
	return nil, nil, errChatDisabled
}
func (disabledChat) RegisterForMessages(chat1.ConversationID, libkbfs.ChatChannelNewMessageCB) {}
func (disabledChat) ClearCache()                                                               {}
