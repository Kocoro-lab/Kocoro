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

func TestPullAndApply_MaterializesMissingProfileOnly(t *testing.T) {
	root := t.TempDir()

	// Existing local agent must NOT be clobbered.
	existing := filepath.Join(root, "keep")
	os.MkdirAll(existing, 0o755)
	os.WriteFile(filepath.Join(existing, "AGENT.md"), []byte("local prompt"), 0o644)
	os.WriteFile(filepath.Join(existing, "PROFILE.yaml"), []byte("avatar: https://cdn/local.png\n"), 0o644)

	deleted := time.Now().UTC()
	pull := func() ([]client.SyncAgentItem, error) {
		return []client.SyncAgentItem{
			{AgentKey: "fresh", Profile: json.RawMessage(`{"category":"coding","avatar":"https://cdn/fresh.png"}`)},
			{AgentKey: "keep", Profile: json.RawMessage(`{"avatar":"https://cdn/cloud.png"}`)},
			{AgentKey: "gone", DeletedAt: &deleted, Profile: json.RawMessage(`{"avatar":"https://cdn/gone.png"}`)},
		}, nil
	}

	if err := pullAndApplyAgents(pull, root); err != nil {
		t.Fatalf("pullAndApplyAgents: %v", err)
	}

	// fresh: missing locally, not deleted -> materialized
	freshProf, err := agents.LoadAgentProfile(filepath.Join(root, "fresh"))
	if err != nil {
		t.Fatalf("load fresh profile: %v", err)
	}
	if freshProf == nil || freshProf.Avatar != "https://cdn/fresh.png" {
		t.Errorf("fresh profile not materialized: %+v", freshProf)
	}

	// keep: already exists locally -> untouched
	keepProf, err := agents.LoadAgentProfile(existing)
	if err != nil {
		t.Fatalf("load keep profile: %v", err)
	}
	if keepProf == nil || keepProf.Avatar != "https://cdn/local.png" {
		t.Errorf("existing local profile was clobbered: %+v", keepProf)
	}

	// gone: soft-deleted -> not materialized
	if _, err := os.Stat(filepath.Join(root, "gone")); !os.IsNotExist(err) {
		t.Errorf("soft-deleted agent should not be materialized")
	}
}
