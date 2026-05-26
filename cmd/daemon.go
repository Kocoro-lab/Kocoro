package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cloudflow"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/heartbeat"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
	"github.com/Kocoro-lab/ShanClaw/internal/watcher"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	_ "modernc.org/sqlite"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Background daemon for channel messaging",
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon (connects to Shannon Cloud for channel messages)",
	RunE: func(cmd *cobra.Command, args []string) error {
		detach, _ := cmd.Flags().GetBool("detach")
		if detach {
			return daemonStartDetached()
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}

		if cfg.Agent.IdleHardTimeoutSecs == 0 {
			log.Printf("daemon: WARN — agent.idle_hard_timeout_secs=0 (watchdog disabled). "+
				"A hung LLM call will block the route up to %s (HTTP transport ceiling). "+
				"Remove the override or set a value <600 to enable watchdog cancellation.",
				600*time.Second)
		} else if cfg.Agent.IdleHardTimeoutSecs >= 600 {
			log.Printf("daemon: WARN — agent.idle_hard_timeout_secs=%d (>= 600s HTTP transport ceiling). "+
				"The 600s transport timeout will fire first, so the watchdog cancellation never "+
				"propagates and you lose the partial-success exit path. Set a value < 600 "+
				"(recommended: 540) to enable watchdog cancellation.",
				cfg.Agent.IdleHardTimeoutSecs)
		}

		shanDir := config.ShannonDir()
		agentsDir := filepath.Join(shanDir, "agents")
		pidPath := filepath.Join(shanDir, "daemon.pid")

		if err := agents.EnsureBuiltins(agentsDir, Version); err != nil {
			log.Printf("WARNING: failed to sync builtin agents: %v", err)
		}
		if err := skills.EnsureBuiltinSkills(shanDir); err != nil {
			log.Printf("WARNING: failed to sync builtin skills: %v", err)
		}

		force, _ := cmd.Flags().GetBool("force")
		if force {
			stopExistingDaemon(pidPath)
		}

		if daemon.IsDaemonServiceLoaded() {
			log.Println("Warning: daemon is managed by launchd. Use 'shan daemon stop' to remove launchd management.")
		}

		pidFile, err := daemon.AcquirePIDFile(pidPath)
		if err != nil {
			return err
		}
		defer pidFile.Close()

		// Clean up orphaned Chrome CDP from a previous hard kill. Must run AFTER
		// AcquirePIDFile — holding the lock guarantees no other daemon is alive,
		// so any Chrome CDP we find is truly orphaned (not owned by a peer).
		mcp.CleanupOrphanedCDPChrome()

		// Apply configured Chrome profile override before any CDP launch.
		mcp.SetCDPChromeProfile(cfg.Daemon.ChromeProfile)

		// Log the effective endpoint so anyone debugging "why does daemon
		// talk to <wrong server>" can see what cfg actually resolved to
		// (covers the KOCORO_ENDPOINT env override path + yaml default
		// fallback). Mirrored to ~/.shannon/logs/audit.log on startup so
		// it survives Desktop's stdout capture.
		log.Printf("daemon: cfg.endpoint=%q (set KOCORO_ENDPOINT env to override)", cfg.Endpoint)

		gw := client.NewGatewayClient(cfg.Endpoint, cfg.APIKey)
		if cfg.Agent.StreamIdleTimeoutSecs > 0 {
			gw.SetStreamIdleTimeout(time.Duration(cfg.Agent.StreamIdleTimeoutSecs) * time.Second)
		}
		// Orphan sweep is startup-only — reload paths must not sweep because they
		// would kill live Chrome owned by in-flight runs.
		tools.CleanupOrphanedChromedp()
		baselineReg, reg, skillsPtr, mcpMgr, cleanup, startMCP, serverErr := tools.RegisterAllWithBaselineAsync(gw, cfg)
		if serverErr != nil {
			log.Printf("Warning: %v", serverErr)
		}
		_ = skillsPtr // skills are set per-request in RunAgent

		tools.RegisterCloudDelegate(reg, gw, cfg, nil, "", "") // daemon: agent forwarding per-message not yet supported
		tools.RegisterPublishTool(reg, gw, cfg)
		tools.RegisterListPublishedFilesTool(reg, gw, cfg)
		tools.RegisterRetractPublishedFileTool(reg, gw, cfg)
		tools.RegisterGenerateImageTool(reg, gw, cfg)
		tools.RegisterEditImageTool(reg, gw, cfg)

		gatewayOverlay := tools.ExtractGatewayTools(reg)
		postOverlays := tools.ExtractPostOverlays(reg, baselineReg)

		// Tee log output to ~/.shannon/logs/daemon.log so helper-spawned
		// daemons (whose stdout is owned by the parent Desktop process)
		// still leave a debuggable trail. The launchd plist already
		// redirects stdout there, but Desktop's ShanClawBridge spawns the
		// helper directly and consumes stdout itself — without this tee
		// the entire run is invisible to anyone but Desktop.
		if shanDir != "" {
			logsDir := filepath.Join(shanDir, "logs")
			_ = os.MkdirAll(logsDir, 0o700)
			if lf, err := os.OpenFile(filepath.Join(logsDir, "daemon.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				log.SetOutput(io.MultiWriter(os.Stderr, lf))
			}
		}

		var auditor *audit.AuditLogger
		if shanDir != "" {
			auditor, _ = audit.NewAuditLogger(filepath.Join(shanDir, "logs"))
		}
		if auditor != nil {
			defer auditor.Close()
		}
		hookRunner := hooks.NewHookRunner(cfg.Hooks)

		// Open the mailbox SQLite database for per-route message queue
		// persistence. Failure to open or migrate the schema is non-fatal —
		// the daemon continues with mailbox persistence disabled so existing
		// flows keep working; only the crash-recovery and ack-on-persist
		// guarantees are lost in that degraded mode.
		mailboxCap := viper.GetInt("daemon.mailbox_max_per_route")
		if mailboxCap <= 0 {
			mailboxCap = 100
		}
		mailboxDir := filepath.Join(shanDir, "sessions")
		if err := os.MkdirAll(mailboxDir, 0o700); err != nil {
			log.Printf("daemon: mkdir for mailbox: %v", err)
		}
		var sessionCache *daemon.SessionCache
		var mailboxDB *sql.DB
		mailboxDBPath := filepath.Join(mailboxDir, "mailbox.db")
		if db, err := sql.Open("sqlite", mailboxDBPath); err != nil {
			log.Printf("daemon: open mailbox db failed (%v); mailbox persistence DISABLED", err)
			sessionCache = daemon.NewSessionCache(shanDir)
		} else {
			db.SetMaxOpenConns(1)
			store, schemaErr := daemon.NewMailboxStore(db)
			if schemaErr != nil {
				log.Printf("daemon: mailbox schema failed (%v); mailbox persistence DISABLED", schemaErr)
				db.Close()
				sessionCache = daemon.NewSessionCache(shanDir)
			} else {
				mailboxDB = db
				sessionCache = daemon.NewSessionCacheWithMailbox(shanDir, store, mailboxCap)
				if pending, perr := store.LoadAllPending(); perr != nil {
					log.Printf("daemon: mailbox recovery load failed: %v", perr)
				} else {
					recovered := 0
					for routeKey, msgs := range pending {
						loaded, dropped := sessionCache.SeedMailbox(routeKey, msgs)
						recovered += loaded
						if dropped > 0 {
							log.Printf("daemon: mailbox recovery dropped %d msgs over cap on route %s", dropped, routeKey)
						}
					}
					if recovered > 0 {
						log.Printf("daemon: mailbox recovery seeded %d msg(s) across %d route(s); they drain on the next message for each route", recovered, len(pending))
					}
				}
				// Daily purge of consumed rows older than 7 days.
				go func(s *daemon.MailboxStore) {
					tick := time.NewTicker(24 * time.Hour)
					defer tick.Stop()
					for range tick.C {
						n, err := s.PurgeConsumedBefore(time.Now().Add(-7 * 24 * time.Hour))
						if err != nil {
							log.Printf("daemon: mailbox purge: %v", err)
							continue
						}
						if n > 0 {
							log.Printf("daemon: mailbox purge: %d row(s)", n)
						}
					}
				}(store)
			}
		}
		if mailboxDB != nil {
			defer mailboxDB.Close()
		}

		wsEndpoint := strings.Replace(cfg.Endpoint, "https://", "wss://", 1)
		wsEndpoint = strings.Replace(wsEndpoint, "http://", "ws://", 1)
		wsEndpoint += "/v1/ws/messages"
		scheduleManager := schedule.NewManager(filepath.Join(shanDir, "schedules.json"))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			log.Println("daemon: shutting down...")
			mcp.StopCDPChrome()
			cancel()
		}()

		deps := &daemon.ServerDeps{
			Config:           cfg,
			GW:               gw,
			Registry:         reg,
			MCPManager:       mcpMgr,
			Cleanup:          cleanup,
			ShannonDir:       shanDir,
			AgentsDir:        agentsDir,
			Auditor:          auditor,
			HookRunner:       hookRunner,
			SessionCache:     sessionCache,
			ScheduleManager:  scheduleManager,
			BaselineReg:      baselineReg,
			GatewayOverlay:   gatewayOverlay,
			PostOverlays:     postOverlays,
			ReadTrackerCache: daemon.NewReadTrackerCache(),
		}
		defer func() {
			if deps.Supervisor != nil {
				deps.Supervisor.Stop()
			}
			deps.ShutdownCleanup()
		}()

		supervisor := mcp.NewSupervisor(mcpMgr)
		supervisor.RegisterCapabilityProbe("playwright", &mcp.PlaywrightProbe{})
		supervisor.SetOnReconnect(func(ctx context.Context, serverName string) {
			if serverName == "playwright" {
				tools.CleanupPlaywrightReconnect(ctx, mcpMgr)
			}
		})
		supervisor.SetOnChange(func(server string, oldState, newState mcp.HealthState) {
			_, _, depsSup := deps.Snapshot()
			if depsSup != supervisor {
				return
			}
			// Read cached layers from deps (refreshed on any config reload)
			bl, gwOv, po, mgr := deps.RebuildLayers()
			newReg := tools.RebuildRegistryForHealth(bl, gwOv, po, supervisor.HealthStates(), mgr, supervisor)
			deps.WriteLock()
			deps.Registry = newReg
			deps.WriteUnlock()
			log.Printf("MCP registry rebuilt: %d tools", len(newReg.All()))
		})

		deps.WriteLock()
		deps.Supervisor = supervisor
		deps.WriteUnlock()

		supervisor.Start(ctx)

		// Force initial registry rebuild to attach the supervisor to MCPTools.
		// CompleteRegistration creates tools before the supervisor exists, so
		// they lack on-demand reconnect. This rebuild replaces them with
		// supervisor-aware instances from the cached tool list.
		{
			bl, gwOv, po, mgr := deps.RebuildLayers()
			initReg := tools.RebuildRegistryForHealth(bl, gwOv, po, supervisor.HealthStates(), mgr, supervisor)
			deps.WriteLock()
			deps.Registry = initReg
			deps.WriteUnlock()
			log.Printf("MCP registry initialized with supervisor: %d tools", len(initReg.All()))
		}

		// Kick off MCP connections in the background. The HTTP listener (set
		// up below) is ready immediately; tools become available as each
		// server's handshake completes (Intercom OAuth: up to 300s). On
		// success we trigger a supervisor probe so OnChange rebuilds the
		// registry with newly-discovered tools.
		if startMCP != nil {
			capturedSupervisor := supervisor
			capturedAuditor := auditor
			capturedMgr := mcpMgr
			go startMCP(ctx, func(name string, connErr error) {
				_, _, depsSup := deps.Snapshot()
				if depsSup != capturedSupervisor {
					return // deps swapped by a config reload — drop stale result.
				}
				if connErr != nil {
					log.Printf("[mcp] %s: async connect failed: %v", name, connErr)
					if capturedAuditor != nil {
						capturedAuditor.Log(audit.AuditEntry{
							Timestamp:     time.Now(),
							ToolName:      "mcp_connect",
							InputSummary:  "mcp_servers." + name,
							OutputSummary: connErr.Error(),
							Decision:      "error",
							Approved:      false,
						})
					}
					return
				}
				log.Printf("[mcp] %s: async connect succeeded; probing supervisor", name)
				capturedSupervisor.ProbeNow(name)
				tools.PostConnectDisconnectIfDiscoveryOnly(capturedMgr, name)
			})
		}

		if !cfg.Daemon.AutoApprove {
			log.Println("daemon: interactive approval mode — tools requiring approval will be sent to the client for user confirmation. Set daemon.auto_approve: true in config to auto-approve all tools.")
		}

		// Create WS client first, then broker (broker needs client's send method).
		var wsClient *daemon.Client
		var broker *daemon.ApprovalBroker
		activeCloudMessages := newActiveCloudMessageTracker()

		wsClient = daemon.NewClient(wsEndpoint, cfg.APIKey, func(msg daemon.MessagePayload) string {
			msgCtx := ctx

			// Wire per-message workflow_id callback via context for streaming card replies.
			// Uses context (not mutable tool field) for concurrency safety.
			if msg.MessageID != "" {
				msgID := msg.MessageID
				msgCtx = cloudflow.WithOnWorkflowStarted(msgCtx, func(workflowID string) {
					_ = wsClient.SendProgressWithWorkflow(msgID, workflowID)
				})
			}

			// Use msg.Source if Cloud populates it; fall back to msg.Channel during rolling deploy
			source := msg.Source
			if source == "" {
				source = msg.Channel
			}
			req := daemon.RunAgentRequest{
				Text:            msg.Text,
				Content:         msg.Content,
				Agent:           msg.AgentName,
				Source:          source,
				Channel:         msg.Channel,
				ThreadID:        msg.ThreadID,
				Sender:          msg.Sender,
				CWD:             msg.CWD,
				Files:           msg.Files,
				CloudMessageID:  msg.MessageID,
				IMStatusContext: msg.IMStatusContext,
			}
			// Fall back to @mention parsing if cloud didn't set agent name.
			// Skip for messaging-platform sources: there the gateway delivers an
			// explicit AgentName (or empty = use default), and any "@<botname>"
			// in the message body is user-facing convention, not an agent name
			// in the local registry. See daemon.IsMessagingPlatform.
			if req.Agent == "" && !daemon.IsMessagingPlatform(source) {
				agentName, prompt := agents.ParseAgentMention(msg.Text)
				req.Agent = agentName
				req.Text = prompt
			}
			if req.Text == "" {
				req.Text = msg.Text
			}
			// Allow file-only messages (no text) from messaging platforms.
			if req.Text == "" && len(req.Files) > 0 {
				req.Text = "[Attached files]"
			}
			if err := req.Validate(); err != nil {
				return daemon.FriendlyAgentError(err)
			}
			req.EnsureRouteKey()

			// Try injecting into an active run on the same route.
			// Probe HasActiveRun first so cold-start routes skip the inject
			// path entirely — otherwise ConvertFilesToInjected would download
			// + base64 every attachment, then RunAgent below would re-download
			// the same files via downloadRemoteFiles. Pattern matches the HTTP
			// guard in internal/daemon/server.go:1367. Tiny race window where
			// the run ends between probe and InjectMessage is acceptable:
			// worst case we paid one download for an InjectNoActiveRun.
			if req.RouteKey != "" && deps.SessionCache.HasActiveRun(req.RouteKey) {
				switch deps.SessionCache.InjectMessage(req.RouteKey, agent.InjectedMessage{
					Text:            req.Text,
					CWD:             req.CWD,
					Files:           daemon.ConvertFilesToInjected(msgCtx, req.Files),
					CloudMessageID:  msg.MessageID,
					IMStatusContext: msg.IMStatusContext,
				}) {
				case daemon.InjectOK:
					emitInjectedMessageReceivedEvent(deps.EventBus, deps.SessionCache, req, msg.MessageID)
					if shouldForwardQueuedFollowUpStatusForMessage(source, msg.IMStatusContext) {
						sendQueuedFollowUpStatusEvent(wsClient, activeCloudMessages.MessageID(req.RouteKey), req.Text)
					}
					// Tell Cloud the IM follow-up was accepted into the running loop so
					// the platform reaction can flip from "received" → "processing"
					// later when the agent loop drains it. No-op on non-IM sources
					// (empty IMStatusContext); see emitLifecycleReceived guards.
					emitLifecycleReceived(wsClient, msg.MessageID, msg.IMStatusContext)
					// Message injected — running loop will incorporate it.
					// Suppress the explicit ack on messaging platforms: the user's
					// own message is already visible in the thread and the active
					// run's streamer (Slack/Feishu/WeCom) is already updating an
					// in-place "Processing..." block. The bracket text would just
					// be persistent noise. CLI/TUI flows still get the ack so
					// users typing into a running agent know their input was
					// queued. See daemon.IsMessagingPlatform.
					if daemon.IsMessagingPlatform(source) {
						return ""
					}
					return "[message received, processing...]"
				case daemon.InjectQueueFull:
					// Active run exists but queue saturated — don't start a new run.
					log.Printf("daemon: inject queue full for route %q, message dropped", req.RouteKey)
					return "[message rejected: the active run already has a queued follow-up; retry after it reaches the next turn]"
				case daemon.InjectBusy:
					return "[message rejected: the active run is still initializing; retry when it reaches the next turn]"
				case daemon.InjectCWDConflict:
					return "[message rejected: the active run is using a different project; wait for it to finish or cancel it before switching cwd]"
				case daemon.InjectNoActiveRun:
					// Fall through to start a new RunAgent
				}
			}

			// Resolve auto_approve: per-agent overrides global
			autoApprove := cfg.Daemon.AutoApprove
			if req.Agent != "" {
				if a, err := agents.LoadAgent(agentsDir, req.Agent); err == nil && a.Config != nil && a.Config.AutoApprove != nil {
					autoApprove = *a.Config.AutoApprove
				}
			}

			handler := &daemonEventHandler{
				broker:      broker,
				ctx:         msgCtx,
				channel:     msg.Channel,
				threadID:    msg.ThreadID,
				agent:       req.Agent,
				source:      req.Source,
				autoApprove: autoApprove,
				shannonDir:  shanDir,
				deps:        deps,
				wsClient:    wsClient,
				messageID:   msg.MessageID,
			}

			clearActiveCloudMessage := activeCloudMessages.Track(req.RouteKey, msg.MessageID)
			defer clearActiveCloudMessage()

			// Tell Cloud the inbound IM message reached the daemon for a fresh
			// run (first user turn). No-op on non-IM sources or when this is
			// not a cold-start path (e.g. queue-full / busy / cwd-conflict
			// returns above don't fall through here). Cloud uses this to flip
			// the platform reaction to "received" before the first LLM call.
			emitLifecycleReceived(wsClient, msg.MessageID, msg.IMStatusContext)

			result, err := daemon.RunAgent(msgCtx, deps, req, handler)
			if err != nil {
				// Full error already logged inside RunAgent; return clean message.
				return daemon.FriendlyAgentError(err)
			}

			log.Printf("daemon: reply to %s (%d tokens, $%.4f)", result.Agent, result.Usage.TotalTokens, result.Usage.CostUSD)
			return result.Reply
		}, func(text string) {
			log.Printf("daemon: [system] %s", text)
		})

		broker = daemon.NewApprovalBroker(wsClient.SendApprovalRequest)
		wsClient.SetApprovalBroker(broker)

		localServer := daemon.NewServer(7533, wsClient, deps, Version)
		localServer.SetCancelFunc(cancel)
		localServer.SetApprovalResolvedNotifier(wsClient.SendApprovalResolved)
		wsClient.SetEventBus(localServer.EventBus())
		deps.EventBus = localServer.EventBus()
		deps.WSClient = wsClient

		// AuthManager wiring. Keychain is macOS-only — on Linux/Windows
		// authMgr stays nil and /local/auth/* responds 503. Legacy path
		// (cfg.APIKey set via setup wizard or yaml) continues to work
		// because wsClient was already constructed with that key above
		// and WSController.Start below dials directly.
		var authMgr *daemon.AuthManager
		var wsCtl *daemon.WSController
		kcStore, kcErr := keychain.NewOSStore(log.Default())
		if kcErr == nil {
			// Pre-seed viper's cloud.api_key from Keychain so the memory
			// subsystem's cold-start gate (memory.ResolveAPIKey at
			// server.go:518) doesn't fire cloud_misconfigured for users
			// whose yaml api_key has been moved to Keychain by the v1
			// migration. Without this, AuthManager.Bootstrap correctly
			// signs the user in, but memSvc has already been constructed
			// with an empty api_key and stays Unavailable forever.
			//
			// Viper override is in-process only — yaml on disk stays as
			// the migration left it (Keychain is the source of truth).
			// Skip when yaml already has a value: the operator may have
			// manually set memory.api_key / cloud.api_key and we should
			// not stomp it.
			if k, kerr := kcStore.GetAPIKey(); kerr == nil && k != "" {
				if viper.GetString("memory.api_key") == "" && viper.GetString("cloud.api_key") == "" {
					viper.Set("cloud.api_key", k)
				}
			}
			authClient := client.NewAuthClient(cfg.Endpoint, gw.HTTPClient())
			authMgr = daemon.NewAuthManager(daemon.AuthManagerConfig{
				Keychain: kcStore,
				Cloud:    authClient,
				Gateway:    gw,
				WSClient:   wsClient,
				Cfg:        cfg,
				ShannonDir: shanDir,
				OnAPIKeyChanged: func(ctx context.Context) {
					localServer.RebuildAuthSensitiveTools(ctx)
				},
				Logger: log.Default(),
			})
			authMgr.SetEventBus(localServer.EventBus())
			wsCtl = daemon.NewWSController(ctx, wsClient)
			authMgr.SetWSController(wsCtl)
			wsClient.SetOnAuthFailure(authMgr.HandleWSAuthFailure)
			localServer.SetAuth(authMgr)
		} else {
			log.Printf("daemon: keychain unavailable (%v) — falling back to legacy cfg.APIKey path", kcErr)
		}

		// Start file watcher and heartbeat manager.
		var triggerMu sync.Mutex
		var fileWatcher *watcher.Watcher
		var hbManager *heartbeat.Manager

		watchRunFn := func(watchCtx context.Context, agentName, prompt string) {
			req := daemon.RunAgentRequest{
				Agent:  agentName,
				Source: "watcher",
				Text:   prompt,
			}
			handler := &autoApproveHandler{}
			result, err := daemon.RunAgent(watchCtx, deps, req, handler)
			if err != nil {
				log.Printf("daemon: watcher agent %q error: %v", agentName, err)
				return
			}
			log.Printf("daemon: watcher agent %q reply (%d tokens): %s", agentName, result.Usage.TotalTokens, truncateReply(result.Reply, 200))
		}
		agentWatches := collectAgentWatches(agentsDir)
		if len(agentWatches) > 0 {
			fw, err := watcher.New(agentWatches, watchRunFn)
			if err != nil {
				log.Printf("daemon: watcher init failed: %v", err)
			} else {
				fw.Start(ctx)
				fileWatcher = fw
				log.Printf("daemon: file watcher started (%d agents)", len(agentWatches))
			}
		}

		hbMgr, err := heartbeat.New(agentsDir, deps)
		if err != nil {
			log.Printf("daemon: heartbeat init failed: %v", err)
		} else {
			hbMgr.Start(ctx)
			hbManager = hbMgr
			log.Printf("daemon: heartbeat manager started")
		}

		// Start internal cron scheduler (evaluates schedules each minute).
		cronScheduler := daemon.NewScheduler(scheduleManager, deps)
		go cronScheduler.Start(ctx)
		log.Println("daemon: cron scheduler started")

		localServer.SetOnReload(func() {
			triggerMu.Lock()
			defer triggerMu.Unlock()

			// Close old watcher/heartbeat.
			if fileWatcher != nil {
				fileWatcher.Close()
				fileWatcher = nil
			}
			if hbManager != nil {
				hbManager.Close()
				hbManager = nil
			}

			// Rebuild from fresh agent configs.
			newWatches := collectAgentWatches(agentsDir)
			if len(newWatches) > 0 {
				fw, err := watcher.New(newWatches, watchRunFn)
				if err != nil {
					log.Printf("daemon: reload watcher init failed: %v", err)
				} else {
					fw.Start(ctx)
					fileWatcher = fw
					log.Printf("daemon: file watcher restarted (%d agents)", len(newWatches))
				}
			}

			newHb, err := heartbeat.New(agentsDir, deps)
			if err != nil {
				log.Printf("daemon: reload heartbeat init failed: %v", err)
			} else {
				newHb.Start(ctx)
				hbManager = newHb
				log.Printf("daemon: heartbeat manager restarted")
			}
		})

		// Wire the WS broker's bus hooks the same way NewServer wires
		// s.approvalBroker — both paths publish identical event payloads.
		daemon.WireApprovalBusHooks(broker, localServer.EventBus())
		serverErrCh := make(chan error, 1)
		go func() {
			serverErrCh <- localServer.Start(ctx)
		}()
		// Give the listener a moment to bind, then check for immediate failure.
		time.Sleep(50 * time.Millisecond)
		select {
		case err := <-serverErrCh:
			return fmt.Errorf("daemon: local server failed to start: %w", err)
		default:
			log.Printf("daemon: local server listening on http://127.0.0.1:7533")
		}

		log.Printf("daemon: WS endpoint %s", wsEndpoint)
		if authMgr != nil {
			// AuthManager owns WS lifecycle. Bootstrap reads any existing
			// Keychain key + validates via /auth/me; if valid it starts
			// the WS, otherwise daemon stays idle until /local/auth/login.
			// We run Bootstrap async so HTTP /local/auth/state responds
			// immediately during startup (default signed_out state is
			// already populated).
			go authMgr.Bootstrap(ctx)
		} else if wsClient != nil {
			// Legacy path (non-darwin or Keychain disabled): start WS
			// directly with whatever key the user pasted via setup
			// wizard. No AuthManager, no /local/auth/*.
			wsCtl = daemon.NewWSController(ctx, wsClient)
			wsCtl.Start(ctx)
		}
		<-ctx.Done()
		if wsCtl != nil {
			wsCtl.Stop()
		}

		triggerMu.Lock()
		if fileWatcher != nil {
			fileWatcher.Close()
			fileWatcher = nil
		}
		if hbManager != nil {
			hbManager.Close()
			hbManager = nil
		}
		triggerMu.Unlock()

		sessionCache.CloseAll()
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the background daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		launchdManaged := daemon.IsDaemonServiceLoaded()
		if launchdManaged {
			if err := daemon.LaunchctlBootout(); err != nil {
				log.Printf("Warning: launchctl bootout failed: %v", err)
			}
			daemon.RemoveDaemonPlist()
		}

		pidPath := filepath.Join(config.ShannonDir(), "daemon.pid")

		// If launchd bootout already killed the process, we're done.
		if launchdManaged {
			// Brief wait for process to exit after bootout.
			time.Sleep(500 * time.Millisecond)
			if _, locked := daemon.IsLocked(pidPath); !locked {
				fmt.Println("Daemon stopped (launchd service removed).")
				return nil
			}
			// Process still alive — fall through to HTTP/SIGTERM.
		}

		// Try graceful HTTP shutdown first.
		resp, err := http.Post("http://127.0.0.1:7533/shutdown", "application/json", nil)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("unexpected response: %s", resp.Status)
			}
			// Wait for process to fully exit (PID file lock released).
			deadline := time.After(5 * time.Second)
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-deadline:
					fmt.Println("Daemon shutdown requested (still exiting).")
					return nil
				case <-ticker.C:
					if _, locked := daemon.IsLocked(pidPath); !locked {
						fmt.Println("Daemon stopped.")
						return nil
					}
				}
			}
		}

		// HTTP failed — fall back to SIGTERM via PID file.
		pid, locked := daemon.IsLocked(pidPath)
		if !locked {
			return fmt.Errorf("daemon not running")
		}
		if pid <= 0 {
			return fmt.Errorf("daemon PID file is locked but contains invalid PID")
		}

		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("cannot find daemon process %d: %w", pid, err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("failed to send SIGTERM to PID %d: %w", pid, err)
		}
		fmt.Printf("Sent SIGTERM to daemon (PID %d).\n", pid)

		// Wait for process to exit (up to 5s).
		deadline := time.After(5 * time.Second)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline:
				fmt.Printf("Warning: daemon (PID %d) did not exit within 5s.\n", pid)
				return nil
			case <-ticker.C:
				if _, locked := daemon.IsLocked(pidPath); !locked {
					fmt.Println("Daemon stopped.")
					return nil
				}
			}
		}
	},
}

// daemonMemoryStatus mirrors internal/memory.MemoryStatus on the wire. Kept
// in cmd/ to avoid pulling internal/memory into the CLI package.
type daemonMemoryStatus struct {
	Provider string         `json:"provider"`
	Reason   *string        `json:"reason"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// daemonStatusResponse mirrors the JSON shape returned by GET /status.
type daemonStatusResponse struct {
	IsConnected bool                `json:"is_connected"`
	ActiveAgent string              `json:"active_agent"`
	Uptime      int                 `json:"uptime"`
	Version     string              `json:"version"`
	Memory      *daemonMemoryStatus `json:"memory,omitempty"`
}

// printMemoryStatus writes a human-readable summary of the memory section
// to w. Older daemons without the memory field produce no output (ms == nil).
//
// Renders up to three lines:
//
//	Memory:    enabled                       (happy path)
//	Memory:    disabled (tlm_binary_too_old) (degraded; reason in parens)
//	           restart_attempts=5            (other top-level detail keys)
//	Repair:    compatibility=incompatible sub_code=no_manifest bundle_version=
//	                                         (only when reason ==
//	                                          tlm_binary_too_old, so users +
//	                                          Desktop see the actionable bits
//	                                          without parsing the JSON detail)
func printMemoryStatus(w io.Writer, ms *daemonMemoryStatus) {
	if ms == nil {
		return
	}
	line := "Memory:    " + ms.Provider
	if ms.Reason != nil && *ms.Reason != "" {
		line += " (" + *ms.Reason + ")"
	}
	fmt.Fprintln(w, line)

	if len(ms.Detail) > 0 {
		topKeys := make([]string, 0, len(ms.Detail))
		for k := range ms.Detail {
			if k == "repair_needed" {
				continue
			}
			topKeys = append(topKeys, k)
		}
		sort.Strings(topKeys)
		if len(topKeys) > 0 {
			parts := make([]string, 0, len(topKeys))
			for _, k := range topKeys {
				parts = append(parts, fmt.Sprintf("%s=%v", k, ms.Detail[k]))
			}
			fmt.Fprintln(w, "           "+strings.Join(parts, " "))
		}
		if repair, ok := ms.Detail["repair_needed"].(map[string]any); ok && len(repair) > 0 {
			rKeys := make([]string, 0, len(repair))
			for k := range repair {
				rKeys = append(rKeys, k)
			}
			sort.Strings(rKeys)
			parts := make([]string, 0, len(rKeys))
			for _, k := range rKeys {
				parts = append(parts, fmt.Sprintf("%s=%v", k, repair[k]))
			}
			fmt.Fprintln(w, "Repair:    "+strings.Join(parts, " "))
		}
	}
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		pidPath := filepath.Join(config.ShannonDir(), "daemon.pid")

		resp, err := http.Get("http://127.0.0.1:7533/status")
		if err != nil {
			// HTTP failed — check PID file to distinguish "not running" from "running but no HTTP server".
			if pid, locked := daemon.IsLocked(pidPath); locked {
				fmt.Printf("Status:    running (HTTP server unavailable)\n")
				fmt.Printf("PID:       %d\n", pid)
				return nil
			}
			fmt.Println("Daemon is not running.")
			return nil
		}
		defer resp.Body.Close()

		var status daemonStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return fmt.Errorf("failed to parse status: %w", err)
		}

		pid, _ := daemon.ReadPID(pidPath)
		fmt.Printf("Status:    running\n")
		if pid > 0 {
			fmt.Printf("PID:       %d\n", pid)
		}
		if status.Version != "" {
			fmt.Printf("Version:   %s\n", status.Version)
		}
		fmt.Printf("Connected: %v\n", status.IsConnected)
		if status.ActiveAgent != "" {
			fmt.Printf("Agent:     %s\n", status.ActiveAgent)
		}
		uptime := time.Duration(status.Uptime) * time.Second
		fmt.Printf("Uptime:    %s\n", uptime)
		printMemoryStatus(os.Stdout, status.Memory)
		if daemon.IsDaemonServiceLoaded() {
			fmt.Printf("Launchd:   managed\n")
		} else {
			fmt.Printf("Launchd:   not installed\n")
		}
		return nil
	},
}

type daemonEventHandler struct {
	broker      *daemon.ApprovalBroker
	ctx         context.Context
	channel     string
	threadID    string
	agent       string
	source      string // RunAgentRequest.Source — threaded into ApprovalRequestMeta for the bus payload
	autoApprove bool
	shannonDir  string
	deps        *daemon.ServerDeps
	sessionID   string         // set by RunAgent after session resolution (EventBus spans sessions)
	wsClient    *daemon.Client // for event forwarding to Cloud
	messageID   string         // scoped to current message
	usage       agent.UsageAccumulator
}

type activeCloudMessageTracker struct {
	mu      sync.Mutex
	byRoute map[string]string
}

func newActiveCloudMessageTracker() *activeCloudMessageTracker {
	return &activeCloudMessageTracker{byRoute: make(map[string]string)}
}

func (t *activeCloudMessageTracker) Track(routeKey, messageID string) func() {
	if t == nil || routeKey == "" || messageID == "" {
		return func() {}
	}
	t.mu.Lock()
	t.byRoute[routeKey] = messageID
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		if t.byRoute[routeKey] == messageID {
			delete(t.byRoute, routeKey)
		}
		t.mu.Unlock()
	}
}

func (t *activeCloudMessageTracker) MessageID(routeKey string) string {
	if t == nil || routeKey == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byRoute[routeKey]
}

func emitInjectedMessageReceivedEvent(eventBus *daemon.EventBus, sessionCache *daemon.SessionCache, req daemon.RunAgentRequest, messageID string) {
	if eventBus == nil || sessionCache == nil || req.RouteKey == "" {
		return
	}
	sessionID := sessionCache.RouteSessionID(req.RouteKey)
	if sessionID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"agent":      req.Agent,
		"source":     req.Source,
		"sender":     req.Sender,
		"session_id": sessionID,
		"message_id": messageID,
		"text":       req.Text,
		"queued":     true,
	})
	eventBus.Emit(daemon.Event{Type: daemon.EventMessageReceived, Payload: payload})
}

func shouldForwardQueuedFollowUpStatus(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case daemon.ChannelSlack, daemon.ChannelWeCom, daemon.ChannelFeishu, daemon.ChannelLark:
		return true
	default:
		return false
	}
}

func shouldForwardQueuedFollowUpStatusForMessage(source string, imStatusContext json.RawMessage) bool {
	if len(imStatusContext) > 0 {
		return false
	}
	return shouldForwardQueuedFollowUpStatus(source)
}

func sendQueuedFollowUpStatusEvent(wsClient *daemon.Client, activeMessageID, text string) {
	if wsClient == nil || activeMessageID == "" {
		return
	}
	message := queuedFollowUpStatusText(text)
	if message == "" {
		return
	}
	if err := wsClient.SendEvent(activeMessageID, "LLM_OUTPUT", message, map[string]interface{}{"queued": true}); err != nil {
		log.Printf("daemon: queued follow-up status forward failed: %v", err)
	}
}

func queuedFollowUpStatusText(text string) string {
	preview := queuedFollowUpPreview(text)
	if preview == "" {
		preview = "[Attached files]"
	}
	return "Queued next:\n> " + preview
}

func queuedFollowUpPreview(text string) string {
	const maxRunes = 160
	preview := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if preview == "" {
		return ""
	}
	runes := []rune(preview)
	if len(runes) <= maxRunes {
		return preview
	}
	return string(runes[:maxRunes]) + "..."
}

func (h *daemonEventHandler) SetSessionID(id string) { h.sessionID = id }

// Usage returns the cumulative usage collected during this handler's lifetime,
// split into LLM and gateway-tool billing so tool synthetic tokens don't
// corrupt the LLM token accounting.
func (h *daemonEventHandler) Usage() agent.AccumulatedUsage { return h.usage.Snapshot() }

// ResetUsage clears accumulated totals. Use between independent messages on
// the same long-lived handler so per-message cost reporting is accurate.
func (h *daemonEventHandler) ResetUsage() { h.usage.Reset() }

func (h *daemonEventHandler) OnToolCall(name string, args string, toolUseID string) {
	// Skip cloud_delegate — it has its own streaming path via SendProgressWithWorkflow.
	// Forwarding it as a daemon event would conflict (creates a daemon: stream that
	// never receives WORKFLOW_COMPLETED from the Temporal workflow).
	if h.wsClient != nil && h.messageID != "" && name != "cloud_delegate" {
		// Send empty message so StreamConsumer uses toolDisplayName mapping
		// (e.g., "web_search" → "Searching the web"). tool_use_id pairs this
		// TOOL_INVOKED frame with its later TOOL_COMPLETED frame; advertised to
		// Cloud via the tool_use_id_events capability token.
		if err := h.wsClient.SendEvent(h.messageID, "TOOL_INVOKED", "", map[string]interface{}{"tool": name, "tool_use_id": toolUseID}); err != nil {
			log.Printf("daemon: event forward failed: %v", err)
		}
	}
}
func (h *daemonEventHandler) OnToolResult(name string, args string, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	log.Printf("daemon: tool %s completed (%.1fs)", name, elapsed.Seconds())
	if h.wsClient != nil && h.messageID != "" && name != "cloud_delegate" {
		if err := h.wsClient.SendEvent(h.messageID, "TOOL_COMPLETED", "", map[string]interface{}{"tool": name, "tool_use_id": toolUseID, "elapsed": elapsed.Seconds()}); err != nil {
			log.Printf("daemon: event forward failed: %v", err)
		}
	}
}
func (h *daemonEventHandler) OnText(text string) {
	if h.wsClient != nil && h.messageID != "" {
		if err := h.wsClient.SendEvent(h.messageID, "LLM_OUTPUT", text, nil); err != nil {
			log.Printf("daemon: event forward failed: %v", err)
		}
	}
}

// OnPreamble forwards mid-turn narration to Cloud over the same LLM_OUTPUT WS
// event used for final-answer text. Cloud distinguishes "preamble vs final" by
// the surrounding TOOL_RUNNING / TOOL_COMPLETED frames, so reusing the same
// wire event preserves the existing channel rendering on Slack/Feishu/etc.
func (h *daemonEventHandler) OnPreamble(text string) {
	if text == "" {
		return
	}
	if h.wsClient != nil && h.messageID != "" {
		if err := h.wsClient.SendEvent(h.messageID, "LLM_OUTPUT", text, nil); err != nil {
			log.Printf("daemon: event forward failed: %v", err)
		}
	}
}
func (h *daemonEventHandler) OnStreamDelta(delta string) {
	if h.wsClient != nil && h.messageID != "" {
		if err := h.wsClient.SendEvent(h.messageID, "LLM_PARTIAL", delta, nil); err != nil {
			log.Printf("daemon: event forward failed: %v", err)
		}
	}
}
func (h *daemonEventHandler) OnUsage(usage agent.TurnUsage) {
	h.usage.Add(usage)
}
func (h *daemonEventHandler) OnCloudAgent(agentID, status, message string)           {}
func (h *daemonEventHandler) OnCloudProgress(completed, total int)                   {}
func (h *daemonEventHandler) OnCloudPlan(planType, content string, needsReview bool) {}

// OnRunStatus satisfies agent.RunStatusHandler on daemonEventHandler. Actual
// bus emission now happens in busEventHandler (see multiHandler wiring in
// runner.go). This implementation is a no-op but must remain so the type
// assertion for RunStatusHandler continues to match the daemonEventHandler
// alongside any future WS-specific forwarding we add.
func (h *daemonEventHandler) OnRunStatus(code, detail string) {}

func (h *daemonEventHandler) OnApprovalNeeded(tool string, args string) bool {
	if h.autoApprove {
		// Treat auto-approve like the scheduled-run gate: ordinary tools skip the
		// broker, while any future entry in the unattended deny-list still
		// requires a human decision. The list is empty as of 2026-05-18.
		if !agent.DisallowsUnattendedAutoApproval(tool) {
			log.Printf("daemon: auto-approving %s (auto_approve=true)", tool)
			return true
		}
		log.Printf("daemon: %s requires per-call approval (auto_approve=true); prompting via broker", tool)
	}
	if h.broker == nil {
		log.Printf("daemon: approval broker unavailable for %s; denying", tool)
		return false
	}
	// defer guards against a panic inside broker.Request leaking a phantom
	// session into the tracker. Temp var kept because some test fixtures
	// construct daemonEventHandler with deps == nil; production always wires
	// it via daemon.NewServer.
	var tracker *daemon.ApprovalTracker
	if h.deps != nil {
		tracker = h.deps.ApprovalTracker
	}
	tracker.Mark(h.sessionID)
	defer tracker.Clear(h.sessionID)
	decision := h.broker.Request(h.ctx, daemon.ApprovalRequestMeta{
		MessageID: h.messageID,
		SessionID: h.sessionID,
		Source:    h.source,
		Channel:   h.channel,
		ThreadID:  h.threadID,
		Agent:     h.agent,
	}, tool, args)
	if decision == daemon.DecisionAlwaysAllow {
		// PR 5: single entry point shared with the SSE path so SSE/WS
		// behavior cannot drift. Handles bash (tool-level for named agents,
		// command-level for default agent), non-bash (tool-level), and
		// always-ask high-risk gates.
		daemon.HandleAlwaysAllowDecision(h.deps, h.broker, h.agent, tool, args)
	}
	return decision == daemon.DecisionAllow || decision == daemon.DecisionAlwaysAllow
}

// autoApproveHandler is a minimal EventHandler for internal triggers (watcher, heartbeat).
type autoApproveHandler struct {
	usage agent.UsageAccumulator
}

// Usage returns the cumulative usage collected during this handler's lifetime.
func (h *autoApproveHandler) Usage() agent.AccumulatedUsage { return h.usage.Snapshot() }

func (h *autoApproveHandler) OnToolCall(name string, args string, toolUseID string) {}
func (h *autoApproveHandler) OnToolResult(name string, args string, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	log.Printf("daemon: tool %s completed (%.1fs)", name, elapsed.Seconds())
}
func (h *autoApproveHandler) OnText(text string)                                     {}
func (h *autoApproveHandler) OnPreamble(text string)                                 {}
func (h *autoApproveHandler) OnStreamDelta(delta string)                             {}
func (h *autoApproveHandler) OnUsage(usage agent.TurnUsage)                          { h.usage.Add(usage) }
func (h *autoApproveHandler) OnCloudAgent(agentID, status, message string)           {}
func (h *autoApproveHandler) OnCloudProgress(completed, total int)                   {}
func (h *autoApproveHandler) OnCloudPlan(planType, content string, needsReview bool) {}

// OnApprovalNeeded auto-approves tools for internal triggers (file-system
// watcher, heartbeat). These are fully unattended, so they route through the
// same unattended deny-list as scheduled runs. The list is empty today.
func (h *autoApproveHandler) OnApprovalNeeded(tool string, args string) bool {
	return !agent.DisallowsUnattendedAutoApproval(tool)
}

func stopExistingDaemon(pidPath string) {
	// Try graceful HTTP shutdown.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://127.0.0.1:7533/shutdown", "application/json", nil)
	if err == nil {
		resp.Body.Close()
	}

	// If HTTP failed, try SIGTERM via PID file.
	if err != nil {
		if pid, locked := daemon.IsLocked(pidPath); locked && pid > 0 {
			if proc, err := os.FindProcess(pid); err == nil {
				proc.Signal(syscall.SIGTERM)
			}
		}
	}

	// Wait for lock to be released (up to 3s).
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			log.Printf("daemon: existing daemon did not stop within 3s, proceeding anyway")
			return
		case <-ticker.C:
			if _, locked := daemon.IsLocked(pidPath); !locked {
				return
			}
		}
	}
}

func daemonStartDetached() error {
	shanDir := config.ShannonDir()
	pidPath := filepath.Join(shanDir, "daemon.pid")

	if _, locked := daemon.IsLocked(pidPath); locked {
		return fmt.Errorf("daemon is already running (PID file locked)")
	}

	logDir := filepath.Join(shanDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logPath := filepath.Join(logDir, "daemon.log")

	plistContent := daemon.GenerateDaemonPlist(daemon.ShanBinary(), logPath)
	plistPath := daemon.DaemonPlistPath()
	if err := daemon.WriteDaemonPlist(plistPath, plistContent); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	if err := daemon.LaunchctlBootstrap(plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}

	fmt.Printf("Daemon started via launchd.\n")
	fmt.Printf("  Plist: %s\n", plistPath)
	fmt.Printf("  Logs:  %s\n", logPath)
	fmt.Printf("Use 'shan daemon stop' to stop.\n")
	return nil
}

func truncateReply(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func collectAgentWatches(agentsDir string) map[string][]watcher.WatchEntry {
	result := make(map[string][]watcher.WatchEntry)
	entries, err := agents.ListAgents(agentsDir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		a, err := agents.LoadAgent(agentsDir, entry.Name)
		if err != nil || a.Config == nil || len(a.Config.Watch) == 0 {
			continue
		}
		for _, w := range a.Config.Watch {
			result[entry.Name] = append(result[entry.Name], watcher.WatchEntry{
				Path: w.Path,
				Glob: w.Glob,
			})
		}
	}
	return result
}

func init() {
	daemonStartCmd.Flags().Bool("force", false, "Stop any existing daemon before starting")
	daemonStartCmd.Flags().BoolP("detach", "d", false, "Run as background service via launchd (macOS only)")
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	rootCmd.AddCommand(daemonCmd)
}
