package daemon

import (
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

func TestBuildSyncItems_IncludesAvatarInProfile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte("category: coding\navatar: https://cdn/a.png\n"), 0o644)

	items, err := buildSyncItems(root)
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

	items, err := buildSyncItems(root)
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
	cwd := t.TempDir()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{
				AgentKey: "fresh",
				Prompt:   "fresh prompt",
				Memory:   &mem,
				Config:   json.RawMessage(`{"cwd":"` + cwd + `"}`),
				Profile:  json.RawMessage(`{"category":"coding","avatar":"https://cdn/fresh.png"}`),
			},
			{AgentKey: "keep", Prompt: "cloud prompt", Profile: json.RawMessage(`{"avatar":"https://cdn/cloud.png"}`)},
			{AgentKey: "gone", DeletedAt: &deleted, Prompt: "gone prompt", Profile: json.RawMessage(`{"avatar":"https://cdn/gone.png"}`)},
		}, nil
	}

	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if a.Config == nil || a.Config.CWD != cwd {
		t.Errorf("fresh config not materialized: %+v", a.Config)
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

	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if err := pullAndApplyAgents(pull, root); err != nil {
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
	if err := pullAndApplyAgents(pull, root); err != nil {
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

	items, err := buildSyncItems(root)
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
