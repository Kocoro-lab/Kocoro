package daemon

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func writeTestGlobalSkill(t *testing.T, shannonDir, name string) {
	t.Helper()
	if err := skills.WriteGlobalSkill(shannonDir, &skills.Skill{
		Name:        name,
		Description: name + " description",
		Prompt:      "prompt for " + name,
	}); err != nil {
		t.Fatalf("write global skill %s: %v", name, err)
	}
}

func TestServer_GlobalSkillStickyRoundTrip(t *testing.T) {
	shannonDir := t.TempDir()
	if err := skills.WriteGlobalSkill(shannonDir, &skills.Skill{
		Name:                  "policy",
		Description:           "policy description",
		Prompt:                "# policy\n\nUse the API.",
		License:               "MIT",
		StickyInstructions:    true,
		StickySnippetOverride: "Use the http tool for platform operations.",
	}); err != nil {
		t.Fatalf("seed global skill: %v", err)
	}

	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/skills/policy", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /skills/policy status = %d", resp.StatusCode)
	}
	var detail struct {
		Name               string `json:"name"`
		StickyInstructions bool   `json:"sticky_instructions"`
		StickySnippet      string `json:"sticky_snippet"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if detail.Name != "policy" {
		t.Fatalf("GET returned name %q", detail.Name)
	}
	if !detail.StickyInstructions {
		t.Fatal("GET dropped sticky_instructions")
	}
	if detail.StickySnippet != "Use the http tool for platform operations." {
		t.Fatalf("GET sticky_snippet = %q", detail.StickySnippet)
	}

	reqBody := `{"description":"updated description","prompt":"# policy\n\nUpdated.","license":"Apache-2.0"}`
	// First PUT without force: must hit the conflict gate since the seed
	// already wrote a `policy` skill to disk. The Desktop client (Swift
	// SkillsViewModel.save) surfaces the 409 as a compare sheet and the user
	// must explicitly confirm overwrite before we retry with force=true.
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/skills/policy", srv.Port()), strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("first PUT /skills/policy status = %d body=%s, want 409", resp2.StatusCode, string(body))
	}
	resp2.Body.Close()

	// Retry with ?force=true — equivalent to the user clicking "Overwrite"
	// in the compare sheet, or to the edit-existing-skill flow where the
	// frontend passes force=true unconditionally because edit implies overwrite.
	forceURL := fmt.Sprintf("http://127.0.0.1:%d/skills/policy?force=true", srv.Port())
	req, err = http.NewRequest(http.MethodPut, forceURL, strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("force PUT /skills/policy status = %d body=%s", resp3.StatusCode, string(body))
	}

	loaded, err := skills.LoadSkills(skills.SkillSource{
		Dir:    filepath.Join(shannonDir, "skills"),
		Source: skills.SourceGlobal,
	})
	if err != nil {
		t.Fatalf("reload skills: %v", err)
	}
	var policy *skills.Skill
	for _, skill := range loaded {
		if skill.Name == "policy" {
			policy = skill
			break
		}
	}
	if policy == nil {
		t.Fatal("reloaded skill not found")
	}
	if !policy.StickyInstructions {
		t.Fatal("PUT dropped sticky instructions")
	}
	if policy.StickySnippetOverride != "Use the http tool for platform operations." {
		t.Fatalf("PUT dropped sticky snippet override: %q", policy.StickySnippetOverride)
	}
	if policy.License != "Apache-2.0" {
		t.Fatalf("license not updated: %q", policy.License)
	}
}

// TestSSEEventHandler_AutoApproveAllowsAllTools pins the 2026-05-18 policy:
// with autoApprove=true, the SSE handler auto-approves every tool without
// a broker round-trip (unattended deny-list is empty). Previously
// publish_to_web / generate_image / edit_image still prompted via the
// broker; the product call moved them off the gate.
func TestSSEEventHandler_AutoApproveAllowsAllTools(t *testing.T) {
	for _, tool := range []string{
		"publish_to_web", "generate_image", "edit_image",
		"bash", "file_write",
	} {
		t.Run(tool, func(t *testing.T) {
			brokerCalled := false
			broker := NewApprovalBroker(func(req ApprovalRequest) error {
				brokerCalled = true
				return nil
			})
			handler := &sseEventHandler{
				broker:      broker,
				ctx:         context.Background(),
				autoApprove: true,
			}

			if !handler.OnApprovalNeeded(tool, `{"path":"report.html"}`) {
				t.Fatalf("autoApprove=true should auto-approve %s without prompting", tool)
			}
			if brokerCalled {
				t.Errorf("%s was auto-approved but broker was still invoked", tool)
			}
		})
	}
}

func TestSSEEventHandler_AutoApproveSkipsBrokerWhenNotPerCallOnly(t *testing.T) {
	brokerCalled := false
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		brokerCalled = true
		return nil
	})
	handler := &sseEventHandler{
		broker:      broker,
		ctx:         context.Background(),
		autoApprove: true,
	}

	if !handler.OnApprovalNeeded("file_read", `{"path":"notes.txt"}`) {
		t.Fatal("non-per-call tool should still be auto-approved")
	}
	if brokerCalled {
		t.Fatal("non-per-call auto-approved tool should not prompt via broker")
	}
}

func TestServer_Health(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, nil, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
	if body["version"] != "test" {
		t.Errorf("version = %q, want %q", body["version"], "test")
	}
}

func TestServer_Status(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, nil, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		IsConnected bool   `json:"is_connected"`
		ActiveAgent string `json:"active_agent"`
		Uptime      int    `json:"uptime"`
		Version     string `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.IsConnected {
		t.Error("should not be connected")
	}
	if body.Uptime < 0 {
		t.Error("uptime should be non-negative")
	}
	if body.Version != "test" {
		t.Errorf("version = %q, want %q", body.Version, "test")
	}
}

func TestServer_Shutdown(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, nil, "test")
	ctx, cancel := context.WithCancel(context.Background())

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	cancel()
	time.Sleep(200 * time.Millisecond)

	_, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", srv.Port()))
	if err == nil {
		t.Error("expected connection refused after shutdown")
	}
}

func TestServer_Agents_Empty(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]json.RawMessage
	json.Unmarshal(body, &parsed)
	if string(parsed["agents"]) != "[]" {
		t.Errorf("expected empty agents array, got %s", string(body))
	}
}

func TestServer_Sessions_Empty(t *testing.T) {
	sessDir := t.TempDir()
	deps := &ServerDeps{
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/sessions", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]json.RawMessage
	json.Unmarshal(body, &parsed)
	if string(parsed["sessions"]) != "[]" {
		t.Errorf("expected empty sessions array, got %s", string(body))
	}
}

func TestServer_Message_MissingText(t *testing.T) {
	deps := &ServerDeps{}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()),
		"application/json",
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestServer_Message_AgentNotFound(t *testing.T) {
	sessDir := t.TempDir()
	deps := &ServerDeps{
		Config:       &config.Config{},
		AgentsDir:    t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()),
		"application/json",
		strings.NewReader(`{"text":"hello","agent":"nonexistent"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Agent falls back to default when not found, but RunAgent will fail
	// because deps are incomplete (no gateway, registry). 500 is expected.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "error") {
		t.Errorf("expected error in body, got %s", string(body))
	}
}

func TestServer_ChromeHandlersUseConfiguredPlaywrightPort(t *testing.T) {
	oldShow := showChromeOnPortFn
	oldHide := hideChromeOnPortFn
	oldStatus := getChromeStatusOnPortFn
	defer func() {
		showChromeOnPortFn = oldShow
		hideChromeOnPortFn = oldHide
		getChromeStatusOnPortFn = oldStatus
	}()

	var showPort, hidePort, statusPort int
	showChromeOnPortFn = func(port int) error {
		showPort = port
		return nil
	}
	hideChromeOnPortFn = func(port int) error {
		hidePort = port
		return nil
	}
	getChromeStatusOnPortFn = func(port int) mcp.CDPChromeStatus {
		statusPort = port
		return mcp.CDPChromeStatus{Running: true, Visible: true}
	}

	deps := &ServerDeps{
		Config: &config.Config{
			MCPServers: map[string]mcp.MCPServerConfig{
				"playwright": {
					Args: []string{"--cdp-endpoint", "http://127.0.0.1:9333"},
				},
			},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	showRec := httptest.NewRecorder()
	srv.handleChromeShow(showRec, httptest.NewRequest(http.MethodPost, "/chrome/show", nil))
	if showPort != 9333 {
		t.Fatalf("show used port %d, want 9333", showPort)
	}
	if showRec.Code != http.StatusOK {
		t.Fatalf("show status = %d, want 200", showRec.Code)
	}
	var showBody map[string]string
	if err := json.NewDecoder(showRec.Body).Decode(&showBody); err != nil {
		t.Fatalf("decode show body: %v", err)
	}
	if showBody["status"] != "visible" {
		t.Fatalf("show body = %v, want visible status", showBody)
	}

	hideRec := httptest.NewRecorder()
	srv.handleChromeHide(hideRec, httptest.NewRequest(http.MethodPost, "/chrome/hide", nil))
	if hidePort != 9333 {
		t.Fatalf("hide used port %d, want 9333", hidePort)
	}
	if hideRec.Code != http.StatusOK {
		t.Fatalf("hide status = %d, want 200", hideRec.Code)
	}
	var hideBody map[string]string
	if err := json.NewDecoder(hideRec.Body).Decode(&hideBody); err != nil {
		t.Fatalf("decode hide body: %v", err)
	}
	if hideBody["status"] != "hidden" {
		t.Fatalf("hide body = %v, want hidden status", hideBody)
	}

	statusRec := httptest.NewRecorder()
	srv.handleChromeStatus(statusRec, httptest.NewRequest(http.MethodGet, "/chrome/status", nil))
	if statusPort != 9333 {
		t.Fatalf("status used port %d, want 9333", statusPort)
	}
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", statusRec.Code)
	}
	var statusBody map[string]bool
	if err := json.NewDecoder(statusRec.Body).Decode(&statusBody); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	if !statusBody["running"] || !statusBody["visible"] {
		t.Fatalf("status body = %v, want running+visible", statusBody)
	}
	if statusBody["probe_error"] {
		t.Fatalf("status body = %v, want probe_error=false", statusBody)
	}
}

func TestServer_ChromeHandlersNormalizeLegacyPlaywrightPort(t *testing.T) {
	oldShow := showChromeOnPortFn
	defer func() { showChromeOnPortFn = oldShow }()

	var showPort int
	showChromeOnPortFn = func(port int) error {
		showPort = port
		return nil
	}

	deps := &ServerDeps{
		Config: &config.Config{
			MCPServers: map[string]mcp.MCPServerConfig{
				"playwright": {
					Args: []string{"--cdp-endpoint", "http://localhost:9222"},
				},
			},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	srv.handleChromeShow(rec, httptest.NewRequest(http.MethodPost, "/chrome/show", nil))
	if showPort != mcp.DefaultCDPPort {
		t.Fatalf("show used port %d, want normalized default %d", showPort, mcp.DefaultCDPPort)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("show status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode show body: %v", err)
	}
	if body["status"] != "visible" {
		t.Fatalf("show body = %v, want visible status", body)
	}
}

func TestServer_ChromeProfileHandlerUsesConfiguredProfile(t *testing.T) {
	oldGet := getChromeProfileStateFn
	defer func() { getChromeProfileStateFn = oldGet }()

	var configured string
	getChromeProfileStateFn = func(profile string) (mcp.ChromeProfileState, error) {
		configured = profile
		return mcp.ChromeProfileState{
			Mode:              "explicit",
			ConfiguredProfile: profile,
			EffectiveProfile:  profile,
			CloneStatus:       mcp.ChromeProfileCloneCurrent,
			Profiles: []mcp.ChromeProfileOption{
				{Name: "Profile 6", DisplayName: "Work", Exists: true, IsConfigured: true, IsEffective: true},
			},
		}, nil
	}

	deps := &ServerDeps{
		Config: &config.Config{
			Daemon: config.DaemonConfig{ChromeProfile: "Profile 6"},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	srv.handleChromeProfile(rec, httptest.NewRequest(http.MethodGet, "/chrome/profile", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	if configured != "Profile 6" {
		t.Fatalf("expected configured profile 'Profile 6', got %q", configured)
	}
	var body mcp.ChromeProfileState
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.EffectiveProfile != "Profile 6" {
		t.Fatalf("expected effective profile 'Profile 6', got %q", body.EffectiveProfile)
	}
	if body.CloneStatus != mcp.ChromeProfileCloneCurrent {
		t.Fatalf("expected clone status %q, got %q", mcp.ChromeProfileCloneCurrent, body.CloneStatus)
	}
}

func TestServer_ChromeProfileUpdateExplicitPersistsAndRefreshesClone(t *testing.T) {
	oldGet := getChromeProfileStateFn
	oldStop := stopChromeFn
	oldReset := resetChromeProfileCloneFn
	oldProfile := mcp.GetCDPChromeProfile()
	defer func() {
		getChromeProfileStateFn = oldGet
		stopChromeFn = oldStop
		resetChromeProfileCloneFn = oldReset
		mcp.SetCDPChromeProfile(oldProfile)
	}()

	getChromeProfileStateFn = func(profile string) (mcp.ChromeProfileState, error) {
		state := mcp.ChromeProfileState{
			Mode:              "explicit",
			ConfiguredProfile: profile,
			EffectiveProfile:  profile,
			CloneStatus:       mcp.ChromeProfileCloneMissing,
			Profiles: []mcp.ChromeProfileOption{
				{Name: "Default", DisplayName: "Default", Exists: true},
				{Name: "Profile 6", DisplayName: "Work", Exists: true, IsConfigured: profile == "Profile 6", IsEffective: profile == "Profile 6"},
			},
		}
		if profile == "" {
			state.Mode = "auto"
			state.DetectedProfile = "Profile 6"
			state.EffectiveProfile = "Profile 6"
		}
		return state, nil
	}

	stopCalls := 0
	stopChromeFn = func() { stopCalls++ }
	resetCalls := 0
	resetChromeProfileCloneFn = func() error {
		resetCalls++
		return nil
	}

	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	deps := &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chrome/profile", strings.NewReader(`{"mode":"explicit","profile":"Profile 6"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChromeProfileUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if deps.Config.Daemon.ChromeProfile != "Profile 6" {
		t.Fatalf("expected in-memory config to be updated, got %q", deps.Config.Daemon.ChromeProfile)
	}
	if mcp.GetCDPChromeProfile() != "Profile 6" {
		t.Fatalf("expected runtime chrome profile override, got %q", mcp.GetCDPChromeProfile())
	}
	if stopCalls != 1 || resetCalls != 1 {
		t.Fatalf("expected stop/reset to be called once each, got stop=%d reset=%d", stopCalls, resetCalls)
	}
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "chrome_profile: Profile 6") {
		t.Fatalf("expected config to persist chrome_profile, got %s", string(data))
	}

	var body mcp.ChromeProfileState
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ConfiguredProfile != "Profile 6" || body.EffectiveProfile != "Profile 6" {
		t.Fatalf("unexpected response body: %+v", body)
	}
	if body.CloneStatus != mcp.ChromeProfileCloneMissing {
		t.Fatalf("expected clone status %q, got %q", mcp.ChromeProfileCloneMissing, body.CloneStatus)
	}
}

func TestServer_ChromeProfileUpdateAutoClearsConfigKey(t *testing.T) {
	oldGet := getChromeProfileStateFn
	oldStop := stopChromeFn
	oldReset := resetChromeProfileCloneFn
	oldProfile := mcp.GetCDPChromeProfile()
	defer func() {
		getChromeProfileStateFn = oldGet
		stopChromeFn = oldStop
		resetChromeProfileCloneFn = oldReset
		mcp.SetCDPChromeProfile(oldProfile)
	}()

	getChromeProfileStateFn = func(profile string) (mcp.ChromeProfileState, error) {
		return mcp.ChromeProfileState{
			Mode:             "auto",
			DetectedProfile:  "Profile 6",
			EffectiveProfile: "Profile 6",
			CloneStatus:      mcp.ChromeProfileCloneMissing,
			Profiles: []mcp.ChromeProfileOption{
				{Name: "Profile 6", DisplayName: "Work", Exists: true, IsLastUsed: true, IsEffective: true},
			},
		}, nil
	}
	stopChromeFn = func() {}
	resetChromeProfileCloneFn = func() error { return nil }

	shannonDir := t.TempDir()
	initial := "daemon:\n  auto_approve: true\n  chrome_profile: Profile 6\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	deps := &ServerDeps{
		ShannonDir: shannonDir,
		Config: &config.Config{
			Daemon: config.DaemonConfig{AutoApprove: true, ChromeProfile: "Profile 6"},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chrome/profile", strings.NewReader(`{"mode":"auto"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChromeProfileUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if deps.Config.Daemon.ChromeProfile != "" {
		t.Fatalf("expected in-memory chrome_profile to be cleared, got %q", deps.Config.Daemon.ChromeProfile)
	}
	if mcp.GetCDPChromeProfile() != "" {
		t.Fatalf("expected runtime chrome profile override to be cleared, got %q", mcp.GetCDPChromeProfile())
	}
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "chrome_profile:") {
		t.Fatalf("expected chrome_profile key to be removed, got %s", text)
	}
	if !strings.Contains(text, "auto_approve: true") {
		t.Fatalf("expected sibling daemon setting to remain, got %s", text)
	}

	var body mcp.ChromeProfileState
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Mode != "auto" || body.CloneStatus != mcp.ChromeProfileCloneMissing {
		t.Fatalf("unexpected response body: %+v", body)
	}
}

func TestServer_ChromeProfileUpdateDoesNotPersistWhenResetFails(t *testing.T) {
	oldGet := getChromeProfileStateFn
	oldStop := stopChromeFn
	oldReset := resetChromeProfileCloneFn
	oldProfile := mcp.GetCDPChromeProfile()
	defer func() {
		getChromeProfileStateFn = oldGet
		stopChromeFn = oldStop
		resetChromeProfileCloneFn = oldReset
		mcp.SetCDPChromeProfile(oldProfile)
	}()

	getChromeProfileStateFn = func(profile string) (mcp.ChromeProfileState, error) {
		return mcp.ChromeProfileState{
			Mode:             "auto",
			DetectedProfile:  "Default",
			EffectiveProfile: "Default",
			CloneStatus:      mcp.ChromeProfileCloneCurrent,
			Profiles: []mcp.ChromeProfileOption{
				{Name: "Default", DisplayName: "Default", Exists: true, IsLastUsed: true, IsEffective: true},
				{Name: "Profile 6", DisplayName: "Work", Exists: true},
			},
		}, nil
	}
	stopChromeFn = func() {}
	resetChromeProfileCloneFn = func() error { return errors.New("directory not empty") }

	shannonDir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(shannonDir, 0o700) })
	initial := "daemon:\n  auto_approve: true\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	deps := &ServerDeps{
		ShannonDir: shannonDir,
		Config: &config.Config{
			Daemon: config.DaemonConfig{AutoApprove: true},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chrome/profile", strings.NewReader(`{"mode":"explicit","profile":"Profile 6"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChromeProfileUpdate(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "directory not empty" {
		t.Fatalf("unexpected error body: %v", body)
	}
	if deps.Config.Daemon.ChromeProfile != "" {
		t.Fatalf("expected in-memory chrome_profile to remain unchanged, got %q", deps.Config.Daemon.ChromeProfile)
	}
	if mcp.GetCDPChromeProfile() != oldProfile {
		t.Fatalf("expected runtime chrome profile override to remain unchanged, got %q", mcp.GetCDPChromeProfile())
	}
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "chrome_profile:") {
		t.Fatalf("expected rolled-back config to remove chrome_profile, got %s", text)
	}
	if !strings.Contains(text, "auto_approve: true") {
		t.Fatalf("expected rolled-back config to keep sibling settings, got %s", text)
	}
}

func TestServer_ChromeProfileUpdateRollbackFailureKeepsMemoryAlignedWithDisk(t *testing.T) {
	oldGet := getChromeProfileStateFn
	oldStop := stopChromeFn
	oldReset := resetChromeProfileCloneFn
	oldProfile := mcp.GetCDPChromeProfile()
	defer func() {
		getChromeProfileStateFn = oldGet
		stopChromeFn = oldStop
		resetChromeProfileCloneFn = oldReset
		mcp.SetCDPChromeProfile(oldProfile)
	}()

	getChromeProfileStateFn = func(profile string) (mcp.ChromeProfileState, error) {
		return mcp.ChromeProfileState{
			Mode:             "auto",
			DetectedProfile:  "Default",
			EffectiveProfile: "Default",
			CloneStatus:      mcp.ChromeProfileCloneCurrent,
			Profiles: []mcp.ChromeProfileOption{
				{Name: "Default", DisplayName: "Default", Exists: true, IsLastUsed: true, IsEffective: true},
				{Name: "Profile 6", DisplayName: "Work", Exists: true},
			},
		}, nil
	}
	stopChromeFn = func() {}

	shannonDir := t.TempDir()
	initial := "daemon:\n  auto_approve: true\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	resetChromeProfileCloneFn = func() error {
		if err := os.RemoveAll(shannonDir); err != nil {
			t.Fatalf("remove shannon dir: %v", err)
		}
		return errors.New("directory not empty")
	}

	deps := &ServerDeps{
		ShannonDir: shannonDir,
		Config: &config.Config{
			Daemon: config.DaemonConfig{AutoApprove: true},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chrome/profile", strings.NewReader(`{"mode":"explicit","profile":"Profile 6"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChromeProfileUpdate(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body["error"], "rollback failed") {
		t.Fatalf("expected rollback failure in error body, got %v", body)
	}
	if deps.Config.Daemon.ChromeProfile != "Profile 6" {
		t.Fatalf("expected in-memory chrome_profile to stay aligned with disk, got %q", deps.Config.Daemon.ChromeProfile)
	}
	if mcp.GetCDPChromeProfile() != "Profile 6" {
		t.Fatalf("expected runtime chrome profile override to stay aligned with disk, got %q", mcp.GetCDPChromeProfile())
	}
	if _, err := os.Stat(shannonDir); !os.IsNotExist(err) {
		t.Fatalf("expected rollback failure setup to remove shannon dir, got err=%v", err)
	}
}

func TestServer_ChromeProfileUpdateDoesNotStopWhenConfigWriteFails(t *testing.T) {
	oldGet := getChromeProfileStateFn
	oldStop := stopChromeFn
	oldReset := resetChromeProfileCloneFn
	oldProfile := mcp.GetCDPChromeProfile()
	defer func() {
		getChromeProfileStateFn = oldGet
		stopChromeFn = oldStop
		resetChromeProfileCloneFn = oldReset
		mcp.SetCDPChromeProfile(oldProfile)
	}()

	getChromeProfileStateFn = func(profile string) (mcp.ChromeProfileState, error) {
		return mcp.ChromeProfileState{
			Mode:             "auto",
			DetectedProfile:  "Default",
			EffectiveProfile: "Default",
			CloneStatus:      mcp.ChromeProfileCloneCurrent,
			Profiles: []mcp.ChromeProfileOption{
				{Name: "Default", DisplayName: "Default", Exists: true, IsLastUsed: true, IsEffective: true},
				{Name: "Profile 6", DisplayName: "Work", Exists: true},
			},
		}, nil
	}

	stopCalls := 0
	resetCalls := 0
	stopChromeFn = func() { stopCalls++ }
	resetChromeProfileCloneFn = func() error {
		resetCalls++
		return nil
	}

	mcp.SetCDPChromeProfile("Default")
	deps := &ServerDeps{
		ShannonDir: filepath.Join(t.TempDir(), "missing-config-dir"),
		Config: &config.Config{
			Daemon: config.DaemonConfig{ChromeProfile: "Default"},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chrome/profile", strings.NewReader(`{"mode":"explicit","profile":"Profile 6"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChromeProfileUpdate(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] == "" {
		t.Fatalf("expected non-empty error body, got %v", body)
	}
	if stopCalls != 0 || resetCalls != 0 {
		t.Fatalf("expected no destructive ops when config write fails, got stop=%d reset=%d", stopCalls, resetCalls)
	}
	if deps.Config.Daemon.ChromeProfile != "Default" {
		t.Fatalf("expected in-memory chrome_profile to remain unchanged, got %q", deps.Config.Daemon.ChromeProfile)
	}
	if mcp.GetCDPChromeProfile() != "Default" {
		t.Fatalf("expected runtime chrome profile override to remain unchanged, got %q", mcp.GetCDPChromeProfile())
	}
}

func TestServer_PatchConfigNullRemovesChromeProfileKey(t *testing.T) {
	shannonDir := t.TempDir()
	initial := "daemon:\n  auto_approve: true\n  chrome_profile: Profile 6\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	srv := NewServer(0, nil, &ServerDeps{ShannonDir: shannonDir}, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(`{"daemon":{"chrome_profile":null}}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handlePatchConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "updated" {
		t.Fatalf("unexpected response body: %v", body)
	}
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "chrome_profile:") {
		t.Fatalf("expected chrome_profile key to be removed, got %s", text)
	}
	if !strings.Contains(text, "auto_approve: true") {
		t.Fatalf("expected sibling daemon setting to remain, got %s", text)
	}
}

func TestServer_PatchConfigRejectsTierKeywordAsModel(t *testing.T) {
	shannonDir := t.TempDir()
	initial := "model_tier: medium\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	srv := NewServer(0, nil, &ServerDeps{ShannonDir: shannonDir}, "test")

	// Cased variant must be rejected too (normalized match).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(`{"agent":{"model":"Large"}}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handlePatchConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "specific model id") {
		t.Fatalf("expected actionable message, got %s", rec.Body.String())
	}
	// The bad value must never reach config.yaml.
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "model:") {
		t.Fatalf("tier keyword should not have been written to config, got %s", string(data))
	}
}

// TestE2E_ModelTierKeywordRejectedAcrossWritePaths drives a running daemon over
// real HTTP and verifies the tier-keyword guard holds end-to-end on every
// config write path: named-agent create, named-agent config replace (PUT), and
// global PATCH /config — plus that legitimate values (specific model id on
// agent.model, tier on model_tier) still pass. This is the "enter from outside"
// regression test for the model_id_unknown stuck-agent bug.
func TestE2E_ModelTierKeywordRejectedAcrossWritePaths(t *testing.T) {
	agentsDir := t.TempDir()
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("model_tier: medium\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(t.TempDir()),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	post := func(path, body string) *http.Response {
		resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}
	do := func(method, path, body string) *http.Response {
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request %s %s: %v", method, path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. Create with a tier word in config.agent.model → rejected, no agent dir.
	resp := post("/agents", `{"display_name":"tierbot","prompt":"hi","config":{"agent":{"model":"large"}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with tier model: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if entries, _ := os.ReadDir(agentsDir); len(entries) != 0 {
		t.Fatalf("rejected create must not write an agent dir, found %d entries", len(entries))
	}

	// 2. Create a valid agent (specific model id + tier on model_tier) → 201.
	resp = post("/agents", `{"display_name":"goodbot","prompt":"hi","config":{"agent":{"model":"claude-opus-4-8","model_tier":"large"}}}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("valid create: want 201, got %d, body=%s", resp.StatusCode, b)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	resp.Body.Close()
	if created.Name == "" {
		t.Fatal("create response missing server-generated slug")
	}

	// 3. PUT that agent's config with a (cased) tier word → rejected.
	resp = do(http.MethodPut, "/agents/"+created.Name+"/config", `{"agent":{"model":"Large"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT config with tier model: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Global PATCH /config with a tier word → rejected, not persisted.
	resp = do(http.MethodPatch, "/config", `{"agent":{"model":" medium "}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH config with tier model: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "model:") {
		t.Fatalf("tier keyword leaked into config.yaml: %s", data)
	}

	// 5. Global PATCH /config with a legitimate model id → accepted + persisted.
	resp = do(http.MethodPatch, "/config", `{"agent":{"model":"claude-opus-4-8"}}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("PATCH config with valid model: want 200, got %d, body=%s", resp.StatusCode, b)
	}
	resp.Body.Close()
	data, err = os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "claude-opus-4-8") {
		t.Fatalf("valid model id should have been persisted, got %s", data)
	}
}

// --- Issue 1: rollback on create failure ---

func TestServer_CreateAgent_Conflict(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	body := `{"display_name":"testbot","prompt":"hello world"}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	// Duplicate display_name create — should get 409
	resp2, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("duplicate: expected 409, got %d", resp2.StatusCode)
	}
}

func TestServer_CreateAgent_RollbackOnWriteFailure(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Make agents dir read-only so WriteAgentPrompt's MkdirAll fails
	os.Chmod(agentsDir, 0500)
	defer os.Chmod(agentsDir, 0700) // restore for cleanup

	body := `{"display_name":"failbot","prompt":"should fail"}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	// Restore permissions and verify no orphaned directory
	os.Chmod(agentsDir, 0700)
	if _, err := os.Stat(filepath.Join(agentsDir, "failbot")); !os.IsNotExist(err) {
		t.Error("agent dir should not exist after rollback")
	}
}

func TestServer_CreateAgent_DoesNotCreateSessionManager(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	sessionCache := NewSessionCache(sessDir)
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: sessionCache,
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	body := `{"display_name":"cache-test","prompt":"hello world"}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	routeKey := "agent:" + created.Name
	sessionCache.mu.Lock()
	route, ok := sessionCache.routes[routeKey]
	sessionCache.mu.Unlock()
	if !ok {
		t.Fatalf("expected route cache entry for %s to exist", routeKey)
	}
	if route.manager != nil {
		t.Fatalf("expected create path to avoid creating a route manager")
	}
}

func TestServer_DeleteAgent_RemovesProfileYAML(t *testing.T) {
	agentsDir := t.TempDir()
	agentDir := filepath.Join(agentsDir, "profiled")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "PROFILE.yaml"), []byte("category: coding\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Runtime state should survive definition deletion and keeps the directory
	// around, making a stale PROFILE.yaml easy to detect.
	if err := os.WriteFile(filepath.Join(agentDir, "MEMORY.md"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(t.TempDir()),
	}
	srv := NewServer(0, nil, deps, "test")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/agents/profiled?confirm=true", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /agents/profiled = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(agentDir, "PROFILE.yaml")); !os.IsNotExist(err) {
		t.Fatalf("PROFILE.yaml should be removed, stat err=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(agentDir, "MEMORY.md")); err != nil || string(data) != "keep me" {
		t.Fatalf("MEMORY.md should be preserved, data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "AGENT.md")); !os.IsNotExist(err) {
		t.Fatalf("AGENT.md should be removed, stat err=%v", err)
	}
}

func TestServer_CreateAgent_AttachesInstalledSkills(t *testing.T) {
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := os.MkdirAll(agentsDir, 0700); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	sessDir := t.TempDir()
	writeTestGlobalSkill(t, shannonDir, "check")
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	body := `{"display_name":"attach-bot","prompt":"hello world","skills":[{"name":"check"}]}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	slug := created.Name

	loaded, err := agents.LoadAgent(agentsDir, slug)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0].Name != "check" {
		t.Fatalf("expected attached global skill 'check', got %+v", loaded.Skills)
	}

	attached, err := agents.ReadAttachedSkills(agentsDir, slug)
	if err != nil {
		t.Fatalf("read attached skills: %v", err)
	}
	if len(attached) != 1 || attached[0] != "check" {
		t.Fatalf("expected manifest to contain check, got %v", attached)
	}

	if _, err := os.Stat(filepath.Join(agentsDir, slug, "skills")); !os.IsNotExist(err) {
		t.Fatalf("expected no agent-local skill directory, got err=%v", err)
	}
}

func TestServer_PutSkill_AttachesInstalledGlobalSkill(t *testing.T) {
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := os.MkdirAll(agentsDir, 0700); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	sessDir := t.TempDir()
	writeTestGlobalSkill(t, shannonDir, "check")
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	createBody := `{"display_name":"skill-bot","prompt":"hello world"}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(createBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	slug := created.Name

	req, err := http.NewRequest(
		http.MethodPut,
		fmt.Sprintf("http://127.0.0.1:%d/agents/%s/skills/check", srv.Port(), slug),
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach: expected 200, got %d", resp.StatusCode)
	}

	loaded, err := agents.LoadAgent(agentsDir, slug)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0].Name != "check" {
		t.Fatalf("expected attached global skill 'check', got %+v", loaded.Skills)
	}
}

func TestServer_DeleteSkill_DetachesManifestAndCleansLegacySkillDir(t *testing.T) {
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := os.MkdirAll(agentsDir, 0700); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	sessDir := t.TempDir()
	writeTestGlobalSkill(t, shannonDir, "check")
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	createBody := `{"display_name":"detach-bot","prompt":"hello world","skills":[{"name":"check"}]}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(createBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	slug := created.Name

	if err := agents.WriteAgentSkill(agentsDir, slug, &skills.Skill{
		Name:        "check",
		Description: "legacy local copy",
		Prompt:      "legacy prompt",
	}); err != nil {
		t.Fatalf("write legacy agent-local skill: %v", err)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		fmt.Sprintf("http://127.0.0.1:%d/agents/%s/skills/check", srv.Port(), slug),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	attached, err := agents.ReadAttachedSkills(agentsDir, slug)
	if err != nil {
		t.Fatalf("read attached skills: %v", err)
	}
	if len(attached) != 0 {
		t.Fatalf("expected empty attached skills after delete, got %v", attached)
	}

	loaded, err := agents.LoadAgent(agentsDir, slug)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if len(loaded.Skills) != 0 {
		t.Fatalf("expected no loaded skills after detach, got %+v", loaded.Skills)
	}

	if _, err := os.Stat(filepath.Join(agentsDir, slug, "skills", "check")); !os.IsNotExist(err) {
		t.Fatalf("expected legacy agent-local skill dir to be removed, got err=%v", err)
	}
}

// --- deepMerge unit tests ---

func TestDeepMerge(t *testing.T) {
	tests := []struct {
		name     string
		dst, src map[string]interface{}
		want     map[string]interface{}
	}{
		{
			name: "scalar replace",
			dst:  map[string]interface{}{"a": "old"},
			src:  map[string]interface{}{"a": "new"},
			want: map[string]interface{}{"a": "new"},
		},
		{
			name: "null deletes key",
			dst:  map[string]interface{}{"a": "val", "b": "keep"},
			src:  map[string]interface{}{"a": nil},
			want: map[string]interface{}{"b": "keep"},
		},
		{
			name: "nested merge preserves siblings",
			dst: map[string]interface{}{
				"agent": map[string]interface{}{"model": "old", "temp": 0.7},
			},
			src: map[string]interface{}{
				"agent": map[string]interface{}{"model": "new"},
			},
			want: map[string]interface{}{
				"agent": map[string]interface{}{"model": "new", "temp": 0.7},
			},
		},
		{
			name: "3-level deep merge",
			dst: map[string]interface{}{
				"a": map[string]interface{}{
					"b": map[string]interface{}{"c": 1, "d": 2},
				},
			},
			src: map[string]interface{}{
				"a": map[string]interface{}{
					"b": map[string]interface{}{"c": 99},
				},
			},
			want: map[string]interface{}{
				"a": map[string]interface{}{
					"b": map[string]interface{}{"c": 99, "d": 2},
				},
			},
		},
		{
			name: "src map replaces dst scalar",
			dst:  map[string]interface{}{"a": "scalar"},
			src:  map[string]interface{}{"a": map[string]interface{}{"nested": true}},
			want: map[string]interface{}{"a": map[string]interface{}{"nested": true}},
		},
		{
			name: "src scalar replaces dst map",
			dst:  map[string]interface{}{"a": map[string]interface{}{"nested": true}},
			src:  map[string]interface{}{"a": "scalar"},
			want: map[string]interface{}{"a": "scalar"},
		},
		{
			name: "new key added",
			dst:  map[string]interface{}{"a": 1},
			src:  map[string]interface{}{"b": 2},
			want: map[string]interface{}{"a": 1, "b": 2},
		},
		{
			name: "empty src is no-op",
			dst:  map[string]interface{}{"a": 1},
			src:  map[string]interface{}{},
			want: map[string]interface{}{"a": 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deepMerge(tc.dst, tc.src)
			gotJSON, _ := json.Marshal(tc.dst)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("got %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// --- Issue 2: PATCH config deep merge ---

func TestServer_PatchConfig_DeepMerge(t *testing.T) {
	shannonDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(sessDir),
		Config:       &config.Config{},
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// Step 1: Set initial config with nested agent block
	initial := map[string]interface{}{
		"agent": map[string]interface{}{
			"model":          "claude-3-5-sonnet",
			"max_iterations": 10,
			"temperature":    0.7,
		},
		"top_level_key": "keep_me",
	}
	initialYAML, _ := yaml.Marshal(initial)
	os.WriteFile(filepath.Join(shannonDir, "config.yaml"), initialYAML, 0600)

	// Step 2: PATCH only agent.model — should preserve max_iterations and temperature
	patch := `{"agent": {"model": "claude-4-opus"}}`
	req, _ := http.NewRequest("PATCH", base+"/config", strings.NewReader(patch))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: expected 200, got %d", resp.StatusCode)
	}

	// Step 3: Read config back and verify deep merge
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := yaml.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	agentBlock, ok := result["agent"].(map[string]interface{})
	if !ok {
		t.Fatalf("agent block not a map: %T", result["agent"])
	}

	// model should be updated
	if agentBlock["model"] != "claude-4-opus" {
		t.Errorf("model = %v, want claude-4-opus", agentBlock["model"])
	}

	// max_iterations and temperature should be preserved (deep merge)
	if agentBlock["max_iterations"] == nil {
		t.Error("max_iterations was lost during PATCH — shallow merge instead of deep merge")
	}
	if agentBlock["temperature"] == nil {
		t.Error("temperature was lost during PATCH — shallow merge instead of deep merge")
	}

	// top_level_key should still be there
	if result["top_level_key"] != "keep_me" {
		t.Errorf("top_level_key = %v, want keep_me", result["top_level_key"])
	}
}

func TestServer_PatchConfig_NullDeletes(t *testing.T) {
	shannonDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(sessDir),
		Config:       &config.Config{},
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// Set initial config
	initial := map[string]interface{}{
		"agent":     map[string]interface{}{"model": "gpt-4"},
		"to_delete": "bye",
	}
	initialYAML, _ := yaml.Marshal(initial)
	os.WriteFile(filepath.Join(shannonDir, "config.yaml"), initialYAML, 0600)

	// PATCH with null to delete a key
	patch := `{"to_delete": null}`
	req, _ := http.NewRequest("PATCH", base+"/config", strings.NewReader(patch))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: expected 200, got %d", resp.StatusCode)
	}

	data, _ := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	var result map[string]interface{}
	yaml.Unmarshal(data, &result)

	if _, exists := result["to_delete"]; exists {
		t.Error("to_delete should have been removed by null patch")
	}
	if result["agent"] == nil {
		t.Error("agent block should still exist")
	}
}

// --- Issue 3: request body size limit ---

func TestServer_BodySizeLimit(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
		Config:       &config.Config{},
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// Send a body exceeding maxBodySize (50MB) to POST /agents — should be rejected
	bigBody := bytes.Repeat([]byte("x"), 51*1024*1024)
	payload := append([]byte(`{"name":"big","prompt":"`), bigBody...)
	payload = append(payload, '"', '}')

	resp, err := http.Post(base+"/agents", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should get 413 or 400 (body too large), not 201
	if resp.StatusCode == http.StatusCreated {
		t.Error("expected rejection for oversized body, got 201 Created")
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Logf("status = %d (acceptable if 400, ideal is 413)", resp.StatusCode)
	}
}

func TestEventsSSEEndpoint(t *testing.T) {
	bus := NewEventBus()
	s := &Server{eventBus: bus}

	handler := http.HandlerFunc(s.handleEvents)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	// Wait for SSE handler to subscribe before emitting
	time.Sleep(50 * time.Millisecond)

	bus.Emit(Event{
		Type:    EventAgentReply,
		Payload: json.RawMessage(`{"agent":"test","text":"hello"}`),
	})

	scanner := bufio.NewScanner(resp.Body)
	var eventLine, dataLine string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventLine = line
		}
		if strings.HasPrefix(line, "data:") {
			dataLine = line
			break
		}
	}

	if eventLine != "event: agent_reply" {
		t.Fatalf("expected 'event: agent_reply', got %q", eventLine)
	}
	if !strings.Contains(dataLine, `"agent":"test"`) {
		t.Fatalf("expected agent in data, got %q", dataLine)
	}
}

// SSE endpoint must replay missed events when last_event_id is provided,
// then switch to live events. This is the core of Desktop reconnection.
func TestEventsSSEReplay(t *testing.T) {
	bus := NewEventBus()
	s := &Server{eventBus: bus}

	// Pre-emit 5 events into ring buffer (IDs 1..5) before any client connects.
	for i := 0; i < 5; i++ {
		bus.Emit(Event{Type: "test", Payload: json.RawMessage(`{"seq":` + strconv.Itoa(i+1) + `}`)})
	}

	handler := http.HandlerFunc(s.handleEvents)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Connect with last_event_id=3 → expect replay of IDs 4, 5
	resp, err := http.Get(srv.URL + "?last_event_id=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var replayed []uint64
	deadline := time.After(2 * time.Second)

	for len(replayed) < 2 {
		lineCh := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
			}
		}()
		select {
		case line := <-lineCh:
			if strings.HasPrefix(line, "id: ") {
				id, _ := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
				replayed = append(replayed, id)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for replayed events, got %d so far: %v", len(replayed), replayed)
		}
	}

	if replayed[0] != 4 || replayed[1] != 5 {
		t.Fatalf("expected replayed IDs [4, 5], got %v", replayed)
	}
}

// SSE endpoint must also support the standard Last-Event-ID header
// (used by browser EventSource on reconnect).
func TestEventsSSEReplayViaHeader(t *testing.T) {
	bus := NewEventBus()
	s := &Server{eventBus: bus}

	for i := 0; i < 5; i++ {
		bus.Emit(Event{Type: "test", Payload: json.RawMessage(`{"seq":` + strconv.Itoa(i+1) + `}`)})
	}

	handler := http.HandlerFunc(s.handleEvents)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Use Last-Event-ID header instead of query param
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Last-Event-ID", "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var replayed []uint64
	deadline := time.After(2 * time.Second)

	for len(replayed) < 2 {
		lineCh := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
			}
		}()
		select {
		case line := <-lineCh:
			if strings.HasPrefix(line, "id: ") {
				id, _ := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
				replayed = append(replayed, id)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for replayed events via header, got %d so far: %v", len(replayed), replayed)
		}
	}

	if replayed[0] != 4 || replayed[1] != 5 {
		t.Fatalf("expected replayed IDs [4, 5], got %v", replayed)
	}
}

// SSE endpoint without last_event_id must behave identically to before
// (backward compatible — no replay, live events only).
func TestEventsSSENoReplayWithoutParam(t *testing.T) {
	bus := NewEventBus()
	s := &Server{eventBus: bus}

	// Pre-emit events
	for i := 0; i < 3; i++ {
		bus.Emit(Event{Type: "old", Payload: json.RawMessage(`{}`)})
	}

	handler := http.HandlerFunc(s.handleEvents)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL) // no last_event_id
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Wait for handler to subscribe
	time.Sleep(50 * time.Millisecond)

	// Emit a live event
	bus.Emit(Event{Type: "live", Payload: json.RawMessage(`{"new":true}`)})

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(2 * time.Second)
	var firstEventType string

	for firstEventType == "" {
		lineCh := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
			}
		}()
		select {
		case line := <-lineCh:
			if strings.HasPrefix(line, "event: ") {
				firstEventType = strings.TrimPrefix(line, "event: ")
			}
		case <-deadline:
			t.Fatal("timeout waiting for live event")
		}
	}

	// Must receive the live event, not the old pre-emitted ones
	if firstEventType != "live" {
		t.Fatalf("expected first event type 'live', got %q (old events leaked without last_event_id)", firstEventType)
	}
}

func TestNormalizePatchKeys(t *testing.T) {
	tests := []struct {
		name       string
		input      map[string]interface{}
		want       map[string]interface{}
		applyTwice bool // set to verify idempotency
	}{
		{
			name:  "camelCase mcpServers renamed",
			input: map[string]interface{}{"mcpServers": map[string]interface{}{"x-twitter": map[string]interface{}{}}},
			want:  map[string]interface{}{"mcp_servers": map[string]interface{}{"x-twitter": map[string]interface{}{}}},
		},
		{
			name:  "PascalCase MCPServers renamed",
			input: map[string]interface{}{"MCPServers": map[string]interface{}{}},
			want:  map[string]interface{}{"mcp_servers": map[string]interface{}{}},
		},
		{
			name:  "apiKey renamed",
			input: map[string]interface{}{"apiKey": "sk_abc"},
			want:  map[string]interface{}{"api_key": "sk_abc"},
		},
		{
			name:  "canonical snake_case unchanged",
			input: map[string]interface{}{"mcp_servers": map[string]interface{}{}, "api_key": "sk_abc"},
			want:  map[string]interface{}{"mcp_servers": map[string]interface{}{}, "api_key": "sk_abc"},
		},
		{
			name:       "idempotent: applying twice gives same result",
			input:      map[string]interface{}{"mcpServers": map[string]interface{}{"s": map[string]interface{}{}}},
			want:       map[string]interface{}{"mcp_servers": map[string]interface{}{"s": map[string]interface{}{}}},
			applyTwice: true,
		},
		{
			name:  "alias + canonical both present: canonical wins, alias discarded",
			input: map[string]interface{}{"mcpServers": map[string]interface{}{"alias": map[string]interface{}{}}, "mcp_servers": map[string]interface{}{"canonical": map[string]interface{}{}}},
			want:  map[string]interface{}{"mcp_servers": map[string]interface{}{"canonical": map[string]interface{}{}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalizePatchKeys(tt.input)
			if tt.applyTwice {
				normalizePatchKeys(tt.input)
			}
			if len(tt.input) != len(tt.want) {
				t.Fatalf("key count mismatch: got %v, want %v", tt.input, tt.want)
			}
			for k := range tt.want {
				if _, ok := tt.input[k]; !ok {
					t.Errorf("missing expected key %q in result %v", k, tt.input)
				}
			}
			for k := range tt.input {
				if _, ok := tt.want[k]; !ok {
					t.Errorf("unexpected key %q in result %v", k, tt.input)
				}
			}
		})
	}
}

func TestStripRedactedSecrets(t *testing.T) {
	tests := []struct {
		name           string
		input          map[string]interface{}
		wantDeleted    []string // top-level keys that should be absent
		wantKept       []string // top-level keys that should still be present
		wantEnvDeleted []string // mcp_servers.x-twitter.env keys that should be absent
		wantEnvKept    []string // mcp_servers.x-twitter.env keys that should still be present
	}{
		{
			name:        "api_key *** is dropped",
			input:       map[string]interface{}{"api_key": "***"},
			wantDeleted: []string{"api_key"},
		},
		{
			name:     "api_key real value is kept",
			input:    map[string]interface{}{"api_key": "sk_real"},
			wantKept: []string{"api_key"},
		},
		{
			name: "mcp env *** dropped, real kept",
			input: map[string]interface{}{
				"mcp_servers": map[string]interface{}{
					"x-twitter": map[string]interface{}{
						"env": map[string]interface{}{
							"ACCESS_TOKEN":  "***",
							"ACCESS_TOKEN2": "realvalue",
						},
					},
				},
			},
			wantEnvDeleted: []string{"ACCESS_TOKEN"},
			wantEnvKept:    []string{"ACCESS_TOKEN2"},
		},
		{
			name:     "literal *** in non-sensitive field is kept",
			input:    map[string]interface{}{"model_tier": "***"},
			wantKept: []string{"model_tier"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stripRedactedSecrets(tt.input)
			for _, k := range tt.wantDeleted {
				if _, ok := tt.input[k]; ok {
					t.Errorf("expected key %q to be deleted, still present", k)
				}
			}
			for _, k := range tt.wantKept {
				if _, ok := tt.input[k]; !ok {
					t.Errorf("expected key %q to be kept, was deleted", k)
				}
			}
			if len(tt.wantEnvDeleted) > 0 || len(tt.wantEnvKept) > 0 {
				servers := tt.input["mcp_servers"].(map[string]interface{})
				env := servers["x-twitter"].(map[string]interface{})["env"].(map[string]interface{})
				for _, k := range tt.wantEnvDeleted {
					if _, ok := env[k]; ok {
						t.Errorf("expected env key %q to be dropped, still present", k)
					}
				}
				for _, k := range tt.wantEnvKept {
					if _, ok := env[k]; !ok {
						t.Errorf("expected env key %q to be kept, was deleted", k)
					}
				}
			}
		})
	}
}

func TestServer_EditMessage_Validation(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(shannonDir),
	}
	srv := &Server{deps: deps}

	tests := []struct {
		name       string
		sessionID  string
		body       string
		wantStatus int
		wantErr    string
	}{
		{
			name:       "empty new_content and no content blocks",
			sessionID:  "test-session",
			body:       `{"message_index":0,"new_content":""}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "new_content or content is required",
		},
		{
			name:       "empty new_content with content blocks passes validation",
			sessionID:  "nonexistent",
			body:       `{"message_index":0,"new_content":"","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}`,
			// Validation passes; the load then 404s because the session is
			// missing (Store.Load now returns os.ErrNotExist unwrapped, so
			// handleEditMessage maps it to 404 like the sibling handlers).
			wantStatus: http.StatusNotFound,
			wantErr:    "not found",
		},
		{
			name:       "valid new_content only passes validation",
			sessionID:  "nonexistent",
			body:       `{"message_index":0,"new_content":"hello"}`,
			wantStatus: http.StatusNotFound,
			wantErr:    "not found",
		},
		{
			name:       "valid new_content with content blocks passes validation",
			sessionID:  "nonexistent",
			body:       `{"message_index":0,"new_content":"analyze this","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}`,
			wantStatus: http.StatusNotFound,
			wantErr:    "not found",
		},
		{
			name:       "missing session id",
			sessionID:  "",
			body:       `{"message_index":0,"new_content":"hello"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "session id required",
		},
		{
			name:       "invalid agent name",
			sessionID:  "test-session",
			body:       `{"message_index":0,"new_content":"hello","agent":"../evil"}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sessions/"+tc.sessionID+"/edit", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", tc.sessionID)

			srv.handleEditMessage(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantErr != "" && !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Errorf("body = %q, want substring %q", rec.Body.String(), tc.wantErr)
			}
		})
	}
}

func TestServer_PatchSession(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{
		ShannonDir:   shannonDir,
		SessionCache: NewSessionCache(shannonDir),
	}
	srv := &Server{deps: deps}

	// Seed a default-agent session via the live manager so handler reads
	// hit the same on-disk + SQLite index state.
	mgr := deps.SessionCache.GetOrCreate("")
	sess := mgr.NewSession()
	sess.Title = "original"
	if err := mgr.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	sessID := sess.ID

	patch := func(t *testing.T, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/sessions/"+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", id)
		srv.handlePatchSession(rec, req)
		return rec
	}

	reload := func(t *testing.T) *session.Session {
		t.Helper()
		s, err := mgr.Resume(sessID)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		return s
	}

	t.Run("title only (back-compat)", func(t *testing.T) {
		rec := patch(t, sessID, `{"title":"renamed"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["title"] != "renamed" {
			t.Errorf("expected echoed title, got %v", resp)
		}
		if _, ok := resp["pinned"]; ok {
			t.Errorf("pinned should be absent when not patched, got %v", resp)
		}
		if reload(t).Title != "renamed" {
			t.Error("title not persisted")
		}
	})

	t.Run("pinned only", func(t *testing.T) {
		rec := patch(t, sessID, `{"pinned":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["pinned"] != true {
			t.Errorf("expected pinned=true in response, got %v", resp)
		}
		s := reload(t)
		if !s.Pinned {
			t.Error("Pinned not persisted")
		}
		if s.Title != "renamed" {
			t.Errorf("title should be unchanged, got %q", s.Title)
		}
	})

	t.Run("favorite only", func(t *testing.T) {
		rec := patch(t, sessID, `{"favorite":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		s := reload(t)
		if !s.Favorite || !s.Pinned {
			t.Errorf("expected both flags true, got pinned=%v favorite=%v", s.Pinned, s.Favorite)
		}
	})

	t.Run("all fields at once", func(t *testing.T) {
		rec := patch(t, sessID, `{"title":"final","pinned":false,"favorite":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		s := reload(t)
		if s.Title != "final" || s.Pinned || s.Favorite {
			t.Errorf("got title=%q pinned=%v favorite=%v", s.Title, s.Pinned, s.Favorite)
		}
	})

	t.Run("empty body rejected", func(t *testing.T) {
		rec := patch(t, sessID, `{}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "at least one of") {
			t.Errorf("expected explanatory error, got %s", rec.Body.String())
		}
	})

	t.Run("empty title rejected", func(t *testing.T) {
		rec := patch(t, sessID, `{"title":"   "}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("unknown session returns 404", func(t *testing.T) {
		rec := patch(t, "no-such-session", `{"pinned":true}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		rec := patch(t, "../evil", `{"pinned":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})
}

func TestRunSyncLoop_StaysAliveWhenInitiallyDisabled(t *testing.T) {
	// Regression: prior to the post-PR-78 fix, the goroutine returned early
	// if sync.enabled was false at startup, so flipping enabled=true via
	// config edit did nothing until daemon restart. The fix keeps the
	// goroutine alive and re-checks Enabled per tick.

	viper.Reset()
	defer viper.Reset()

	// Sync disabled at startup but with a valid (short) interval so the
	// ticker actually fires while the test is watching.
	viper.Set("sync.enabled", false)
	viper.Set("sync.daemon_interval", "100ms")
	viper.Set("sync.daemon_startup_delay", "0")

	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, nil, "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		srv.runSyncLoop(ctx)
		close(done)
	}()

	// Wait through several ticker periods. Goroutine MUST still be running.
	// (Pre-fix: it returned within microseconds because !Enabled at startup.)
	select {
	case <-done:
		t.Fatalf("runSyncLoop returned early while sync.enabled=false — should stay alive for hot-enable")
	case <-time.After(500 * time.Millisecond):
		// Good: goroutine is still in its tick loop.
	}

	// Cancel ctx and confirm goroutine exits promptly.
	cancel()
	select {
	case <-done:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatalf("runSyncLoop did not exit within 2s of ctx cancel")
	}
}

func TestRunSyncLoop_ReturnsImmediatelyOnZeroInterval(t *testing.T) {
	// The only legitimate early-return path: misconfigured DaemonInterval <= 0.
	viper.Reset()
	defer viper.Reset()

	viper.Set("sync.enabled", true)
	viper.Set("sync.daemon_interval", "0s") // misconfigured

	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, nil, "test")

	done := make(chan struct{})
	go func() {
		srv.runSyncLoop(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// Good: returned immediately on misconfig.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("runSyncLoop should return immediately when DaemonInterval <= 0")
	}
}

func TestServer_PatchConfigSetsDaemonAutoApprove(t *testing.T) {
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("daemon:\n  auto_approve: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	srv := NewServer(0, nil, &ServerDeps{ShannonDir: shannonDir}, "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(`{"daemon":{"auto_approve":true}}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handlePatchConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "updated" {
		t.Fatalf("unexpected response body: %v", body)
	}

	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "auto_approve: true") {
		t.Fatalf("expected auto_approve: true in persisted config, got %s", data)
	}
}

// --- Upload skill helpers ---

func makeUploadZip(t *testing.T, name, description, prompt string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("SKILL.md")
	fmt.Fprintf(f, "---\nname: %s\ndescription: %s\n---\n\n%s\n", name, description, prompt)
	zw.Close()
	return buf.Bytes()
}

func buildUploadRequest(t *testing.T, url string, zipData []byte) *http.Request {
	t.Helper()
	boundary := "testboundary"
	var body bytes.Buffer
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"skill.zip\"\r\n")
	body.WriteString("Content-Type: application/zip\r\n\r\n")
	body.Write(zipData)
	body.WriteString("\r\n--" + boundary + "--\r\n")
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	return req
}

func TestServer_UploadSkill_Success(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	zipBody := makeUploadZip(t, "upload-skill", "does uploads", "Upload things.")
	req := buildUploadRequest(t, fmt.Sprintf("http://127.0.0.1:%d/skills/upload", srv.Port()), zipBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, body)
	}
	var meta struct {
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&meta)
	if meta.Name != "upload-skill" {
		t.Errorf("name = %q", meta.Name)
	}
}

func TestServer_UploadSkill_Conflict(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	zipBody := makeUploadZip(t, "upload-skill", "does uploads", "Upload things.")
	req := buildUploadRequest(t, fmt.Sprintf("http://127.0.0.1:%d/skills/upload", srv.Port()), zipBody)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	req2 := buildUploadRequest(t, fmt.Sprintf("http://127.0.0.1:%d/skills/upload", srv.Port()), zipBody)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp2.StatusCode)
	}
	var respBody map[string]string
	json.NewDecoder(resp2.Body).Decode(&respBody)
	if respBody["error"] != "skill_already_exists" {
		t.Errorf("error = %q", respBody["error"])
	}
	if respBody["existing_name"] != "upload-skill" {
		t.Errorf("existing_name = %q", respBody["existing_name"])
	}
}

func TestServer_UploadSkill_ForceOverwrite(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	zip1 := makeUploadZip(t, "upload-skill", "v1 desc", "v1.")
	req := buildUploadRequest(t, fmt.Sprintf("http://127.0.0.1:%d/skills/upload", srv.Port()), zip1)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	zip2 := makeUploadZip(t, "upload-skill", "v2 desc", "v2.")
	forceURL := fmt.Sprintf("http://127.0.0.1:%d/skills/upload?force=true", srv.Port())
	req2 := buildUploadRequest(t, forceURL, zip2)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("want 201, got %d: %s", resp2.StatusCode, body)
	}
}

// TestServer_PutGlobalSkill_Conflict verifies the same conflict gate that
// /skills/upload uses also applies to manual create via PUT /skills/{name}.
// Response body shape must match /skills/upload so the Desktop client can
// reuse SkillUploadConflict + SkillUploadCompareSheet.
func TestServer_PutGlobalSkill_Conflict(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d/skills/manual-create", srv.Port())
	body1 := `{"description":"v1 desc","prompt":"v1 prompt"}`
	req, _ := http.NewRequest("PUT", url, strings.NewReader(body1))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first PUT want 200, got %d", resp.StatusCode)
	}

	body2 := `{"description":"v2 desc","prompt":"v2 prompt"}`
	req2, _ := http.NewRequest("PUT", url, strings.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second PUT want 409, got %d", resp2.StatusCode)
	}

	var respBody map[string]string
	json.NewDecoder(resp2.Body).Decode(&respBody)
	if respBody["error"] != "skill_already_exists" {
		t.Errorf("error = %q, want skill_already_exists", respBody["error"])
	}
	if respBody["existing_name"] != "manual-create" {
		t.Errorf("existing_name = %q, want manual-create", respBody["existing_name"])
	}
	if respBody["existing_description"] != "v1 desc" {
		t.Errorf("existing_description = %q, want v1 desc", respBody["existing_description"])
	}
	if respBody["existing_prompt"] != "v1 prompt" {
		t.Errorf("existing_prompt = %q, want v1 prompt", respBody["existing_prompt"])
	}
	if respBody["new_description"] != "v2 desc" {
		t.Errorf("new_description = %q, want v2 desc", respBody["new_description"])
	}
	if respBody["new_prompt"] != "v2 prompt" {
		t.Errorf("new_prompt = %q, want v2 prompt", respBody["new_prompt"])
	}
}

func TestServer_PutGlobalSkill_ForceOverwrite(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d/skills/manual-create", srv.Port())
	body1 := `{"description":"v1 desc","prompt":"v1 prompt"}`
	req, _ := http.NewRequest("PUT", url, strings.NewReader(body1))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("seed PUT want 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	forceURL := url + "?force=true"
	body2 := `{"description":"v2 desc","prompt":"v2 prompt"}`
	req2, _ := http.NewRequest("PUT", forceURL, strings.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("force PUT want 200, got %d: %s", resp2.StatusCode, b)
	}
}

// TestServer_PutGlobalSkill_Builtin verifies that PUT cannot replace a builtin
// (kocoro / kocoro-generative-ui) even with force=true — EnsureBuiltinSkills
// would wipe any override on the next daemon restart, and during the running
// session a defaced kocoro misleads the AI about the daemon's HTTP surface.
// Mirrors TestServer_UploadSkill_Builtin which enforces the same guard on
// /skills/upload.
func TestServer_PutGlobalSkill_Builtin(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	for _, slug := range []string{"kocoro", "kocoro-generative-ui"} {
		for _, qs := range []string{"", "?force=true"} {
			url := fmt.Sprintf("http://127.0.0.1:%d/skills/%s%s", srv.Port(), slug, qs)
			body := `{"description":"hijack","prompt":"# hijack"}`
			req, _ := http.NewRequest("PUT", url, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", slug, qs, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("PUT %s%s: got %d %s, want 403", slug, qs, resp.StatusCode, b)
			}
			if !strings.Contains(string(b), "skill_is_builtin") {
				t.Errorf("PUT %s%s: body = %s, want skill_is_builtin", slug, qs, b)
			}
		}
	}
}

// TestServer_PutGlobalSkill_MalformedExisting verifies the boundary where a
// SKILL.md exists on disk but its frontmatter fails to parse — LoadSkills
// silently skips it (fail-open), so without an explicit guard force=true would
// proceed and clobber the malformed file's AllowedTools/Metadata with zero
// values. The handler must instead refuse with 422 so the user can fix or
// delete the file first.
func TestServer_PutGlobalSkill_MalformedExisting(t *testing.T) {
	shannonDir := t.TempDir()
	// Seed a malformed SKILL.md — no frontmatter at all, so loadSkillMD errors
	// and LoadSkills logs+skips it.
	globalDir := filepath.Join(shannonDir, "skills", "broken")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "SKILL.md"), []byte("no frontmatter here"), 0o600); err != nil {
		t.Fatal(err)
	}

	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	for _, qs := range []string{"", "?force=true"} {
		url := fmt.Sprintf("http://127.0.0.1:%d/skills/broken%s", srv.Port(), qs)
		body := `{"description":"fix","prompt":"# fix"}`
		req, _ := http.NewRequest("PUT", url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", qs, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("PUT broken%s: got %d %s, want 422", qs, resp.StatusCode, b)
		}
		if !strings.Contains(string(b), "malformed") {
			t.Errorf("PUT broken%s: body = %s, want 'malformed' hint", qs, b)
		}
	}
}

// TestServer_PutGlobalSkill_ConflictPromptTruncated verifies the 409 body caps
// existing_prompt and new_prompt at skills.ConflictPromptPreviewBytes — without
// the cap, an oversized existing prompt + oversized new prompt could produce a
// ~100 MB JSON response (request body limit is 50 MB). Shares the cap with the
// /skills/upload path via skills.TruncatePromptPreview.
func TestServer_PutGlobalSkill_ConflictPromptTruncated(t *testing.T) {
	shannonDir := t.TempDir()
	bigPrompt := "# header\n\n" + strings.Repeat("A", skills.ConflictPromptPreviewBytes*2)
	if err := skills.WriteGlobalSkill(shannonDir, &skills.Skill{
		Name:        "policy",
		Description: "policy description",
		Prompt:      bigPrompt,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d/skills/policy", srv.Port())
	newPrompt := "# new\n\n" + strings.Repeat("B", skills.ConflictPromptPreviewBytes*2)
	payload, _ := json.Marshal(map[string]string{"description": "new desc", "prompt": newPrompt})
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(body["existing_prompt"]); got > skills.ConflictPromptPreviewBytes {
		t.Errorf("existing_prompt len = %d, want <= %d", got, skills.ConflictPromptPreviewBytes)
	}
	if got := len(body["new_prompt"]); got > skills.ConflictPromptPreviewBytes {
		t.Errorf("new_prompt len = %d, want <= %d", got, skills.ConflictPromptPreviewBytes)
	}
	// Sanity: truncated previews must not be empty, otherwise the compare
	// sheet has nothing to render.
	if body["existing_prompt"] == "" || body["new_prompt"] == "" {
		t.Errorf("truncated prompts should not be empty: existing=%q new=%q",
			body["existing_prompt"], body["new_prompt"])
	}
	if !strings.Contains(body["existing_prompt"], "[truncated]") {
		t.Errorf("existing_prompt missing [truncated] marker: %q", body["existing_prompt"])
	}
	if !strings.Contains(body["new_prompt"], "[truncated]") {
		t.Errorf("new_prompt missing [truncated] marker: %q", body["new_prompt"])
	}
}

func TestServer_UploadSkill_Builtin(t *testing.T) {
	shannonDir := t.TempDir()
	// Mirror the real on-disk layout: EnsureBuiltinSkills syncs builtins into
	// skills/<slug>/. The handler must still reject the upload regardless.
	globalDir := filepath.Join(shannonDir, "skills", "kocoro")
	os.MkdirAll(globalDir, 0700)
	os.WriteFile(filepath.Join(globalDir, "SKILL.md"),
		[]byte("---\nname: kocoro\ndescription: builtin\n---\n\nBuiltin.\n"), 0600)

	deps := &ServerDeps{ShannonDir: shannonDir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	zipBody := makeUploadZip(t, "kocoro", "replacement", "Replace.")
	// Without force.
	req := buildUploadRequest(t, fmt.Sprintf("http://127.0.0.1:%d/skills/upload", srv.Port()), zipBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}

	// With force=true the guard must still hold.
	forceURL := fmt.Sprintf("http://127.0.0.1:%d/skills/upload?force=true", srv.Port())
	req2 := buildUploadRequest(t, forceURL, zipBody)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("force=true: want 403, got %d", resp2.StatusCode)
	}
}

// TestHandleAddGlobalAlwaysAllow covers the PR 6 HTTP endpoint at the edge:
// request validation, non-persistable rejection (400), and in-memory mirror
// sync. The disk-write path is exercised by config package tests independently.
//
// 2026-05-18 update: publish_to_web / generate_image / edit_image used to be
// on the non-persistable deny-list. The list is now empty, so they accept
// 200 like any other tool. The 400 case is still asserted with a synthetic
// tool name guard so the endpoint's validation surface stays covered when a
// future tool re-occupies the deny-list slot.
func TestHandleAddGlobalAlwaysAllow(t *testing.T) {
	shannonDir := t.TempDir()
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}, "test")

	// (a) success: writes to disk, mirrors in-memory.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/permissions/always-allow",
		strings.NewReader(`{"tool":"bash"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleAddGlobalAlwaysAllow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, tool := range srv.deps.Config.Permissions.AlwaysAllowTools {
		if tool == "bash" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("in-memory mirror not updated: %v", srv.deps.Config.Permissions.AlwaysAllowTools)
	}
	cfgData, _ := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if !strings.Contains(string(cfgData), "bash") {
		t.Errorf("disk config should contain 'bash', got: %s", cfgData)
	}

	// (b) publish_to_web is no longer non-persistable — should accept 200.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/permissions/always-allow",
		strings.NewReader(`{"tool":"publish_to_web"}`))
	srv.handleAddGlobalAlwaysAllow(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("publish_to_web should now persist (deny-list is empty), got %d; body=%s",
			rec.Code, rec.Body.String())
	}

	// (c) missing tool field: 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/permissions/always-allow",
		strings.NewReader(`{"foo":"bar"}`))
	srv.handleAddGlobalAlwaysAllow(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing tool should return 400, got %d", rec.Code)
	}
}

// TestHandleRemoveGlobalAlwaysAllow covers the symmetric remove endpoint.
func TestHandleRemoveGlobalAlwaysAllow(t *testing.T) {
	shannonDir := t.TempDir()
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config: &config.Config{
			Permissions: permissions.PermissionsConfig{
				AlwaysAllowTools: []string{"bash", "file_write"},
			},
		},
	}, "test")
	// Seed disk so Remove has something to find.
	if err := config.AppendGlobalAlwaysAllowTool(shannonDir, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := config.AppendGlobalAlwaysAllowTool(shannonDir, "file_write"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/permissions/always-allow",
		strings.NewReader(`{"tool":"bash"}`))
	srv.handleRemoveGlobalAlwaysAllow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, tool := range srv.deps.Config.Permissions.AlwaysAllowTools {
		if tool == "bash" {
			t.Error("bash should be removed from in-memory mirror")
		}
	}
	stillHas := false
	for _, tool := range srv.deps.Config.Permissions.AlwaysAllowTools {
		if tool == "file_write" {
			stillHas = true
			break
		}
	}
	if !stillHas {
		t.Error("file_write should still be in in-memory mirror")
	}
}

func TestHandleMessage_RejectsPathTraversal(t *testing.T) {
	sessDir := t.TempDir()
	deps := &ServerDeps{
		Config:       &config.Config{},
		AgentsDir:    t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	body := `{"text":"x","session_id":"../../../../etc/passwd","source":"kocoro"}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "/etc/passwd") {
		t.Errorf("response body echoes attacker id (info leak): %s", b)
	}
}

func TestMCPConfigChanged_KeepAliveDetected(t *testing.T) {
	makeCfg := func(keepAlive bool) *config.Config {
		return &config.Config{
			MCPServers: map[string]mcp.MCPServerConfig{
				"playwright": {Command: "npx", Args: []string{"@playwright/mcp"}, KeepAlive: keepAlive},
			},
		}
	}
	tests := []struct {
		name     string
		old, new *config.Config
		want     bool
	}{
		{"keep_alive false→true", makeCfg(false), makeCfg(true), true},
		{"keep_alive true→false", makeCfg(true), makeCfg(false), true},
		{"keep_alive unchanged", makeCfg(true), makeCfg(true), false},
		{"both false unchanged", makeCfg(false), makeCfg(false), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpConfigChanged(tc.old, tc.new); got != tc.want {
				t.Fatalf("mcpConfigChanged(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestServer_DisplayName_AutoSlug verifies that POSTing without a name but
// with a display_name auto-generates a slug and returns the display_name.
// TestServer_CreateAgent_IgnoresClientName pins the breaking-change contract:
// a client-supplied "name" in the POST body must be ignored (the slug is always
// server-generated). Guards against a regression where AgentCreateRequest.Name
// is accidentally given a json tag again, which would re-open slug injection.
func TestServer_CreateAgent_IgnoresClientName(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"name":"injected","display_name":"X","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, body)
	}
	var api struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(api.Name, "agent-") {
		t.Errorf("slug = %q, want server-generated agent-<hex>", api.Name)
	}
	if api.Name == "injected" {
		t.Errorf("client-supplied name was honored — slug injection regression")
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "injected", "AGENT.md")); err == nil {
		t.Errorf("an agent dir was created under the client-supplied name")
	}
}

func TestServer_DisplayName_AutoSlug(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"客服助手","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, body)
	}
	var api struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(api.Name, "agent-") {
		t.Errorf("expected auto-slug name to start with 'agent-', got %q", api.Name)
	}
	if api.DisplayName != "客服助手" {
		t.Errorf("expected display_name %q, got %q", "客服助手", api.DisplayName)
	}
}

// TestServer_DisplayName_ListIncludesIt verifies GET /agents (the list endpoint,
// which uses its own DTO) surfaces display_name per entry.
func TestServer_DisplayName_ListIncludesIt(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"客服助手","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	listResp, err := http.Get(base + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var list struct {
		Agents []struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var got string
	for _, a := range list.Agents {
		if a.Name == created.Name {
			got = a.DisplayName
		}
	}
	if got != "客服助手" {
		t.Errorf("list entry display_name = %q, want %q", got, "客服助手")
	}
}

// TestServer_DisplayName_CreateDuplicate verifies that creating a second agent
// with an already-taken display_name returns 409.
func TestServer_DisplayName_CreateDuplicate(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"TakenName","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create first-bot: expected 201, got %d", resp.StatusCode)
	}

	resp2, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"TakenName","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	var dup struct{ Error, Code string }
	json.NewDecoder(resp2.Body).Decode(&dup)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("duplicate display_name: expected 409, got %d", resp2.StatusCode)
	}
	if dup.Code != "display_name_taken" {
		t.Errorf("duplicate display_name: expected code display_name_taken, got %q", dup.Code)
	}
}

// TestServer_DisplayName_RenameDuplicate verifies that renaming an agent to an
// already-taken display_name returns 409.
func TestServer_DisplayName_RenameDuplicate(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// Create agent A with display_name "NameA".
	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"NameA","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create agent-a: expected 201, got %d", resp.StatusCode)
	}

	// Create agent B with display_name "NameB"; capture its server-minted slug.
	resp2, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"NameB","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	var createdB struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&createdB); err != nil {
		t.Fatalf("decode agent-b create response: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("create agent-b: expected 201, got %d", resp2.StatusCode)
	}

	// Rename agent B to "NameA" — should 409.
	dn := "NameA"
	body, _ := json.Marshal(map[string]interface{}{"display_name": &dn})
	req, _ := http.NewRequest(http.MethodPut, base+"/agents/"+createdB.Name, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusConflict {
		t.Errorf("rename to taken display_name: expected 409, got %d", resp3.StatusCode)
	}
}

// TestServer_DisplayName_RenameToEmptyRejected verifies that clearing
// display_name to "" is rejected with 400 (a named agent must keep a
// human-readable label rather than fall back to the opaque slug), and that the
// existing display_name is left unchanged.
func TestServer_DisplayName_RenameToEmptyRejected(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// Create agent with display_name "Temp"; capture the slug.
	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"Temp","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	slug := created.Name

	// PUT display_name="" must be rejected with 400.
	empty := ""
	putBody, _ := json.Marshal(map[string]interface{}{"display_name": &empty})
	req, _ := http.NewRequest(http.MethodPut, base+"/agents/"+slug, bytes.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var empErr struct{ Error, Code string }
	json.NewDecoder(resp2.Body).Decode(&empErr)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT display_name='': expected 400, got %d", resp2.StatusCode)
	}
	if empErr.Code != "display_name_required" {
		t.Errorf("PUT display_name='': expected code display_name_required, got %q", empErr.Code)
	}

	// The existing display_name must be unchanged.
	a, err := agents.LoadAgent(agentsDir, slug)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if api := a.ToAPI(); api.DisplayName != "Temp" {
		t.Errorf("display_name should be unchanged after rejected clear, got %q", api.DisplayName)
	}
}

// TestServer_DisplayName_RenameHappyPath verifies a successful display_name
// rename: 200 status, updated display_name, and slug unchanged.
func TestServer_DisplayName_RenameHappyPath(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"Old","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	slug := created.Name

	newName := "New"
	putBody, _ := json.Marshal(map[string]interface{}{"display_name": &newName})
	req, _ := http.NewRequest(http.MethodPut, base+"/agents/"+slug, bytes.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("PUT rename: expected 200, got %d body=%s", resp2.StatusCode, body)
	}

	a, err := agents.LoadAgent(agentsDir, slug)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	api := a.ToAPI()
	if api.DisplayName != "New" {
		t.Errorf("expected display_name %q, got %q", "New", api.DisplayName)
	}
	if api.Name != slug {
		t.Errorf("expected slug %q to be unchanged, got %q", slug, api.Name)
	}
}

// TestServer_DisplayName_NestedConfigIgnored verifies that a display_name
// supplied inside the config object is ignored on both POST /agents and
// PUT /agents/{name}, so the uniqueness check cannot be bypassed.
func TestServer_DisplayName_NestedConfigIgnored(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// Create "taken-bot" with top-level display_name so "TakenName" is a real
	// taken name.
	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"TakenName","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create taken-bot: expected 201, got %d", resp.StatusCode)
	}

	// POST /agents with a unique top-level display_name plus a nested
	// config.display_name — the nested value must be ignored (not persisted).
	resp2, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"RealCreate","prompt":"p","config":{"display_name":"TakenName"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var createdCreate struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&createdCreate); err != nil {
		t.Fatalf("decode bypass-create response: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("bypass-create: expected 201, got %d", resp2.StatusCode)
	}
	loaded, err := agents.LoadAgent(agentsDir, createdCreate.Name)
	if err != nil {
		t.Fatalf("load bypass-create: %v", err)
	}
	if dn := loaded.ToAPI().DisplayName; dn != "RealCreate" {
		t.Errorf("expected display_name %q (nested config value ignored), got %q", "RealCreate", dn)
	}

	// Create a second agent to test PUT bypass.
	resp3, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"RealUpdate","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	var createdUpdate struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&createdUpdate); err != nil {
		t.Fatalf("decode bypass-update response: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusCreated {
		t.Fatalf("bypass-update: expected 201, got %d", resp3.StatusCode)
	}

	// PUT /agents/{name} with display_name only inside config — must not write
	// the duplicate display_name; the existing "RealUpdate" stays.
	req, _ := http.NewRequest(http.MethodPut, base+"/agents/"+createdUpdate.Name,
		strings.NewReader(`{"config":{"display_name":"TakenName"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp4, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("PUT bypass-update: expected 200, got %d", resp4.StatusCode)
	}
	loaded2, err := agents.LoadAgent(agentsDir, createdUpdate.Name)
	if err != nil {
		t.Fatalf("load bypass-update: %v", err)
	}
	if dn := loaded2.ToAPI().DisplayName; dn != "RealUpdate" {
		t.Errorf("expected display_name %q (nested config value ignored on PUT), got %q", "RealUpdate", dn)
	}
}

func TestServer_DisplayName_ConfigMutationsPreserveLabel(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	resp, err := http.Post(base+"/agents", "application/json",
		strings.NewReader(`{"display_name":"KeepMe","prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	cwd := t.TempDir()
	cfgBody, _ := json.Marshal(map[string]any{
		"cwd":          cwd,
		"display_name": "NestedBypass",
	})
	req, _ := http.NewRequest(http.MethodPut, base+"/agents/"+created.Name+"/config", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("PUT config: expected 200, got %d", resp2.StatusCode)
	}
	loaded, err := agents.LoadAgent(agentsDir, created.Name)
	if err != nil {
		t.Fatalf("load after PUT config: %v", err)
	}
	if got := loaded.ToAPI().DisplayName; got != "KeepMe" {
		t.Fatalf("PUT config changed display_name to %q", got)
	}
	if loaded.Config == nil || loaded.Config.CWD != cwd {
		t.Fatalf("PUT config did not write cwd, config=%+v", loaded.Config)
	}

	reqDel, _ := http.NewRequest(http.MethodDelete, base+"/agents/"+created.Name+"/config", nil)
	resp3, err := http.DefaultClient.Do(reqDel)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("DELETE config: expected 200, got %d", resp3.StatusCode)
	}
	loaded, err = agents.LoadAgent(agentsDir, created.Name)
	if err != nil {
		t.Fatalf("load after DELETE config: %v", err)
	}
	if got := loaded.ToAPI().DisplayName; got != "KeepMe" {
		t.Fatalf("DELETE config cleared display_name to %q", got)
	}
	if loaded.Config == nil || loaded.Config.CWD != "" {
		t.Fatalf("DELETE config should preserve only display_name, config=%+v", loaded.Config)
	}

	req, _ = http.NewRequest(http.MethodPut, base+"/agents/"+created.Name+"/config", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	resp4, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("second PUT config: expected 200, got %d", resp4.StatusCode)
	}
	reqClear, _ := http.NewRequest(http.MethodPut, base+"/agents/"+created.Name, strings.NewReader(`{"config":null}`))
	reqClear.Header.Set("Content-Type", "application/json")
	resp5, err := http.DefaultClient.Do(reqClear)
	if err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("PUT config:null: expected 200, got %d", resp5.StatusCode)
	}
	loaded, err = agents.LoadAgent(agentsDir, created.Name)
	if err != nil {
		t.Fatalf("load after config:null: %v", err)
	}
	if got := loaded.ToAPI().DisplayName; got != "KeepMe" {
		t.Fatalf("config:null cleared display_name to %q", got)
	}
	if loaded.Config == nil || loaded.Config.CWD != "" {
		t.Fatalf("config:null should preserve only display_name, config=%+v", loaded.Config)
	}
}

func TestServer_CreateAgent_WritesAvatar(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	body := `{"display_name":"avatar-bot","prompt":"hi","avatar":"https://cdn.example.com/a.png"}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	agentDir := filepath.Join(agentsDir, created.Name)
	profile, err := agents.LoadAgentProfile(agentDir)
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if profile == nil {
		t.Fatal("expected PROFILE.yaml to be written, got nil")
	}
	if profile.Avatar != "https://cdn.example.com/a.png" {
		t.Fatalf("avatar = %q, want %q", profile.Avatar, "https://cdn.example.com/a.png")
	}
}

func TestServer_UpdateAgent_WritesAvatar(t *testing.T) {
	agentsDir := t.TempDir()
	sessDir := t.TempDir()
	deps := &ServerDeps{
		AgentsDir:    agentsDir,
		ShannonDir:   t.TempDir(),
		SessionCache: NewSessionCache(sessDir),
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Create agent first
	createBody := `{"display_name":"update-avatar-bot","prompt":"hi"}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()), "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// Update with avatar
	updateBody := `{"avatar":"https://cdn.example.com/b.png"}`
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/agents/%s", srv.Port(), created.Name), strings.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	updateResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer updateResp.Body.Close()
	if updateResp.StatusCode != http.StatusOK {
		t.Fatalf("update status=%d", updateResp.StatusCode)
	}

	agentDir := filepath.Join(agentsDir, created.Name)
	profile, err := agents.LoadAgentProfile(agentDir)
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if profile == nil {
		t.Fatal("expected PROFILE.yaml to be written, got nil")
	}
	if profile.Avatar != "https://cdn.example.com/b.png" {
		t.Fatalf("avatar = %q, want %q", profile.Avatar, "https://cdn.example.com/b.png")
	}
}
