package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
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
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
	"gopkg.in/yaml.v3"
)

type Server struct {
	port                   int
	client                 *Client
	deps                   *ServerDeps
	server                 *http.Server
	listener               net.Listener
	version                string
	ctx                    context.Context // daemon lifecycle context, set on Start
	cancel                 context.CancelFunc
	approvalBroker         *ApprovalBroker
	eventBus               *EventBus
	notifyApprovalResolved func(p ApprovalResolvedPayload) error
	// pendingBrokers maps requestID → per-request ApprovalBroker.
	// SSE handlers register here so POST /approval can find the right broker.
	pendingBrokers sync.Map // map[string]*ApprovalBroker
	onReload       func()   // called after config reload to restart watchers/heartbeat

	marketplace *skills.MarketplaceClient
	slugLocks   *skills.SlugLocks
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
	return &Server{
		port:                   port,
		client:                 client,
		deps:                   deps,
		version:                version,
		approvalBroker:         NewApprovalBroker(func(req ApprovalRequest) error { return nil }),
		eventBus:               NewEventBus(),
		notifyApprovalResolved: func(p ApprovalResolvedPayload) error { return nil },
		marketplace:            skills.NewMarketplaceClient(resolveRegistryURL(deps), 1*time.Hour),
		slugLocks:              skills.NewSlugLocks(),
	}
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
	if s.listener != nil {
		return s.listener.Addr().(*net.TCPAddr).Port
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

func (s *Server) Start(ctx context.Context) error {
	s.ctx = ctx
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /agents", s.handleAgents)
	mux.HandleFunc("GET /agents/{name}", s.handleGetAgent)
	mux.HandleFunc("POST /agents", s.handleCreateAgent)
	mux.HandleFunc("PUT /agents/{name}", s.handleUpdateAgent)
	mux.HandleFunc("DELETE /agents/{name}", s.handleDeleteAgent)
	mux.HandleFunc("PUT /agents/{name}/config", s.handlePutAgentConfig)
	mux.HandleFunc("DELETE /agents/{name}/config", s.handleDeleteAgentConfig)
	mux.HandleFunc("PUT /agents/{name}/commands/{cmd}", s.handlePutCommand)
	mux.HandleFunc("DELETE /agents/{name}/commands/{cmd}", s.handleDeleteCommand)
	mux.HandleFunc("PUT /agents/{name}/skills/{skill}", s.handlePutSkill)
	mux.HandleFunc("DELETE /agents/{name}/skills/{skill}", s.handleDeleteSkill)
	mux.HandleFunc("GET /skills/downloadable", s.handleListDownloadableSkills)
	mux.HandleFunc("POST /skills/install/{name}", s.handleInstallSkill)
	mux.HandleFunc("POST /skills/marketplace/install/{slug}", s.handleMarketplaceInstall)
	mux.HandleFunc("GET /skills/marketplace", s.handleMarketplaceList)
	mux.HandleFunc("GET /skills/marketplace/entry/{slug}", s.handleMarketplaceDetail)
	mux.HandleFunc("GET /skills", s.handleListSkills)
	mux.HandleFunc("GET /skills/{name}", s.handleGetSkill)
	mux.HandleFunc("PUT /skills/{name}", s.handlePutGlobalSkill)
	mux.HandleFunc("DELETE /skills/{name}", s.handleDeleteGlobalSkill)
	mux.HandleFunc("GET /skills/{name}/scripts", s.handleListSkillScripts)
	mux.HandleFunc("PUT /skills/{name}/scripts/{filename}", s.handlePutSkillScripts)
	mux.HandleFunc("DELETE /skills/{name}/scripts/{filename}", s.handleDeleteSkillScripts)
	mux.HandleFunc("GET /skills/{name}/references", s.handleListSkillReferences)
	mux.HandleFunc("PUT /skills/{name}/references/{filename}", s.handlePutSkillReferences)
	mux.HandleFunc("DELETE /skills/{name}/references/{filename}", s.handleDeleteSkillReferences)
	mux.HandleFunc("GET /skills/{name}/assets", s.handleListSkillAssets)
	mux.HandleFunc("GET /skills/{name}/usage", s.handleSkillUsage)
	mux.HandleFunc("PUT /skills/{name}/assets/{filename}", s.handlePutSkillAssets)
	mux.HandleFunc("DELETE /skills/{name}/assets/{filename}", s.handleDeleteSkillAssets)
	mux.HandleFunc("GET /schedules", s.handleListSchedules)
	mux.HandleFunc("GET /schedules/{id}", s.handleGetSchedule)
	mux.HandleFunc("POST /schedules", s.handleCreateSchedule)
	mux.HandleFunc("PATCH /schedules/{id}", s.handlePatchSchedule)
	mux.HandleFunc("DELETE /schedules/{id}", s.handleDeleteSchedule)
	mux.HandleFunc("GET /config", s.handleGetConfig)
	mux.HandleFunc("GET /config/status", s.handleConfigStatus)
	mux.HandleFunc("PATCH /config", s.handlePatchConfig)
	mux.HandleFunc("POST /config/reload", s.handleConfigReload)
	mux.HandleFunc("GET /instructions", s.handleGetInstructions)
	mux.HandleFunc("PUT /instructions", s.handlePutInstructions)
	mux.HandleFunc("GET /sessions", s.handleSessions)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("PATCH /sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("POST /sessions/{id}/edit", s.handleEditMessage)
	mux.HandleFunc("GET /sessions/{id}/summary", s.handleSessionSummary)
	mux.HandleFunc("GET /sessions/search", s.handleSessionSearch)
	mux.HandleFunc("GET /permissions", s.handlePermissions)
	mux.HandleFunc("POST /permissions/request", s.handlePermissionsRequest)
	mux.HandleFunc("POST /approval", s.handleApproval)
	mux.HandleFunc("POST /message", s.handleMessage)
	mux.HandleFunc("POST /cancel", s.handleCancel)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /chrome/status", s.handleChromeStatus)
	mux.HandleFunc("GET /chrome/profile", s.handleChromeProfile)
	mux.HandleFunc("POST /chrome/profile", s.handleChromeProfileUpdate)
	mux.HandleFunc("POST /chrome/profile/refresh", s.handleChromeProfileRefresh)
	mux.HandleFunc("POST /chrome/show", s.handleChromeShow)
	mux.HandleFunc("POST /chrome/hide", s.handleChromeHide)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)

	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", s.port))
	if err != nil {
		return fmt.Errorf("daemon server listen: %w", err)
	}
	s.listener = ln
	s.server = &http.Server{Handler: mux}

	go func() {
		<-ctx.Done()
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
		RouteKey  string `json:"route_key,omitempty"`
		SessionID string `json:"session_id,omitempty"`
		Agent     string `json:"agent,omitempty"`
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

	s.deps.SessionCache.CancelRoute(key)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cancelled", "route": key})
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
	// Notify Cloud and emit event BEFORE unblocking the agent.
	// This ensures ShanClaw dismisses the approval card before seeing the agent reply.
	_ = s.notifyApprovalResolved(ApprovalResolvedPayload{
		RequestID:  req.RequestID,
		Decision:   req.Decision,
		ResolvedBy: "shanclaw",
	})

	if s.eventBus != nil {
		payload, _ := json.Marshal(map[string]string{
			"request_id":  req.RequestID,
			"decision":    string(req.Decision),
			"resolved_by": "shanclaw",
		})
		s.eventBus.Emit(Event{Type: EventApprovalResolved, Payload: payload})
	}

	// Look up the per-request broker (SSE path) or fall back to server broker (WS path).
	if b, ok := s.pendingBrokers.Load(req.RequestID); ok {
		b.(*ApprovalBroker).Resolve(req.RequestID, req.Decision)
	} else {
		s.approvalBroker.Resolve(req.RequestID, req.Decision)
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
	json.NewEncoder(w).Encode(map[string]interface{}{
		"is_connected": s.client.IsConnected(),
		"active_agent": s.client.ActiveAgent(),
		"uptime":       int(s.client.Uptime().Seconds()),
		"version":      s.version,
	})
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
	// Filter out empty sessions (created but never used).
	filtered := make([]session.SessionSummary, 0, len(summaries))
	for _, s := range summaries {
		if s.MsgCount > 0 {
			filtered = append(filtered, s)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": filtered})
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
	// 防止路径穿越
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid session id")
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
		if os.IsNotExist(err) {
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
	// Prevent path traversal — session IDs must be safe filenames.
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid session id")
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
	if err := mgr.Delete(id); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
	// Prevent path traversal — session ID must be a safe filename.
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title cannot be empty")
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
	if err := mgr.PatchTitle(id, title); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "title": title})
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

// handleEditMessage 截断指定 session 的历史消息并以新内容重新触发 agent。
// 请求体：{"message_index": N, "new_content": "...", "agent": "可选"}
// message_index 表示保留前 N 条消息，N 之后的消息全部丢弃，再以 new_content 重新发送。
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
	// 防止路径穿越
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	var body struct {
		MessageIndex int    `json:"message_index"`
		NewContent   string `json:"new_content"`
		Agent        string `json:"agent,omitempty"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.NewContent) == "" {
		writeError(w, http.StatusBadRequest, "new_content is required")
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
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 以新内容重新触发 agent，复用现有消息发送流程
	runReq := RunAgentRequest{
		Text:      body.NewContent,
		Agent:     body.Agent,
		SessionID: id,
		Source:    "shanclaw",
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
	if id != filepath.Base(id) {
		writeError(w, http.StatusBadRequest, "invalid session id")
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
		if os.IsNotExist(err) {
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
	if req.Source == "" {
		req.Source = "shanclaw"
	}
	// Normalize "default" → "" early so downstream guards are consistent.
	if req.Agent == "default" {
		req.Agent = ""
	}
	// Named agents always resume their single long-lived session.
	// Clear new_session so clients cannot fork a named agent's context.
	if req.Agent != "" {
		req.NewSession = false
	}
	req.EnsureRouteKey()
	if err := req.Validate(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Try injecting into an in-flight run on the same route.
	if req.RouteKey != "" {
		switch s.deps.SessionCache.InjectMessage(req.RouteKey, agent.InjectedMessage{Text: req.Text, CWD: req.CWD}) {
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
		case InjectNoActiveRun:
			// Fall through to start a new RunAgent
		}
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
	reqBroker := NewApprovalBroker(func(areq ApprovalRequest) error {
		data := mustJSON(areq)
		_, err := fmt.Fprintf(w, "event: approval\ndata: %s\n\n", data)
		flusher.Flush()
		return err
	})
	// Inherit onRequest callback from the server broker for EventBus emission.
	reqBroker.onRequest = s.approvalBroker.onRequest
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

	handler := &sseEventHandler{w: w, flusher: flusher, broker: reqBroker, ctx: r.Context(), autoApprove: autoApprove, deps: s.deps}
	result, err := RunAgent(r.Context(), s.deps, req, handler)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"error": err.Error()}))
		flusher.Flush()
		return
	}

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
type httpEventHandler struct{}

func (h *httpEventHandler) OnToolCall(name string, args string) {}
func (h *httpEventHandler) OnToolResult(name string, args string, result agent.ToolResult, elapsed time.Duration) {
	log.Printf("http: tool %s completed (%.1fs)", name, elapsed.Seconds())
}
func (h *httpEventHandler) OnText(text string)            {}
func (h *httpEventHandler) OnStreamDelta(delta string)    {}
func (h *httpEventHandler) OnUsage(usage agent.TurnUsage) {}

// OnApprovalNeeded auto-approves for local HTTP API calls.
// Threat model: localhost-only, unauthenticated but local-trusted.
// Permission engine (hard-blocks, denied_commands) runs before this.
// If daemon ever listens on non-localhost, this MUST require auth.
func (h *httpEventHandler) OnCloudAgent(agentID, status, message string)           {}
func (h *httpEventHandler) OnCloudProgress(completed, total int)                   {}
func (h *httpEventHandler) OnCloudPlan(planType, content string, needsReview bool) {}

func (h *httpEventHandler) OnApprovalNeeded(tool string, args string) bool {
	return true
}

// sseEventHandler streams agent events as SSE to an HTTP response.
type sseEventHandler struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	broker      *ApprovalBroker
	ctx         context.Context
	autoApprove bool
	deps        *ServerDeps
}

func (h *sseEventHandler) OnToolCall(name string, args string) {
	data := mustJSON(map[string]interface{}{"tool": name, "status": "running"})
	fmt.Fprintf(h.w, "event: tool\ndata: %s\n\n", data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnToolResult(name string, args string, result agent.ToolResult, elapsed time.Duration) {
	// SSE is request-scoped (one tool stream per HTTP request), so session_id
	// is intentionally omitted here; session correlation is handled at the client
	// session boundary.
	data := mustJSON(map[string]interface{}{
		"tool":    name,
		"status":  "completed",
		"elapsed": elapsed.Seconds(),
	})
	fmt.Fprintf(h.w, "event: tool\ndata: %s\n\n", data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnText(text string) {}

func (h *sseEventHandler) OnStreamDelta(delta string) {
	data := mustJSON(map[string]string{"text": delta})
	fmt.Fprintf(h.w, "event: delta\ndata: %s\n\n", data)
	h.flusher.Flush()
}

func (h *sseEventHandler) OnUsage(usage agent.TurnUsage) {}

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
		log.Printf("sse: auto-approving %s (auto_approve=true)", tool)
		return true
	}
	decision := h.broker.Request(h.ctx, "", "", "", tool, args)
	if decision == DecisionAlwaysAllow {
		if tool == "bash" {
			cmd := permissions.ExtractField(args, "command")
			if cmd != "" {
				if err := config.AppendAllowedCommand(h.deps.ShannonDir, cmd); err != nil {
					log.Printf("sse: failed to persist always-allow: %v", err)
				} else {
					h.deps.WriteLock()
					perms := &h.deps.Config.Permissions
					found := false
					for _, c := range perms.AllowedCommands {
						if c == cmd {
							found = true
							break
						}
					}
					if !found {
						perms.AllowedCommands = append(perms.AllowedCommands, cmd)
					}
					h.deps.WriteUnlock()
					log.Printf("sse: always-allow persisted: %s", cmd)
				}
			}
		} else {
			h.broker.SetToolAutoApprove(tool)
			log.Printf("sse: always-allow (session): %s", tool)
		}
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
	// Only return global (installed) skills — bundled skills are hidden from
	// the skills list API. Users install them on demand via POST /skills/install.
	global := skills.SkillSource{
		Dir:    filepath.Join(s.deps.ShannonDir, "skills"),
		Source: skills.SourceGlobal,
	}
	return []skills.SkillSource{global}, nil
}

func skillNamesFromRequest(entries []*skills.Skill) []string {
	names := make([]string, 0, len(entries))
	for _, skill := range entries {
		if skill != nil && skill.Name != "" {
			names = append(names, skill.Name)
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
	installed := make(map[string]bool, len(list))
	for _, skill := range list {
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

	writeJSON(w, http.StatusOK, api)
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req agents.AgentCreateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateInstalledSkills(skillNamesFromRequest(req.Skills)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Serialize creates for the same agent name to prevent concurrent rollback races.
	routeKey := "agent:" + req.Name
	s.deps.SessionCache.LockRoute(routeKey)
	defer s.deps.SessionCache.UnlockRoute(routeKey)

	agentDir := filepath.Join(s.deps.AgentsDir, req.Name)
	if _, err := os.Stat(filepath.Join(agentDir, "AGENT.md")); err == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("agent %q already exists", req.Name))
		return
	}
	// If name matches a builtin, materialize user override first so the
	// subsequent writes land in the user dir and override the builtin.
	if agents.IsBuiltinAgent(req.Name) {
		if err := agents.MaterializeBuiltin(s.deps.AgentsDir, req.Name); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to materialize builtin: %s", err))
			return
		}
	}
	// Write all agent files — rollback on any failure.
	// Only remove files we materialized; preserve MEMORY.md and sessions/
	// which are runtime state that may exist from prior builtin agent usage.
	rollback := func() {
		dir := filepath.Join(s.deps.AgentsDir, req.Name)
		os.Remove(filepath.Join(dir, "AGENT.md"))
		os.Remove(filepath.Join(dir, "config.yaml"))
		os.Remove(filepath.Join(dir, "_attached.yaml"))
		os.RemoveAll(filepath.Join(dir, "commands"))
		os.RemoveAll(filepath.Join(dir, "skills"))
	}
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
	a, err := agents.LoadAgent(s.deps.AgentsDir, req.Name)
	if err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
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
		parsedConfig = &cfg
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
		if err := skills.ValidateSkillName(skill.Name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.validateInstalledSkills(skillNamesFromRequest(req.Skills)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

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
			if err := os.Remove(filepath.Join(agentDir, "config.yaml")); err != nil && !os.IsNotExist(err) {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete config: %v", err))
				return
			}
		} else {
			if err := agents.WriteAgentConfig(s.deps.AgentsDir, name, parsedConfig); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
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
	a, err := agents.LoadAgent(s.deps.AgentsDir, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.ToAPI())
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
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
	s.deps.SessionCache.Evict(name)
	// Remove only definition files — preserve runtime state (MEMORY.md, sessions/)
	// so the builtin can resurface with existing history intact.
	agentDir := filepath.Join(s.deps.AgentsDir, name)
	var errs []string
	for _, f := range []string{"AGENT.md", "config.yaml", "_attached.yaml"} {
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
	if len(errs) > 0 {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("partial delete: %s", strings.Join(errs, "; ")))
		return
	}
	// Clean up empty dir if no runtime state remains
	if entries, err := os.ReadDir(agentDir); err == nil && len(entries) == 0 {
		os.Remove(agentDir)
	}
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
	// Materialize builtin AFTER validation passes — avoids orphaned override dirs on bad input.
	if !s.materializeIfBuiltin(w, name) {
		return
	}
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
	path := filepath.Join(s.deps.AgentsDir, name, "config.yaml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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

	err = skills.InstallFromMarketplace(r.Context(), s.deps.ShannonDir, *entry, s.slugLocks)
	switch {
	case err == nil:
		// Load the freshly installed skill so the response body reflects
		// on-disk truth (frontmatter name, description, source) rather
		// than synthesized data from the registry. Mirrors the pattern
		// used by handleInstallSkill for Anthropic-repo installs.
		sources, _ := s.skillSources()
		list, _ := skills.LoadSkills(sources...)
		for _, skill := range list {
			if skill.Name == entry.Slug {
				writeJSON(w, http.StatusCreated, skill.ToMeta())
				return
			}
		}
		// Fallback: install succeeded but the skill did not show up in
		// LoadSkills. This shouldn't happen because InstallFromMarketplace
		// guarantees a valid SKILL.md on success, but we return a stable
		// 201 with minimal info rather than misleading the client.
		writeJSON(w, http.StatusCreated, skills.SkillMeta{
			Name:        entry.Slug,
			Description: entry.Description,
			Source:      "global",
		})
	case errors.Is(err, skills.ErrMaliciousSkill):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, skills.ErrSkillAlreadyInstalled):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, skills.ErrInvalidSkillPayload):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, skills.ErrMarketplaceUpstreamFailure):
		writeError(w, http.StatusBadGateway, fmt.Sprintf("install failed: %v", err))
	default:
		// Local disk/staging failures → 500, per spec error matrix.
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("install failed: %v", err))
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

	metas := make([]skills.SkillMeta, 0, len(list))
	for _, skill := range list {
		metas = append(metas, skill.ToMeta())
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
	name := r.PathValue("name")
	if !skills.IsDownloadable(name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("skill %q is not available for download", name))
		return
	}

	if err := skills.InstallSkillFromRepo(s.deps.ShannonDir, name); err != nil {
		if strings.Contains(err.Error(), "already installed") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Load the installed skill to return its metadata
	sources, _ := s.skillSources()
	list, _ := skills.LoadSkills(sources...)
	for _, skill := range list {
		if skill.Name == name {
			writeJSON(w, http.StatusCreated, skill.ToMeta())
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "installed", "name": name})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
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
		if skill.Name == name {
			detail := skills.SkillDetail{
				Name:            skill.Name,
				Description:     skill.Description,
				Prompt:          skill.Prompt,
				Source:          skill.Source,
				InstallSource:   skill.InstallSource,
				MarketplaceSlug: skill.MarketplaceSlug,
				License:         skill.License,
				Compatibility:   skill.Compatibility,
				Metadata:        skill.Metadata,
			}
			if len(skill.AllowedTools) > 0 {
				detail.AllowedTools = skill.AllowedTools
			}
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
	var req struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		License     string `json:"license"`
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
	if err := skills.WriteGlobalSkill(s.deps.ShannonDir, &skills.Skill{
		Name:        name,
		Description: req.Description,
		Prompt:      req.Prompt,
		License:     req.License,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteGlobalSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := skills.ValidateSkillName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
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
			writeJSON(w, http.StatusOK, map[string][]string{"files": []string{}})
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

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.ScheduleManager == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	var req struct {
		Agent  string `json:"agent"`
		Cron   string `json:"cron"`
		Prompt string `json:"prompt"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	id, err := s.deps.ScheduleManager.Create(req.Agent, req.Cron, req.Prompt)
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
		Cron    *string `json:"cron"`
		Prompt  *string `json:"prompt"`
		Enabled *bool   `json:"enabled"`
	}
	if !decodeBody(w, r, &patch) {
		return
	}
	if patch.Cron == nil && patch.Prompt == nil && patch.Enabled == nil {
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	update := &schedule.UpdateOpts{
		Cron:    patch.Cron,
		Prompt:  patch.Prompt,
		Enabled: patch.Enabled,
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
	if err := s.patchGlobalConfig(patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
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
		for name, srv := range cfg.MCPServers {
			if srv.Disabled {
				mcpStatus[name] = "disabled"
			} else if connected[name] {
				mcpStatus[name] = "connected"
			} else {
				mcpStatus[name] = "enabled"
			}
		}
		resp["mcp_servers"] = mcpStatus
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
			oldSrv.Context != newSrv.Context || !slices.Equal(oldSrv.Args, newSrv.Args) ||
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
		newBaseline, newReg, _, newMCPMgr, newCleanup, regErr = tools.RegisterAllWithBaseline(s.deps.GW, newCfg)
		if regErr != nil {
			log.Printf("daemon: reload warning: %v", regErr)
		}
		tools.RegisterCloudDelegate(newReg, s.deps.GW, newCfg, nil, "", "")

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

		s.deps.mu.Lock()
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
	} else {
		// Config changed but MCP servers didn't — update config and refresh
		// cached rebuild layers so health-driven rebuilds use current settings.
		newBaseline, _, newBaseCleanup := tools.RegisterLocalTools(newCfg)
		// Re-register gateway tools on top of fresh baseline clone.
		// Use a short timeout — if the gateway is unavailable, keep existing overlay.
		freshReg := newBaseline.Clone()
		gwCtx, gwCancel := context.WithTimeout(r.Context(), 5*time.Second)
		gwErr := tools.RegisterServerTools(gwCtx, s.deps.GW, freshReg)
		gwCancel()
		tools.RegisterCloudDelegate(freshReg, s.deps.GW, newCfg, nil, "", "")
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

		s.deps.mu.Lock()
		oldCleanup := s.deps.Cleanup
		s.deps.Config = newCfg
		s.deps.BaselineReg = newBaseline
		s.deps.GatewayOverlay = newGatewayOverlay
		s.deps.PostOverlays = newPostOverlays
		s.deps.Cleanup = func() { newBaseCleanup(); oldCleanup() }
		s.deps.mu.Unlock()
	}

	if s.onReload != nil {
		go s.onReload()
	}

	resp := map[string]interface{}{"status": "reloaded"}
	if oldCfg != nil && (oldCfg.Endpoint != newCfg.Endpoint || oldCfg.APIKey != newCfg.APIKey) {
		resp["restart_required"] = true
		resp["restart_reason"] = "endpoint or api_key changed — restart daemon to apply"
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
	var body struct {
		Content *string `json:"content"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	path := filepath.Join(s.deps.ShannonDir, "instructions.md")
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
