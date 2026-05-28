// Package handlers — registry (UPDATED for step 8).
//
// Adds AIHandler for /w ai / /d ai / /v ai commands.
package handlers

import (
	"context"
	"database/sql"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/cert/acme"
	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/peers"
)

type Dependencies struct {
	CertStore    *cert.Store
	ACMEClient   *acme.Client
	ChallengeDir string

	DB             *sql.DB
	ProgramVersion string

	PeerMgr *peers.Manager

	NotifyAll         func(ctx context.Context) error
	PeerStateProvider func() map[string]PeerHeartbeatState
	PendingResolver   func(id string) error
	OnUpgrade         func(ctx context.Context) error

	LocalNodeIP string

	// Step 8: identity.json path (used by AIHandler to load/save AI config)
	IdentityPath string

	// Step 10: SSL routing wiring. Optional; when nil, SSLHandler degrades
	// to single-node behaviour (works fine for clusters where the bot node
	// owns every IP/domain it issues).
	SSLPeerIPs            func() ([]string, error)
	SSLDelegateExec       func(ctx context.Context, peerIP, command string) (output string, ok bool, err error)
	SSLChallengeBroadcast func(ctx context.Context, peerIPs []string, token, keyAuth string) error
	SSLChallengeCleanup   func(ctx context.Context, peerIPs []string, token string)
	SSLCertBroadcast      func(ctx context.Context, certPEM, keyPEM []byte, source string)
}

func DefaultRegistry() command.Registry {
	r := command.Registry{}
	r["domain"] = &DomainHandler{}
	return r
}

func NewRegistry(deps Dependencies) command.Registry {
	r := command.Registry{}

	// A class
	r["domain"] = &DomainHandler{}
	r["ssl"] = &SSLHandler{
		CertStore:    deps.CertStore,
		ACMEClient:   deps.ACMEClient,
		ChallengeDir: deps.ChallengeDir,

		LocalNodeIP:        deps.LocalNodeIP,
		PeerIPs:            deps.SSLPeerIPs,
		DelegateExec:       deps.SSLDelegateExec,
		ChallengeBroadcast: deps.SSLChallengeBroadcast,
		ChallengeCleanup:   deps.SSLChallengeCleanup,
		CertBroadcast:      deps.SSLCertBroadcast,
	}
	r["sslfile"] = &SSLFileHandler{CertStore: deps.CertStore}

	// Internal
	r["internal"] = &InternalHandler{PeerMgr: deps.PeerMgr}

	// Cluster metadata
	r["export"] = &ExportHandler{DB: deps.DB, ProgramVersion: deps.ProgramVersion}
	r["status"] = &StatusHandler{
		NodeIP:         deps.LocalNodeIP,
		ProgramVersion: deps.ProgramVersion,
		PeerMgr:        deps.PeerMgr,
	}
	r["nodes"] = &NodesHandler{
		PeerMgr:           deps.PeerMgr,
		PeerStateProvider: deps.PeerStateProvider,
	}
	r["stats"] = &StatsHandler{}

	// System actions
	r["sync"] = &SyncHandler{NotifyAll: deps.NotifyAll}
	r["target"] = &TargetHandler{}
	r["upgrade"] = &UpgradeHandler{OnUpgrade: deps.OnUpgrade}
	r["help"] = &HelpHandler{}
	r["menu"] = &MenuHandler{}
	r["confirm"] = &ConfirmHandler{PendingResolver: deps.PendingResolver}

	// B class — rule objects
	r["cache"] = &CacheHandler{}
	r["defense"] = &DefenseHandler{}
	r["redirect"] = &RedirectHandler{}
	r["header"] = &HeaderHandler{}

	// C class — bindings
	r["bind"] = &BindHandler{}

	// Step 8: AI
	r["ai"] = &AIHandler{IdentityPath: deps.IdentityPath}

	return r
}
