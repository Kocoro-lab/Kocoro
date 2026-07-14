package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// newPullServer builds a minimal *Server with an initialized SessionCache and
// AgentsDir, sufficient to drive pullAndApplyAgents in tests.
func newPullServer(t *testing.T, agentsDir string) *Server {
	t.Helper()
	sc := NewSessionCache(filepath.Join(agentsDir, "_sessions"))
	t.Cleanup(func() { sc.CloseAll() })
	return &Server{deps: &ServerDeps{AgentsDir: agentsDir, ShannonDir: agentsDir, SessionCache: sc}}
}

func TestBuildSyncItems_IncludesAvatarInProfile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("category: coding\navatar: https://cdn/a.png\n"), 0o644)

	items, err := newPullServer(t, root).buildSyncItems(root)
	if err != nil {
		t.Fatalf("buildSyncItems: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.AgentKey != "demo" {
			continue
		}
		found = true
		var prof map[string]any
		if err := json.Unmarshal(it.Profile, &prof); err != nil {
			t.Fatalf("profile json: %v", err)
		}
		if prof["avatar"] != "https://cdn/a.png" {
			t.Errorf("avatar missing in profile blob: %v", prof)
		}
	}
	if !found {
		t.Fatal("demo agent not in items")
	}
}

func TestBuildSyncItems_SkipsPureBuiltins(t *testing.T) {
	root := t.TempDir()

	// User agent: directly under root -> Builtin=false -> synced.
	user := filepath.Join(root, "myagent")
	os.MkdirAll(user, 0o755)
	os.WriteFile(filepath.Join(user, "AGENT.md"), []byte("user prompt"), 0o644)

	// Pure builtin: lives under _builtin and is NOT overridden -> skipped.
	builtin := filepath.Join(root, "_builtin", "builtinagent")
	os.MkdirAll(builtin, 0o755)
	os.WriteFile(filepath.Join(builtin, "AGENT.md"), []byte("builtin prompt"), 0o644)

	items, err := newPullServer(t, root).buildSyncItems(root)
	if err != nil {
		t.Fatalf("buildSyncItems: %v", err)
	}
	keys := make(map[string]bool, len(items))
	for _, it := range items {
		keys[it.AgentKey] = true
	}
	if !keys["myagent"] {
		t.Errorf("user agent should be synced, got items %v", keys)
	}
	if keys["builtinagent"] {
		t.Errorf("pure builtin must NOT be synced, got items %v", keys)
	}
}

func TestTriggerAgentSync_CoalescesWithoutBlocking(t *testing.T) {
	s := &Server{agentSyncTrigger: make(chan struct{}, 1)}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.triggerAgentSync()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("triggerAgentSync blocked on a full buffer")
	}

	if got := len(s.agentSyncTrigger); got != 1 {
		t.Errorf("expected exactly 1 pending trigger, got %d", got)
	}
}

func TestPullAndApply_FullyMaterializesMissingAgent(t *testing.T) {
	root := t.TempDir()

	// Existing local agent must NOT be clobbered.
	existing := filepath.Join(root, "keep")
	os.MkdirAll(existing, 0o755)
	os.WriteFile(filepath.Join(existing, "AGENT.md"), []byte("local prompt"), 0o644)
	os.WriteFile(filepath.Join(existing, "PROFILE.yaml"), []byte("avatar: https://cdn/local.png\n"), 0o644)

	deleted := time.Now().UTC()
	mem := "cloud memory"
	remoteCWD := t.TempDir()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey: "fresh",
				Prompt:   "fresh prompt",
				Memory:   &mem,
				Config:   json.RawMessage(`{"cwd":"` + remoteCWD + `","auto_approve":true}`),
				Profile:  json.RawMessage(`{"category":"coding","avatar":"https://cdn/fresh.png"}`),
			},
			{AgentKey: "keep", Prompt: "cloud prompt", Profile: json.RawMessage(`{"avatar":"https://cdn/cloud.png"}`)},
			{AgentKey: "gone", DeletedAt: &deleted, Prompt: "gone prompt", Profile: json.RawMessage(`{"avatar":"https://cdn/gone.png"}`)},
		}, nil
	}

	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// fresh: missing locally, not deleted -> fully materialized AND enumerable.
	a, err := agents.LoadAgent(root, "fresh")
	if err != nil {
		t.Fatalf("fresh agent not loadable (AGENT.md missing?): %v", err)
	}
	if a.Prompt != "fresh prompt" {
		t.Errorf("fresh prompt not materialized: %q", a.Prompt)
	}
	if a.Memory != "cloud memory" {
		t.Errorf("fresh memory not materialized: %q", a.Memory)
	}
	if a.Config == nil || a.Config.AutoApprove == nil || !*a.Config.AutoApprove {
		t.Errorf("fresh syncable config not materialized: %+v", a.Config)
	}
	if a.Config.CWD != "" {
		t.Errorf("cloud cwd must not materialize on another device: %+v", a.Config)
	}

	// Enumerable: ListAgents must now include the freshly-materialized agent.
	entries, err := agents.ListAgents(root)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var enumerated bool
	for _, e := range entries {
		if e.Name == "fresh" {
			enumerated = true
		}
	}
	if !enumerated {
		t.Errorf("fresh agent not enumerable; would be soft-deleted on next full push")
	}

	freshProf, err := agents.LoadAgentProfile(filepath.Join(root, "fresh"))
	if err != nil {
		t.Fatalf("load fresh profile: %v", err)
	}
	if freshProf == nil || freshProf.Avatar != "https://cdn/fresh.png" {
		t.Errorf("fresh profile not materialized: %+v", freshProf)
	}

	// keep: already exists locally -> untouched (prompt + profile preserved).
	keepProf, err := agents.LoadAgentProfile(existing)
	if err != nil {
		t.Fatalf("load keep profile: %v", err)
	}
	if keepProf == nil || keepProf.Avatar != "https://cdn/local.png" {
		t.Errorf("existing local profile was clobbered: %+v", keepProf)
	}
	if b, _ := os.ReadFile(filepath.Join(existing, "AGENT.md")); string(b) != "local prompt" {
		t.Errorf("existing local prompt was clobbered: %q", b)
	}

	// gone: soft-deleted -> not materialized
	if _, err := os.Stat(filepath.Join(root, "gone")); !os.IsNotExist(err) {
		t.Errorf("soft-deleted agent should not be materialized")
	}
}

// Fix 1 (real bug): MEMORY.md churn from the memory_append tool must NOT make a
// local agent look "newer" and silently drop a genuine cloud profile/avatar
// edit. The LWW clock ignores MEMORY.md, so a cloud item whose UpdatedAt is
// newer than the DEFINITION files (but older than the churned MEMORY.md) still
// wins.
func TestPullAndApply_MemoryChurnDoesNotBlockCloudEdit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("local prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("avatar: https://cdn/old.png\n"), 0o644)

	// Definition files are OLD (well in the past).
	defTime := time.Now().Add(-2 * time.Hour).UTC()
	stampAgentMtime(dir, defTime)

	// MEMORY.md was just churned by memory_append — its mtime is NOW. This must
	// not gate the definition LWW clock.
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("just appended"), 0o644)
	now := time.Now().UTC()
	os.Chtimes(filepath.Join(dir, "MEMORY.md"), now, now)

	// Cloud edit timestamp is between the old definition time and now.
	cloudTS := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "cloud prompt",
				Profile:   json.RawMessage(`{"avatar":"https://cdn/new.png"}`),
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// The cloud edit MUST have been applied (definition LWW ignores MEMORY.md).
	if b, _ := os.ReadFile(filepath.Join(dir, "AGENT.md")); string(b) != "cloud prompt" {
		t.Errorf("cloud edit dropped due to MEMORY.md churn: prompt = %q", b)
	}
	prof, _ := agents.LoadAgentProfile(dir)
	if prof == nil || prof.Avatar != "https://cdn/new.png" {
		t.Errorf("cloud avatar edit dropped due to MEMORY.md churn: %+v", prof)
	}
}

// Fix 1: agentLastModified must reflect ONLY definition files, not MEMORY.md.
func TestAgentLastModified_IgnoresMemory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("p"), 0o644)
	defTime := time.Now().Add(-1 * time.Hour).UTC()
	stampAgentMtime(dir, defTime)

	// MEMORY.md churned to NOW.
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("m"), 0o644)
	now := time.Now().UTC()
	os.Chtimes(filepath.Join(dir, "MEMORY.md"), now, now)

	got := agentLastModified(dir).Truncate(time.Second)
	if got.After(defTime.Add(time.Second)) {
		t.Errorf("agentLastModified followed MEMORY.md mtime: got %v want ≈ %v", got, defTime)
	}
}

func TestPullAndApply_RejectsInvalidAgentKey(t *testing.T) {
	root := t.TempDir()
	// A sentinel file outside the agents dir that a traversal key would target.
	outside := filepath.Join(filepath.Dir(root), "evil-AGENT.md")
	defer os.Remove(outside)

	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "../evil", Prompt: "pwned", Profile: json.RawMessage(`{"avatar":"x"}`)},
		}, nil
	}

	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// Nothing written anywhere — neither inside nor a traversal target outside.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil")); !os.IsNotExist(err) {
		t.Errorf("traversal key wrote outside agents dir")
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) != 0 {
		t.Errorf("invalid agent key produced files in agents dir: %v", entries)
	}
}

func TestPullAndApply_TombstoneDeletesLocalAgent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "victim")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("local prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: /tmp\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("avatar: https://cdn/v.png\n"), 0o644)
	// Runtime state must survive a tombstone delete (mirrors handleDeleteAgent).
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("keep me"), 0o644)

	deleted := time.Now().UTC()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "victim", DeletedAt: &deleted, UpdatedAt: deleted},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// Definition files gone -> no longer enumerable.
	entries, err := agents.ListAgents(root)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	for _, e := range entries {
		if e.Name == "victim" {
			t.Fatalf("tombstoned agent still enumerable")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENT.md")); !os.IsNotExist(err) {
		t.Errorf("AGENT.md not removed by tombstone")
	}
	// MEMORY.md (runtime state) preserved per handleDeleteAgent semantics.
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err != nil {
		t.Errorf("tombstone should preserve MEMORY.md (runtime state): %v", err)
	}
}

func TestPullAndApply_TombstoneMissingLocalIsNoop(t *testing.T) {
	root := t.TempDir()
	deleted := time.Now().UTC()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "ghost", DeletedAt: &deleted, UpdatedAt: deleted},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ghost")); !os.IsNotExist(err) {
		t.Errorf("tombstone of a locally-missing agent should not create anything")
	}
}

func TestPullAndApply_CloudNewerOverwritesLocal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old local prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("avatar: https://cdn/old.png\n"), 0o644)

	// Force the local mtime well into the past so cloud is strictly newer.
	past := time.Now().Add(-1 * time.Hour).UTC()
	stampAgentMtime(dir, past)

	cloudTS := time.Now().UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "new cloud prompt",
				Profile:   json.RawMessage(`{"avatar":"https://cdn/new.png"}`),
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	a, err := agents.LoadAgent(root, "agt")
	if err != nil {
		t.Fatalf("load agt: %v", err)
	}
	if a.Prompt != "new cloud prompt" {
		t.Errorf("cloud-newer item did not overwrite local prompt: %q", a.Prompt)
	}
	prof, _ := agents.LoadAgentProfile(dir)
	if prof == nil || prof.Avatar != "https://cdn/new.png" {
		t.Errorf("cloud-newer item did not overwrite avatar: %+v", prof)
	}

	// mtime must be stamped to the cloud timestamp (no ping-pong on next push).
	if got := agentLastModified(dir).Truncate(time.Second); !got.Equal(cloudTS) {
		t.Errorf("mtime not stamped to cloud UpdatedAt: got %v want %v", got, cloudTS)
	}
}

func TestPullAndApply_CloudOlderDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("fresh local prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("avatar: https://cdn/local.png\n"), 0o644)

	// Local is "now"; cloud item is an hour old -> local wins, keep local.
	now := time.Now().UTC()
	stampAgentMtime(dir, now)
	cloudTS := now.Add(-1 * time.Hour)

	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "stale cloud prompt",
				Profile:   json.RawMessage(`{"avatar":"https://cdn/stale.png"}`),
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	if b, _ := os.ReadFile(filepath.Join(dir, "AGENT.md")); string(b) != "fresh local prompt" {
		t.Errorf("cloud-older item clobbered locally-newer prompt: %q", b)
	}
	prof, _ := agents.LoadAgentProfile(dir)
	if prof == nil || prof.Avatar != "https://cdn/local.png" {
		t.Errorf("cloud-older item clobbered local avatar: %+v", prof)
	}
}

func TestPullAndApply_DropsInvalidAvatarKeepsAgent(t *testing.T) {
	root := t.TempDir()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "p",
				Profile:   json.RawMessage(`{"avatar":"javascript:alert(1)"}`),
				UpdatedAt: time.Now().UTC(),
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	if _, err := agents.LoadAgent(root, "agt"); err != nil {
		t.Fatalf("agent should still be materialized: %v", err)
	}
	prof, _ := agents.LoadAgentProfile(filepath.Join(root, "agt"))
	if prof != nil && prof.Avatar != "" {
		t.Errorf("invalid avatar should have been dropped, got %q", prof.Avatar)
	}
}

func TestPullAndApply_StampsMtimeOnMaterialize(t *testing.T) {
	root := t.TempDir()
	cloudTS := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "fresh", Prompt: "p", Profile: json.RawMessage(`{"avatar":"https://cdn/a.png"}`), UpdatedAt: cloudTS},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	if got := agentLastModified(filepath.Join(root, "fresh")).Truncate(time.Second); !got.Equal(cloudTS) {
		t.Errorf("materialized mtime not stamped to cloud UpdatedAt: got %v want %v", got, cloudTS)
	}
}

func TestAgentSyncWorker_WaitsForPullDone(t *testing.T) {
	// Drive the REAL worker. With deps == nil, cloudGateway() returns nil so the
	// worker never performs a network push — but it still consumes the trigger
	// once it passes the pullDone gate and finishes the 2s debounce. We assert
	// the trigger is NOT consumed while pullDone is open, then IS consumed after
	// it closes.
	s := &Server{
		agentSyncTrigger: make(chan struct{}, 1),
		pullDone:         make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.agentSyncWorker(ctx)

	// Queue a change before the gate opens.
	s.triggerAgentSync()

	// While pullDone is open the worker is blocked on the gate, so the buffered
	// trigger stays pending (len==1). Give the scheduler a moment.
	time.Sleep(150 * time.Millisecond)
	if got := len(s.agentSyncTrigger); got != 1 {
		t.Fatalf("worker consumed trigger before pullDone closed (pending=%d)", got)
	}

	// Open the gate. The worker now consumes the trigger; after the 2s debounce
	// it drains and the channel empties. Poll until empty (or fail).
	close(s.pullDone)
	deadline := time.After(5 * time.Second)
	for {
		if len(s.agentSyncTrigger) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not consume trigger after pullDone closed")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestBuildSyncItems_SetsRealLastModified(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("avatar: https://cdn/a.png\n"), 0o644)

	fi, err := os.Stat(filepath.Join(dir, "PROFILE.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	want := fi.ModTime()

	items, err := newPullServer(t, root).buildSyncItems(root)
	if err != nil {
		t.Fatalf("buildSyncItems: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.AgentKey != "demo" {
			continue
		}
		found = true
		if it.UpdatedAt.IsZero() {
			t.Fatal("UpdatedAt is zero; LWW would be meaningless")
		}
		if diff := it.UpdatedAt.Sub(want.UTC()); diff < -time.Second || diff > time.Second {
			t.Errorf("UpdatedAt %v not ≈ file mtime %v", it.UpdatedAt, want.UTC())
		}
	}
	if !found {
		t.Fatal("demo agent not in items")
	}
}

func TestBuildSyncItems_StripsDeviceLocalCWD(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := agents.WriteAgentConfig(root, "demo", &agents.AgentConfigAPI{
		CWD:         t.TempDir(),
		AutoApprove: &yes,
	}); err != nil {
		t.Fatal(err)
	}

	items, err := newPullServer(t, root).buildSyncItems(root)
	if err != nil {
		t.Fatalf("buildSyncItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	var cfg map[string]any
	if err := json.Unmarshal(items[0].Config, &cfg); err != nil {
		t.Fatalf("config json: %v", err)
	}
	if _, ok := cfg["cwd"]; ok {
		t.Fatalf("device-local cwd leaked into sync payload: %s", items[0].Config)
	}
	if cfg["auto_approve"] != true {
		t.Fatalf("syncable sibling field lost: %s", items[0].Config)
	}

	if err := agents.WriteAgentConfig(root, "demo", &agents.AgentConfigAPI{CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	items, err = newPullServer(t, root).buildSyncItems(root)
	if err != nil {
		t.Fatalf("buildSyncItems cwd-only: %v", err)
	}
	if len(items) != 1 || len(items[0].Config) != 0 {
		t.Fatalf("cwd-only config should be absent from sync payload: %+v", items)
	}
}

func TestHandleCreateAgent_RejectsBadAvatar(t *testing.T) {
	root := t.TempDir()
	s := &Server{deps: &ServerDeps{AgentsDir: root, ShannonDir: root, EventBus: NewEventBus()}}
	body := `{"display_name":"Bad Avatar","prompt":"hello","avatar":"javascript:alert(1)"}`
	req := httptest.NewRequest(http.MethodPost, "/agents", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCreateAgent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	// Nothing should have been written for the rejected create.
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("rejected create left files behind: %v", entries)
	}
}

func TestHandleUpdateAgent_RejectsBadAvatar(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("p"), 0o644)
	s := &Server{deps: &ServerDeps{AgentsDir: root, ShannonDir: root, EventBus: NewEventBus()}}
	body := `{"avatar":"http://insecure/a.png"}`
	req := httptest.NewRequest(http.MethodPut, "/agents/agt", strings.NewReader(body))
	req.SetPathValue("name", "agt")
	w := httptest.NewRecorder()
	s.handleUpdateAgent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

func TestAgentConfigWrites_RejectInvalidCWDWithoutMutation(t *testing.T) {
	missingCWD := filepath.Join(t.TempDir(), "missing")

	t.Run("create", func(t *testing.T) {
		root := t.TempDir()
		s := &Server{deps: &ServerDeps{AgentsDir: root, ShannonDir: root}}
		body, _ := json.Marshal(map[string]any{
			"display_name": "Bad CWD",
			"prompt":       "hello",
			"config":       map[string]any{"cwd": missingCWD},
		})
		req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCreateAgent(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("invalid create mutated agents dir: %v", entries)
		}
	})

	for _, tc := range []struct {
		name string
		path string
		body func() []byte
		call func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name: "full update",
			path: "/agents/agt",
			body: func() []byte {
				b, _ := json.Marshal(map[string]any{"prompt": "must not apply", "config": map[string]any{"cwd": missingCWD}})
				return b
			},
			call: (*Server).handleUpdateAgent,
		},
		{
			name: "config put",
			path: "/agents/agt/config",
			body: func() []byte {
				b, _ := json.Marshal(map[string]any{"cwd": missingCWD})
				return b
			},
			call: (*Server).handlePutAgentConfig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "agt")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("original prompt"), 0o644); err != nil {
				t.Fatal(err)
			}
			originalConfig := []byte("display_name: Agent\nauto_approve: true\n")
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), originalConfig, 0o644); err != nil {
				t.Fatal(err)
			}
			s := &Server{deps: &ServerDeps{AgentsDir: root, ShannonDir: root}}
			req := httptest.NewRequest(http.MethodPut, tc.path, bytes.NewReader(tc.body()))
			req.SetPathValue("name", "agt")
			w := httptest.NewRecorder()
			tc.call(s, w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "AGENT.md")); string(got) != "original prompt" {
				t.Fatalf("invalid request changed prompt: %q", got)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); !bytes.Equal(got, originalConfig) {
				t.Fatalf("invalid request changed config:\n%s", got)
			}
		})
	}
}

func TestHandleGetAgent_InvalidCWDReturnsWarning(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingCWD := filepath.Join(t.TempDir(), "deleted")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: "+missingCWD+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{deps: &ServerDeps{AgentsDir: root, ShannonDir: root}}
	req := httptest.NewRequest(http.MethodGet, "/agents/agt", nil)
	req.SetPathValue("name", "agt")
	w := httptest.NewRecorder()
	s.handleGetAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var api agents.AgentAPI
	if err := json.Unmarshal(w.Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if api.Config == nil || api.Config.CWD != missingCWD {
		t.Fatalf("broken cwd is not repairable from response: %+v", api.Config)
	}
	if len(api.Warnings) != 1 || !strings.Contains(api.Warnings[0], "cwd") {
		t.Fatalf("warnings = %v, want cwd warning", api.Warnings)
	}
}

// A cloud clear removes synced config fields but preserves cwd, which belongs
// to this device and must never be overwritten by cross-device sync.
func TestPullAndApply_ClearedFieldsOnOverwriteRemoveStaleFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: /tmp\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("stale memory"), 0o644)
	os.WriteFile(filepath.Join(dir, "_attached.yaml"), []byte("skills:\n  - foo\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("avatar: https://cdn/old.png\n"), 0o644)

	// Local is well in the past so the cloud item is strictly newer.
	past := time.Now().Add(-1 * time.Hour).UTC()
	stampAgentMtime(dir, past)

	cloudTS := time.Now().UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "new prompt",
				Memory:    nil,                     // cleared
				Config:    json.RawMessage(`null`), // cleared
				Skills:    json.RawMessage(`[]`),   // cleared
				Profile:   json.RawMessage(`{"avatar":"https://cdn/new.png"}`),
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// Synced config fields clear, but device-local cwd survives.
	loaded, err := agents.LoadAgent(root, "agt")
	if err != nil {
		t.Fatalf("load after clear: %v", err)
	}
	if loaded.Config == nil || loaded.Config.CWD != "/tmp" {
		t.Fatalf("cloud clear removed device-local cwd: %+v", loaded.Config)
	}
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); !os.IsNotExist(err) {
		t.Errorf("cleared MEMORY.md should be removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_attached.yaml")); !os.IsNotExist(err) {
		t.Errorf("cleared _attached.yaml should be removed (err=%v)", err)
	}

	// Mandatory + present fields → updated.
	if b, _ := os.ReadFile(filepath.Join(dir, "AGENT.md")); string(b) != "new prompt" {
		t.Errorf("AGENT.md not updated: %q", b)
	}
	prof, _ := agents.LoadAgentProfile(dir)
	if prof == nil || prof.Avatar != "https://cdn/new.png" {
		t.Errorf("PROFILE not updated: %+v", prof)
	}

	// mtime stamped to cloud timestamp (no ping-pong).
	if got := agentLastModified(dir).Truncate(time.Second); !got.Equal(cloudTS) {
		t.Errorf("mtime not stamped to cloud UpdatedAt: got %v want %v", got, cloudTS)
	}
}

// Fix 1 (counterpart): a cloud-OLDER item must NOT clobber locally-newer files,
// even with cleared fields — LWW still wins for local.
func TestPullAndApply_ClearedFieldsCloudOlderDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("local prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: /tmp\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("local memory"), 0o644)

	now := time.Now().UTC()
	stampAgentMtime(dir, now)
	cloudTS := now.Add(-1 * time.Hour)

	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "agt", Prompt: "cloud", Config: json.RawMessage(`null`), UpdatedAt: cloudTS},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Errorf("cloud-older clear must NOT remove locally-newer config.yaml: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md")); string(b) != "local memory" {
		t.Errorf("cloud-older clear clobbered local MEMORY.md: %q", b)
	}
}

// When cloud has config, synced fields update while the local cwd survives.
func TestPullAndApply_OverwritePreservesPresentFields(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old"), 0o644)
	localCWD := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: "+localCWD+"\nauto_approve: false\n"), 0o644)
	// CWD preservation must not depend on a full LoadAgent succeeding; an
	// unrelated bad profile should not let cloud sync erase device-local state.
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("category: not-a-real-category\n"), 0o644)
	stampAgentMtime(dir, time.Now().Add(-1*time.Hour).UTC())

	remoteCWD := t.TempDir()
	mem := "new memory"
	cloudTS := time.Now().UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "new",
				Memory:    &mem,
				Config:    json.RawMessage(`{"cwd":"` + remoteCWD + `","auto_approve":true}`),
				Profile:   json.RawMessage(`{"avatar":"https://cdn/a.png"}`),
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	a, err := agents.LoadAgent(root, "agt")
	if err != nil {
		t.Fatalf("load agt: %v", err)
	}
	if a.Config == nil || a.Config.CWD != localCWD {
		t.Errorf("cloud config overwrote device-local cwd: %+v", a.Config)
	}
	if a.Config.AutoApprove == nil || !*a.Config.AutoApprove {
		t.Errorf("synced config field not updated: %+v", a.Config)
	}
	if a.Memory != "new memory" {
		t.Errorf("present memory not updated: %q", a.Memory)
	}
}

// Semantic empty-config detection clears synced fields but preserves cwd.
func TestPullAndApply_SemanticEmptyConfigCleared(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: /tmp\n"), 0o644)
	stampAgentMtime(dir, time.Now().Add(-1*time.Hour).UTC())

	cloudTS := time.Now().UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "new",
				Config:    json.RawMessage("  {\n}  \n"), // semantically empty
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	a, err := agents.LoadAgent(root, "agt")
	if err != nil {
		t.Fatalf("load after semantic clear: %v", err)
	}
	if a.Config == nil || a.Config.CWD != "/tmp" {
		t.Fatalf("semantic clear removed device-local cwd: %+v", a.Config)
	}
}

// Fix 3: a malformed-but-present config must NOT wipe the existing config.yaml —
// leave it untouched rather than deleting on decode error.
func TestPullAndApply_MalformedConfigLeavesExisting(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cwd: /keep\n"), 0o644)
	stampAgentMtime(dir, time.Now().Add(-1*time.Hour).UTC())

	cloudTS := time.Now().UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey:  "agt",
				Prompt:    "new",
				Config:    json.RawMessage(`{"cwd":`), // malformed
				UpdatedAt: cloudTS,
			},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); string(b) != "cwd: /keep\n" {
		t.Errorf("malformed config wiped existing config.yaml: %q", b)
	}
}

// Fix 3: the tombstone pull path must Evict the session cache (like
// handleDeleteAgent) so a cloud-originated delete doesn't pull AGENT.md out from
// under a cached route. Observable effect: GetOrCreate returns a fresh manager.
func TestPullAndApply_TombstoneEvictsSessionCache(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "victim")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("p"), 0o644)

	s := newPullServer(t, root)
	mgr := s.deps.SessionCache.GetOrCreate("victim")
	if mgr == nil {
		t.Fatal("expected a manager")
	}

	deleted := time.Now().UTC()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{{AgentKey: "victim", DeletedAt: &deleted, UpdatedAt: deleted}}, nil
	}
	if err := s.pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	if mgr2 := s.deps.SessionCache.GetOrCreate("victim"); mgr2 == mgr {
		t.Error("tombstone pull did not Evict the session cache (stale manager reused)")
	}
}

// Fix 4: when a per-file write fails mid-materialize, stampAgentMtime must be
// SKIPPED so the local mtime stays old and the next pull retries (otherwise the
// half-written agent's mtime == cloud's and the pull never retries).
func TestPullAndApply_PartialWriteFailureSkipsMtimeStamp(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old"), 0o644)

	// Force a write failure: make MEMORY.md a DIRECTORY so the atomic rename in
	// WriteAgentMemory fails when the cloud item carries non-empty memory.
	os.MkdirAll(filepath.Join(dir, "MEMORY.md"), 0o755)

	defTime := time.Now().Add(-1 * time.Hour).UTC()
	stampAgentMtime(dir, defTime)

	mem := "cloud memory"
	cloudTS := time.Now().UTC().Truncate(time.Second)
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "agt", Prompt: "new", Memory: &mem, UpdatedAt: cloudTS},
		}, nil
	}
	if err := newPullServer(t, root).pullAndApplyAgents(pull); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// AGENT.md still got written (best-effort continues), but mtime must NOT be
	// stamped to cloudTS — it must remain ≈ the old definition time so the next
	// pull sees cloud as strictly-newer and retries.
	got := agentLastModified(dir).Truncate(time.Second)
	if !got.Before(cloudTS) {
		t.Errorf("mtime stamped despite partial write failure: got %v, cloudTS %v", got, cloudTS)
	}
}

// Fix 1: a SUCCESSFUL startup pull must close pullDone AND trigger exactly one
// full_sync push (reconciling local-only / locally-newer agents up to Cloud).
func TestRunStartupAgentSync_SuccessTriggersPush(t *testing.T) {
	root := t.TempDir()
	s := newPullServer(t, root)
	s.pullDone = make(chan struct{})
	s.agentSyncTrigger = make(chan struct{}, 1)

	s.runStartupAgentSync(func() ([]client.SyncAgentItem, error) {
		return nil, nil // clean pull, empty mirror
	})

	select {
	case <-s.pullDone:
	default:
		t.Fatal("pullDone not closed after startup sync")
	}
	if got := len(s.agentSyncTrigger); got != 1 {
		t.Fatalf("expected exactly one push trigger after clean pull, got %d", got)
	}
	if !s.agentPullClean.Load() {
		t.Fatal("agentPullClean must be set after a clean pull (else pushes stay upsert-only)")
	}
}

// Fix 1: a FAILED startup pull must still close pullDone (so the worker doesn't
// hang) but must NOT trigger a push — a full_sync over an un-merged local set
// could wrongly soft-delete cloud agents.
func TestRunStartupAgentSync_FailureDoesNotTrigger(t *testing.T) {
	root := t.TempDir()
	s := newPullServer(t, root)
	s.pullDone = make(chan struct{})
	s.agentSyncTrigger = make(chan struct{}, 1)

	s.runStartupAgentSync(func() ([]client.SyncAgentItem, error) {
		return nil, context.DeadlineExceeded
	})

	select {
	case <-s.pullDone:
	default:
		t.Fatal("pullDone not closed after failed pull")
	}
	if got := len(s.agentSyncTrigger); got != 0 {
		t.Fatalf("expected no push trigger after failed pull, got %d", got)
	}
	if s.agentPullClean.Load() {
		t.Fatal("agentPullClean must stay false after a failed pull (pushes must remain upsert-only)")
	}
}

// Fix 1: an UNCONFIGURED gateway (pull == nil) must close pullDone but NOT
// trigger a push.
func TestRunStartupAgentSync_UnconfiguredDoesNotTrigger(t *testing.T) {
	s := &Server{pullDone: make(chan struct{}), agentSyncTrigger: make(chan struct{}, 1)}

	s.runStartupAgentSync(nil)

	select {
	case <-s.pullDone:
	default:
		t.Fatal("pullDone not closed when unconfigured")
	}
	if got := len(s.agentSyncTrigger); got != 0 {
		t.Fatalf("expected no push trigger when unconfigured, got %d", got)
	}
	if s.agentPullClean.Load() {
		t.Fatal("agentPullClean must stay false when Cloud is unconfigured")
	}
}

// Fix 1 (data-loss guard): pushAllAgents must NOT reconcile deletes (full_sync)
// until a clean startup pull has merged the cloud mirror. A failed/never-run
// pull leaves agentPullClean=false, so a push driven by a later user edit goes
// up upsert-only and can't soft-delete cloud-only agents the pull never pulled.
func TestPushAllAgents_GatesFullSyncOnCleanPull(t *testing.T) {
	root := t.TempDir()
	// One local agent so the push is non-empty.
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("prompt"), 0o644)

	var gotFullSync bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var body struct {
				FullSync bool `json:"full_sync"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotFullSync = body.FullSync
			w.Write([]byte(`{"synced":1,"soft_deleted":0}`))
		}
	}))
	defer srv.Close()
	gw := client.NewGatewayClient(srv.URL, "k")

	s := newPullServer(t, root)

	// No clean pull yet → upsert-only (full_sync must be false).
	if err := s.pushAllAgents(context.Background(), gw, root); err != nil {
		t.Fatalf("pushAllAgents (pre-pull): %v", err)
	}
	if gotFullSync {
		t.Fatal("push reconciled deletes (full_sync=true) before a clean pull — can soft-delete cloud-only agents")
	}

	// After a clean pull → full_sync may reconcile deletes.
	s.agentPullClean.Store(true)
	if err := s.pushAllAgents(context.Background(), gw, root); err != nil {
		t.Fatalf("pushAllAgents (post-pull): %v", err)
	}
	if !gotFullSync {
		t.Fatal("push did not reconcile deletes (full_sync=false) after a clean pull")
	}
}

// Fix 2: handleUpdateAgent must acquire the per-agent route lock around its file
// mutations. Hold the lock from another goroutine and assert the update blocks
// until released, then completes (proving it serializes on the same lock and
// does not self-deadlock).
func TestHandleUpdateAgent_SerializesOnRouteLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("old"), 0o644)
	s := newPullServer(t, root)
	s.deps.EventBus = NewEventBus()

	routeKey := "agent:agt"
	s.deps.SessionCache.LockRoute(routeKey)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPut, "/agents/agt",
			strings.NewReader(`{"prompt":"new prompt"}`))
		req.SetPathValue("name", "agt")
		w := httptest.NewRecorder()
		s.handleUpdateAgent(w, req)
		done <- w.Code
	}()

	// While the lock is held the update must not complete.
	select {
	case <-done:
		t.Fatal("handleUpdateAgent completed while route lock was held")
	case <-time.After(150 * time.Millisecond):
	}

	s.deps.SessionCache.UnlockRoute(routeKey)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("update status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleUpdateAgent did not complete after lock released (deadlock?)")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "AGENT.md")); string(got) != "new prompt" {
		t.Errorf("prompt = %q, want %q", got, "new prompt")
	}
}

// Fix 2: handleDeleteAgent must call Evict OUTSIDE the route lock (no
// self-deadlock) and serialize the file removal under the lock. Holding the
// lock blocks the removal; releasing it lets the delete complete.
func TestHandleDeleteAgent_SerializesOnRouteLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("p"), 0o644)
	s := newPullServer(t, root)
	s.deps.EventBus = NewEventBus()

	routeKey := "agent:agt"
	s.deps.SessionCache.LockRoute(routeKey)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete, "/agents/agt?confirm=true", nil)
		req.SetPathValue("name", "agt")
		w := httptest.NewRecorder()
		s.handleDeleteAgent(w, req)
		done <- w.Code
	}()

	select {
	case <-done:
		t.Fatal("handleDeleteAgent completed while route lock was held")
	case <-time.After(150 * time.Millisecond):
	}

	s.deps.SessionCache.UnlockRoute(routeKey)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("delete status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleDeleteAgent did not complete after lock released (deadlock?)")
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENT.md")); !os.IsNotExist(err) {
		t.Errorf("AGENT.md still exists after delete: %v", err)
	}
}
