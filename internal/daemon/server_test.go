package daemon

import (
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
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
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

	body := `{"name":"testbot","prompt":"hello world"}`
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

	// Duplicate create — should get 409
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

	body := `{"name":"failbot","prompt":"should fail"}`
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

	body := `{"name":"cache-test","prompt":"hello world"}`
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

	sessionCache.mu.Lock()
	route, ok := sessionCache.routes["agent:cache-test"]
	sessionCache.mu.Unlock()
	if !ok {
		t.Fatalf("expected route cache entry for agent:cache-test to exist")
	}
	if route.manager != nil {
		t.Fatalf("expected create path to avoid creating a route manager")
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

	body := `{"name":"attach-bot","prompt":"hello world","skills":[{"name":"check"}]}`
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

	loaded, err := agents.LoadAgent(agentsDir, "attach-bot")
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0].Name != "check" {
		t.Fatalf("expected attached global skill 'check', got %+v", loaded.Skills)
	}

	attached, err := agents.ReadAttachedSkills(agentsDir, "attach-bot")
	if err != nil {
		t.Fatalf("read attached skills: %v", err)
	}
	if len(attached) != 1 || attached[0] != "check" {
		t.Fatalf("expected manifest to contain check, got %v", attached)
	}

	if _, err := os.Stat(filepath.Join(agentsDir, "attach-bot", "skills")); !os.IsNotExist(err) {
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

	createBody := `{"name":"skill-bot","prompt":"hello world"}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(createBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	req, err := http.NewRequest(
		http.MethodPut,
		fmt.Sprintf("http://127.0.0.1:%d/agents/skill-bot/skills/check", srv.Port()),
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

	loaded, err := agents.LoadAgent(agentsDir, "skill-bot")
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

	createBody := `{"name":"detach-bot","prompt":"hello world","skills":[{"name":"check"}]}`
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/agents", srv.Port()),
		"application/json",
		strings.NewReader(createBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	if err := agents.WriteAgentSkill(agentsDir, "detach-bot", &skills.Skill{
		Name:        "check",
		Description: "legacy local copy",
		Prompt:      "legacy prompt",
	}); err != nil {
		t.Fatalf("write legacy agent-local skill: %v", err)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		fmt.Sprintf("http://127.0.0.1:%d/agents/detach-bot/skills/check", srv.Port()),
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

	attached, err := agents.ReadAttachedSkills(agentsDir, "detach-bot")
	if err != nil {
		t.Fatalf("read attached skills: %v", err)
	}
	if len(attached) != 0 {
		t.Fatalf("expected empty attached skills after delete, got %v", attached)
	}

	loaded, err := agents.LoadAgent(agentsDir, "detach-bot")
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if len(loaded.Skills) != 0 {
		t.Fatalf("expected no loaded skills after detach, got %+v", loaded.Skills)
	}

	if _, err := os.Stat(filepath.Join(agentsDir, "detach-bot", "skills", "check")); !os.IsNotExist(err) {
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
			wantStatus: http.StatusBadRequest,
			wantErr:    "no such file or directory",
		},
		{
			name:       "valid new_content only passes validation",
			sessionID:  "nonexistent",
			body:       `{"message_index":0,"new_content":"hello"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "no such file or directory",
		},
		{
			name:       "valid new_content with content blocks passes validation",
			sessionID:  "nonexistent",
			body:       `{"message_index":0,"new_content":"analyze this","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "no such file or directory",
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
