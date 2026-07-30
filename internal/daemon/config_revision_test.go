package daemon

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

func TestConfigReloadStateDetectsExternalEdit(t *testing.T) {
	shannonDir := t.TempDir()
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("skills:\n  disabled:\n    - old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}, "test")

	if err := os.WriteFile(configPath, []byte("skills:\n  disabled: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	required, reason, _ := srv.configReloadState()
	if !required {
		t.Fatal("expected external edit to require reload")
	}
	if !strings.Contains(reason, "POST /config/reload") {
		t.Fatalf("reload reason lacks action: %q", reason)
	}

	rr := httptest.NewRecorder()
	srv.handleGetConfig(rr, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config = %d, body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["reload_required"] != true {
		t.Fatalf("GET /config reload_required = %v, want true", response["reload_required"])
	}
	if !strings.Contains(response["reload_reason"].(string), "POST /config/reload") {
		t.Fatalf("GET /config reload_reason = %v", response["reload_reason"])
	}
}

func TestConfigReloadWarningLogsOncePerExternalRevision(t *testing.T) {
	shannonDir := t.TempDir()
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}, "test")

	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	}()

	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv.warnIfConfigReloadRequired()
	srv.warnIfConfigReloadRequired()
	const action = "call POST /config/reload or restart the daemon"
	if count := strings.Count(logs.String(), action); count != 1 {
		t.Fatalf("warning count for one revision = %d, want 1; logs=%q", count, logs.String())
	}

	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv.warnIfConfigReloadRequired()
	if count := strings.Count(logs.String(), action); count != 2 {
		t.Fatalf("warning count after second revision = %d, want 2; logs=%q", count, logs.String())
	}
}

func TestConfigStatusReportsNoReloadAfterAppliedAPIMutation(t *testing.T) {
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}, "test")

	req := httptest.NewRequest(http.MethodPost, "/skills/disabled", strings.NewReader(`{"skill":"demo"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAddGlobalDisabledSkill(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /skills/disabled = %d, body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.handleConfigStatus(rr, httptest.NewRequest(http.MethodGet, "/config/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["reload_required"] != false {
		t.Fatalf("reload_required = %v, want false", response["reload_required"])
	}
}

func TestAppliedAPIMutationDoesNotHideEarlierExternalEdit(t *testing.T) {
	shannonDir := t.TempDir()
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config: &config.Config{
			Agent: config.AgentConfig{Temperature: 0.2},
		},
	}, "test")

	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/skills/disabled", strings.NewReader(`{"skill":"demo"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAddGlobalDisabledSkill(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /skills/disabled = %d, body=%s", rr.Code, rr.Body.String())
	}

	required, _, _ := srv.configReloadState()
	if !required {
		t.Fatal("partial in-memory API mutation must not mark earlier external edits as loaded")
	}
	if srv.deps.Config.Agent.Temperature != 0.2 {
		t.Fatalf("live temperature changed without reload: %v", srv.deps.Config.Agent.Temperature)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "temperature: 0.8") {
		t.Fatalf("API mutation overwrote external edit:\n%s", data)
	}
}

func TestPatchConfigMakesPendingReloadObservable(t *testing.T) {
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}, "test")

	req := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(`{"agent":{"temperature":0.7}}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handlePatchConfig(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /config = %d, body=%s", rr.Code, rr.Body.String())
	}
	required, _, _ := srv.configReloadState()
	if !required {
		t.Fatal("PATCH /config should remain pending until POST /config/reload")
	}

	revision, err := config.FileRevision(shannonDir)
	if err != nil {
		t.Fatal(err)
	}
	srv.recordConfigRevisionApplied(revision)
	required, _, _ = srv.configReloadState()
	if required {
		t.Fatal("recording a successful reload should clear reload_required")
	}
}

func TestAppliedRevisionDoesNotBlessLaterExternalEdit(t *testing.T) {
	shannonDir := t.TempDir()
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{Agent: config.AgentConfig{Temperature: 0.2}},
	}, "test")
	loadedRevision, err := config.FileRevision(shannonDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv.recordConfigRevisionApplied(loadedRevision)
	if required, _, _ := srv.configReloadState(); !required {
		t.Fatal("a later external edit was incorrectly marked as applied")
	}
}

func TestAlwaysAllowInternalWriteAdvancesAppliedRevision(t *testing.T) {
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := &ServerDeps{ShannonDir: shannonDir, Config: &config.Config{}}
	srv := NewServer(0, nil, deps, "test")
	broker := NewApprovalBroker(func(ApprovalRequest) error { return nil })

	persistGlobalToolAlwaysAllow(deps, broker, "bash")
	if required, _, _ := srv.configReloadState(); required {
		t.Fatal("daemon-owned always-allow write was misclassified as an external edit")
	}
	if got := deps.Config.Permissions.AlwaysAllowTools; len(got) != 1 || got[0] != "bash" {
		t.Fatalf("live always-allow state = %v, want [bash]", got)
	}
}

func TestPatchGlobalConfigUsesSharedConfigLock(t *testing.T) {
	shannonDir := t.TempDir()
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     &config.Config{},
	}, "test")

	lockFile, err := os.OpenFile(configPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_ = fslock.Unlock(lockFile.Fd())
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- srv.patchGlobalConfig(map[string]interface{}{
			"agent": map[string]interface{}{"temperature": 0.7},
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("patch returned before shared lock was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := fslock.Unlock(lockFile.Fd()); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("patch after unlock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("patch remained blocked after shared lock was released")
	}
}
