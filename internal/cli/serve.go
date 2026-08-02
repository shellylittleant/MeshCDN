// Daemon serve mode — UPDATED for step 8.
//
// Changes from step 7:
//   - Wires ai.Assistant into bot.Client when AI is configured in identity.json
//   - Periodically sweeps expired AI conversations
//   - On identity.json reload (after /w ai ...), bot picks up new AI config
//     on next message (we re-read identity per-message rather than caching)
package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/example/meshcdn/internal/ai"
	"github.com/example/meshcdn/internal/bot"
	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/cert/acme"
	"github.com/example/meshcdn/internal/cert/renew"
	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/command/handlers"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/identity"
	"github.com/example/meshcdn/internal/logs"
	"github.com/example/meshcdn/internal/mesh"
	"github.com/example/meshcdn/internal/peers"
	"github.com/example/meshcdn/internal/snapshot"
	"github.com/example/meshcdn/internal/version"
)

// PendingConfirmStore — same as step 7
type PendingConfirmStore struct {
	mu sync.Mutex
	m  map[string]pendingEntry
}

type pendingEntry struct {
	command   string
	createdAt time.Time
}

func NewPendingConfirmStore() *PendingConfirmStore {
	return &PendingConfirmStore{m: make(map[string]pendingEntry)}
}

func (p *PendingConfirmStore) Add(id, cmdText string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[id] = pendingEntry{command: cmdText, createdAt: time.Now()}
}

func (p *PendingConfirmStore) Resolve(id string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[id]
	if !ok {
		return "", fmt.Errorf("no pending confirmation with ID %q", id)
	}
	if time.Since(e.createdAt) > 5*time.Minute {
		delete(p.m, id)
		return "", fmt.Errorf("confirmation %q expired", id)
	}
	delete(p.m, id)
	return e.command, nil
}

func (p *PendingConfirmStore) Sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for id, e := range p.m {
		if e.createdAt.Before(cutoff) {
			delete(p.m, id)
		}
	}
}

func Serve(args []string) error {
	dbPath := pathOverride("MESHCDN_DB_PATH", DefaultDBPath)
	certsDir := pathOverride("MESHCDN_CERTS_DIR", DefaultCertsDir)
	nginxDir := pathOverride("MESHCDN_NGINX_DIR", DefaultNginxConfDir)
	challengeDir := pathOverride("MESHCDN_CHALLENGE_DIR", DefaultChallengeDir)
	disableACME := os.Getenv("MESHCDN_ACME_DISABLE") == "1"
	disableMesh := os.Getenv("MESHCDN_MESH_DISABLE") == "1"
	disableBot := os.Getenv("MESHCDN_BOT_DISABLE") == "1"
	binaryPath := pathOverride("MESHCDN_BINARY_PATH", "/usr/local/bin/cdn-agent")

	id, err := identity.Load()
	if err != nil {
		log.Printf("Warning: identity.json not loadable: %v", err)
		log.Printf("  mesh AND bot will be disabled")
		disableMesh = true
		disableBot = true
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	conn, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()

	// Task 3: access-stats store (runtime/logs.db) + the collector that tails
	// nginx's JSON stats log. Best-effort — a stats failure must never take
	// down the agent, so we log and continue with stats disabled.
	logsDBPath := pathOverride("MESHCDN_LOGS_DB_PATH", "/etc/meshcdn/runtime/logs.db")
	statsLogPath := pathOverride("MESHCDN_STATS_LOG", "/etc/meshcdn/runtime/logs/stats.log")
	var logStore *logs.Store
	if ls, lerr := logs.Open(logsDBPath); lerr != nil {
		log.Printf("[logs] open %s failed: %v (/v stats disabled)", logsDBPath, lerr)
	} else {
		logStore = ls
		defer logStore.Close()
	}

	store, err := cert.NewStore(certsDir)
	if err != nil {
		return fmt.Errorf("open cert store: %w", err)
	}

	var acmeClient *acme.Client
	if !disableACME {
		accountKeyPath := filepath.Join("/etc/meshcdn/persistent", "acme-account.key")
		if v := os.Getenv("MESHCDN_ACME_ACCOUNT_KEY"); v != "" {
			accountKeyPath = v
		}
		acmeClient, err = acme.NewClient(accountKeyPath, "", challengeDir+"/.well-known/acme-challenge")
		if err != nil {
			log.Printf("Warning: ACME client init failed: %v", err)
			acmeClient = nil
		}
	}

	peerMgr, err := peers.NewManager(peers.Path())
	if err != nil {
		return fmt.Errorf("open peers.json: %w", err)
	}

	pendingStore := NewPendingConfirmStore()

	nodeIP := ""
	if id != nil {
		nodeIP = id.NodeIP
	}

	// Bot-node override (Task 4): once /v target sets an explicit bot node, it
	// wins over the install-time MESHCDN_BOT_DISABLE env — only the designated
	// node polls Telegram. Takes effect at boot (state-layer scope; live switch
	// is a follow-up). No override set → env behavior unchanged.
	if id != nil {
		if override, oerr := db.GetBotNodeIP(context.Background(), conn); oerr == nil && override != "" {
			disableBot = override != nodeIP
			log.Printf("[bot] explicit bot-node override=%s (this node=%s → runs bot: %v)",
				override, nodeIP, !disableBot)
		}
	}

	var meshClient *mesh.Client
	var meshCoord *mesh.Coordinator
	var meshServer *mesh.Server
	var botClient *bot.Client

	notifyAllFn := func(ctx context.Context) error {
		if meshCoord == nil {
			return fmt.Errorf("mesh disabled")
		}
		v, err := db.CurrentVersion(ctx, conn)
		if err != nil {
			return err
		}
		meshCoord.PushNotifyToAll(ctx, v)
		return nil
	}
	peerStateFn := func() map[string]handlers.PeerHeartbeatState {
		if meshCoord == nil {
			return nil
		}
		raw := meshCoord.PeerStates()
		out := make(map[string]handlers.PeerHeartbeatState, len(raw))
		for k, v := range raw {
			out[k] = handlers.PeerHeartbeatState{
				LastSeenAt:          v.LastSeenAt,
				LastRTT:             v.LastRTT,
				ConfigVersion:       v.ConfigVersion,
				ProgramVersion:      v.ProgramVersion,
				ConsecutiveFailures: v.ConsecutiveFailures,
			}
		}
		return out
	}
	pendingResolverFn := func(idStr string) error {
		cmdText, err := pendingStore.Resolve(idStr)
		if err != nil {
			return err
		}
		ctx := command.WithForce(context.Background())
		if confirmExecutorRef == nil {
			return fmt.Errorf("executor not yet wired")
		}
		result, err := confirmExecutorRef.ExecuteBatch(ctx, cmdText)
		if err != nil {
			return err
		}
		if result.AnyFailed() {
			for _, o := range result.Outcomes {
				if o.Err != nil {
					return o.Err
				}
			}
		}
		return nil
	}

	upgradeFn := func(ctx context.Context) (string, error) {
		if meshClient == nil || id == nil {
			return "", fmt.Errorf("mesh disabled")
		}
		all, err := peerMgr.All()
		if err != nil {
			return "", err
		}
		ips := make([]string, 0, len(all))
		for _, p := range all {
			ips = append(ips, p.IP)
		}
		return meshClient.TriggerUpgradeForAll(ctx, ips, mesh.DefaultPort, id.NodeIP, binaryPath, version.Version)
	}

	// Step 10: SSL routing wiring. These callbacks reference meshClient
	// which is created later by startMesh; we wire them as closures that
	// capture the variable so the actual Client is used when called.
	sslPeerIPsFn := func() ([]string, error) {
		all, err := peerMgr.All()
		if err != nil {
			return nil, err
		}
		out := make([]string, len(all))
		for i, p := range all {
			out[i] = p.IP
		}
		return out, nil
	}
	sslDelegateExecFn := func(ctx context.Context, peerIP, cmdText string) (string, bool, error) {
		if meshClient == nil {
			return "", false, fmt.Errorf("mesh client not yet ready")
		}
		req := &mesh.ExecRequest{FromIP: nodeIP, Command: cmdText}
		resp, err := meshClient.Exec(ctx, peerIP, mesh.DefaultPort, req)
		if err != nil {
			return "", false, err
		}
		return resp.Output, resp.OK, nil
	}
	sslChallengeBroadcastFn := func(ctx context.Context, peerIPs []string, token, keyAuth string) error {
		if meshClient == nil {
			return fmt.Errorf("mesh client not yet ready")
		}
		post := meshClient.MakePostEvent(nodeIP, mesh.DefaultPort)
		return bot.BroadcastChallenge(post)(ctx, peerIPs, token, keyAuth)
	}
	sslChallengeCleanupFn := func(ctx context.Context, peerIPs []string, token string) {
		if meshClient == nil {
			return
		}
		post := meshClient.MakePostEvent(nodeIP, mesh.DefaultPort)
		bot.CleanupChallenge(post)(ctx, peerIPs, token)
	}
	sslCertBroadcastFn := func(ctx context.Context, certPEM, keyPEM []byte, source string) {
		if meshClient == nil {
			return
		}
		post := meshClient.MakePostEvent(nodeIP, mesh.DefaultPort)
		// Broadcast to ALL peers except ourselves
		allPeers := func() []string {
			all, _ := peerMgr.All()
			out := make([]string, 0, len(all))
			for _, p := range all {
				if p.IP != nodeIP {
					out = append(out, p.IP)
				}
			}
			return out
		}
		bot.BroadcastCert(post, allPeers)(ctx, certPEM, keyPEM, source)
	}

	registry := handlers.NewRegistry(handlers.Dependencies{
		CertStore:         store,
		ACMEClient:        acmeClient,
		ChallengeDir:      challengeDir + "/.well-known/acme-challenge",
		DB:                conn,
		ProgramVersion:    version.Version,
		PeerMgr:           peerMgr,
		LocalNodeIP:       nodeIP,
		NotifyAll:         notifyAllFn,
		PeerStateProvider: peerStateFn,
		PendingResolver:   pendingResolverFn,
		OnUpgrade:         upgradeFn,
		IdentityPath:      identity.Path(),
		LogStore:          logStore,

		SSLPeerIPs:            sslPeerIPsFn,
		SSLDelegateExec:       sslDelegateExecFn,
		SSLChallengeBroadcast: sslChallengeBroadcastFn,
		SSLChallengeCleanup:   sslChallengeCleanupFn,
		SSLCertBroadcast:      sslCertBroadcastFn,
	})

	executor := command.NewExecutor(conn, registry).
		WithNginxIntegration(store, nodeIP, nginxDir).
		WithSnapshotMaintenance(snapshot.Path(), version.Version)

	// Cache purge wiring (Task 2). CacheDir is emptied on a purge action;
	// BroadcastExec fans `/v cache - purge-all` out to every peer via /mesh/exec,
	// each peer clearing its own cache. meshClient is captured by closure since
	// startMesh assigns it later.
	executor.CacheDir = pathOverride("MESHCDN_CACHE_DIR", command.DefaultCacheDir)
	// Lets snapshot replay reconcile persistent/peers.json, so a peer removed
	// anywhere is removed everywhere rather than only on the node that ran the
	// command.
	executor.PeerMgr = peerMgr
	executor.BroadcastExec = func(ctx context.Context, cmdText string) string {
		if meshClient == nil {
			return "  (mesh 未就绪，未广播到 peer)"
		}
		all, err := peerMgr.All()
		if err != nil {
			return fmt.Sprintf("  peer 广播失败: %v", err)
		}
		ok, fail, total := 0, 0, 0
		for _, p := range all {
			if p.IP == nodeIP {
				continue
			}
			total++
			req := &mesh.ExecRequest{FromIP: nodeIP, Command: cmdText}
			resp, err := meshClient.Exec(ctx, p.IP, mesh.DefaultPort, req)
			if err != nil || resp == nil || !resp.OK {
				fail++
				continue
			}
			ok++
		}
		if total == 0 {
			return "  集群: 仅本节点 (无其他 peer)"
		}
		suffix := ""
		if fail > 0 {
			suffix = fmt.Sprintf("，%d 失败(将由后续心跳/手动重试)", fail)
		}
		return fmt.Sprintf("  集群广播: 本节点 + %d/%d peer 已清理%s", ok, total, suffix)
	}

	confirmExecutorRef = executor

	if rebuilt, err := MaybeRebuildFromSnapshot(context.Background(), conn, executor); err != nil {
		log.Printf("Warning: snapshot rebuild failed: %v", err)
	} else if rebuilt {
		log.Printf("[boot] DB rebuilt from snapshot.cmd successfully")
	}

	// AlertSink starts as LoggerSink (just writes to journalctl). It will be
	// upgraded below to one of:
	//   - TelegramAlertSink — if this node is the bot node (talks to Telegram directly)
	//   - MeshAlertSink — if this node is a worker (forwards alerts to the bot node via mesh)
	loggerSink := renew.LoggerSink{}
	worker := renew.NewWorker(store, acmeClient, loggerSink)
	scanner := &renew.Scanner{Store: store, Worker: worker}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("received signal %v, shutting down", s)
		cancel()
	}()

	log.Printf("MeshCDN agent %s starting", version.Version)

	workerDone := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(workerDone)
	}()
	scannerDone := make(chan struct{})
	go func() {
		scanner.Run(ctx)
		close(scannerDone)
	}()

	// Task 3: access-stats collector loop (the "4th loop", V4-DESIGN §A.3).
	// Tracked with collectorDone so shutdown waits for an in-flight CollectOnce
	// (esp. a Store.Commit tx) to finish before deferred logStore.Close() runs.
	collectorDone := make(chan struct{})
	if logStore != nil {
		go func() {
			defer close(collectorDone)
			collector := &logs.Collector{Store: logStore, LogPath: statsLogPath}
			collector.Run(ctx)
		}()
	} else {
		close(collectorDone) // nothing to wait for; avoid blocking below
	}

	pendingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				close(pendingDone)
				return
			case <-t.C:
				pendingStore.Sweep()
			}
		}
	}()

	var coordDone chan struct{}
	if !disableMesh && id != nil {
		var err error
		meshServer, meshCoord, meshClient, coordDone, err =
			startMesh(ctx, id, store, executor, peerMgr, binaryPath)
		if err != nil {
			log.Printf("Warning: mesh failed to start: %v", err)
		}
	}

	// Worker-node alert forwarding: when bot is disabled (this node is not
	// the bot node), wire the renew worker's AlertSink to forward alerts
	// over the mesh to the bot node, which posts them to Telegram.
	//
	// V4-DESIGN: only the bot node connects to api.telegram.org; worker
	// nodes are network-restricted-friendly (they only need mesh:9443 to
	// the bot node). This is the implementation of "internal forwarding".
	if disableBot && meshClient != nil && id != nil {
		sink := &bot.MeshAlertSink{
			Client:      meshClient,
			Port:        mesh.DefaultPort,
			LocalNodeIP: id.NodeIP,
			// Route alerts to the effective bot node: explicit override if set,
			// else the join-order default. Follows a /v target transfer.
			BotNodeIP: func() string {
				if override, err := db.GetBotNodeIP(context.Background(), conn); err == nil && override != "" {
					return override
				}
				return peerMgr.BotNodeIP()
			},
		}
		worker.AlertSink = sink
		log.Printf("[alert] worker-node mode: forwarding alerts via mesh to bot node")

		// Step 10: worker nodes also handle cluster events (acme-challenge-prepare,
		// acme-challenge-cleanup, cert-install). They don't forward alerts to
		// Telegram (no bot), so AlertSink stays nil here.
		clusterEvents := &bot.ClusterEventHandler{
			ChallengeDir: challengeDir + "/.well-known/acme-challenge",
			CertStore:    store,
			// AlertSink is nil — worker doesn't talk to Telegram
		}
		if meshServer != nil {
			meshServer.OnEvent = func(msg *mesh.EventMessage) error {
				clusterEvents.Handle(msg.Type, msg.FromIP, msg.Payload)
				return nil
			}
		}
	}

	// Bot + AI Assistant
	var botDone chan struct{}
	var aiSweepDone chan struct{}
	if !disableBot && id != nil {
		// Try to build AI Assistant from identity config
		var assistant *ai.Assistant
		if id.AIConfigured() {
			activeModel := id.AIActiveModel()
			provider, perr := ai.NewProvider(id.AIProvider, id.GetAPIKey(id.AIProvider), activeModel)
			if perr != nil {
				log.Printf("[ai] provider init failed: %v (AI disabled until /w ai key ...)", perr)
			} else {
				assistant = ai.NewAssistant(provider, ai.SystemPrompt)
				log.Printf("[ai] enabled (provider=%s model=%s)", id.AIProvider, activeModel)
			}
		} else {
			log.Printf("[ai] not configured — use /w ai key <KEY> to enable")
		}

		botClient = &bot.Client{
			Token:          id.BotToken,
			GroupID:        id.GroupID,
			Executor:       executor,
			Assistant:      assistant,
			PendingStore:   pendingStore,
			LocalNodeIP:    id.NodeIP,
			ProgramVersion: version.Version,
		}

		alertSink := &bot.TelegramAlertSink{Bot: botClient}
		worker.AlertSink = alertSink

		// Step 10: cluster event handler — handles alert + acme-challenge-* + cert-install.
		clusterEvents := &bot.ClusterEventHandler{
			ChallengeDir: challengeDir + "/.well-known/acme-challenge",
			CertStore:    store,
			AlertSink:    alertSink,
		}
		if meshServer != nil {
			meshServer.OnEvent = func(msg *mesh.EventMessage) error {
				clusterEvents.Handle(msg.Type, msg.FromIP, msg.Payload)
				return nil
			}
		}

		botDone = make(chan struct{})
		go func() {
			defer close(botDone)
			if err := botClient.Start(ctx); err != nil {
				log.Printf("[bot] start: %v", err)
			}
		}()

		botClient.WireFileFetcherWhenReady(ctx, id.BotToken)

		// AI conversation sweeper
		if assistant != nil {
			aiSweepDone = make(chan struct{})
			go func() {
				t := time.NewTicker(5 * time.Minute)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						close(aiSweepDone)
						return
					case <-t.C:
						assistant.Sweep()
					}
				}
			}()
		}
	}

	<-ctx.Done()

	if meshServer != nil {
		_ = meshServer.Stop()
	}
	<-workerDone
	<-scannerDone
	<-collectorDone
	<-pendingDone
	if coordDone != nil {
		<-coordDone
	}
	if botDone != nil {
		<-botDone
	}
	if aiSweepDone != nil {
		<-aiSweepDone
	}

	log.Printf("shutdown complete")
	return nil
}

var confirmExecutorRef *command.Executor

func startMesh(
	ctx context.Context,
	id *identity.Identity,
	store *cert.Store,
	executor *command.Executor,
	peerMgr *peers.Manager,
	binaryPath string,
) (*mesh.Server, *mesh.Coordinator, *mesh.Client, chan struct{}, error) {

	selfFP, err := findSelfSignedFor(store, id.NodeIP)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("find self-signed: %w", err)
	}
	certPath, keyPath := store.Paths(selfFP)

	server := mesh.NewServer()
	server.Token = id.Secret()
	server.LocalNodeIP = id.NodeIP
	server.Port = mesh.DefaultPort
	server.CertFile = certPath
	server.KeyFile = keyPath

	server.AddUpgradeRoutes(binaryPath, server.Mux)

	client := mesh.NewClient(id.Secret())
	coord := mesh.NewCoordinator(client, mesh.DefaultPort)
	coord.LocalNodeIP = id.NodeIP

	coord.GetLocalState = func() (int64, string, int) {
		v, err := db.CurrentVersion(ctx, executor.DB)
		if err != nil {
			return 0, version.Version, 0
		}
		all, _ := peerMgr.All()
		return v, version.Version, len(all)
	}
	coord.GetPeers = func() ([]mesh.PeerInfo, error) {
		all, err := peerMgr.All()
		if err != nil {
			return nil, err
		}
		out := make([]mesh.PeerInfo, len(all))
		for i, p := range all {
			out[i] = mesh.PeerInfo{IP: p.IP, JoinOrder: p.JoinOrder}
		}
		return out, nil
	}
	// OnRemoteAhead fires once per peer that reports a higher config_version,
	// and the heartbeat pings every peer concurrently — so on a large cluster a
	// single round where this node is behind used to launch one pull per peer
	// ahead, all of them wiping and rebuilding the same nginx directory at
	// once. Collapse them: one pull at a time, the rest are dropped. A pull
	// fetches the *latest* snapshot, not a per-peer delta, so a dropped
	// trigger costs nothing — and anything still missing is picked up by the
	// next heartbeat or notify-version.
	var pullInFlight int32
	coord.OnRemoteAhead = func(peerIP string, remoteVersion int64) {
		if !atomic.CompareAndSwapInt32(&pullInFlight, 0, 1) {
			return // a pull is already running; it will fetch at least this version
		}
		go func() {
			defer atomic.StoreInt32(&pullInFlight, 0)
			pullCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := coord.PullFromBest(pullCtx, remoteVersion); err != nil {
				log.Printf("[mesh] pull from peer ahead: %v", err)
			} else {
				log.Printf("[mesh] caught up to v%d", remoteVersion)
			}
		}()
	}
	coord.OnReceiveAhead = func(remoteVersion int64) (int64, error) {
		return db.CurrentVersion(ctx, executor.DB)
	}
	coord.PullAndApply = func(pctx context.Context, peerIP string) error {
		text, err := client.SnapshotPull(pctx, peerIP, mesh.DefaultPort)
		if err != nil {
			return err
		}
		_, err = executor.ApplySnapshot(pctx, text)
		return err
	}

	executor.WithMeshNotify(func(newVersion int64) {
		notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		coord.PushNotifyToAll(notifyCtx, newVersion)
	})

	server.OnPing = func(req *mesh.PingRequest) (*mesh.PingResponse, error) {
		v, err := db.CurrentVersion(ctx, executor.DB)
		if err != nil {
			return nil, err
		}
		all, _ := peerMgr.All()
		return &mesh.PingResponse{
			NodeIP:         id.NodeIP,
			ConfigVersion:  v,
			ProgramVersion: version.Version,
			PeerKnownCount: len(all),
		}, nil
	}

	// Step 10: remote command execution. Used by /w ssl <id> when the
	// identifier resolves to a peer's IP — the bot node delegates here.
	server.OnExec = func(req *mesh.ExecRequest) (*mesh.ExecResponse, error) {
		log.Printf("[mesh/exec] from=%s command=%q", req.FromIP, req.Command)
		execCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := executor.ExecuteBatch(execCtx, req.Command)
		if err != nil {
			return &mesh.ExecResponse{
				OK:     false,
				Reason: err.Error(),
			}, nil
		}
		output := command.FormatReport(result)
		return &mesh.ExecResponse{
			OK:     !result.AnyFailed(),
			Output: output,
		}, nil
	}
	server.OnNotifyVersion = func(req *mesh.NotifyVersionRequest) error {
		return coord.HandleNotifyVersion(req)
	}
	server.OnSnapshotPull = func() (string, error) {
		exp := &snapshot.Exporter{DB: executor.DB, ProgramVersion: version.Version}
		snap, err := exp.Export(ctx)
		if err != nil {
			return "", err
		}
		return snap.String(), nil
	}
	server.OnHandshake = func(req *mesh.HandshakeRequest) (*mesh.HandshakeResponse, error) {
		newPeer, err := peerMgr.Add(req.NodeIP)
		if err != nil {
			return &mesh.HandshakeResponse{OK: false, Reason: err.Error()}, nil
		}
		go func() {
			cmdText := fmt.Sprintf("/w internal peer-add %s %d", newPeer.IP, newPeer.JoinOrder)
			if _, err := executor.ExecuteBatch(ctx, cmdText); err != nil {
				log.Printf("[mesh/handshake] peer-add command: %v", err)
			}
		}()

		all, _ := peerMgr.All()
		piList := make([]mesh.PeerInfo, len(all))
		for i, p := range all {
			piList[i] = mesh.PeerInfo{IP: p.IP, JoinOrder: p.JoinOrder}
		}
		exp := &snapshot.Exporter{DB: executor.DB, ProgramVersion: version.Version}
		snap, err := exp.Export(ctx)
		if err != nil {
			return &mesh.HandshakeResponse{OK: false, Reason: err.Error()}, nil
		}
		return &mesh.HandshakeResponse{
			OK:        true,
			JoinOrder: newPeer.JoinOrder,
			Peers:     piList,
			Snapshot:  snap.String(),
		}, nil
	}
	server.OnEvent = func(msg *mesh.EventMessage) error {
		log.Printf("[mesh/event] from=%s type=%s payload=%v", msg.FromIP, msg.Type, msg.Payload)
		return nil
	}

	if err := server.Start(); err != nil {
		return nil, nil, nil, nil, err
	}

	coordDone := make(chan struct{})
	go func() {
		coord.Run(ctx)
		close(coordDone)
	}()

	log.Printf("[mesh] coordinator started (peers known: %d)", peerCount(peerMgr))
	return server, coord, client, coordDone, nil
}

func peerCount(m *peers.Manager) int {
	all, _ := m.All()
	return len(all)
}

func findSelfSignedFor(store *cert.Store, nodeIP string) (string, error) {
	all, err := store.All()
	if err != nil {
		return "", err
	}
	for _, cm := range all {
		if cm.Source != cert.SourceSelf {
			continue
		}
		if cm.Subject == nodeIP {
			return cm.FingerprintPrefix, nil
		}
		for _, san := range cm.SAN {
			if san == nodeIP {
				return cm.FingerprintPrefix, nil
			}
		}
	}
	return "", fmt.Errorf("no self-signed cert found for %s", nodeIP)
}
