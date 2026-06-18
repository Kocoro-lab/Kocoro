package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
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
