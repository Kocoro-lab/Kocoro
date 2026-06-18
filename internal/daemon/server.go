package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cloudflow"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/migrate/claudecode"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	syncpkg "github.com/Kocoro-lab/ShanClaw/internal/sync"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Server struct {
	port           int
	client         *Client
	deps           *ServerDeps
	server         *http.Server
	listenerMu     sync.Mutex // protects listener
	listener       net.Listener
	version        string
	ctx            context.Context // daemon lifecycle context, set on Start
	cancel         context.CancelFunc
	approvalBroker *ApprovalBroker
	eventBus       *EventBus
	// notifyApprovalResolved is set once at startup (SetApprovalResolvedNotifier,
	// before the WS connects or any approval can fire) and read without a lock
	// from both the /approval handler and every cleanup-notify goroutine. Safe
	// only because of that set-once invariant — switch to an atomic.Pointer if
	// it ever needs to be re-set at runtime.
	notifyApprovalResolved func(p ApprovalResolvedPayload) error
	// pendingBrokers maps requestID → per-request ApprovalBroker.
	// SSE handlers register here so POST /approval can find the right broker.
	pendingBrokers sync.Map // map[string]*ApprovalBroker
	onReload       func()   // called after config reload to restart watchers/heartbeat

	marketplace  *skills.MarketplaceClient
	slugLocks    *skills.SlugLocks
	secretsStore *skills.SecretsStore
	memSvc       *memory.Service
	suggestions  *agent.SuggestionState
	migratePlans *claudecode.PlanStore

	// auth manages the /local/auth/* email/password flow. May be nil on
	// non-darwin platforms where Keychain is unavailable — handlers
	// short-circuit to 503 in that case so the path stays observable in
	// Desktop logs rather than silently 404-ing.
	auth *AuthManager

	// shareTasks tracks in-flight and recently-completed async share
	// goroutines. Lifecycle:
	//   1. POST /sessions/{id}/share spawns a goroutine, registers state here, returns 202+task_id
	//   2. The goroutine emits EventShareProgress and mutates state via updateShareTask
	//   3. shareTaskRetainAfterDone after terminal phase, an AfterFunc drops the entry
	// Lookups by GET /sessions/{id}/share/tasks/{task_id} race with the GC; a
	// 404 is the legitimate "task expired" response in that case.
	shareTasksMu sync.RWMutex
	shareTasks   map[string]*shareTaskState

	// agentSyncTrigger coalesces agent create/update/delete notifications into a
	// single serialized, debounced full push to Cloud (agentSyncWorker). Buffered
	// size 1: a pending trigger absorbs a burst of changes into one push.
	agentSyncTrigger chan struct{}

	// pullDone is closed by Start() once the one-time startup agent pull
	// completes (success OR failure). agentSyncWorker blocks on it before its
	// first push so a create/update/delete that races startup cannot trigger a
	// full_sync over an incomplete local set (which would soft-delete
	// not-yet-pulled cloud agents).
	pullDone chan struct{}
}

// requireDeps returns true if s.deps is non-nil, otherwise writes a 500
// and returns false. Marketplace handlers dereference s.deps.ShannonDir
// and s.deps.AgentsDir; without this guard they'd panic when the server
// is constructed with nil deps (which some existing tests and callers
// do — NewServer stays nil-safe via resolveRegistryURL below, so the
// handlers must match that contract).
func (s *Server) requireDeps(w http.ResponseWriter) bool {
	if s == nil || s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon not fully initialized")
		return false
	}
	return true
}

// auditHTTPOp logs an HTTP API write operation to the audit log.
func (s *Server) auditHTTPOp(method, path, summary string) {
	if s.deps == nil || s.deps.Auditor == nil {
		return
	}
	s.deps.Auditor.Log(audit.AuditEntry{
		Timestamp:    time.Now(),
		ToolName:     "http_api",
		InputSummary: method + " " + path + ": " + summary,
		Decision:     "approved",
		Approved:     true,
	})
}

// auditMCPConnectFailure records a background MCP connect failure (async
// startup path). The supervisor's periodic probes deliberately do NOT
// reconnect — so a failed first attempt stays Disconnected until the user
// re-toggles. Logging here gives operators a forensic trail for "I clicked
// the toggle but tools never appeared" reports.
func (s *Server) auditMCPConnectFailure(serverName string, err error) {
	if s.deps == nil || s.deps.Auditor == nil {
		return
	}
	output := ""
	if err != nil {
		output = err.Error()
	}
	s.deps.Auditor.Log(audit.AuditEntry{
		Timestamp:     time.Now(),
		ToolName:      "mcp_connect",
		InputSummary:  "mcp_servers." + serverName,
		OutputSummary: output,
		Decision:      "error",
		Approved:      false,
	})
}

// auditHTTPOpError logs an HTTP API write operation that failed. Unlike
// auditHTTPOp the schema splits endpoint into input_summary and the failure
// detail into output_summary. Keeps input_summary short for grep/dashboards
// while preserving the full upstream error (git stderr, etc.) in
// output_summary up to the 500-char truncation cap.
func (s *Server) auditHTTPOpError(method, path, summary string, err error) {
	if s.deps == nil || s.deps.Auditor == nil {
		return
	}
	output := summary
	if err != nil {
		output += ": " + err.Error()
	}
	s.deps.Auditor.Log(audit.AuditEntry{
		Timestamp:     time.Now(),
		ToolName:      "http_api",
		InputSummary:  method + " " + path,
		OutputSummary: output,
		Decision:      "error",
		Approved:      false,
	})
}

// resolveRegistryURL returns the configured marketplace registry URL, falling
// back to the public default. Tolerates nil deps / nil Config so tests that
// construct NewServer with nil deps continue to work.
func resolveRegistryURL(deps *ServerDeps) string {
	const defaultURL = "https://raw.githubusercontent.com/Kocoro-lab/shanclaw-skill-registry/main/index.json"
	if deps == nil || deps.Config == nil {
		return defaultURL
	}
	if u := deps.Config.Skills.Marketplace.RegistryURL; u != "" {
		return u
	}
	return defaultURL
}

var (
	showChromeOnPortFn        = mcp.ShowCDPChromeOnPort
	hideChromeOnPortFn        = mcp.HideCDPChromeOnPort
	getChromeStatusOnPortFn   = mcp.GetCDPChromeStatusOnPort
	getChromeProfileStateFn   = mcp.GetChromeProfileState
	stopChromeFn              = mcp.StopCDPChrome
	resetChromeProfileCloneFn = mcp.ResetCDPProfileClone
)

func NewServer(port int, client *Client, deps *ServerDeps, version string) *Server {
	var shannonDir string
	if deps != nil {
		shannonDir = deps.ShannonDir
	}
	store := skills.NewSecretsStore(shannonDir)
	if deps != nil {
		deps.SecretsStore = store
	}
	// suggestions is initialized unconditionally so HTTP handlers work even
	// when deps == nil (existing test fixtures pass nil). The same pointer is
	// wired into deps.Suggestions below — when deps is non-nil — so the
	// runner's post-Run hook reaches the same SuggestionState the handler
	// reads from. Order matters: construct the Server first, then assign
	// deps.Suggestions; flipping these would either panic on nil deps or
	// race the eventBus subscriber test fixtures.
	s := &Server{
		port:                   port,
		client:                 client,
		deps:                   deps,
		version:                version,
		approvalBroker:         NewApprovalBroker(func(req ApprovalRequest) error { return nil }),
		eventBus:               NewEventBus(),
		notifyApprovalResolved: func(p ApprovalResolvedPayload) error { return nil },
		marketplace:            skills.NewMarketplaceClient(resolveRegistryURL(deps), 1*time.Hour),
		slugLocks:              skills.NewSlugLocks(),
		secretsStore:           store,
		suggestions:            agent.NewSuggestionState(),
		migratePlans:           claudecode.NewPlanStore(),
		agentSyncTrigger:       make(chan struct{}, 1),
		pullDone:               make(chan struct{}),
	}
	// Wire approval bus hooks so SSE per-request brokers (which inherit from
	// s.approvalBroker in handleMessageSSE) publish EventApprovalRequest /
	// EventApprovalResolved alongside the WS path's broker. The cloud notifier
	// is read lazily through s.notifyApprovalResolved: NewServer seeds it with a
	// no-op and cmd/daemon.go installs the real WS sender via
	// SetApprovalResolvedNotifier after construction, so the closure must defer
	// the lookup to cleanup time. This broker covers the SSE source; the WS
	// broker in cmd/daemon.go is wired with the same notifier for cloud sources.
	// A given approval lives in exactly one broker (independent pending maps
	// keyed by random request IDs), so there is no double-notify.
	WireApprovalBusHooks(s.approvalBroker, s.eventBus, func(p ApprovalResolvedPayload) error {
		return s.notifyApprovalResolved(p)
	})
	if deps != nil {
		deps.Suggestions = s.suggestions
		if deps.ApprovalTracker == nil {
			deps.ApprovalTracker = NewApprovalTracker()
		}
	}
	// Rehydrate notification history from disk so /notifications survives a
	// daemon restart. Best-effort: errors are logged but never fatal — a
	// corrupt log should never block daemon startup. newNotifStore returns
	// whatever events it could parse before an error too, so we restore those
	// even on partial-failure paths (e.g. compaction rewrite failed but the
	// read succeeded). Order matters: install the persister after restore so
	// rehydrated events don't get appended back to disk.
	if shannonDir != "" {
		store, restored, err := newNotifStore(shannonDir)
		if err != nil {
			log.Printf("daemon: notification history load partial: %v", err)
		}
		s.eventBus.RestoreNotifications(restored)
		if store != nil {
			s.eventBus.SetNotifPersister(store.Append)
		}
	}
	return s
}

func (s *Server) chromeControlPort() int {
	if s == nil || s.deps == nil {
		return mcp.DefaultCDPPort
	}
	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil || cfg.MCPServers == nil {
		return mcp.DefaultCDPPort
	}
	playwright, ok := cfg.MCPServers["playwright"]
	if !ok {
		return mcp.DefaultCDPPort
	}
	return mcp.PlaywrightCDPPort(mcp.NormalizePlaywrightCDPConfig(playwright))
}

func (s *Server) configuredChromeProfile() string {
	if s == nil || s.deps == nil {
		return ""
	}
	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil {
		return ""
	}
	return cfg.Daemon.ChromeProfile
}

func (s *Server) setConfiguredChromeProfile(profile string) {
	if s == nil || s.deps == nil {
		return
	}
	s.deps.WriteLock()
	if s.deps.Config == nil {
		s.deps.Config = &config.Config{}
	}
	s.deps.Config.Daemon.ChromeProfile = profile
	mcp.SetCDPChromeProfile(profile)
	s.deps.WriteUnlock()
}

// SetApprovalResolvedNotifier sets the function called to notify Cloud when
// Ptfrog resolves an approval before the external channel does.
func (s *Server) SetApprovalResolvedNotifier(fn func(ApprovalResolvedPayload) error) {
	s.notifyApprovalResolved = fn
}

func (s *Server) Port() int {
	s.listenerMu.Lock()
	ln := s.listener
	s.listenerMu.Unlock()
	if ln != nil {
		return ln.Addr().(*net.TCPAddr).Port
	}
	return s.port
}

// SetCancelFunc sets a cancel function that handleShutdown will call to stop the daemon.
func (s *Server) SetCancelFunc(cancel context.CancelFunc) {
	s.cancel = cancel
}

// SetOnReload sets a callback invoked after config reload to restart watchers/heartbeat.
func (s *Server) SetOnReload(fn func()) {
	s.onReload = fn
}

// SetAuth installs the AuthManager so /local/auth/* handlers can serve.
// Nil is permitted (non-darwin platforms) — handlers respond 503.
func (s *Server) SetAuth(a *AuthManager) {
	s.auth = a
}

func (s *Server) liveAPIKey(cfg *config.Config) string {
	if s != nil && s.auth != nil && s.deps != nil && s.deps.GW != nil {
		return s.deps.GW.APIKey()
	}
	if cfg == nil {
		return ""
	}
	return cfg.APIKey
}

func (s *Server) configWithLiveAPIKey(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	out := config.Clone(cfg)
	if s != nil && s.auth != nil && s.deps != nil && s.deps.GW != nil {
		out.APIKey = s.deps.GW.APIKey()
	}
	return out
}

// RegisterAuthRoutes wires only the /local/auth/* endpoints onto mux.
// Used by E2E tests that need a focused handler tree without the full
// route surface (which would 500 on nil deps).
func (s *Server) RegisterAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /local/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /local/auth/register", s.handleAuthRegister)
	mux.HandleFunc("POST /local/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /local/auth/resend-verification", s.handleAuthResendVerification)
	mux.HandleFunc("POST /local/auth/forgot-password", s.handleAuthForgotPassword)
	mux.HandleFunc("POST /local/auth/sign-out", s.handleAuthSignOut)
	mux.HandleFunc("POST /local/auth/sign-out-full", s.handleAuthSignOutFull)
	mux.HandleFunc("POST /local/auth/adopt-key", s.handleAuthAdoptKey)
}

// RebuildAuthSensitiveTools re-registers the gateway tools whose
// construction captures cfg.APIKey by value (cloud_delegate,
// publish_to_web, list_my_published_files, retract_published_file,
// generate_image, edit_image). The AuthManager calls this after a key
// change so sign-out invalidates these tools (cfg.APIKey == "" causes
// the Register* helpers to skip) and login re-arms them with the new
// key.
//
// This is intentionally a narrow rebuild — it does NOT reload yaml or
// touch MCP / local tools, which keeps the path safe to call repeatedly
// without disturbing in-flight agent runs that hold concurrent reads on
// the local-tool subset.
func (s *Server) RebuildAuthSensitiveTools(_ context.Context) {
	if s == nil || s.deps == nil || s.deps.Registry == nil || s.deps.GW == nil {
		return
	}
	cfg := s.configWithLiveAPIKey(s.deps.Config)
	if cfg == nil {
		return
	}
	reg := s.deps.Registry
	for _, name := range []string{
		"cloud_delegate",
		"publish_to_web",
		"list_my_published_files",
		"retract_published_file",
		"generate_image",
		"edit_image",
	} {
		reg.Remove(name)
	}
	tools.RegisterCloudDelegate(reg, s.deps.GW, cfg, nil, "", "")
	tools.RegisterPublishTool(reg, s.deps.GW, cfg)
	tools.RegisterListPublishedFilesTool(reg, s.deps.GW, cfg)
	tools.RegisterRetractPublishedFileTool(reg, s.deps.GW, cfg)
	tools.RegisterGenerateImageTool(reg, s.deps.GW, cfg)
	tools.RegisterEditImageTool(reg, s.deps.GW, cfg)
}

// registerRoutes wires every HTTP handler onto mux. Extracted from Start so
// offline tests (test/e2e/suggestion_test.go) can build a mux without
// listening on a port or spinning up memSvc / sync ticker. Production Start
// calls this; nothing else mutates s here, so it is safe to call before
// listening.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /agents", s.handleAgents)
	mux.HandleFunc("GET /agents/{name}", s.handleGetAgent)
	mux.HandleFunc("POST /agents", s.handleCreateAgent)
	mux.HandleFunc("PUT /agents/{name}", s.handleUpdateAgent)
	mux.HandleFunc("DELETE /agents/{name}", s.handleDeleteAgent)
	mux.HandleFunc("PUT /agents/{name}/config", s.handlePutAgentConfig)
	mux.HandleFunc("DELETE /agents/{name}/config", s.handleDeleteAgentConfig)
	mux.HandleFunc("POST /agents/{name}/permissions/always-allow", s.handleAddAgentAlwaysAllow)
	mux.HandleFunc("DELETE /agents/{name}/permissions/always-allow", s.handleRemoveAgentAlwaysAllow)
	mux.HandleFunc("POST /permissions/always-allow", s.handleAddGlobalAlwaysAllow)
	mux.HandleFunc("DELETE /permissions/always-allow", s.handleRemoveGlobalAlwaysAllow)
	mux.HandleFunc("PUT /agents/{name}/commands/{cmd}", s.handlePutCommand)
	mux.HandleFunc("DELETE /agents/{name}/commands/{cmd}", s.handleDeleteCommand)
	mux.HandleFunc("PUT /agents/{name}/skills/{skill}", s.handlePutSkill)
	mux.HandleFunc("DELETE /agents/{name}/skills/{skill}", s.handleDeleteSkill)
	mux.HandleFunc("GET /skills/downloadable", s.handleListDownloadableSkills)
	mux.HandleFunc("POST /skills/install/{name}", s.handleInstallSkill)
	mux.HandleFunc("POST /skills/marketplace/install/{slug}", s.handleMarketplaceInstall)
	mux.HandleFunc("POST /skills/upload", s.handleUploadSkill)
	mux.HandleFunc("POST /channels/feishu/app-installs", s.handleCreateFeishuAppInstall)
	mux.HandleFunc("GET /channels/feishu/app-installs", s.handleListFeishuAppInstalls)
	mux.HandleFunc("DELETE /channels/feishu/app-installs/{id}", s.handleDeleteFeishuAppInstall)
	mux.HandleFunc("GET /skills/marketplace", s.handleMarketplaceList)
	mux.HandleFunc("GET /skills/marketplace/entry/{slug}", s.handleMarketplaceDetail)
	mux.HandleFunc("GET /skills", s.handleListSkills)
	mux.HandleFunc("GET /skills/{name}", s.handleGetSkill)
	mux.HandleFunc("PUT /skills/{name}", s.handlePutGlobalSkill)
	mux.HandleFunc("DELETE /skills/{name}", s.handleDeleteGlobalSkill)
	mux.HandleFunc("GET /skills/{name}/scripts", s.handleListSkillScripts)
	mux.HandleFunc("PUT /skills/{name}/scripts/{filename}", s.handlePutSkillScripts)
	mux.HandleFunc("DELETE /skills/{name}/scripts/{filename}", s.handleDeleteSkillScripts)
	mux.HandleFunc("PUT /skills/{name}/secrets", s.handlePutSkillSecrets)
	mux.HandleFunc("DELETE /skills/{name}/secrets", s.handleDeleteSkillSecrets)
	mux.HandleFunc("DELETE /skills/{name}/secrets/{key}", s.handleDeleteSkillSecretKey)
	mux.HandleFunc("GET /skills/{name}/references", s.handleListSkillReferences)
	mux.HandleFunc("PUT /skills/{name}/references/{filename}", s.handlePutSkillReferences)
	mux.HandleFunc("DELETE /skills/{name}/references/{filename}", s.handleDeleteSkillReferences)
	mux.HandleFunc("GET /skills/{name}/assets", s.handleListSkillAssets)
	mux.HandleFunc("GET /skills/{name}/usage", s.handleSkillUsage)
	mux.HandleFunc("PUT /skills/{name}/assets/{filename}", s.handlePutSkillAssets)
	mux.HandleFunc("DELETE /skills/{name}/assets/{filename}", s.handleDeleteSkillAssets)
	mux.HandleFunc("GET /schedules", s.handleListSchedules)
	mux.HandleFunc("GET /schedules/{id}", s.handleGetSchedule)
	mux.HandleFunc("GET /schedules/{id}/last-run", s.handleScheduleLastRun)
	mux.HandleFunc("POST /schedules", s.handleCreateSchedule)
	mux.HandleFunc("PATCH /schedules/{id}", s.handlePatchSchedule)
	mux.HandleFunc("DELETE /schedules/{id}", s.handleDeleteSchedule)
	mux.HandleFunc("GET /uploads", s.handleListUploads)
	mux.HandleFunc("DELETE /uploads/{id}", s.handleDeleteUpload)
	mux.HandleFunc("GET /config", s.handleGetConfig)
	mux.HandleFunc("GET /config/status", s.handleConfigStatus)
	mux.HandleFunc("PATCH /config", s.handlePatchConfig)
	mux.HandleFunc("POST /config/reload", s.handleConfigReload)
	mux.HandleFunc("GET /instructions", s.handleGetInstructions)
	mux.HandleFunc("PUT /instructions", s.handlePutInstructions)
	mux.HandleFunc("GET /rules", s.handleListRules)
	mux.HandleFunc("GET /rules/{name}", s.handleGetRule)
	mux.HandleFunc("PUT /rules/{name}", s.handlePutRule)
	mux.HandleFunc("DELETE /rules/{name}", s.handleDeleteRule)
	mux.HandleFunc("POST /project/init", s.handleProjectInit)
	mux.HandleFunc("GET /sessions", s.handleSessions)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("PATCH /sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("POST /sessions/{id}/edit", s.handleEditMessage)
	mux.HandleFunc("POST /sessions/{id}/reset", s.handleResetSession)
	mux.HandleFunc("POST /sessions/{id}/rewind", s.handleRewind)
	mux.HandleFunc("GET /sessions/{id}/summary", s.handleSessionSummary)
	mux.HandleFunc("POST /sessions/{id}/share", s.handleSessionShare)
	mux.HandleFunc("DELETE /sessions/{id}/share", s.handleSessionShareRetract)
	mux.HandleFunc("GET /sessions/{id}/shares", s.handleSessionShares)
	mux.HandleFunc("GET /sessions/{id}/share/tasks/{task_id}", s.handleSessionShareTask)
	mux.HandleFunc("GET /sessions/search", s.handleSessionSearch)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/suggestion", s.handleGetSuggestion)
	mux.HandleFunc("POST /agents/{name}/sessions/{id}/suggestion/accept", s.handleAcceptSuggestion)
	// Default-agent parallel routes — validateSuggestionRoute returns
	// agentName="" for these, and SessionCache.GetOrCreate("") maps to
	// ~/.shannon/sessions. Desktop's default workspace has no named agent
	// in the URL and would otherwise hit a 404 via agentExists.
	mux.HandleFunc("GET /sessions/{id}/suggestion", s.handleGetSuggestion)
	mux.HandleFunc("POST /sessions/{id}/suggestion/accept", s.handleAcceptSuggestion)
	mux.HandleFunc("GET /permissions", s.handlePermissions)
	mux.HandleFunc("POST /permissions/request", s.handlePermissionsRequest)
	mux.HandleFunc("POST /approval", s.handleApproval)
	mux.HandleFunc("GET /approvals", s.handleApprovals)
	mux.HandleFunc("POST /message", s.handleMessage)
	mux.HandleFunc("POST /inject/retract", s.handleRetractInject)
	mux.HandleFunc("POST /cancel", s.handleCancel)
	// Per-route mailbox (see references/queue.md and references/cancel.md).
	mux.HandleFunc("POST /queue", s.handleEnqueueQueue)
	mux.HandleFunc("GET /queue", s.handleGetQueue)
	mux.HandleFunc("DELETE /queue/{id}", s.handleDeleteQueueItem)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /notifications", s.handleNotifications)
	mux.HandleFunc("GET /chrome/status", s.handleChromeStatus)
	mux.HandleFunc("GET /chrome/profile", s.handleChromeProfile)
	mux.HandleFunc("POST /chrome/profile", s.handleChromeProfileUpdate)
	mux.HandleFunc("POST /chrome/profile/refresh", s.handleChromeProfileRefresh)
	mux.HandleFunc("POST /chrome/show", s.handleChromeShow)
	mux.HandleFunc("POST /chrome/hide", s.handleChromeHide)
	mux.HandleFunc("POST /migrate/claude-code/preview", s.handleClaudeMigratePreview)
	mux.HandleFunc("POST /migrate/claude-code/apply", s.handleClaudeMigrateApply)

	// /local/auth/* — email/password authentication endpoints for the
	// Kocoro Desktop UI. macOS-only; on other platforms s.auth is nil
	// and the handlers return 503 platform_unsupported.
	mux.HandleFunc("GET /local/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /local/auth/register", s.handleAuthRegister)
	mux.HandleFunc("POST /local/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /local/auth/resend-verification", s.handleAuthResendVerification)
	mux.HandleFunc("POST /local/auth/forgot-password", s.handleAuthForgotPassword)
	mux.HandleFunc("POST /local/auth/sign-out", s.handleAuthSignOut)
	mux.HandleFunc("POST /local/auth/sign-out-full", s.handleAuthSignOutFull)
	mux.HandleFunc("POST /local/auth/adopt-key", s.handleAuthAdoptKey)

	mux.HandleFunc("POST /shutdown", s.handleShutdown)
}

// Handler returns an http.Handler with every route registered. Used by
// offline E2E tests that need to exercise HTTP handlers without listening
// on a port or starting the memSvc / sync ticker side effects of Start.
// Production code paths should use Start instead.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return mux
}

func (s *Server) Start(ctx context.Context) error {
	s.ctx = ctx
	s.recoverMigrationOrphans()
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", s.port))
	if err != nil {
		return fmt.Errorf("daemon server listen: %w", err)
	}
	s.listenerMu.Lock()
	s.listener = ln
	s.listenerMu.Unlock()
	s.server = &http.Server{Handler: mux}

	// Spawn the gated session-sync ticker. It self-disables when sync is
	// not enabled, so it's always safe to start unconditionally here.
	go s.runSyncLoop(ctx)

	// Serialized, debounced worker that pushes agent changes to Cloud.
	go s.agentSyncWorker(ctx)

	// One-time agent pull on startup: applies the cloud mirror to local disk
	// (bidirectional LWW — materializes missing, overwrites cloud-newer, deletes
	// tombstoned). No-op when Cloud is unconfigured. pullDone is ALWAYS closed
	// when this finishes (success, failure, or unconfigured) so agentSyncWorker
	// never blocks forever waiting on the gate before its first push.
	go func() {
		gw := s.cloudGateway()
		if gw == nil {
			s.runStartupAgentSync(nil) // unconfigured
			return
		}
		s.runStartupAgentSync(func() ([]client.SyncAgentItem, error) {
			return gw.PullAgents(ctx)
		})
	}()

	// Memory feature (Phase 2.3). Service is constructed once and Start runs
	// the cold-path gates synchronously then spawns the supervisor goroutine.
	// Failure modes (provider=disabled, tlm missing, cloud misconfigured) all
	// resolve to Status=Disabled/Unavailable; the memory tool falls back.
	memCfg := memory.LoadConfig(viper.GetViper())
	memCfg.APIKey = memory.ResolveAPIKey(viper.GetViper())
	memCfg.Endpoint = memory.ResolveEndpoint(viper.GetViper())
	// When shan runs inside the Desktop app bundle, prefer the embedded tlm
	// binary over PATH unless the operator explicitly set memory.tlm_path.
	if memCfg.TLMPath == "" {
		if bundlePath := memory.BundleRelativeTLMPath(); bundlePath != "" {
			memCfg.TLMPath = bundlePath
		}
	}
	var memAudit memory.AuditLogger
	if s.deps != nil && s.deps.Auditor != nil {
		memAudit = memoryAuditAdapter{logger: s.deps.Auditor}
	}
	s.memSvc = memory.NewService(memCfg, memAudit)
	if s.deps != nil {
		s.deps.MemSvc = s.memSvc
	}
	go func() {
		if err := s.memSvc.Start(ctx); err != nil {
			log.Printf("daemon memory: start error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		// Stop the memory sidecar before HTTP shutdown so SIGTERM reaches the
		// child process while the daemon is still alive to drain its exit.
		if s.memSvc != nil {
			_ = s.memSvc.Stop()
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.server.Shutdown(shutCtx)
	}()

	if err := s.server.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleChromeShow(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := showChromeOnPortFn(s.chromeControlPort()); err != nil {
		if errors.Is(err, mcp.ErrChromeNotRunning) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "chrome_not_running"})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "visible"})
}

func (s *Server) handleChromeHide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := hideChromeOnPortFn(s.chromeControlPort()); err != nil {
		if errors.Is(err, mcp.ErrChromeNotRunning) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "chrome_not_running"})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "hidden"})
}

func (s *Server) handleChromeStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := getChromeStatusOnPortFn(s.chromeControlPort())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"running":     status.Running,
		"visible":     status.Visible,
		"probe_error": status.ProbeError,
	})
}

func (s *Server) handleChromeProfile(w http.ResponseWriter, r *http.Request) {
	state, err := getChromeProfileStateFn(s.configuredChromeProfile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleChromeProfileUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode    string `json:"mode"`
		Profile string `json:"profile,omitempty"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	switch req.Mode {
	case "auto":
		req.Profile = ""
	case "explicit":
		if !mcp.ValidChromeProfileName(req.Profile) {
			writeError(w, http.StatusBadRequest, "invalid chrome profile name")
			return
		}
		state, err := getChromeProfileStateFn("")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		found := false
		for _, profile := range state.Profiles {
			if profile.Name == req.Profile {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusBadRequest, "chrome profile not found")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, `mode must be "auto" or "explicit"`)
		return
	}

	patch := map[string]interface{}{
		"daemon": map[string]interface{}{
			"chrome_profile": nil,
		},
	}
	if req.Profile != "" {
		patch["daemon"] = map[string]interface{}{
			"chrome_profile": req.Profile,
		}
	}
	prevProfile := s.configuredChromeProfile()
	if err := s.patchGlobalConfig(patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setConfiguredChromeProfile(req.Profile)
	stopChromeFn()
	if err := resetChromeProfileCloneFn(); err != nil {
		rollbackPatch := map[string]interface{}{
			"daemon": map[string]interface{}{
				"chrome_profile": nil,
			},
		}
		if prevProfile != "" {
			rollbackPatch["daemon"] = map[string]interface{}{
				"chrome_profile": prevProfile,
			}
		}
		if rollbackErr := s.patchGlobalConfig(rollbackPatch); rollbackErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to refresh chrome profile clone: %v (rollback failed: %v)", err, rollbackErr))
			return
		}
		s.setConfiguredChromeProfile(prevProfile)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	state, err := getChromeProfileStateFn(req.Profile)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleChromeProfileRefresh(w http.ResponseWriter, r *http.Request) {
	stopChromeFn()
	if err := resetChromeProfileCloneFn(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	state, err := getChromeProfileStateFn(s.configuredChromeProfile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "shutting_down"})
	if s.cancel != nil {
		log.Println("daemon: shutdown requested via /shutdown")
		mcp.StopCDPChrome()
		go s.cancel()
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RouteKey    string `json:"route_key,omitempty"`
		SessionID   string `json:"session_id,omitempty"`
		Agent       string `json:"agent,omitempty"`
		Reason      string `json:"reason,omitempty"`
		RestoreLast bool   `json:"restore_last,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	key := req.RouteKey
	if key == "" && req.SessionID != "" {
		key = "session:" + sanitizeRouteValue(req.SessionID)
	}
	if key == "" && req.Agent != "" {
		key = "agent:" + sanitizeRouteValue(req.Agent)
	}
	if key == "" {
		http.Error(w, `{"error":"route_key, session_id, or agent required"}`, http.StatusBadRequest)
		return
	}

	reason, ok := agenttypes.ParseCancelReason(req.Reason)
	if !ok {
		http.Error(w, `{"error":"unknown reason; expected user_cancel|interrupt|background|idle_timeout"}`, http.StatusBadRequest)
		return
	}

	// Fast path: legacy callers that don't request a restore get the old
	// fire-and-forget semantics. Slow path waits for the run to exit so we
	// can safely slice the session.
	if !req.RestoreLast {
		s.deps.SessionCache.CancelRouteWithReason(key, reason)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"status":   "cancelled",
			"route":    key,
			"reason":   reason.String(),
			"restored": false,
		})
		return
	}

	restored, err := s.deps.SessionCache.CancelRouteForRestore(key, reason, true, 5*time.Second)
	if errors.Is(err, ErrCancelRestoreTimeout) {
		writeError(w, http.StatusGatewayTimeout, "agent run did not exit within 5s; restore aborted")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if restored != nil {
		s.publishCancelRestored(key, restored)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"status":   "cancelled",
			"route":    key,
			"reason":   reason.String(),
			"restored": true,
			"text":     restored.Text,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"status":   "cancelled",
		"route":    key,
		"reason":   reason.String(),
		"restored": false,
	})
}

// publishCancelRestored emits the cancel.restored SSE event so subscribed UI
// clients can fill the user's last message back into their input box.
func (s *Server) publishCancelRestored(routeKey string, restored *session.RestoredMessage) {
	if s.deps == nil || s.deps.EventBus == nil || restored == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"route_key":   routeKey,
		"text":        restored.Text,
		"attachments": restored.Attachments,
	})
	if err != nil {
		return
	}
	s.deps.EventBus.Emit(Event{Type: EventCancelRestored, Payload: payload})
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	var req ApprovalResponse
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RequestID == "" {
		http.Error(w, `{"error":"request_id required"}`, http.StatusBadRequest)
		return
	}
	switch req.Decision {
	case DecisionAllow, DecisionDeny, DecisionAlwaysAllow:
	default:
		http.Error(w, `{"error":"decision must be allow, deny, or always_allow"}`, http.StatusBadRequest)
		return
	}
	// Claim the request under the broker's lock first; gate Cloud notify +
	// bus emit on whether we actually won the claim. Without this, a
	// concurrent daemon-cleanup path (timeout / ctx-cancel / CancelAll on
	// disconnect) could emit a deny/daemon terminal event for the same
	// request_id that we're about to mark allow/kocoro — Desktop would
	// have no way to tell which is authoritative. See the at-most-one
	// terminal-event contract documented on EventApprovalResolved.
	//
	// Bus emit runs as Resolve's beforeDeliver so the approval_resolved
	// event gets an ID assigned BEFORE pa.ch is written — the Request
	// goroutine, and the agent loop reading its decision, cannot resume on
	// another P and emit a tool_status with a lower bus ID. See the doc
	// comment on ApprovalBroker.Resolve. Cloud notify lives outside the
	// broker mutex: Cloud has its own event fan-out so local bus ordering
	// is the only invariant that matters here.
	emitResolved := func() {
		emitBusJSON(s.eventBus, EventApprovalResolved, map[string]any{
			"request_id":  req.RequestID,
			"decision":    string(req.Decision),
			"resolved_by": "kocoro",
			"ts":          nowISO(),
		})
	}
	// Look up the per-request broker (SSE path), then the server broker (SSE
	// source), then the WS broker (cloud/IM sources). A given approval lives in
	// exactly one broker, and Resolve only runs emitResolved after winning the
	// claim, so the fallback chain emits at most one terminal event. Reaching
	// the WS broker here is what lets Desktop resolve an IM-originated approval:
	// without it the request stayed pending until ApprovalTimeout and Cloud was
	// never told to dismiss the channel card.
	var claimed bool
	if b, ok := s.pendingBrokers.Load(req.RequestID); ok {
		claimed = b.(*ApprovalBroker).Resolve(req.RequestID, req.Decision, emitResolved)
	} else if s.approvalBroker.Resolve(req.RequestID, req.Decision, emitResolved) {
		claimed = true
	} else if s.client != nil {
		claimed = s.client.ResolveApproval(req.RequestID, req.Decision, emitResolved)
	}
	if claimed {
		_ = s.notifyApprovalResolved(ApprovalResolvedPayload{
			RequestID:  req.RequestID,
			Decision:   req.Decision,
			ResolvedBy: "kocoro",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Subscribe with atomic replay if client provides a last event ID.
	// Check both query param (custom clients) and Last-Event-ID header
	// (standard SSE EventSource reconnection per spec).
	var ch <-chan Event
	lastIDStr := r.URL.Query().Get("last_event_id")
	if lastIDStr == "" {
		lastIDStr = r.Header.Get("Last-Event-ID")
	}
	if lastIDStr != "" {
		if lastID, err := strconv.ParseUint(lastIDStr, 10, 64); err == nil {
			var missed []Event
			missed, ch = s.eventBus.SubscribeWithReplay(lastID)
			for _, evt := range missed {
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.ID, evt.Type, string(evt.Payload))
			}
			flusher.Flush()
		}
	}
	if ch == nil {
		ch = s.eventBus.Subscribe()
	}
	defer s.eventBus.Unsubscribe(ch)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case evt := <-ch:
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.ID, evt.Type, string(evt.Payload))
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// EventBus returns the server's EventBus for emitting events.
func (s *Server) EventBus() *EventBus {
	return s.eventBus
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"is_connected": s.client.IsConnected(),
		"active_agent": s.client.ActiveAgent(),
		"uptime":       int(s.client.Uptime().Seconds()),
		"version":      s.version,
		// Daemon capability tokens — Desktop reads this to gate features
		// behind tokens advertised by this daemon version. Same list the WS
		// handshake sends to Cloud (Capabilities in client.go).
		"capabilities": Capabilities,
	}
	if s.memSvc != nil {
		resp["memory"] = s.memSvc.MemoryProviderStatus()
	}
	json.NewEncoder(w).Encode(resp)
}

// handleAgents lists available agents with optional memory status.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.deps == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"agents": []interface{}{}})
		return
	}

	entries, err := agents.ListAgents(s.deps.AgentsDir)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	type agentInfo struct {
		Name         string `json:"name"`
		DisplayName  string `json:"display_name"` // falls back to Name when unset
		Builtin      bool   `json:"builtin"`
		Override     bool   `json:"override"`
		HasMemory    bool   `json:"has_memory"`
		HasConfig    bool   `json:"has_config"`
		CommandCount int    `json:"command_count"`
		SkillCount   int    `json:"skill_count"`
	}
	result := make([]agentInfo, 0, len(entries))
	for _, entry := range entries {
		// Resolve effective directory for definition files
		dir := filepath.Join(s.deps.AgentsDir, entry.Name)
		if entry.Builtin {
			dir = filepath.Join(s.deps.AgentsDir, "_builtin", entry.Name)
		}
		// Memory is always in top-level runtime dir
		runtimeDir := filepath.Join(s.deps.AgentsDir, entry.Name)
		_, memErr := os.Stat(filepath.Join(runtimeDir, "MEMORY.md"))
		_, cfgErr := os.Stat(filepath.Join(dir, "config.yaml"))
		cmdFiles, _ := filepath.Glob(filepath.Join(dir, "commands", "*.md"))
		skillFiles, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
		result = append(result, agentInfo{
			Name:         entry.Name,
			DisplayName:  entry.DisplayName,
			Builtin:      entry.Builtin,
			Override:     entry.Override,
			HasMemory:    memErr == nil,
			HasConfig:    cfgErr == nil,
			CommandCount: len(cmdFiles),
			SkillCount:   len(skillFiles),
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"agents": result})
}

// handleSessions lists sessions, optionally filtered by agent.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.deps == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
		return
	}

	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
	}
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	summaries, err := mgr.List()
	if err != nil {
		// If the directory doesn't exist, return empty list.
		if os.IsNotExist(err) {
			json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	// Enrich each summary with runtime state so the frontend can flag
	// "running" / "awaiting approval" sessions without a second round-trip.
	// Both sources are nil-safe: cache is always non-nil here, tracker may
	// be nil when deps was constructed outside NewServer.
	activeIDs := s.deps.SessionCache.ActiveSessionIDs()
	var awaitingIDs map[string]struct{}
	if s.deps.ApprovalTracker != nil {
		awaitingIDs = s.deps.ApprovalTracker.AwaitingSet()
	}

	// Filter out empty sessions (created but never used).
	filtered := make([]session.SessionSummary, 0, len(summaries))
	for _, sum := range summaries {
		if sum.MsgCount == 0 {
			continue
		}
		if _, ok := activeIDs[sum.ID]; ok {
			sum.InProgress = true
		}
		if _, ok := awaitingIDs[sum.ID]; ok {
			sum.AwaitingApproval = true
		}
		sum.Kind = kindOf(sum.Source)
		filtered = append(filtered, sum)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": filtered})
}

// handleApprovals returns the set of session IDs currently blocked on a
// user-approval prompt. Lets the frontend re-sync on page refresh / first
// connect — the SSE EventApprovalRequest stream covers live updates but is
// lossy across reconnect.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.deps == nil || s.deps.ApprovalTracker == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []string{}})
		return
	}
	ids := s.deps.ApprovalTracker.SessionIDs()
	if ids == nil {
		ids = []string{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": ids})
}

// handleNotifications returns the notification history captured by the
// EventBus (notification / approval_request / heartbeat_alert / agent_error).
// Backed by ~/.shannon/notifications.jsonl so it survives a daemon restart;
// capped at notifRingSize entries (oldest evicted, log rewritten on load).
//
// Query params (strict parsing intentionally NOT enforced — invalid values
// silently fall back to defaults so a malformed Desktop cursor never blocks
// the UI):
//   - since: only return events with ID strictly greater than this (cursor).
//   - limit: cap result count (most-recent kept on truncate); 0 = no cap.
//   - types: comma-separated subset of event types to include.
//
// Response: {"notifications":[...], "next_cursor": <last event ID or sinceID>}.
//
// Cursor caveat: if a client changes the `types` filter between paginated
// calls, events of newly-included types with ID ≤ cursor are NOT replayed.
// Clients that switch filters mid-session should rewind the cursor to 0.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()

	var since uint64
	if v := q.Get("since"); v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = parsed
		}
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	var typeFilter map[string]struct{}
	if v := q.Get("types"); v != "" {
		typeFilter = make(map[string]struct{})
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				typeFilter[t] = struct{}{}
			}
		}
	}

	events := s.eventBus.Notifications(since, typeFilter, limit)
	if events == nil {
		events = []Event{}
	}
	cursor := since
	if n := len(events); n > 0 {
		cursor = events[n-1].ID
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"notifications": events,
		"next_cursor":   cursor,
	})
}

// handleGetSession 返回指定 session 的完整内容（包含消息列表）。
// 前端可通过消息数组的下标作为 message_index 传给 POST /sessions/{id}/edit。
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	sess, err := mgr.Load(id)
	if err != nil {
		// errors.Is traverses %w chains (os.IsNotExist does not), so a future
		// Store.Load wrap can't regress this 404 to a 500.
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	// Cancel any active route bound to this session before clearing in-memory
	// bindings. ClearSessionBindings now takes per-entry locks, which blocks
	// behind any long bash/browser run on a route bound to id; without the
	// cancel, the delete handler can hang past upstream HTTP timeouts.
	// Mirrors handleResetSession's ordering.
	s.deps.SessionCache.CancelBySessionID(id)
	if err := mgr.Delete(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.deps.SessionCache.ClearSessionBindings(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleResetSession clears a named-agent session's conversation history in
// place while preserving ID/Title/CWD and other metadata.
// Query: ?agent=<name> (required) — default-agent sessions should be discarded
// via DELETE /sessions/{id} instead; this endpoint is only for named agents
// whose routing identity must survive the wipe.
// Active runs are cancelled before the history is cleared.
//
// Known race (matches handleEditMessage): CancelBySessionID only fires the
// cancel signal and does not wait for the agent loop to exit. If the loop is
// in a mid-turn checkpoint save, its Save() may land after Reset(), leaving
// InProgress set or partial history re-applied. Callers should ensure no run
// is active before invoking /reset; a second /reset clears any residue. A
// proper barrier belongs in SessionCache and is out of scope here.
func (s *Server) handleResetSession(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agentName := r.URL.Query().Get("agent")
	if agentName == "" {
		writeError(w, http.StatusBadRequest, "agent query parameter is required; use DELETE /sessions/{id} to discard a default-agent session")
		return
	}
	if err := agents.ValidateAgentName(agentName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.deps.SessionCache.CancelBySessionID(id)

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	if err := mgr.Reset(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.deps.SessionCache.ClearSessionBindings(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset", "id": id})
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Title    *string `json:"title,omitempty"`
		Pinned   *bool   `json:"pinned,omitempty"`
		Favorite *bool   `json:"favorite,omitempty"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Title == nil && body.Pinned == nil && body.Favorite == nil {
		writeError(w, http.StatusBadRequest, "request body must include at least one of: title, pinned, favorite")
		return
	}
	var title string
	if body.Title != nil {
		title = strings.TrimSpace(*body.Title)
		if title == "" {
			writeError(w, http.StatusBadRequest, "title cannot be empty")
			return
		}
	}
	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	resp := map[string]interface{}{"status": "updated"}
	if body.Title != nil {
		if err := mgr.PatchTitle(id, title); err != nil {
			// errors.Is traverses fmt.Errorf("%w") chains; os.IsNotExist does not.
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["title"] = title
	}
	if body.Pinned != nil || body.Favorite != nil {
		if err := mgr.PatchFlags(id, body.Pinned, body.Favorite); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if body.Pinned != nil {
			resp["pinned"] = *body.Pinned
		}
		if body.Favorite != nil {
			resp["favorite"] = *body.Favorite
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessionSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.deps == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}})
		return
	}

	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
	}
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, `{"error":"q parameter required"}`, http.StatusBadRequest)
		return
	}

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	results, err := mgr.Search(query, 20)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []session.SearchResult{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"results": results})
}

// handleEditMessage truncates session history and re-runs the agent with new content.
// Body: {"message_index": N, "new_content": "...", "content": [...], "agent": "optional"}
// message_index keeps the first N messages; everything after is discarded.
// content is an optional array of multimodal blocks (images, files, etc.), same format as POST /message.
func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body struct {
		MessageIndex int                   `json:"message_index"`
		NewContent   string                `json:"new_content"`
		Content      []RequestContentBlock `json:"content,omitempty"`
		Agent        string                `json:"agent,omitempty"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// Note: Validate() not called here — inline validation runs before truncation
	// to avoid side-effects on bad input.
	if strings.TrimSpace(body.NewContent) == "" && len(body.Content) == 0 {
		writeError(w, http.StatusBadRequest, "new_content or content is required")
		return
	}
	if body.Agent != "" {
		if err := agents.ValidateAgentName(body.Agent); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Cancel any active run using this session, regardless of route key type
	// (agent:<name>, session:<id>, default:<source>:<channel>).
	s.deps.SessionCache.CancelBySessionID(id)

	// 截断 session 历史消息
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(body.Agent))
	if err := mgr.TruncateMessages(id, body.MessageIndex); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
		case errors.Is(err, session.ErrMessageIndexOutOfRange):
			// Genuine client error: the requested index is out of range.
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			// Load corruption / Save IO failure etc. is server-side, not 400.
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// 以新内容重新触发 agent，复用现有消息发送流程
	runReq := RunAgentRequest{
		Text:      body.NewContent,
		Content:   body.Content,
		Agent:     body.Agent,
		SessionID: id,
		Source:    "kocoro",
	}
	runReq.EnsureRouteKey()

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleMessageSSE(w, r, runReq)
		return
	}

	handler := &httpEventHandler{}
	result, err := RunAgent(r.Context(), s.deps, runReq, handler)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleSessionSummary 生成面向人类阅读的会话摘要，带缓存。
// 缓存失效条件：消息数量或 UpdatedAt 变化（新消息追加或编辑 truncate）。
// TODO: 对同一 session 的并发请求可能触发多次 LLM 调用，低优先级优化。
func (s *Server) handleSessionSummary(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	sess, err := mgr.Load(id)
	if err != nil {
		// errors.Is traverses %w chains (os.IsNotExist does not), so a future
		// Store.Load wrap can't regress this 404 to a 500.
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if len(sess.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "session has no messages")
		return
	}

	// 缓存 key = "消息数:UpdatedAt纳秒"，任何消息变更或编辑都会使之失效
	cacheKey := fmt.Sprintf("%d:%d", len(sess.Messages), sess.UpdatedAt.UnixNano())

	// 缓存命中
	if sess.SummaryCache != "" && sess.SummaryCacheKey == cacheKey {
		writeJSON(w, http.StatusOK, map[string]any{
			"summary":       sess.SummaryCache,
			"cached":        true,
			"message_count": len(sess.Messages),
		})
		return
	}

	// 缓存未命中：调用 LLM 生成摘要
	summary, err := ctxwin.SummarizeForUser(r.Context(), s.deps.GW, sess.Messages)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("summarization failed: %v", err))
		return
	}

	// 从磁盘重新读取最新 session 后仅 patch 缓存字段，避免覆盖 agent 期间追加的新消息
	if saveErr := mgr.PatchSummaryCache(id, summary, cacheKey); saveErr != nil {
		log.Printf("daemon: failed to save summary cache for session %s: %v", id, saveErr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"summary":       summary,
		"cached":        false,
		"message_count": len(sess.Messages),
	})
}

// handleMessage runs an agent turn via POST. Supports synchronous JSON and SSE streaming.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil {
		http.Error(w, `{"error":"daemon deps not configured"}`, http.StatusInternalServerError)
		return
	}

	var req RunAgentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.SessionID != "" {
		if err := ValidateSessionID(req.SessionID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Source == "" {
		req.Source = "kocoro"
	}
	// Normalize "default" → "" early so downstream guards are consistent.
	if req.Agent == "default" {
		req.Agent = ""
	}
	// Named agents honor new_session / session_id exactly like the default
	// agent — they are no longer locked to a single long-lived session.
	// Forking is driven by ComputeRouteKey (session_id → exact resume;
	// new_session → fresh) and the kind-filtered cold-start fallback
	// (resumeNamedAgentColdStart resolves the latest interactive session).
	req.EnsureRouteKey()
	if err := req.Validate(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Slash-command routing: /research, /swarm and /dag dispatch directly to Shannon
	// Cloud's Gateway, bypassing the local agent loop AND the in-flight injection
	// path. A slash request always starts a fresh cloud workflow — never injects
	// as mid-run user text into an active routed session.
	if cmd := cloudflow.ParseSlash(req.Text); cmd != nil {
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			http.Error(w, `{"error":"slash commands require Accept: text/event-stream"}`, http.StatusBadRequest)
			return
		}
		if len(req.Content) > 0 {
			http.Error(w, `{"error":"slash commands do not support attachments"}`, http.StatusBadRequest)
			return
		}
		s.handleSlashSSE(w, r, req, cmd)
		return
	}

	// Try injecting into an in-flight run on the same route. Both plain-text
	// and attachment-bearing follow-ups inject: req.Content is lowered to
	// InjectedMessage.Files via contentBlocksToInjected (reusing
	// resolveContentBlocks for image compression / file_ref disk reads /
	// document passthrough). When no active run exists we fall through to the
	// normal session-start path below, which consumes req.Content via
	// resolveContentBlocks. The Desktop client always carries a RouteKey on
	// every send (including new sessions), so gate on an actual active run —
	// not RouteKey alone — to avoid misrouting fresh requests through inject.
	if req.RouteKey != "" {
		if s.deps.SessionCache.HasActiveRun(req.RouteKey) {
			injectText := req.Text
			var injectFiles []agent.InjectedFile
			if len(req.Content) > 0 {
				ctext, cfiles := contentBlocksToInjected(req.Content)
				injectFiles = cfiles
				if ctext != "" {
					if injectText != "" {
						injectText += "\n\n" + ctext
					} else {
						injectText = ctext
					}
				}
			}
			switch s.deps.SessionCache.InjectMessage(req.RouteKey, agent.InjectedMessage{Text: injectText, CWD: req.CWD, Files: injectFiles, ClientMessageID: req.ClientMessageID}) {
			case InjectOK:
				if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("Connection", "keep-alive")
					fmt.Fprintf(w, "event: injected\ndata: %s\n\n", req.RouteKey)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{
					"status": "injected",
					"route":  req.RouteKey,
				})
				return
			case InjectQueueFull:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]string{
					"status": "rejected",
					"reason": "queue_full",
					"route":  req.RouteKey,
				})
				return
			case InjectBusy:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{
					"status": "rejected",
					"reason": "active_run_not_ready",
					"route":  req.RouteKey,
				})
				return
			case InjectCWDConflict:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{
					"status": "rejected",
					"reason": "cwd_conflict",
					"route":  req.RouteKey,
				})
				return
			case InjectRetracted:
				// The client retracted this id before the inject arrived (late
				// POST racing a Cmd+Enter retract+cancel+resend). Dropping it
				// here IS the retraction taking effect — report success so the
				// fire-and-forget client treats it as settled.
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{
					"status": "retracted_before_delivery",
					"route":  req.RouteKey,
				})
				return
			case InjectNoActiveRun:
				// Run ended between HasActiveRun and InjectMessage. Fall through;
				// the inject_only guard below 409s instead of starting a new run.
			}
		}
	}

	// inject_only clients (Desktop busy-state inject) never start a new run: if
	// we reach here the follow-up could not be injected into an active run (none
	// present, or it ended mid-request), so 409 and let the client re-queue
	// locally instead of spawning a duplicate fresh run.
	if req.InjectOnly {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "rejected",
			"reason": "no_active_run",
			"route":  req.RouteKey,
		})
		return
	}

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleMessageSSE(w, r, req)
		return
	}

	handler := &httpEventHandler{}
	result, err := RunAgent(r.Context(), s.deps, req, handler)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleRetractInject marks a previously-injected steering follow-up as
// cancelled. The Desktop client calls this when the user retracts a queued-draft
// card whose inject was already sent to the active run (steering inject fires on
// enqueue, so a cancel can race ahead of the loop's drain). The agent loop drops
// the matching client_message_id at the next drain boundary so it never reaches
// the model. No-op-safe: retracting an id the run already drained, or a route
// with no active run, just leaves a tombstone reaped at run end.
func (s *Server) handleRetractInject(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		http.Error(w, `{"error":"daemon deps not configured"}`, http.StatusInternalServerError)
		return
	}
	var req RunAgentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ClientMessageID) == "" {
		writeError(w, http.StatusBadRequest, "client_message_id is required")
		return
	}
	if req.SessionID != "" {
		if err := ValidateSessionID(req.SessionID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Source == "" {
		req.Source = "kocoro"
	}
	if req.Agent == "default" {
		req.Agent = ""
	}
	req.EnsureRouteKey()
	if req.RouteKey == "" {
		writeError(w, http.StatusBadRequest, "could not resolve a route to retract from")
		return
	}
	// Atomically resolve which side of the commit race this retract landed on.
	// "already_committed" tells the client its follow-up already entered an LLM
	// turn (the text is a persisted user message) and must NOT be re-sent as a
	// fresh message — the force-send / pop-back duplicate. "retracted" plants a
	// TTL-reaped tombstone honored at every consumption point: the loop's drain
	// filter, the end_turn survivor drain, ingestion of a late inject on a
	// future run, and the next run's mailbox drain. Tombstones are planted even
	// without an active run — that is exactly the teardown race the durable
	// ledger exists for.
	status := s.deps.SessionCache.RetractInjectWithStatus(req.RouteKey, req.ClientMessageID)
	// Cascade: if the inject already survived into the durable mailbox
	// (ReEnqueueInjectSurvivors ran before the retract arrived), delete the
	// row so the next run's startup drain cannot prepend the cancelled text.
	if status == "retracted" {
		s.deps.SessionCache.RetractMailboxByClientMessageID(req.RouteKey, req.ClientMessageID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":            status,
		"route":             req.RouteKey,
		"client_message_id": req.ClientMessageID,
	})
}

// newSSEApprovalSendFn builds the per-request broker sendFn that frames an
// ApprovalRequest as the `event: approval` SSE frame. Named (rather than an
// inline closure in handleMessageSSE) so the wire-fixture test exercises the
// real framing — event name included — instead of reconstructing it.
func newSSEApprovalSendFn(w io.Writer, flusher http.Flusher) func(ApprovalRequest) error {
	return func(areq ApprovalRequest) error {
		data := mustJSON(areq)
		_, err := fmt.Fprintf(w, "event: approval\ndata: %s\n\n", data)
		flusher.Flush()
		return err
	}
}

// handleMessageSSE streams agent events as SSE.
func (s *Server) handleMessageSSE(w http.ResponseWriter, r *http.Request, req RunAgentRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Create a per-request broker to avoid racing with concurrent SSE requests.
	// Each SSE stream gets its own broker with its own sendFn and pending map.
	reqBroker := NewApprovalBroker(newSSEApprovalSendFn(w, flusher))
	// Inherit bus hooks from the server broker so EventBus emission stays
	// consistent with the WS path (request payload + daemon-cleanup deny).
	reqBroker.onRequest = s.approvalBroker.onRequest
	reqBroker.onCleanup = s.approvalBroker.onCleanup
	// Register pending requestIDs so POST /approval can find this broker.
	reqBroker.onRegister = func(requestID string) { s.pendingBrokers.Store(requestID, reqBroker) }
	reqBroker.onDeregister = func(requestID string) { s.pendingBrokers.Delete(requestID) }

	// Cancel only this request's pending approvals when the SSE stream ends.
	defer reqBroker.CancelAll()

	// Resolve auto_approve: per-agent overrides global
	cfg, _, _ := s.deps.Snapshot()
	autoApprove := cfg.Daemon.AutoApprove
	if req.Agent != "" {
		if a, err := agents.LoadAgent(s.deps.AgentsDir, req.Agent); err == nil && a.Config != nil && a.Config.AutoApprove != nil {
			autoApprove = *a.Config.AutoApprove
		}
	}

	handler := &sseEventHandler{w: w, flusher: flusher, broker: reqBroker, ctx: r.Context(), autoApprove: autoApprove, deps: s.deps, agent: req.Agent, source: req.Source}
	result, err := RunAgent(r.Context(), s.deps, req, handler)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"error": err.Error()}))
		flusher.Flush()
		return
	}

	fmt.Fprintf(w, "event: done\ndata: %s\n\n", mustJSON(result))
	flusher.Flush()
}

// handleSlashSSE streams a /research or /swarm cloud workflow over SSE.
// Output shape matches handleMessageSSE so Desktop's existing done-event
// consumer works unchanged.
func (s *Server) handleSlashSSE(w http.ResponseWriter, r *http.Request, req RunAgentRequest, cmd *cloudflow.SlashCommand) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Reuse the existing sseEventHandler so cloud_agent / cloud_progress /
	// cloud_plan / delta / usage events go out the same wire format the
	// Desktop already consumes (server.go:1276+).
	// broker and autoApprove are zero-valued: cloud workflows don't use the
	// approval broker (no tool calls needing local approval) and autoApprove
	// defaulting to false is safe because OnApprovalNeeded is never called
	// from cloudflow.Run.
	handler := &sseEventHandler{w: w, flusher: flusher, ctx: r.Context(), deps: s.deps}

	result, err := RunSlashWorkflow(r.Context(), s.deps, req, cmd, handler)
	if errors.Is(err, ErrSlashRouteBusy) {
		// The stream has already started, so this is an SSE-level conflict
		// response, not an HTTP 409. The reason matches the injection layer's
		// active-run conflict semantic (server.go:1129-1136).
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{
			"error":  err.Error(),
			"reason": "active_run_not_ready",
		}))
		flusher.Flush()
		return
	}
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"error": err.Error()}))
		flusher.Flush()
		return
	}

	// Same payload shape as handleMessageSSE's done event (server.go:1214) —
	// Desktop's done consumer parses RunAgentResult JSON and renders result.Reply.
	fmt.Fprintf(w, "event: done\ndata: %s\n\n", mustJSON(result))
	flusher.Flush()
}

// handlePermissions returns current macOS TCC permission status.
func (s *Server) handlePermissions(w http.ResponseWriter, r *http.Request) {
	result := probePermissions(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handlePermissionsRequest triggers macOS permission dialogs for the requested permission.
func (s *Server) handlePermissionsRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Permission string `json:"permission"` // "screen_recording", "accessibility", or "automation"
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	switch req.Permission {
	case "screen_recording", "accessibility", "automation":
		// valid
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "unsupported permission: " + req.Permission})
		return
	}
	result := requestPermission(r.Context(), req.Permission)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// httpEventHandler is an EventHandler for synchronous HTTP responses.
type httpEventHandler struct {
	usage agent.UsageAccumulator
}

// Usage returns the cumulative usage collected during this handler's lifetime.
func (h *httpEventHandler) Usage() agent.AccumulatedUsage { return h.usage.Snapshot() }

func (h *httpEventHandler) OnToolCall(name string, args string, toolUseID string) {}
func (h *httpEventHandler) OnToolResult(name string, args string, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	log.Printf("http: tool %s completed (%.1fs)", name, elapsed.Seconds())
}
func (h *httpEventHandler) OnText(text string)            {}
func (h *httpEventHandler) OnPreamble(text string)        {}
func (h *httpEventHandler) OnStreamDelta(delta string)    {}
func (h *httpEventHandler) OnUsage(usage agent.TurnUsage) { h.usage.Add(usage) }

// OnApprovalNeeded auto-approves for local HTTP API calls.
// Threat model: localhost-only, unauthenticated but local-trusted.
// Permission engine (hard-blocks, denied_commands) runs before this.
// If daemon ever listens on non-localhost, this MUST require auth.
func (h *httpEventHandler) OnCloudAgent(agentID, status, message string)           {}
func (h *httpEventHandler) OnCloudProgress(completed, total int)                   {}
func (h *httpEventHandler) OnCloudPlan(planType, content string, needsReview bool) {}

// OnApprovalNeeded auto-approves for local HTTP API calls except for tools on
// the unattended deny-list. HTTP callers are often scripts / automation, so the
// path stays aligned with scheduled runs. The list is empty as of 2026-05-18.
func (h *httpEventHandler) OnApprovalNeeded(tool string, args string) bool {
	return !agent.DisallowsUnattendedAutoApproval(tool)
}

// sseEventHandler streams agent events as SSE to an HTTP response.
type sseEventHandler struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	broker      *ApprovalBroker
	ctx         context.Context
	autoApprove bool
	deps        *ServerDeps
	// agent identifies the per-agent config.yaml to write always-allow tools to.
	// Empty for default-agent / non-routed paths (e.g. /research, /swarm, /dag) — in
	// that case persistAgentAlwaysAllow falls back to session-only.
	agent string
	// source mirrors RunAgentRequest.Source ("kocoro" for local Desktop,
	// "slack"/"wecom"/"schedule"/... for cloud-distributed channels). Threaded
	// into ApprovalRequestMeta so the approval_request bus payload carries the
	// channel-bucket distinct from the more specific Channel field.
	source string
	// sessionID is set by RunAgent via SetSessionID after the session is
	// resolved. Approval Mark/Clear keys on this so a paused session can be
	// surfaced via deps.ApprovalTracker.
	//
	// Plain string (no atomic) is intentional: SetSessionID is called exactly
	// once from RunAgent before the first tool call, and OnApprovalNeeded
	// reads it on the same agent loop goroutine — single-writer, then
	// happens-before-ordered single-reader via RunAgent's sequencing.
	// Contrast routeEntry.sessionID (atomic.Pointer) which is read from
	// arbitrary HTTP handler goroutines (CancelBySessionID scans all routes).
	sessionID string
	usage     agent.UsageAccumulator
}

// SetSessionID captures the resolved session ID. Called by RunAgent's
// multiHandler interface-assertion path (see runner.go SetSessionID injection).
//
// Also emits an SSE `session_started` event so SSE consumers (Desktop) can
// capture session_id BEFORE the first delta/tool/done event. Without this,
// Desktop only learns the session_id on the `done` event, which means a
// follow-up message sent mid-turn (or before `done` arrives) goes out with
// no session_id and the daemon opens a fresh session instead of continuing
// the same conversation.
func (h *sseEventHandler) SetSessionID(id string) {
	h.sessionID = id
	if id == "" || h.w == nil {
		return
	}
	fmt.Fprintf(h.w, "event: session_started\ndata: %s\n\n", mustJSON(map[string]string{"session_id": id}))
	if h.flusher != nil {
		h.flusher.Flush()
	}
}

// Usage returns the cumulative usage collected during this handler's lifetime.
func (h *sseEventHandler) Usage() agent.AccumulatedUsage { return h.usage.Snapshot() }

func (h *sseEventHandler) OnToolCall(name string, args string, toolUseID string) {
	// Match bus payload: redact-first, then truncate. `audit.RedactSecrets ∘
	// truncate` is wrong — a secret that straddles the byte-200 boundary
	// gets chopped into a fragment before the redaction regex sees it, and
	// then leaks through SSE. See redactAndTruncate + the boundary
	// regression test in bus_handler_test.go. tool_use_id pairs this running
	// frame with its later completed frame (see bus_handler.go).
	data := mustJSON(map[string]interface{}{
		"tool":        name,
		"tool_use_id": toolUseID,
		"status":      "running",
		"args":        redactAndTruncate(args, 200),
	})
	fmt.Fprintf(h.w, "event: tool\ndata: %s\n\n", data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnToolResult(name string, args string, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	// SSE is request-scoped (one tool stream per HTTP request), so session_id
	// is intentionally omitted here; session correlation is handled at the client
	// session boundary. `is_error` and `preview` mirror the bus payload so the
	// Desktop foreground pill can render errors / a short result preview.
	// tool_use_id pairs this completed frame with its earlier running frame.
	data := mustJSON(map[string]interface{}{
		"tool":        name,
		"tool_use_id": toolUseID,
		"status":      "completed",
		"elapsed":     elapsed.Seconds(),
		"is_error":    result.IsError,
		"preview":     redactAndTruncate(toolResultPreview(result), 200),
	})
	fmt.Fprintf(h.w, "event: tool\ndata: %s\n\n", data)
	h.flusher.Flush()
}

// OnText is a no-op on the per-request SSE path: the final-answer text is
// delivered to the HTTP /messages stream client via the trailing
// `event: done` payload (handleMessageSSE / handleSlashSSE), so an extra
// `assistant_text` event would duplicate it. Mid-turn preamble flows through
// OnPreamble below.
func (h *sseEventHandler) OnText(text string) {}

// OnInjectedCommitted implements agent.InjectCommitHandler: when the loop drains
// a mid-run injected follow-up into the live turn, emit an injected_committed
// SSE frame so the Desktop client flips its queued-draft card (keyed by the
// client-supplied message_id) into a real user bubble at the consume boundary.
func (h *sseEventHandler) OnInjectedCommitted(clientMessageID, text string) {
	if clientMessageID == "" {
		return
	}
	data := mustJSON(map[string]string{"message_id": clientMessageID, "text": text})
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", EventInjectedCommitted, data)
	if h.flusher != nil {
		h.flusher.Flush()
	}
}

// OnPreamble streams mid-turn agent narration to the per-request SSE client
// (HTTP POST /messages with stream=true). Mirrors busEventHandler.OnPreamble
// so HTTP-stream subscribers see the same preamble events as EventBus subscribers.
func (h *sseEventHandler) OnPreamble(text string) {
	if text == "" {
		return
	}
	data := mustJSON(map[string]string{"text": text})
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", EventAssistantText, data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnStreamDelta(delta string) {
	data := mustJSON(map[string]string{"text": delta})
	fmt.Fprintf(h.w, "event: delta\ndata: %s\n\n", data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnUsage(usage agent.TurnUsage) {
	h.usage.Add(usage)
	// Also emit as SSE event so clients can render live cost meters.
	data := mustJSON(map[string]interface{}{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.TotalTokens,
		"cost_usd":      usage.CostUSD,
		"llm_calls":     usage.LLMCalls,
		"model":         usage.Model,
	})
	fmt.Fprintf(h.w, "event: usage\ndata: %s\n\n", data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnCloudAgent(agentID, status, message string) {
	data, _ := json.Marshal(map[string]interface{}{
		"agent_id": agentID,
		"status":   status,
		"message":  message,
	})
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", EventCloudAgent, data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnCloudProgress(completed, total int) {
	data, _ := json.Marshal(map[string]interface{}{
		"completed": completed,
		"total":     total,
	})
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", EventCloudProgress, data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnCloudPlan(planType, content string, needsReview bool) {
	data, _ := json.Marshal(map[string]interface{}{
		"type":         planType,
		"content":      content,
		"needs_review": needsReview,
	})
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", EventCloudPlan, data)
	h.flusher.Flush()
}

// OnApprovalNeeded sends an approval request over SSE and blocks until the
// client responds via POST /approval or the request context is cancelled.
func (h *sseEventHandler) OnApprovalNeeded(tool string, args string) bool {
	if h.autoApprove {
		// daemon.auto_approve=true is a "skip prompts" global, but keep this
		// routed through the unattended deny-list so a future non-unattended-safe
		// tool can still force a broker round-trip. Empty as of 2026-05-18.
		if !agent.DisallowsUnattendedAutoApproval(tool) {
			log.Printf("sse: auto-approving %s (auto_approve=true)", tool)
			return true
		}
		log.Printf("sse: %s requires per-call approval (auto_approve=true); prompting via broker", tool)
	}
	if h.broker == nil {
		log.Printf("sse: approval broker unavailable for %s; denying", tool)
		return false
	}
	// Local SSE path: no Cloud claim, so messageID is empty. The broker stays
	// in-process via its own pending map, no WS envelope round-trips Cloud.
	//
	// Mark/Clear use defer so a panic inside broker.Request (e.g. SSE writer
	// failure) can't leak a phantom session in the tracker — net/http's
	// per-request panic recovery would otherwise leave IsAwaiting stuck at
	// true forever. The temp var stays because test fixtures construct
	// sseEventHandler with deps == nil (see server_test.go); production
	// always wires deps via NewServer.
	var tracker *ApprovalTracker
	if h.deps != nil {
		tracker = h.deps.ApprovalTracker
	}
	tracker.Mark(h.sessionID)
	defer tracker.Clear(h.sessionID)
	decision := h.broker.Request(h.ctx, ApprovalRequestMeta{
		SessionID: h.sessionID,
		Source:    h.source,
		Agent:     h.agent,
	}, tool, args)
	if decision == DecisionAlwaysAllow {
		HandleAlwaysAllowDecision(h.deps, h.broker, h.agent, tool, args)
	}
	return decision == DecisionAllow || decision == DecisionAlwaysAllow
}

// mustJSON marshals v to JSON, returning "{}" on error.
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// isJSONNull checks if a json.RawMessage represents a JSON null value.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

const (
	maxBodySize   = 50 << 20 // 50 MB — accommodates base64-encoded attachments (30 MB file → ~40 MB base64)
	maxUploadSize = 10 << 20
)

var skillSubresourceFileRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)

// decodeBody reads a JSON request body with a size limit. Returns false and
// writes an error response if decoding fails.
func decodeBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	return true
}

func decodeOptionalBody(w http.ResponseWriter, r *http.Request, v interface{}) (ok bool, provided bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return false, false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return true, false
	}
	if err := json.Unmarshal(data, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false, false
	}
	return true, true
}

func (s *Server) skillSources() ([]skills.SkillSource, error) {
	if s.deps == nil {
		return nil, fmt.Errorf("daemon deps not configured")
	}
	// Only return global (installed) skills. Builtin skills (kocoro) are
	// auto-installed to global by EnsureBuiltinSkills at startup.
	global := skills.SkillSource{
		Dir:    filepath.Join(s.deps.ShannonDir, "skills"),
		Source: skills.SourceGlobal,
	}
	return []skills.SkillSource{global}, nil
}

// skillNamesFromRequest extracts the URL-safe identifier (Slug, falling
// back to Name for legacy clients) for each skill entry. The returned
// list is what gets persisted to _attached.yaml and what URL-based
// attach routes also use — keeping body-based and URL-based attach in
// sync so the same skill can be resolved by the loader regardless of
// which API path wrote the manifest.
func skillNamesFromRequest(entries []*skills.Skill) []string {
	names := make([]string, 0, len(entries))
	for _, skill := range entries {
		if skill == nil {
			continue
		}
		ident := skill.Slug
		if ident == "" {
			ident = skill.Name
		}
		if ident != "" {
			names = append(names, ident)
		}
	}
	return names
}

func (s *Server) validateInstalledSkills(names []string) error {
	if len(names) == 0 {
		return nil
	}
	if s.deps == nil {
		return fmt.Errorf("daemon deps not configured")
	}
	list, err := agents.LoadGlobalSkills(s.deps.ShannonDir)
	if err != nil {
		return fmt.Errorf("load installed skills: %w", err)
	}
	// Accept either Slug (directory / marketplace identifier) or Name
	// (frontmatter display label) as the identifier. Slug is the primary
	// key we advise clients to use; Name is kept for backward compat.
	installed := make(map[string]bool, len(list)*2)
	for _, skill := range list {
		installed[skill.Slug] = true
		installed[skill.Name] = true
	}
	var missing []string
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !installed[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	if len(missing) == 1 {
		return fmt.Errorf("skill %q is not installed", missing[0])
	}
	return fmt.Errorf("skills not installed: %s", strings.Join(missing, ", "))
}

func (s *Server) resolveSkillDir(name string) (string, string, bool, error) {
	if s.deps == nil {
		return "", "", false, fmt.Errorf("daemon deps not configured")
	}
	globalDir := filepath.Join(s.deps.ShannonDir, "skills", name)
	if _, err := os.Stat(filepath.Join(globalDir, "SKILL.md")); err == nil {
		return globalDir, skills.SourceGlobal, false, nil
	}
	return "", "", false, os.ErrNotExist
}

func isValidSkillFileName(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	return filepath.Base(name) == name && skillSubresourceFileRE.MatchString(name)
}

func modeForSubresource(subdir string) os.FileMode {
	switch subdir {
	case "scripts":
		return 0755
	default:
		return 0644
	}
}

// configKeyAliases maps known camelCase/PascalCase JSON field names that legacy
// clients may send back to their canonical snake_case YAML equivalents.
var configKeyAliases = map[string]string{
	"apiKey":          "api_key",
	"APIKey":          "api_key",
	"modelTier":       "model_tier",
	"ModelTier":       "model_tier",
	"autoUpdateCheck": "auto_update_check",
	"AutoUpdateCheck": "auto_update_check",
	"mcpServers":      "mcp_servers",
	"MCPServers":      "mcp_servers",
}

// normalizePatchKeys rewrites known camelCase aliases to snake_case at the
// top level of m only. All aliases in configKeyAliases are top-level config
// keys; nested maps are intentionally not traversed to avoid false-positive
// renames of unrelated fields that share an alias name.
// When both an alias and its canonical key are present, the canonical wins
// and the alias is discarded.
func normalizePatchKeys(m map[string]interface{}) {
	if m == nil {
		return
	}
	for k := range m {
		canonical, aliased := configKeyAliases[k]
		if !aliased {
			continue
		}
		if _, canonicalExists := m[canonical]; !canonicalExists {
			m[canonical] = m[k]
		}
		delete(m, k)
	}
}

// stripRedactedSecrets removes "***" placeholder values from the known sensitive
// paths only: top-level api_key and mcp_servers.<name>.env.<var>. This prevents
// a GET→PATCH round-trip from overwriting real credentials with redacted values,
// without globally blocking the literal string "***" as a config value elsewhere.
func stripRedactedSecrets(m map[string]interface{}) {
	if m == nil {
		return
	}
	if s, ok := m["api_key"].(string); ok && s == "***" {
		delete(m, "api_key")
	}
	servers, ok := m["mcp_servers"].(map[string]interface{})
	if !ok {
		return
	}
	for _, srv := range servers {
		srvMap, ok := srv.(map[string]interface{})
		if !ok {
			continue
		}
		env, ok := srvMap["env"].(map[string]interface{})
		if !ok {
			continue
		}
		for k, v := range env {
			if s, ok := v.(string); ok && s == "***" {
				delete(env, k)
			}
		}
	}
}

// redactConfigSecrets removes sensitive values from a config map before
// sending it over the API. Redacts api_key at top level and env vars
// inside mcp_servers entries.
func redactConfigSecrets(m map[string]interface{}) {
	if m == nil {
		return
	}
	if _, ok := m["api_key"]; ok {
		m["api_key"] = "***"
	}
	servers, ok := m["mcp_servers"].(map[string]interface{})
	if !ok {
		return
	}
	for _, srv := range servers {
		srvMap, ok := srv.(map[string]interface{})
		if !ok {
			continue
		}
		if env, ok := srvMap["env"].(map[string]interface{}); ok {
			for k := range env {
				env[k] = "***"
			}
		}
	}
}

// deepMerge merges src into dst recursively (RFC 7386 JSON Merge Patch).
// null values delete keys, nested maps merge, scalars replace.
func deepMerge(dst, src map[string]interface{}) {
	for key, srcVal := range src {
		if srcVal == nil {
			delete(dst, key)
			continue
		}
		srcMap, srcIsMap := srcVal.(map[string]interface{})
		if srcIsMap {
			if dstVal, ok := dst[key]; ok {
				if dstMap, dstIsMap := dstVal.(map[string]interface{}); dstIsMap {
					deepMerge(dstMap, srcMap)
					continue
				}
			}
		}
		dst[key] = srcVal
	}
}

func pruneEmptyMaps(m map[string]interface{}) bool {
	for key, val := range m {
		switch v := val.(type) {
		case nil:
			delete(m, key)
		case map[string]interface{}:
			if pruneEmptyMaps(v) {
				delete(m, key)
			}
		}
	}
	return len(m) == 0
}

func (s *Server) patchGlobalConfig(patch map[string]interface{}) error {
	globalPath := filepath.Join(s.deps.ShannonDir, "config.yaml")
	globalData, _ := os.ReadFile(globalPath)
	var current map[string]interface{}
	if len(globalData) > 0 {
		if err := yaml.Unmarshal(globalData, &current); err != nil {
			return fmt.Errorf("existing config is corrupt: %v", err)
		}
	}
	if current == nil {
		current = make(map[string]interface{})
	}

	normalizePatchKeys(patch)
	stripRedactedSecrets(patch)
	deepMerge(current, patch)
	pruneEmptyMaps(current)

	data, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	return agents.AtomicWrite(globalPath, data)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeErrorCode writes an error response carrying a stable machine-readable
// code alongside the English fallback message, so clients can localize by code.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// --- Agent CRUD handlers ---

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	a, err := agents.LoadAgent(s.deps.AgentsDir, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to load agent %s: %v", name, err))
		return
	}
	api := a.ToAPI()

	// Add builtin metadata — match ListAgents semantics:
	// Builtin=true only when loaded from _builtin (no user override).
	// Overridden=true when a user override exists for a builtin.
	builtinDir := filepath.Join(s.deps.AgentsDir, "_builtin", name)
	userDir := filepath.Join(s.deps.AgentsDir, name)
	_, builtinErr := os.Stat(filepath.Join(builtinDir, "AGENT.md"))
	_, userErr := os.Stat(filepath.Join(userDir, "AGENT.md"))
	hasBuiltin := builtinErr == nil
	hasUser := userErr == nil
	api.Builtin = hasBuiltin && !hasUser   // builtin-only, no user override
	api.Overridden = hasBuiltin && hasUser // user override of a builtin

	// Populate non-fatal trigger-conflict warnings (heartbeat ⊕ schedule).
	// Best-effort — missing schedule manager or list errors yield no warnings.
	if s.deps.ScheduleManager != nil {
		if list, err := s.deps.ScheduleManager.List(); err == nil {
			refs := make([]agents.ScheduleRef, 0, len(list))
			for _, sc := range list {
				refs = append(refs, agents.ScheduleRef{ID: sc.ID, Agent: sc.Agent, Enabled: sc.Enabled})
			}
			api.Warnings = agents.DetectTriggerConflicts(s.deps.AgentsDir, name, refs)
		}
	}

	writeJSON(w, http.StatusOK, api)
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req agents.AgentCreateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		var dne *agents.DisplayNameError
		if errors.As(err, &dne) {
			writeErrorCode(w, http.StatusBadRequest, dne.Code, dne.Error())
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	// Slug is always server-generated and immutable; clients supply only display_name.
	slug, err := agents.GenerateAgentSlug(s.deps.AgentsDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Name = slug
	// display_name is honored only via the dedicated top-level field below
	// (uniqueness-checked). Ignore any client-supplied config.display_name,
	// which would otherwise bypass the uniqueness check.
	if req.Config != nil {
		req.Config.DisplayName = ""
	}
	// Enforce global display-name uniqueness (normalized). Best-effort: the
	// check is not serialized against concurrent creates/renames (the route
	// lock below is per-slug, not per-display-name). Accepted under the
	// single-user local-daemon model; the failure mode is a duplicate label,
	// never routing/Cloud corruption.
	if req.DisplayName != "" {
		taken, err := agents.DisplayNameTaken(s.deps.AgentsDir, req.DisplayName, "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if taken {
			writeErrorCode(w, http.StatusConflict, agents.CodeDisplayNameTaken,
				fmt.Sprintf("display name %q is already in use", req.DisplayName))
			return
		}
		// Fold display_name into the config so it is persisted.
		if req.Config == nil {
			req.Config = &agents.AgentConfigAPI{}
		}
		req.Config.DisplayName = req.DisplayName
	}
	if err := s.validateInstalledSkills(skillNamesFromRequest(req.Skills)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Avatar != "" {
		if err := agents.ValidateAvatarURL(req.Avatar); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Serialize creates for the same agent name to prevent concurrent rollback races.
	routeKey := "agent:" + req.Name
	s.deps.SessionCache.LockRoute(routeKey)
	defer s.deps.SessionCache.UnlockRoute(routeKey)

	agentDir := filepath.Join(s.deps.AgentsDir, req.Name)
	// Defensive only: GenerateAgentSlug already returns a slug with no existing
	// AGENT.md, so this collision never fires in practice. Kept as a guard.
	// (Builtin override on create is impossible now — slugs are always
	// agent-<hex>, never a builtin name; customize builtins via PUT instead.)
	if _, err := os.Stat(filepath.Join(agentDir, "AGENT.md")); err == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("agent %q already exists", req.Name))
		return
	}
	// Write all agent files — rollback on any failure. The slug is freshly
	// minted (GenerateAgentSlug verified nothing exists at agentDir), so there
	// is no prior runtime state to preserve and a full dir removal is safe.
	rollback := func() { os.RemoveAll(agentDir) }
	if err := agents.WriteAgentPrompt(s.deps.AgentsDir, req.Name, req.Prompt); err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Memory != nil {
		if err := agents.WriteAgentMemory(s.deps.AgentsDir, req.Name, *req.Memory); err != nil {
			rollback()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write memory: %v", err))
			return
		}
	}
	if req.Config != nil {
		if err := agents.WriteAgentConfig(s.deps.AgentsDir, req.Name, req.Config); err != nil {
			rollback()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write config: %v", err))
			return
		}
	}
	for name, content := range req.Commands {
		if err := agents.WriteAgentCommand(s.deps.AgentsDir, req.Name, name, content); err != nil {
			rollback()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write command %s: %v", name, err))
			return
		}
	}
	if len(req.Skills) > 0 {
		if err := agents.SetAttachedSkills(s.deps.AgentsDir, req.Name, skillNamesFromRequest(req.Skills)); err != nil {
			rollback()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write skill manifest: %v", err))
			return
		}
	}
	if req.Avatar != "" {
		if err := agents.WriteAgentProfile(s.deps.AgentsDir, req.Name, &agents.AgentProfile{Avatar: req.Avatar}); err != nil {
			rollback()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write profile: %v", err))
			return
		}
	}
	a, err := agents.LoadAgent(s.deps.AgentsDir, req.Name)
	if err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("POST", "/agents", "created agent "+req.Name)
	s.triggerAgentSync()
	writeJSON(w, http.StatusCreated, a.ToAPI())
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	agentDir := filepath.Join(s.deps.AgentsDir, name)
	var req agents.AgentUpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}

	// --- Pre-validate all fields before any mutations ---
	if req.Prompt != nil && *req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt cannot be empty")
		return
	}
	var parsedMemory *string
	if req.Memory != nil && !isJSONNull(req.Memory) {
		var mem string
		if err := json.Unmarshal(req.Memory, &mem); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid memory value: %v", err))
			return
		}
		parsedMemory = &mem
	}
	var parsedConfig *agents.AgentConfigAPI
	if req.Config != nil && !isJSONNull(req.Config) {
		var cfg agents.AgentConfigAPI
		if err := json.Unmarshal(req.Config, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid config value: %v", err))
			return
		}
		if cfg.Tools != nil {
			if err := agents.ValidateToolsFilter(cfg.Tools); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if err := agents.ValidateAgentModelConfig(cfg.Agent); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		parsedConfig = &cfg
	}
	if req.DisplayName != nil {
		// Best-effort uniqueness: not serialized against concurrent renames
		// (see handleCreateAgent). Accepted for the single-user local daemon.
		trimmed := strings.TrimSpace(*req.DisplayName)
		req.DisplayName = &trimmed
		// A named agent must keep a human-readable label: reject clearing it to
		// empty (which would fall back to the opaque auto-generated slug). Use
		// null / omit the field to leave the display name unchanged.
		if trimmed == "" {
			writeErrorCode(w, http.StatusBadRequest, agents.CodeDisplayNameRequired, "display_name cannot be empty")
			return
		}
		if err := agents.ValidateDisplayName(*req.DisplayName); err != nil {
			var dne *agents.DisplayNameError
			if errors.As(err, &dne) {
				writeErrorCode(w, http.StatusBadRequest, dne.Code, dne.Error())
			} else {
				writeError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		taken, err := agents.DisplayNameTaken(s.deps.AgentsDir, *req.DisplayName, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if taken {
			writeErrorCode(w, http.StatusConflict, agents.CodeDisplayNameTaken,
				fmt.Sprintf("display name %q is already in use", *req.DisplayName))
			return
		}
	}
	for cmdName := range req.Commands {
		if err := agents.ValidateCommandName(cmdName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	for _, skill := range req.Skills {
		if skill == nil {
			writeError(w, http.StatusBadRequest, "skill entry cannot be null")
			return
		}
		// Validate the URL-safe identifier (Slug) rather than the
		// display Name. Legacy clients that only send Name fall through
		// to Name validation for backward compatibility.
		ident := skill.Slug
		if ident == "" {
			ident = skill.Name
		}
		if err := skills.ValidateSkillName(ident); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.validateInstalledSkills(skillNamesFromRequest(req.Skills)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Avatar != nil && *req.Avatar != "" {
		if err := agents.ValidateAvatarURL(*req.Avatar); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Serialize all file mutations for this agent on the same per-route lock the
	// create/delete handlers and the startup pull use, so an async pull can't
	// interleave with this update and produce mixed file state / a lost update.
	// Acquired AFTER validation (cheap, no file writes) and held through the
	// final LoadAgent. Evict is never called inside this lock (no Evict on the
	// update path), so there is no self-deadlock risk.
	routeKey := "agent:" + name
	s.deps.SessionCache.LockRoute(routeKey)
	defer s.deps.SessionCache.UnlockRoute(routeKey)

	// Materialize builtin AFTER validation passes — avoids orphaned override dirs on bad input.
	if !s.materializeIfBuiltin(w, name) {
		return
	}

	// --- Apply mutations (all inputs validated) ---
	if req.Prompt != nil {
		if err := agents.WriteAgentPrompt(s.deps.AgentsDir, name, *req.Prompt); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.Memory != nil {
		if isJSONNull(req.Memory) {
			if err := os.Remove(filepath.Join(agentDir, "MEMORY.md")); err != nil && !os.IsNotExist(err) {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete memory: %v", err))
				return
			}
		} else {
			if err := agents.WriteAgentMemory(s.deps.AgentsDir, name, *parsedMemory); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	if req.Config != nil {
		if isJSONNull(req.Config) {
			if err := clearAgentConfigPreservingDisplayName(s.deps.AgentsDir, name); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete config: %v", err))
				return
			}
		} else {
			// display_name is managed only via the dedicated top-level field
			// (uniqueness-checked). Preserve the existing on-disk value across a
			// full config rewrite and ignore any client-supplied
			// config.display_name, which would otherwise bypass the check.
			parsedConfig.DisplayName = readAgentConfigDisplayName(s.deps.AgentsDir, name)
			if err := agents.WriteAgentConfig(s.deps.AgentsDir, name, parsedConfig); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	if req.DisplayName != nil {
		// Update only display_name in config.yaml, preserving all other fields.
		// Slug/dir/sessions/Cloud bindings are untouched by design.
		// When a PUT carries both config and display_name this is a second write (config first, then this); a crash between them is acceptable under the single-user local daemon.
		if err := agents.SetAgentDisplayName(s.deps.AgentsDir, name, *req.DisplayName); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	for cmdName, content := range req.Commands {
		if err := agents.WriteAgentCommand(s.deps.AgentsDir, name, cmdName, content); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write command %s: %v", cmdName, err))
			return
		}
	}
	if req.Skills != nil {
		// Write attached skills manifest — agent loader resolves content from global/bundled.
		if err := agents.SetAttachedSkills(s.deps.AgentsDir, name, skillNamesFromRequest(req.Skills)); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write skill manifest: %v", err))
			return
		}
		// Clean up any legacy agent-scoped SKILL.md files
		agentSkillsDir := filepath.Join(s.deps.AgentsDir, name, "skills")
		_ = os.RemoveAll(agentSkillsDir)
	}
	if req.Avatar != nil {
		cur, lerr := agents.LoadAgentProfile(agentDir)
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("load profile: %v", lerr))
			return
		}
		if cur == nil {
			cur = &agents.AgentProfile{}
		}
		cur.Avatar = *req.Avatar
		if err := agents.WriteAgentProfile(s.deps.AgentsDir, name, cur); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write profile: %v", err))
			return
		}
	}
	a, err := agents.LoadAgent(s.deps.AgentsDir, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.triggerAgentSync()
	writeJSON(w, http.StatusOK, a.ToAPI())
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if requireConfirm(r.URL.Query().Get("confirm")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "confirmation_required",
			"message": "This will permanently delete the agent definition. Add ?confirm=true to proceed.",
		})
		return
	}
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	// Cannot delete a builtin-only agent (no user override)
	userDir := filepath.Join(s.deps.AgentsDir, name)
	builtinDir := filepath.Join(s.deps.AgentsDir, "_builtin", name)
	_, userErr := os.Stat(filepath.Join(userDir, "AGENT.md"))
	_, builtinErr := os.Stat(filepath.Join(builtinDir, "AGENT.md"))
	if userErr != nil && builtinErr == nil {
		writeError(w, http.StatusForbidden, "cannot delete system-managed builtin agent")
		return
	}
	// Evict handles its own per-route locking — do NOT wrap with Lock/Unlock
	// (that would self-deadlock since Evict calls evictRoute which acquires entry.mu).
	// It MUST therefore run OUTSIDE the route lock below.
	s.deps.SessionCache.Evict(name)
	// Remove only definition files — preserve runtime state (MEMORY.md, sessions/)
	// so the builtin can resurface with existing history intact. Serialize the
	// file removal on the per-route lock so an async pull can't interleave.
	agentDir := filepath.Join(s.deps.AgentsDir, name)
	routeKey := "agent:" + name
	s.deps.SessionCache.LockRoute(routeKey)
	var errs []string
	for _, f := range []string{"AGENT.md", "config.yaml", "_attached.yaml", "PROFILE.yaml"} {
		p := filepath.Join(agentDir, f)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err.Error())
		}
	}
	for _, d := range []string{"commands", "skills"} {
		p := filepath.Join(agentDir, d)
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// Clean up empty dir if no runtime state remains (still under the lock).
	if entries, err := os.ReadDir(agentDir); err == nil && len(entries) == 0 {
		os.Remove(agentDir)
	}
	s.deps.SessionCache.UnlockRoute(routeKey)
	if len(errs) > 0 {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("partial delete: %s", strings.Join(errs, "; ")))
		return
	}
	s.auditHTTPOp("DELETE", "/agents/"+name, "deleted agent")
	s.triggerAgentSync()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Agent sub-resource handlers ---

// agentExists checks that the agent directory has AGENT.md. Returns false
// and writes a 404 error if the agent does not exist.
func (s *Server) agentExists(w http.ResponseWriter, name string) bool {
	agentDir := filepath.Join(s.deps.AgentsDir, name)
	if _, err := os.Stat(filepath.Join(agentDir, "AGENT.md")); os.IsNotExist(err) {
		// Also check _builtin fallback
		builtinDir := filepath.Join(s.deps.AgentsDir, "_builtin", name)
		if _, err := os.Stat(filepath.Join(builtinDir, "AGENT.md")); os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("agent %q not found", name))
			return false
		}
	}
	return true
}

// materializeIfBuiltin checks if the agent exists only as a builtin (no user
// override) and materializes it to the user dir so writes can proceed. Returns
// true if the caller should continue, false if an error was already written.
func (s *Server) materializeIfBuiltin(w http.ResponseWriter, name string) bool {
	userDir := filepath.Join(s.deps.AgentsDir, name)
	if _, err := os.Stat(filepath.Join(userDir, "AGENT.md")); err != nil {
		if agents.IsBuiltinAgent(name) {
			if err := agents.MaterializeBuiltin(s.deps.AgentsDir, name); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to materialize builtin: %s", err))
				return false
			}
		}
	}
	return true
}

func readAgentConfigDisplayName(agentsDir, name string) string {
	data, err := os.ReadFile(filepath.Join(agentsDir, name, "config.yaml"))
	if err != nil {
		return ""
	}
	var cfg agents.AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.DisplayName
}

func clearAgentConfigPreservingDisplayName(agentsDir, name string) error {
	displayName := readAgentConfigDisplayName(agentsDir, name)
	if displayName == "" {
		path := filepath.Join(agentsDir, name, "config.yaml")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return agents.WriteAgentConfig(agentsDir, name, &agents.AgentConfigAPI{DisplayName: displayName})
}

func (s *Server) handlePutAgentConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	var cfg agents.AgentConfigAPI
	if !decodeBody(w, r, &cfg) {
		return
	}
	if cfg.Tools != nil {
		if err := agents.ValidateToolsFilter(cfg.Tools); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := agents.ValidateAgentModelConfig(cfg.Agent); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Materialize builtin AFTER validation passes — avoids orphaned override dirs on bad input.
	if !s.materializeIfBuiltin(w, name) {
		return
	}
	// display_name is identity-adjacent metadata owned by the top-level
	// agent create/rename contract. Config replacement must not clear it or
	// accept a nested bypass value.
	cfg.DisplayName = readAgentConfigDisplayName(s.deps.AgentsDir, name)
	if err := agents.WriteAgentConfig(s.deps.AgentsDir, name, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteAgentConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	if !s.materializeIfBuiltin(w, name) {
		return
	}
	if err := clearAgentConfigPreservingDisplayName(s.deps.AgentsDir, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// alwaysAllowToolRequest is the JSON body for add/remove always-allow endpoints.
type alwaysAllowToolRequest struct {
	Tool string `json:"tool"`
}

// handleAddAgentAlwaysAllow appends a tool to permissions.always_allow_tools
// for an agent. Tools in agent.DisallowsAutoApproval return 400 — the list
// is currently empty as of 2026-05-18 (publish_to_web / generate_image /
// edit_image used to be on it and were moved off), so no production tool
// is refused today, but the gate stays in place for future use.
func (s *Server) handleAddAgentAlwaysAllow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	var req alwaysAllowToolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if !s.materializeIfBuiltin(w, name) {
		return
	}
	if err := agents.AppendAlwaysAllowTool(s.deps.AgentsDir, name, req.Tool); err != nil {
		if errors.Is(err, agents.ErrToolNotPersistable) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("POST", "/agents/"+name+"/permissions/always-allow", "added "+req.Tool)
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

// handleRemoveAgentAlwaysAllow removes a tool from
// permissions.always_allow_tools. No-op (200) if the tool isn't present.
func (s *Server) handleRemoveAgentAlwaysAllow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, name) {
		return
	}
	var req alwaysAllowToolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if err := agents.RemoveAlwaysAllowTool(s.deps.AgentsDir, name, req.Tool); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("DELETE", "/agents/"+name+"/permissions/always-allow", "removed "+req.Tool)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handleAddGlobalAlwaysAllow appends a tool to the GLOBAL
// permissions.always_allow_tools list in ~/.shannon/config.yaml. Applies to
// every agent (including the default agent). High-risk tools and high-risk
// bash command prefixes are still blocked at runtime regardless. Use the
// per-agent endpoint (/agents/{name}/permissions/always-allow) when trust
// should be limited to a specific agent.
func (s *Server) handleAddGlobalAlwaysAllow(w http.ResponseWriter, r *http.Request) {
	var req alwaysAllowToolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if agent.DisallowsAutoApproval(req.Tool) {
		writeError(w, http.StatusBadRequest,
			"tool requires fresh approval each call and cannot be persisted as always-allow")
		return
	}
	if err := config.AppendGlobalAlwaysAllowTool(s.deps.ShannonDir, req.Tool); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Mirror into the in-memory config so subsequent requests in this
	// daemon session see the new entry without a full reload.
	s.deps.WriteLock()
	perms := &s.deps.Config.Permissions
	found := false
	for _, t := range perms.AlwaysAllowTools {
		if t == req.Tool {
			found = true
			break
		}
	}
	if !found {
		perms.AlwaysAllowTools = append(perms.AlwaysAllowTools, req.Tool)
	}
	s.deps.WriteUnlock()
	s.auditHTTPOp("POST", "/permissions/always-allow", "added "+req.Tool)
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

// handleRemoveGlobalAlwaysAllow removes a tool from the global
// permissions.always_allow_tools list. No-op (200) if absent.
func (s *Server) handleRemoveGlobalAlwaysAllow(w http.ResponseWriter, r *http.Request) {
	var req alwaysAllowToolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if err := config.RemoveGlobalAlwaysAllowTool(s.deps.ShannonDir, req.Tool); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Mirror removal into the in-memory config.
	s.deps.WriteLock()
	perms := &s.deps.Config.Permissions
	filtered := perms.AlwaysAllowTools[:0]
	for _, t := range perms.AlwaysAllowTools {
		if t != req.Tool {
			filtered = append(filtered, t)
		}
	}
	perms.AlwaysAllowTools = filtered
	s.deps.WriteUnlock()
	s.auditHTTPOp("DELETE", "/permissions/always-allow", "removed "+req.Tool)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handlePutCommand(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("name")
	cmdName := r.PathValue("cmd")
	if err := agents.ValidateAgentName(agentName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, agentName) {
		return
	}
	if err := agents.ValidateCommandName(cmdName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	// Materialize builtin AFTER validation passes — avoids orphaned override dirs on bad input.
	if !s.materializeIfBuiltin(w, agentName) {
		return
	}
	if err := agents.WriteAgentCommand(s.deps.AgentsDir, agentName, cmdName, body.Content); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteCommand(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("name")
	cmdName := r.PathValue("cmd")
	if err := agents.ValidateAgentName(agentName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, agentName) {
		return
	}
	if !s.materializeIfBuiltin(w, agentName) {
		return
	}
	if err := agents.ValidateCommandName(cmdName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := agents.DeleteAgentCommand(s.deps.AgentsDir, agentName, cmdName); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePutSkill(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("name")
	skillName := r.PathValue("skill")
	if err := agents.ValidateAgentName(agentName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, agentName) {
		return
	}
	if err := skills.ValidateSkillName(skillName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if ok, provided := decodeOptionalBody(w, r, &body); !ok {
		return
	} else if provided && body.Name != "" && body.Name != skillName {
		writeError(w, http.StatusBadRequest, "skill name in body must match URL")
		return
	}
	if err := s.validateInstalledSkills([]string{skillName}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Materialize builtin AFTER validation passes — avoids orphaned override dirs on bad input.
	if !s.materializeIfBuiltin(w, agentName) {
		return
	}
	if err := agents.AttachSkill(s.deps.AgentsDir, agentName, skillName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := agents.DeleteAgentSkill(s.deps.AgentsDir, agentName, skillName); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("name")
	skillName := r.PathValue("skill")
	if err := agents.ValidateAgentName(agentName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.agentExists(w, agentName) {
		return
	}
	if !s.materializeIfBuiltin(w, agentName) {
		return
	}
	if err := skills.ValidateSkillName(skillName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := agents.DetachSkill(s.deps.AgentsDir, agentName, skillName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := agents.DeleteAgentSkill(s.deps.AgentsDir, agentName, skillName); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Marketplace handlers ---

func (s *Server) handleMarketplaceList(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	idx, err := s.marketplace.Load(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("marketplace unavailable: %v", err))
		return
	}

	q := r.URL.Query()
	page := parseIntParam(q.Get("page"), 1)
	size := parseIntParam(q.Get("size"), 20)
	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "downloads"
	}
	search := q.Get("q")

	entries, total := skills.FilterSortPaginate(idx.Skills, search, sortKey, page, size)

	// Mark `installed` flag for entries already on disk.
	installed := installedSkillSet(s.deps.ShannonDir)
	type listItem struct {
		skills.MarketplaceEntry
		Installed bool `json:"installed"`
	}
	items := make([]listItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, listItem{MarketplaceEntry: e, Installed: installed[e.Slug]})
	}

	if s.marketplace.IsStale() {
		w.Header().Set("X-Cache-Stale", "true")
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":  total,
		"page":   page,
		"size":   size,
		"skills": items,
	})
}

func (s *Server) handleSkillUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	used, err := agents.AgentsAttachingSkill(s.deps.AgentsDir, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read attached skills: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"skill":  name,
		"agents": used,
	})
}

func (s *Server) handleMarketplaceInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	slug := r.PathValue("slug")
	endpoint := "/skills/marketplace/install/" + slug
	if err := skills.ValidateSkillName(slug); err != nil {
		s.auditHTTPOpError("POST", endpoint, "invalid slug", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	idx, err := s.marketplace.Load(r.Context())
	if err != nil {
		s.auditHTTPOpError("POST", endpoint, "marketplace unavailable", err)
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("marketplace unavailable: %v", err))
		return
	}
	var entry *skills.MarketplaceEntry
	for i := range idx.Skills {
		if idx.Skills[i].Slug == slug {
			entry = &idx.Skills[i]
			break
		}
	}
	if entry == nil {
		s.auditHTTPOpError("POST", endpoint, "not in marketplace index", nil)
		writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found in marketplace", slug))
		return
	}

	err = skills.InstallFromMarketplace(r.Context(), s.deps.ShannonDir, *entry, s.slugLocks)
	switch {
	case err == nil:
		// Audit success here, before the metadata-load early returns below,
		// so the log entry survives even if LoadSkills can't find the
		// freshly-installed skill (fallback writeJSON path still runs).
		s.auditHTTPOp("POST", endpoint, "installed skill: "+entry.Slug)
		// Load the freshly installed skill so the response body reflects
		// on-disk truth (frontmatter name, description, source) rather
		// than synthesized data from the registry. Mirrors the pattern
		// used by handleInstallSkill for Anthropic-repo installs.
		sources, _ := s.skillSources()
		list, _ := skills.LoadSkills(sources...)
		for _, skill := range list {
			if skill.Slug == entry.Slug {
				writeJSON(w, http.StatusCreated, skill.ToMeta())
				return
			}
		}
		// Fallback: install succeeded but the skill did not show up in
		// LoadSkills. This shouldn't happen because InstallFromMarketplace
		// guarantees a valid SKILL.md on success, but we return a stable
		// 201 with minimal info rather than misleading the client. Slug
		// is the primary identifier clients use for subsequent CRUD, so
		// populate it explicitly instead of leaving it empty.
		fallbackName := entry.Name
		if fallbackName == "" {
			fallbackName = entry.Slug
		}
		writeJSON(w, http.StatusCreated, skills.SkillMeta{
			Name:        fallbackName,
			Slug:        entry.Slug,
			Description: entry.Description,
			Source:      "global",
		})
	case errors.Is(err, skills.ErrMaliciousSkill):
		s.auditHTTPOpError("POST", endpoint, "malicious skill", err)
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, skills.ErrSkillAlreadyInstalled):
		s.auditHTTPOpError("POST", endpoint, "already installed", err)
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, skills.ErrInvalidSkillPayload):
		s.auditHTTPOpError("POST", endpoint, "invalid payload", err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, skills.ErrMarketplaceUpstreamFailure):
		s.auditHTTPOpError("POST", endpoint, "upstream failure", err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("install failed: %v", err))
	default:
		// Local disk/staging failures → 500, per spec error matrix.
		s.auditHTTPOpError("POST", endpoint, "install failed", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("install failed: %v", err))
	}
}

// uploadSkillMaxBodyBytes caps the entire multipart request body. The actual
// 50 MB ZIP limit lives in skills.maxZipCompressedBytes; the extra ~2 MB here
// is headroom for multipart boundaries / form-field overhead so a 50 MB ZIP
// isn't rejected at the HTTP layer before reaching skills.InstallFromZipData
// (which performs the authoritative size check and returns ErrZipTooLarge).
const uploadSkillMaxBodyBytes int64 = 52 * 1024 * 1024

// uploadSkillInMemoryBytes is the multipart in-memory threshold; anything over
// this spills the form to a tempfile via mime/multipart. Note this only bounds
// multipart parser scratch — InstallFromZipData reads the file part fully into
// memory (io.ReadAll under the 50 MB zip cap) for extraction, so each in-flight
// upload of a different slug still costs roughly the compressed payload size
// in RAM. Concurrent uploads of the same slug are serialized by s.slugLocks.
const uploadSkillInMemoryBytes int64 = 1 << 20 // 1 MB

func (s *Server) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadSkillMaxBodyBytes)
	if err := r.ParseMultipartForm(uploadSkillInMemoryBytes); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			s.auditHTTPOpError("POST", "/skills/upload", "request body too large", err)
			writeError(w, http.StatusRequestEntityTooLarge, "zip too large (maximum 50 MB)")
			return
		}
		s.auditHTTPOpError("POST", "/skills/upload", "invalid multipart form", err)
		writeError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.auditHTTPOpError("POST", "/skills/upload", "missing 'file' field in multipart form", err)
		writeError(w, http.StatusBadRequest, "missing 'file' field in multipart form")
		return
	}
	defer file.Close()

	force := r.URL.Query().Get("force") == "true"
	skill, err := skills.InstallFromZipData(s.deps.ShannonDir, file, force, s.slugLocks)

	var conflict *skills.SkillConflictError
	switch {
	case err == nil:
		s.auditHTTPOp("POST", "/skills/upload", "installed skill via zip: "+skill.Slug)
		writeJSON(w, http.StatusCreated, skill.ToMeta())
	case errors.As(err, &conflict):
		s.auditHTTPOpError("POST", "/skills/upload", "conflict: "+conflict.ExistingName, err)
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":                "skill_already_exists",
			"existing_name":        conflict.ExistingName,
			"existing_description": conflict.ExistingDescription,
			"existing_prompt":      conflict.ExistingPrompt,
			"new_description":      conflict.NewDescription,
			"new_prompt":           conflict.NewPrompt,
		})
	case errors.Is(err, skills.ErrSkillIsBuiltin):
		s.auditHTTPOpError("POST", "/skills/upload", "builtin skill rejected", err)
		writeError(w, http.StatusForbidden, "skill_is_builtin")
	case errors.Is(err, skills.ErrZipTooLarge):
		s.auditHTTPOpError("POST", "/skills/upload", "zip too large", err)
		writeError(w, http.StatusRequestEntityTooLarge, "zip too large (maximum 50 MB)")
	case errors.Is(err, skills.ErrInvalidSkillPayload):
		s.auditHTTPOpError("POST", "/skills/upload", "invalid payload", err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.auditHTTPOpError("POST", "/skills/upload", "upload failed", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("upload failed: %v", err))
	}
}

func (s *Server) handleMarketplaceDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	slug := r.PathValue("slug")
	if err := skills.ValidateSkillName(slug); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	idx, err := s.marketplace.Load(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("marketplace unavailable: %v", err))
		return
	}
	var entry *skills.MarketplaceEntry
	for i := range idx.Skills {
		if idx.Skills[i].Slug == slug {
			entry = &idx.Skills[i]
			break
		}
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found in marketplace", slug))
		return
	}

	// Consistent with list + install: malicious entries are hidden.
	if entry.IsMalicious() {
		writeError(w, http.StatusForbidden, "skill blocked by security scan")
		return
	}

	// Response wraps the registry entry plus live state. Preview holds the
	// installed SKILL.md body when present — empty string otherwise, so the
	// field is always part of the schema. NO omitempty so Desktop clients
	// can rely on the field's existence regardless of install state.
	type detailResponse struct {
		skills.MarketplaceEntry
		Installed bool   `json:"installed"`
		Preview   string `json:"preview"`
	}

	resp := detailResponse{MarketplaceEntry: *entry}
	skillDir := filepath.Join(s.deps.ShannonDir, "skills", slug)
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if body, err := os.ReadFile(skillFile); err == nil {
		resp.Installed = true
		resp.Preview = string(body)
	}

	writeJSON(w, http.StatusOK, resp)
}

// parseIntParam parses a positive int query parameter, falling back to def
// on empty or invalid input. Shared by marketplace handlers.
func parseIntParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// installedSkillSet returns the set of skill slugs present in
// ~/.shannon/skills/. Missing directory → empty set, no error.
func installedSkillSet(shannonDir string) map[string]bool {
	out := make(map[string]bool)
	entries, err := os.ReadDir(filepath.Join(shannonDir, "skills"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(shannonDir, "skills", e.Name(), "SKILL.md")); err == nil {
			out[e.Name()] = true
		}
	}
	return out
}

// --- Global skills handlers ---

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	sources, err := s.skillSources()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	list, err := skills.LoadSkills(sources...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// hidden: true is display-only — the skill is still loaded and invokable
	// via use_skill. Admin/management UIs can pass ?include_hidden=true.
	includeHidden := r.URL.Query().Get("include_hidden") == "true"
	metas := make([]skills.SkillMeta, 0, len(list))
	for _, skill := range list {
		if skill.Hidden && !includeHidden {
			continue
		}
		meta := skill.ToMeta()
		meta.RequiredSecrets = skill.RequiredSecrets()
		meta.ConfiguredSecrets = s.secretsStore.ConfiguredKeys(skill.Slug)
		metas = append(metas, meta)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"skills": metas})
}

func (s *Server) handleListDownloadableSkills(w http.ResponseWriter, r *http.Request) {
	globalDir := filepath.Join(s.deps.ShannonDir, "skills")
	result := make([]skills.DownloadableSkill, 0, len(skills.DownloadableSkills))
	for _, ds := range skills.DownloadableSkills {
		installed := false
		if _, err := os.Stat(filepath.Join(globalDir, ds.Name, "SKILL.md")); err == nil {
			installed = true
		}
		result = append(result, skills.DownloadableSkill{
			Name:        ds.Name,
			Description: ds.Description,
			Installed:   installed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"skills": result})
}

func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	name := r.PathValue("name")
	endpoint := "/skills/install/" + name
	if !skills.IsDownloadable(name) {
		s.auditHTTPOpError("POST", endpoint, "not in downloadable registry", nil)
		writeError(w, http.StatusBadRequest, fmt.Sprintf("skill %q is not available for download", name))
		return
	}

	if err := skills.InstallSkillFromRepo(s.deps.ShannonDir, name); err != nil {
		if strings.Contains(err.Error(), "already installed") {
			s.auditHTTPOpError("POST", endpoint, "already installed", err)
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.auditHTTPOpError("POST", endpoint, "install failed", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit success here, before the metadata-load early returns below, so the
	// log entry survives even if LoadSkills can't find the freshly-installed
	// skill (rare, but the fallback writeJSON path still runs).
	s.auditHTTPOp("POST", endpoint, "installed skill")

	// Load the installed skill to return its metadata
	sources, _ := s.skillSources()
	list, _ := skills.LoadSkills(sources...)
	for _, skill := range list {
		if skill.Slug == name {
			writeJSON(w, http.StatusCreated, skill.ToMeta())
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "installed", "name": name})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	// Intentionally does NOT filter by skill.Hidden — single-skill lookup is
	// for callers that already know the slug (admin UIs, kocoro secrets
	// management). Hidden is a browse-list display filter, not an access
	// control. Do not add a hidden check here without revisiting handleListSkills.
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	sources, err := s.skillSources()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := skills.LoadSkills(sources...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, skill := range list {
		if skill.Slug == name {
			detail := skills.SkillDetail{
				Name:               skill.Name,
				Slug:               skill.Slug,
				Description:        skill.Description,
				Prompt:             skill.Prompt,
				Source:             skill.Source,
				InstallSource:      skill.InstallSource,
				MarketplaceSlug:    skill.MarketplaceSlug,
				License:            skill.License,
				Compatibility:      skill.Compatibility,
				Metadata:           skill.Metadata,
				StickyInstructions: skill.StickyInstructions,
				Hidden:             skill.Hidden,
				StickySnippet:      skill.StickySnippetOverride,
			}
			if len(skill.AllowedTools) > 0 {
				detail.AllowedTools = skill.AllowedTools
			}
			detail.RequiredSecrets = skill.RequiredSecrets()
			detail.ConfiguredSecrets = s.secretsStore.ConfiguredKeys(skill.Slug)
			writeJSON(w, http.StatusOK, detail)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found", name))
}

func (s *Server) handlePutGlobalSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Builtin guard: refuse to write over kocoro / kocoro-generative-ui.
	// Matches /skills/upload's ErrSkillIsBuiltin behavior — EnsureBuiltinSkills
	// would wipe any override on the next daemon restart anyway, and during the
	// running session a defaced kocoro misleads the AI about the daemon's HTTP
	// surface. `force=true` does NOT override this; the guard is unconditional.
	if skills.IsBuiltinSkill(name) {
		writeError(w, http.StatusForbidden, "skill_is_builtin")
		return
	}
	var req struct {
		Description        string  `json:"description"`
		Prompt             string  `json:"prompt"`
		License            string  `json:"license"`
		StickyInstructions *bool   `json:"sticky_instructions,omitempty"`
		StickySnippet      *string `json:"sticky_snippet,omitempty"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusBadRequest, "description is required")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	// Serialize concurrent writes to the same slug. Mirrors the lock
	// InstallFromZipData takes for /skills/upload — without it, two PUTs
	// racing on the same slug can both observe "no file on disk", both
	// skip the conflict gate, and one of them clobbers AllowedTools /
	// Metadata with the empty zero-value that the PUT body doesn't carry.
	// Also gives PUT vs POST /skills/upload vs DELETE /skills/{name}
	// mutual exclusion via the same SlugLocks instance — see
	// handleDeleteGlobalSkill for the DELETE-side acquisition.
	unlock := s.slugLocks.Lock(name)
	defer unlock()
	// Load the existing skill so we can preserve fields the PUT body doesn't
	// carry (AllowedTools, Metadata, Compatibility). If the skill directory
	// already exists but load fails, refuse the write rather than silently
	// clobber those fields with zero values — kocoro's `allowed-tools:
	// http file_read` is security-critical and must not be dropped on a
	// transient FS error.
	var skillToWrite skills.Skill
	skillExistsOnDisk := false
	if s.deps != nil && s.deps.ShannonDir != "" {
		if _, statErr := os.Stat(filepath.Join(s.deps.ShannonDir, "skills", name, "SKILL.md")); statErr == nil {
			skillExistsOnDisk = true
		}
	}
	loadedExisting := false
	sources, err := s.skillSources()
	if err != nil {
		if skillExistsOnDisk {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("cannot resolve skill sources to preserve existing fields: %v", err))
			return
		}
	} else {
		list, loadErr := skills.LoadSkills(sources...)
		if loadErr != nil && skillExistsOnDisk {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("cannot load existing skill %q (refusing to clobber AllowedTools/Metadata): %v", name, loadErr))
			return
		}
		// URL param is the slug (directory identifier), not the
		// frontmatter display label. Match by Slug so a skill whose
		// Name differs from its Slug (e.g. "Docker" / "docker") is
		// found and its AllowedTools/Metadata are preserved.
		for _, existing := range list {
			if existing.Slug == name {
				skillToWrite = *existing
				loadedExisting = true
				break
			}
		}
	}
	// LoadSkills silently skips per-skill parse errors (loader.go fail-open).
	// If the directory has a SKILL.md but the loader didn't return a match,
	// the on-disk frontmatter is malformed. Refuse the write — letting force=true
	// proceed here would clobber AllowedTools/Metadata with zero values, which
	// is exactly what the comment above protects against on transient errors.
	if skillExistsOnDisk && !loadedExisting {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("existing skill %q has malformed SKILL.md frontmatter; fix or delete it before updating via PUT", name))
		return
	}
	// Conflict gate: when a skill with this slug already exists and the
	// caller did not opt into overwrite, return 409 with both sides'
	// description+prompt so the Desktop client can present a compare sheet.
	// Same JSON shape as /skills/upload's SkillConflictError, with prompts
	// run through skills.TruncatePromptPreview so the response size is
	// bounded the same way (≤ 8 KB per prompt) regardless of write path.
	force := r.URL.Query().Get("force") == "true"
	if skillExistsOnDisk && !force {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":                "skill_already_exists",
			"existing_name":        name,
			"existing_description": skillToWrite.Description,
			"existing_prompt":      skills.TruncatePromptPreview(skillToWrite.Prompt),
			"new_description":      req.Description,
			"new_prompt":           skills.TruncatePromptPreview(req.Prompt),
		})
		return
	}
	skillToWrite.Slug = name
	// Preserve the existing Name when updating; only overwrite if the
	// skill is brand-new (Name was never set). The URL slug must not
	// replace a carefully chosen display label like "Docker".
	if skillToWrite.Name == "" {
		skillToWrite.Name = name
	}
	skillToWrite.Description = req.Description
	skillToWrite.Prompt = req.Prompt
	skillToWrite.License = req.License
	if req.StickyInstructions != nil {
		skillToWrite.StickyInstructions = *req.StickyInstructions
	}
	if req.StickySnippet != nil {
		skillToWrite.StickySnippetOverride = strings.TrimSpace(*req.StickySnippet)
	}
	if err := skills.WriteGlobalSkill(s.deps.ShannonDir, &skillToWrite); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("PUT", "/skills/"+name, "wrote global skill")
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteGlobalSkill(w http.ResponseWriter, r *http.Request) {
	if requireConfirm(r.URL.Query().Get("confirm")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "confirmation_required",
			"message": "This will permanently delete the skill. Add ?confirm=true to proceed.",
		})
		return
	}
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	// Serialize against concurrent PUT /skills/{name} and POST /skills/upload
	// on the same slug. Without this lock, a PUT can read the existing skill,
	// DELETE can race in and remove the directory, then PUT proceeds and
	// recreates it with state copied from the deleted version. Same lock
	// instance as handlePutGlobalSkill / InstallFromZipData / InstallFromMarketplace.
	unlock := s.slugLocks.Lock(name)
	defer unlock()
	globalDir := filepath.Join(s.deps.ShannonDir, "skills", name)
	skillFile := filepath.Join(globalDir, "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found in global directory", name))
		return
	}
	if err := skills.DeleteGlobalSkill(s.deps.ShannonDir, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("DELETE", "/skills/"+name, "deleted skill")
	s.secretsStore.Delete(name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePutSkillSecrets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var secrets map[string]string
	if !decodeBody(w, r, &secrets) {
		return
	}
	if len(secrets) == 0 {
		writeError(w, http.StatusBadRequest, "no secrets provided")
		return
	}
	keys := make([]string, 0, len(secrets))
	for key := range secrets {
		if !skills.IsValidEnvKey(key) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid secret key %q: must match [A-Z0-9_]+", key))
			return
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := s.secretsStore.Set(name, secrets); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("PUT", "/skills/"+name+"/secrets", "set secrets for skill: "+strings.Join(keys, ","))
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteSkillSecrets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.secretsStore.Delete(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("DELETE", "/skills/"+name+"/secrets", "cleared all secrets for skill")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleDeleteSkillSecretKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	key := r.PathValue("key")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	if err := s.secretsStore.DeleteKey(name, key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("DELETE", "/skills/"+name+"/secrets/"+key, "removed secret key")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListSkillScripts(w http.ResponseWriter, r *http.Request) {
	s.handleListSkillSubresource(w, r, "scripts")
}

func (s *Server) handlePutSkillScripts(w http.ResponseWriter, r *http.Request) {
	s.handlePutSkillSubresource(w, r, "scripts")
}

func (s *Server) handleDeleteSkillScripts(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteSkillSubresource(w, r, "scripts")
}

func (s *Server) handleListSkillReferences(w http.ResponseWriter, r *http.Request) {
	s.handleListSkillSubresource(w, r, "references")
}

func (s *Server) handlePutSkillReferences(w http.ResponseWriter, r *http.Request) {
	s.handlePutSkillSubresource(w, r, "references")
}

func (s *Server) handleDeleteSkillReferences(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteSkillSubresource(w, r, "references")
}

func (s *Server) handleListSkillAssets(w http.ResponseWriter, r *http.Request) {
	s.handleListSkillSubresource(w, r, "assets")
}

func (s *Server) handlePutSkillAssets(w http.ResponseWriter, r *http.Request) {
	s.handlePutSkillSubresource(w, r, "assets")
}

func (s *Server) handleDeleteSkillAssets(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteSkillSubresource(w, r, "assets")
}

func (s *Server) handleListSkillSubresource(w http.ResponseWriter, r *http.Request, subdir string) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dir, _, _, err := s.resolveSkillDir(name)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	target := filepath.Join(dir, subdir)
	entries, err := os.ReadDir(target)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string][]string{"files": {}})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	writeJSON(w, http.StatusOK, map[string][]string{"files": files})
}

func (s *Server) handlePutSkillSubresource(w http.ResponseWriter, r *http.Request, subdir string) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dir, _, readOnly, err := s.resolveSkillDir(name)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if readOnly {
		writeError(w, http.StatusBadRequest, "bundled skill is read-only; create a global override first via PUT /skills/{name}")
		return
	}
	filename := r.PathValue("filename")
	if !isValidSkillFileName(filename) {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	targetDir := filepath.Join(dir, subdir)
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	fileMode := modeForSubresource(subdir)
	tmp, err := os.CreateTemp(targetDir, ".skill-file-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Chmod(tmpPath, fileMode); err != nil {
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dest := filepath.Join(targetDir, filename)
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteSkillSubresource(w http.ResponseWriter, r *http.Request, subdir string) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dir, _, readOnly, err := s.resolveSkillDir(name)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("skill %q not found", name))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if readOnly {
		writeError(w, http.StatusBadRequest, "bundled skill is read-only; create a global override first via PUT /skills/{name}")
		return
	}
	filename := r.PathValue("filename")
	if !isValidSkillFileName(filename) {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	if err := os.Remove(filepath.Join(dir, subdir, filename)); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Schedule handlers ---

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	list, err := s.deps.ScheduleManager.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []schedule.Schedule{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"schedules": list})
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	sched, err := s.deps.ScheduleManager.Get(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

// handleScheduleLastRun resolves a schedule's most recent run through the
// shared schedule.SummarizeLastRun resolver. Returns the same JSON shape the
// schedule_show LLM tool emits — LastRunSummary — so Desktop and any other
// HTTP client read the same wire format. Optional ?max_turns=N tunes the
// window (the resolver clamps).
func (s *Server) handleScheduleLastRun(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	sched, err := s.deps.ScheduleManager.Get(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	maxTurns := 5
	if v := r.URL.Query().Get("max_turns"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			maxTurns = n
		}
	}
	summary, err := schedule.SummarizeLastRun(*sched, s.deps.ShannonDir, maxTurns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	var req struct {
		Agent             string  `json:"agent"`
		Cron              string  `json:"cron"`
		Prompt            string  `json:"prompt"`
		Stateful          *bool   `json:"stateful"` // nil → default (false / stateless + fresh); true → accumulate
		Broadcast         *string `json:"broadcast,omitempty"`
		CreatedFromSource string  `json:"created_from_source,omitempty"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	stateful := false
	if req.Stateful != nil {
		stateful = *req.Stateful
	}
	if !isValidScheduleSource(req.CreatedFromSource) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("created_from_source %q is not a recognized origin", req.CreatedFromSource))
		return
	}
	opts := schedule.CreateOpts{CreatedFromSource: req.CreatedFromSource}
	if req.Broadcast != nil {
		bPtr, ok := schedule.ParseBroadcastEnum(*req.Broadcast)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("broadcast must be one of \"auto\", \"on\", \"off\"; got %q", *req.Broadcast))
			return
		}
		opts.Broadcast = bPtr
	}
	id, err := s.deps.ScheduleManager.CreateWithOpts(req.Agent, req.Cron, req.Prompt, stateful, opts)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
		} else if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "prompt cannot be empty") {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	sched, err := s.deps.ScheduleManager.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sched)
}

func (s *Server) handlePatchSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	var patch struct {
		Cron      *string `json:"cron"`
		Prompt    *string `json:"prompt"`
		Enabled   *bool   `json:"enabled"`
		Stateful  *bool   `json:"stateful"`
		Broadcast *string `json:"broadcast,omitempty"` // "auto"|"on"|"off"; absent leaves field unchanged
	}
	if !decodeBody(w, r, &patch) {
		return
	}
	if patch.Cron == nil && patch.Prompt == nil && patch.Enabled == nil && patch.Stateful == nil && patch.Broadcast == nil {
		writeError(w, http.StatusBadRequest, "no fields to update: provide at least one of cron, prompt, enabled, stateful, or broadcast")
		return
	}
	update := &schedule.UpdateOpts{
		Cron:     patch.Cron,
		Prompt:   patch.Prompt,
		Enabled:  patch.Enabled,
		Stateful: patch.Stateful,
	}
	// Parse the optional broadcast enum. Absent → leave Schedule.Broadcast
	// alone (UpdateOpts.Broadcast == nil). Present → ParseBroadcastEnum maps
	// "auto"/"on"/"off" to *bool; the BroadcastOpt wrapper distinguishes
	// "leave alone" (UpdateOpts.Broadcast == nil) from "rewrite to nil/true/false"
	// (UpdateOpts.Broadcast != nil with the *bool inside).
	if patch.Broadcast != nil {
		b, ok := schedule.ParseBroadcastEnum(*patch.Broadcast)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("broadcast must be one of \"auto\", \"on\", \"off\"; got %q", *patch.Broadcast))
			return
		}
		update.Broadcast = &schedule.BroadcastOpt{Value: b}
	}
	if err := s.deps.ScheduleManager.Update(id, update); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if strings.Contains(err.Error(), "no fields to update") ||
			strings.Contains(err.Error(), "invalid") ||
			strings.Contains(err.Error(), "prompt cannot be empty") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sched, err := s.deps.ScheduleManager.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if requireConfirm(r.URL.Query().Get("confirm")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "confirmation_required",
			"message": "This will permanently delete the schedule. Add ?confirm=true to proceed.",
		})
		return
	}
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.deps.ScheduleManager.Remove(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("DELETE", "/schedules/"+id, "deleted schedule")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Global config + instructions handlers ---

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	globalPath := filepath.Join(s.deps.ShannonDir, "config.yaml")
	globalData, err := os.ReadFile(globalPath)
	var globalMap map[string]interface{}
	if err == nil {
		if yamlErr := yaml.Unmarshal(globalData, &globalMap); yamlErr != nil {
			log.Printf("daemon: GET /config: global config parse error: %v", yamlErr)
		}
	}

	cfg, _, _ := s.deps.Snapshot()
	effectiveJSON, _ := json.Marshal(cfg)
	var effectiveMap map[string]interface{}
	json.Unmarshal(effectiveJSON, &effectiveMap)

	// Redact secrets from both maps before responding
	redactConfigSecrets(globalMap)
	redactConfigSecrets(effectiveMap)

	// Collect unique source files from config merge
	var sources []string
	if cfg != nil && cfg.Sources != nil {
		seen := make(map[string]bool)
		for _, src := range cfg.Sources {
			if src.File != "" && !seen[src.File] {
				seen[src.File] = true
				sources = append(sources, src.File)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"global":    globalMap,
		"effective": effectiveMap,
		"sources":   sources,
	})
}

func (s *Server) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	var patch map[string]interface{}
	if !decodeBody(w, r, &patch) {
		return
	}

	// Normalize aliases FIRST so security checks see canonical keys.
	// e.g. "apiKey" → "api_key", "mcpServers" → "mcp_servers".
	normalizePatchKeys(patch)

	// Check protected fields — hard block, no bypass header.
	// These fields must be edited directly in ~/.shannon/config.yaml.
	if reason, isProtected := checkProtectedFields(patch); isProtected {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "protected_field",
			"message": reason + " — edit ~/.shannon/config.yaml directly",
		})
		return
	}

	// Validate MCP server commands
	if servers, ok := patch["mcp_servers"].(map[string]interface{}); ok {
		confirmed := r.Header.Get("X-Confirm") != ""
		if err := validateMCPCommands(servers, confirmed); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":   "mcp_command_validation",
				"message": err.Error(),
			})
			return
		}
		// Built-ins own command/args/type/url/context — refuse mutations to
		// those keys. disabled / env / keep_alive remain writable.
		if _, msg, blocked := validateBuiltinMCPPatch(servers); blocked {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":   "builtin_mcp_immutable",
				"message": msg,
			})
			return
		}
	}

	// agent.model is a specific model id (→ Gateway specific_model); a tier word
	// here is sent verbatim and fails every run with "model_id_unknown". Reject
	// at the boundary so the bad value never reaches config.yaml (where it would
	// otherwise fail the whole config load at next reload). model_tier is the
	// knob for small/medium/large.
	if agentPatch, ok := patch["agent"].(map[string]interface{}); ok {
		if model, ok := agentPatch["model"].(string); ok && agents.IsModelTierKeyword(model) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("agent.model expects a specific model id (e.g. \"claude-opus-4-8\"), not the tier %q; use model_tier for tiers", model))
			return
		}
	}

	if err := s.patchGlobalConfig(patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("PATCH", "/config", "updated config")
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleConfigStatus returns current MCP server status without restarting processes.
func (s *Server) handleConfigStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _, _ := s.deps.Snapshot()
	resp := make(map[string]interface{})

	if cfg != nil && len(cfg.MCPServers) > 0 {
		// Build set of live-connected server names from MCPManager.
		s.deps.mu.RLock()
		mgr := s.deps.MCPManager
		s.deps.mu.RUnlock()
		connected := make(map[string]bool)
		if mgr != nil {
			for _, name := range mgr.ConnectedServers() {
				connected[name] = true
			}
		}

		mcpStatus := make(map[string]string, len(cfg.MCPServers))
		mcpServerInfo := make(map[string]map[string]interface{})
		for name, srv := range cfg.MCPServers {
			if srv.Disabled {
				mcpStatus[name] = "disabled"
			} else if connected[name] {
				mcpStatus[name] = "connected"
			} else {
				mcpStatus[name] = "enabled"
			}
			if srv.Builtin {
				info := map[string]interface{}{"builtin": true}
				if entry, ok := mcp.BuiltinMCPServers[name]; ok {
					if entry.DisplayName != "" {
						info["display_name"] = entry.DisplayName
					}
					if entry.Description != "" {
						info["description"] = entry.Description
					}
					if entry.AuthHint != "" {
						info["auth_hint"] = entry.AuthHint
					}
					if entry.RequiresAuth {
						info["requires_auth"] = true
						// `authorized` is the dynamic counterpart to
						// `requires_auth`: requires_auth says "this
						// server class needs OAuth"; authorized says
						// "the current user already has a usable token".
						// Desktop skips the confirm modal on enable
						// when authorized=true (mcp-remote will reuse
						// the cached token, no browser pop happens).
						// Field is always set (true/false) for clarity;
						// older clients ignore the key.
						authorized := false
						if entry.IsAuthorized != nil {
							authorized = entry.IsAuthorized()
						}
						info["authorized"] = authorized
					}
				}
				mcpServerInfo[name] = info
			}
		}
		resp["mcp_servers"] = mcpStatus
		if len(mcpServerInfo) > 0 {
			resp["mcp_server_info"] = mcpServerInfo
		}
	}

	_, _, sup := s.deps.Snapshot()
	if sup != nil {
		healthData := make(map[string]interface{})
		for name, h := range sup.HealthStates() {
			entry := map[string]interface{}{
				"state":                h.State.String(),
				"since":                h.Since.Format(time.RFC3339),
				"consecutive_failures": h.ConsecutiveFailures,
			}
			if !h.LastTransportOK.IsZero() {
				entry["last_transport_ok"] = h.LastTransportOK.Format(time.RFC3339)
			}
			if !h.LastCapabilityOK.IsZero() {
				entry["last_capability_ok"] = h.LastCapabilityOK.Format(time.RFC3339)
			}
			if h.LastTransportError != "" {
				entry["last_transport_error"] = h.LastTransportError
			}
			if h.LastCapabilityError != "" {
				entry["last_capability_error"] = h.LastCapabilityError
			}
			healthData[name] = entry
		}
		if len(healthData) > 0 {
			resp["mcp_health"] = healthData
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// retryDisconnectedEnabledMCPServers fires a fresh async connect attempt
// for every server that is enabled in cfg but not currently in the mgr's
// connected set. Used by /config/reload as the user's explicit retry
// signal — without it, the supervisor's no-auto-reconnect policy leaves a
// server stuck in "enabled" status forever after a single connect failure
// (the Bug 2 from the 2026-05-25 Intercom OAuth report).
//
// Safe to call when no MCP config change happened — if every enabled
// server is already connected, this is a no-op.
func (s *Server) retryDisconnectedEnabledMCPServers(cfg *config.Config) {
	if cfg == nil || len(cfg.MCPServers) == 0 {
		return
	}
	s.deps.mu.RLock()
	mgr := s.deps.MCPManager
	sup := s.deps.Supervisor
	s.deps.mu.RUnlock()
	if mgr == nil || sup == nil {
		return
	}
	connected := make(map[string]bool)
	for _, name := range mgr.ConnectedServers() {
		connected[name] = true
	}
	var retry map[string]mcp.MCPServerConfig
	for name, srv := range cfg.MCPServers {
		if srv.Disabled || connected[name] {
			continue
		}
		// Don't retry servers that are disconnected by design (the
		// discover-then-disconnect optimization, e.g. playwright with
		// KeepAlive=false). Retrying them would relaunch Chrome on every
		// reload and defeat the optimization. mgr.CachedTools()
		// non-empty is the signal that the previous disconnect was
		// intentional, not a failed connect.
		if tools.ShouldSkipReloadRetry(mgr, name, srv) {
			continue
		}
		if retry == nil {
			retry = make(map[string]mcp.MCPServerConfig)
		}
		retry[name] = srv
	}
	if len(retry) == 0 {
		return
	}
	defaultTimeout := time.Duration(cfg.MCP.DefaultConnectTimeoutSecs) * time.Second
	if defaultTimeout <= 0 {
		defaultTimeout = 60 * time.Second
	}
	capturedSup := sup
	capturedMgr := mgr
	go func() {
		// Final stale-check before spawning subprocesses: a fast follow-up
		// reload may have swapped deps between scheduling this goroutine
		// and the runtime picking it up. If so, abort here so we don't
		// strand subprocesses inside an mgr that has no reachable owner.
		_, _, depsSup := s.deps.Snapshot()
		if depsSup != capturedSup {
			return
		}
		capturedMgr.StartConnectAll(s.ctx, retry, defaultTimeout, func(name string, connErr error) {
			_, _, depsSup := s.deps.Snapshot()
			if depsSup != capturedSup {
				return // deps swapped by a newer reload — drop stale result.
			}
			if connErr != nil {
				log.Printf("[mcp] %s: reload retry failed: %v", name, connErr)
				s.auditMCPConnectFailure(name, connErr)
				return
			}
			log.Printf("[mcp] %s: reload retry succeeded; probing supervisor", name)
			capturedSup.ProbeNow(name)
			tools.PostConnectDisconnectIfDiscoveryOnly(capturedMgr, name)
		})
	}()
}

// mcpConfigChanged returns true if MCP server configuration differs between old and new config.
func mcpConfigChanged(oldCfg, newCfg *config.Config) bool {
	if oldCfg == nil {
		return len(newCfg.MCPServers) > 0
	}
	if len(oldCfg.MCPServers) != len(newCfg.MCPServers) {
		return true
	}
	for name, oldSrv := range oldCfg.MCPServers {
		newSrv, ok := newCfg.MCPServers[name]
		if !ok {
			return true
		}
		if oldSrv.Command != newSrv.Command || oldSrv.Type != newSrv.Type ||
			oldSrv.URL != newSrv.URL || oldSrv.Disabled != newSrv.Disabled ||
			oldSrv.Context != newSrv.Context || oldSrv.KeepAlive != newSrv.KeepAlive ||
			!slices.Equal(oldSrv.Args, newSrv.Args) ||
			!maps.Equal(oldSrv.Env, newSrv.Env) {
			return true
		}
	}
	return false
}

func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	oldCfg, _, _ := s.deps.Snapshot()

	newCfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("config load failed: %v", err))
		return
	}

	mcpChanged := mcpConfigChanged(oldCfg, newCfg)
	mcp.SetCDPChromeProfile(newCfg.Daemon.ChromeProfile)

	var regErr error
	if mcpChanged {
		var newReg *agent.ToolRegistry
		var newMCPMgr *mcp.ClientManager
		var newCleanup func()
		var newBaseline *agent.ToolRegistry
		var startMCP tools.StartMCPFunc
		newBaseline, newReg, _, newMCPMgr, newCleanup, startMCP, regErr = tools.RegisterAllWithBaselineAsync(s.deps.GW, newCfg)
		if regErr != nil {
			log.Printf("daemon: reload warning: %v", regErr)
		}
		toolCfg := s.configWithLiveAPIKey(newCfg)
		tools.RegisterCloudDelegate(newReg, s.deps.GW, toolCfg, nil, "", "")
		tools.RegisterPublishTool(newReg, s.deps.GW, toolCfg)
		tools.RegisterListPublishedFilesTool(newReg, s.deps.GW, toolCfg)
		tools.RegisterRetractPublishedFileTool(newReg, s.deps.GW, toolCfg)
		tools.RegisterGenerateImageTool(newReg, s.deps.GW, toolCfg)
		tools.RegisterEditImageTool(newReg, s.deps.GW, toolCfg)

		newGatewayOverlay := tools.ExtractGatewayTools(newReg)
		newPostOverlays := tools.ExtractPostOverlays(newReg, newBaseline)

		newSupervisor := mcp.NewSupervisor(newMCPMgr)
		newSupervisor.RegisterCapabilityProbe("playwright", &mcp.PlaywrightProbe{})
		newSupervisor.SetOnReconnect(func(ctx context.Context, serverName string) {
			if serverName == "playwright" {
				tools.CleanupPlaywrightReconnect(ctx, newMCPMgr)
			}
		})
		newSupervisor.SetOnChange(func(server string, oldState, newState mcp.HealthState) {
			_, _, depsSup := s.deps.Snapshot()
			if depsSup != newSupervisor {
				return
			}
			bl, gwOv, po, mgr := s.deps.RebuildLayers()
			rebuilt := tools.RebuildRegistryForHealth(bl, gwOv, po, newSupervisor.HealthStates(), mgr, newSupervisor)
			s.deps.WriteLock()
			s.deps.Registry = rebuilt
			s.deps.WriteUnlock()
			log.Printf("MCP registry rebuilt (reload): %d tools", len(rebuilt.All()))
		})

		var oldBrowser *tools.BrowserTool
		s.deps.mu.Lock()
		// Snapshot OLD browser under the deps lock so it is consistent with the
		// oldCleanup / oldSupervisor values captured below.
		if s.deps.BaselineReg != nil {
			if bt, ok := s.deps.BaselineReg.Get("browser"); ok {
				oldBrowser, _ = bt.(*tools.BrowserTool)
			}
		}
		oldCleanup := s.deps.Cleanup
		oldSupervisor := s.deps.Supervisor
		s.deps.Config = newCfg
		s.deps.Registry = newReg
		s.deps.MCPManager = newMCPMgr
		s.deps.Supervisor = newSupervisor
		s.deps.Cleanup = newCleanup
		s.deps.BaselineReg = newBaseline
		s.deps.GatewayOverlay = newGatewayOverlay
		s.deps.PostOverlays = newPostOverlays
		s.deps.mu.Unlock()

		if oldSupervisor != nil {
			oldSupervisor.Stop()
		}

		// Mark OLD browser deprecated BEFORE oldCleanup() runs so the cleanup
		// closure (register.go) skips browser.Cleanup() and lease teardown
		// handles it instead. HandBrowserOff also handles the fast-path /
		// watchdog branches.
		if oldBrowser != nil {
			backstop := time.Duration(newCfg.Daemon.BrowserReloadBackstopSecs) * time.Second
			if backstop <= 0 {
				backstop = 120 * time.Second // defensive: zero config or explicit 0
			}
			tools.HandBrowserOff(oldBrowser, backstop)
		}
		if oldCleanup != nil {
			oldCleanup()
		}

		newSupervisor.Start(s.ctx)

		// Force registry rebuild to attach supervisor to MCPTools (same
		// reason as initial startup — CompleteRegistration creates tools
		// before the supervisor exists).
		{
			bl, gwOv, po, mgr := s.deps.RebuildLayers()
			initReg := tools.RebuildRegistryForHealth(bl, gwOv, po, newSupervisor.HealthStates(), mgr, newSupervisor)
			s.deps.WriteLock()
			s.deps.Registry = initReg
			s.deps.WriteUnlock()
			log.Printf("MCP registry initialized with supervisor (reload): %d tools", len(initReg.All()))
		}

		// Kick off per-server connect goroutines in the background. HTTP
		// returns immediately after this function exits; each connect
		// respects its own ConnectTimeoutSeconds (Intercom: 300s for OAuth)
		// so a stalled OAuth flow can't pin the daemon. On success we
		// trigger a supervisor probe which transitions state to Healthy and
		// fires OnChange → RebuildRegistryForHealth → MCP tools appear in
		// the live registry.
		if startMCP != nil {
			capturedSupervisor := newSupervisor
			capturedMgr := newMCPMgr
			go startMCP(s.ctx, func(name string, connErr error) {
				_, _, depsSup := s.deps.Snapshot()
				if depsSup != capturedSupervisor {
					return // deps swapped by a newer reload — drop result.
				}
				if connErr != nil {
					log.Printf("[mcp] %s: async connect failed: %v", name, connErr)
					s.auditMCPConnectFailure(name, connErr)
					return
				}
				log.Printf("[mcp] %s: async connect succeeded; probing supervisor", name)
				capturedSupervisor.ProbeNow(name)
				tools.PostConnectDisconnectIfDiscoveryOnly(capturedMgr, name)
			})
		}
	} else {
		// Config changed but MCP servers didn't — update config and refresh
		// cached rebuild layers so health-driven rebuilds use current settings.
		newBaseline, _, newBaseCleanup := tools.RegisterLocalTools(newCfg, s.secretsStore)
		// Re-register gateway tools on top of fresh baseline clone.
		// Use a short timeout — if the gateway is unavailable, keep existing overlay.
		freshReg := newBaseline.Clone()
		gwCtx, gwCancel := context.WithTimeout(r.Context(), 5*time.Second)
		gwErr := tools.RegisterServerTools(gwCtx, s.deps.GW, freshReg)
		gwCancel()
		toolCfg := s.configWithLiveAPIKey(newCfg)
		tools.RegisterCloudDelegate(freshReg, s.deps.GW, toolCfg, nil, "", "")
		tools.RegisterPublishTool(freshReg, s.deps.GW, toolCfg)
		tools.RegisterListPublishedFilesTool(freshReg, s.deps.GW, toolCfg)
		tools.RegisterRetractPublishedFileTool(freshReg, s.deps.GW, toolCfg)
		tools.RegisterGenerateImageTool(freshReg, s.deps.GW, toolCfg)
		tools.RegisterEditImageTool(freshReg, s.deps.GW, toolCfg)
		var newGatewayOverlay []agent.Tool
		if gwErr != nil {
			log.Printf("daemon: reload: gateway refresh failed, keeping existing overlay: %v", gwErr)
			s.deps.mu.RLock()
			newGatewayOverlay = s.deps.GatewayOverlay
			s.deps.mu.RUnlock()
		} else {
			newGatewayOverlay = tools.ExtractGatewayTools(freshReg)
		}
		newPostOverlays := tools.ExtractPostOverlays(freshReg, newBaseline)

		var oldBrowser *tools.BrowserTool
		s.deps.mu.Lock()
		// Snapshot OLD browser under the deps lock so it is consistent with the
		// oldCleanup value captured below.
		if s.deps.BaselineReg != nil {
			if bt, ok := s.deps.BaselineReg.Get("browser"); ok {
				oldBrowser, _ = bt.(*tools.BrowserTool)
			}
		}
		oldCleanup := s.deps.Cleanup
		s.deps.Config = newCfg
		s.deps.BaselineReg = newBaseline
		s.deps.GatewayOverlay = newGatewayOverlay
		s.deps.PostOverlays = newPostOverlays
		s.deps.Cleanup = func() { newBaseCleanup(); oldCleanup() }
		s.deps.mu.Unlock()

		if oldBrowser != nil {
			backstop := time.Duration(newCfg.Daemon.BrowserReloadBackstopSecs) * time.Second
			if backstop <= 0 {
				backstop = 120 * time.Second // defensive: zero config or explicit 0
			}
			tools.HandBrowserOff(oldBrowser, backstop)
		}

		// MCP config unchanged → existing mgr / supervisor still in place.
		// But "POST /config/reload" is the user's explicit retry signal,
		// so any server that's `disabled: false` but not currently
		// connected (e.g. a previous async-connect attempt failed) gets
		// another shot here. Without this, a Desktop "Retry" button has
		// no effect once a server falls into the disconnected-disabled-
		// supervisor-no-auto-reconnect hole.
		s.retryDisconnectedEnabledMCPServers(newCfg)
	}

	if s.onReload != nil {
		go s.onReload()
	}

	resp := map[string]interface{}{"status": "reloaded"}
	// Endpoint change ALWAYS needs a restart — AuthClient.baseURL and the
	// WS dialer URL are captured at construction and cannot be swapped
	// in place today.
	//
	// api_key change needs a restart ONLY when there is no AuthManager
	// running the live key rotation:
	//   - On macOS with AuthManager installed: sign-in/sign-out/Bootstrap
	//     call applyAPIKey which propagates the new key to GatewayClient
	//     and WS Client at runtime. yaml api_key field is also irrelevant
	//     because v1 migration has moved it into Keychain. Mutating yaml's
	//     api_key on this path is a no-op for in-process state, and
	//     surfacing restart_required to Desktop made it chain a /shutdown
	//     after every sign-out (the "Bug 2" reported 2026-05-20).
	//   - On non-darwin / AuthManager-absent path: reload does NOT push
	//     newCfg.APIKey into GatewayClient (captured value), so a yaml
	//     api_key edit really does need a restart to take effect.
	needsRestart := false
	var reason string
	if oldCfg != nil && oldCfg.Endpoint != newCfg.Endpoint {
		needsRestart = true
		reason = "endpoint changed — restart daemon to apply"
	} else if oldCfg != nil && oldCfg.APIKey != newCfg.APIKey && s.auth == nil {
		needsRestart = true
		reason = "api_key changed in yaml (legacy path) — restart daemon to apply"
	}
	if needsRestart {
		resp["restart_required"] = true
		resp["restart_reason"] = reason
	}

	// MCP server status for UI indicators
	if len(newCfg.MCPServers) > 0 {
		// Build set of live-connected server names from MCPManager.
		s.deps.mu.RLock()
		mgr := s.deps.MCPManager
		s.deps.mu.RUnlock()
		connected := make(map[string]bool)
		if mgr != nil {
			for _, name := range mgr.ConnectedServers() {
				connected[name] = true
			}
		}

		mcpStatus := make(map[string]string, len(newCfg.MCPServers))
		for name, srv := range newCfg.MCPServers {
			if srv.Disabled {
				mcpStatus[name] = "disabled"
			} else if connected[name] {
				mcpStatus[name] = "connected"
			} else {
				mcpStatus[name] = "enabled"
			}
		}
		// Mark failed servers from registration error
		if regErr != nil {
			errMsg := regErr.Error()
			for name := range newCfg.MCPServers {
				if newCfg.MCPServers[name].Disabled {
					continue
				}
				if strings.Contains(errMsg, name+":") {
					mcpStatus[name] = "error"
				}
			}
		}
		resp["mcp_servers"] = mcpStatus
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetInstructions(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.deps.ShannonDir, "instructions.md")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, map[string]interface{}{"content": nil})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": string(data)})
}

func (s *Server) handlePutInstructions(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.deps.ShannonDir, "instructions.md")

	// Content-Type negotiation: text/markdown and text/plain accept the
	// request body verbatim as the file contents. This lets clients send
	// raw markdown without JSON-string-escaping every quote/newline (which
	// is fragile when the LLM hand-writes the body field). Anything else
	// (including application/json and the default empty Content-Type) goes
	// through the existing {"content": "..."} JSON shape for back-compat.
	if isTextContentType(r.Header.Get("Content-Type")) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "error reading body")
			return
		}
		if err := agents.AtomicWrite(path, data); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}

	var body struct {
		Content *string `json:"content"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Content == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := agents.AtomicWrite(path, []byte(*body.Content)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// isTextContentType reports whether the Content-Type header indicates a
// text body (markdown or plain) that should be written verbatim. Returns
// false for empty / JSON / malformed / anything else so the JSON path
// stays the default. Uses mime.ParseMediaType for robust handling of
// charset suffixes, casing, comma-joined types, and other RFC 7231 edge
// cases.
func isTextContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "text/markdown" || mt == "text/plain"
}

// syncAuditAdapter bridges the daemon's *audit.AuditLogger (which writes
// AuditEntry rows) to the sync.AuditLogger interface (which emits
// event-name + structured fields). The sync package only ever calls Log;
// we project the fields into AuditEntry.InputSummary so they land in
// audit.log alongside tool-call entries.
type syncAuditAdapter struct {
	logger *audit.AuditLogger
}

func (a syncAuditAdapter) Log(event string, fields map[string]any) {
	if a.logger == nil {
		return
	}
	// Render fields as a stable, compact string. JSON gives us deterministic
	// formatting and is already what the rest of audit.log uses.
	var summary string
	if data, err := json.Marshal(fields); err == nil {
		summary = string(data)
	} else {
		summary = fmt.Sprintf("%v", fields)
	}
	a.logger.Log(audit.AuditEntry{
		Timestamp:    time.Now(),
		ToolName:     event,
		InputSummary: summary,
		Decision:     "logged",
		Approved:     true,
	})
}

// runSyncLoop runs sync.Run on a startup-delayed ticker until ctx is canceled.
// Reads config + rebuilds deps on each tick so config changes (enable/disable,
// fixed missing endpoint/api_key) take effect on the next iteration.
//
// Note: the goroutine stays alive even when sync is initially disabled, so
// enabling sync via config edit without restarting the daemon is picked up
// on the next tick. The ticker cadence itself (DaemonInterval) is still read
// once at startup — changing it requires a daemon restart.
func (s *Server) runSyncLoop(ctx context.Context) {
	initialCfg := syncpkg.LoadConfig(viper.GetViper())
	if initialCfg.DaemonInterval <= 0 {
		return // misconfigured: nothing to do
	}

	// Wait for startup delay, but respect ctx.
	if initialCfg.DaemonStartupDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialCfg.DaemonStartupDelay):
		}
	}

	tick := func() {
		cfg := syncpkg.LoadConfig(viper.GetViper())
		if !cfg.Enabled {
			return // disabled now; cheap to re-check next tick
		}
		deps, ok := s.buildSyncDeps(cfg)
		if !ok {
			return // config incomplete; buildSyncDeps already logged the reason
		}
		if err := syncpkg.Run(ctx, deps); err != nil {
			log.Printf("daemon sync: run error: %v", err)
		}
	}

	if initialCfg.Enabled {
		tick() // catch-up only if currently enabled
	}

	t := time.NewTicker(initialCfg.DaemonInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// cloudGateway returns the live Cloud gateway client when Cloud is usable,
// else nil. It reuses s.deps.GW (whose API key is hot-swapped on login via
// SetAPIKey) rather than constructing a new client from viper, which holds a
// stale/empty key after a runtime login on macOS (the key lives in the
// keychain; viper's copy is blanked by config.Save). The "usable" predicate
// mirrors the sibling cloud features (uploads / share / feishu install).
func (s *Server) cloudGateway() *client.GatewayClient {
	if s == nil || s.deps == nil {
		return nil
	}
	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil || !cfg.Cloud.Enabled || s.liveAPIKey(cfg) == "" || s.deps.GW == nil {
		return nil
	}
	return s.deps.GW
}

// buildSyncDeps returns ok=false if the config is incomplete (missing
// endpoint or api_key in non-dry-run mode). Caller must skip this iteration
// if !ok. This is re-evaluated every tick so a deferred secret load takes
// effect on the next iteration without restarting the daemon.
func (s *Server) buildSyncDeps(cfg syncpkg.Config) (syncpkg.Deps, bool) {
	home, _ := os.UserHomeDir()
	shannonHome := filepath.Join(home, ".shannon")

	var uploader syncpkg.Uploader
	if cfg.DryRun {
		uploader = &syncpkg.DryRunUploader{
			OutboxDir: filepath.Join(shannonHome, "sync_outbox"),
			Now:       time.Now,
		}
	} else {
		endpoint := syncpkg.ResolveEndpoint(cfg, viper.GetViper())
		apiKey := viper.GetString("cloud.api_key")
		if endpoint == "" || apiKey == "" {
			log.Printf("daemon sync: missing endpoint (sync.endpoint or cloud.endpoint) or cloud.api_key; skipping until configured")
			return syncpkg.Deps{}, false
		}
		gw := client.NewGatewayClient(endpoint, apiKey)
		uploader = &syncpkg.CloudUploader{Client: gw}
	}

	loader := func(dir, id string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, id+".json"))
	}

	var auditSink syncpkg.AuditLogger
	if s.deps != nil && s.deps.Auditor != nil {
		auditSink = syncAuditAdapter{logger: s.deps.Auditor}
	}

	var onSyncDone func()
	if s.memSvc != nil {
		onSyncDone = s.memSvc.NotifySyncDone
	}
	return syncpkg.Deps{
		Cfg:        cfg,
		HomeDir:    shannonHome,
		ClientVer:  "kocoro/daemon",
		Uploader:   uploader,
		Loader:     loader,
		Audit:      auditSink,
		Now:        time.Now,
		OnSyncDone: onSyncDone,
	}, true
}
