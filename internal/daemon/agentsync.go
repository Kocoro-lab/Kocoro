package daemon

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// agentSyncWorker coalesces agent-change notifications into serialized,
// debounced full pushes to Cloud. One push at a time, latest state wins.
func (s *Server) agentSyncWorker(ctx context.Context) {
	// Gate the first push on the startup pull completing. Without this a
	// create/update/delete that arrives before the initial pull finishes would
	// trigger a full_sync push over an incomplete local set, soft-deleting
	// cloud agents that simply hadn't been pulled yet. pullDone is always
	// closed by Start() (success OR failure), so this never blocks forever.
	select {
	case <-ctx.Done():
		return
	case <-s.pullDone:
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.agentSyncTrigger:
			// debounce: collect a burst of changes into one push
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			// drain any extra triggers that arrived during the debounce
			select {
			case <-s.agentSyncTrigger:
			default:
			}
			if gw := s.cloudGateway(); gw != nil {
				if err := pushAllAgents(ctx, gw, s.deps.AgentsDir); err != nil {
					log.Printf("agentsync: push failed: %v", err)
				}
			}
		}
	}
}

// triggerAgentSync requests a coalesced push from agentSyncWorker. Non-blocking:
// if a push is already pending, the trigger is dropped (the pending push will
// pick up the latest state).
func (s *Server) triggerAgentSync() {
	select {
	case s.agentSyncTrigger <- struct{}{}:
	default: // a push is already pending; coalesced
	}
}

// agentProfileBlob is the JSON shape carried in SyncAgentItem.Profile. It packs
// the user-facing presentation metadata (incl. avatar) so the avatar rides
// inside `profile` rather than as a top-level sync field. Keys mirror the
// PROFILE.yaml field names.
type agentProfileBlob struct {
	Category     string                 `json:"category,omitempty"`
	Description  agents.LocalizedString `json:"description,omitempty"`
	GuidePrompts []agents.GuidePrompt   `json:"guide_prompts,omitempty"`
	Examples     []agents.AgentExample  `json:"examples,omitempty"`
	Avatar       string                 `json:"avatar,omitempty"`
}

// buildSyncItems lists local agents and packs each into a SyncAgentItem. The
// avatar is carried inside the `profile` JSON blob. Agents that fail to load
// are logged and skipped rather than failing the whole push. UpdatedAt is set
// to the agent's real last-modified time so cross-device LWW is meaningful.
func buildSyncItems(agentsDir string) ([]client.SyncAgentItem, error) {
	entries, err := agents.ListAgents(agentsDir)
	if err != nil {
		return nil, err
	}
	items := make([]client.SyncAgentItem, 0, len(entries))
	for _, e := range entries {
		// Only sync user-defined agents (and user-overridden builtins, which
		// carry user edits). Pure builtins live in the app bundle and must not
		// be pushed to Cloud.
		if e.Builtin && !e.Override {
			continue
		}
		a, err := agents.LoadAgent(agentsDir, e.Name)
		if err != nil {
			log.Printf("agentsync: skipping agent %q: load failed: %v", e.Name, err)
			continue
		}
		api := a.ToAPI()

		var category string
		if api.Category != nil {
			category = api.Category.Code
		}
		profile, err := json.Marshal(agentProfileBlob{
			Category:     category,
			Description:  api.Description,
			GuidePrompts: api.GuidePrompts,
			Examples:     api.Examples,
			Avatar:       api.Avatar,
		})
		if err != nil {
			log.Printf("agentsync: skipping agent %q: marshal profile: %v", e.Name, err)
			continue
		}

		var config json.RawMessage
		if api.Config != nil {
			if b, err := json.Marshal(api.Config); err == nil {
				config = b
			}
		}
		var skills json.RawMessage
		if api.Skills != nil {
			if b, err := json.Marshal(api.Skills); err == nil {
				skills = b
			}
		}

		items = append(items, client.SyncAgentItem{
			AgentKey:    e.Name,
			DisplayName: api.DisplayName,
			Prompt:      api.Prompt,
			Memory:      api.Memory,
			Config:      config,
			Skills:      skills,
			Profile:     profile,
			UpdatedAt:   agentLastModified(filepath.Join(agentsDir, e.Name)).UTC(),
		})
	}
	return items, nil
}

// agentLastModified returns the latest ModTime among an agent's definition
// files, so cross-device LWW reflects the real local edit time rather than the
// push time. Falls back to the agent dir's own ModTime when no definition file
// is present.
func agentLastModified(dir string) time.Time {
	var latest time.Time
	for _, f := range []string{"AGENT.md", "config.yaml", "PROFILE.yaml", "_attached.yaml", "MEMORY.md"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil {
			if fi.ModTime().After(latest) {
				latest = fi.ModTime()
			}
		}
	}
	if latest.IsZero() {
		if fi, err := os.Stat(dir); err == nil {
			latest = fi.ModTime()
		}
	}
	return latest
}

// pushAllAgents builds the full local agent set and pushes it to Cloud as a
// full sync (so deletes are reconciled). The sync_started_at timestamp is
// captured BEFORE the local snapshot so Cloud's gated full_sync soft-delete
// only removes agents whose cloud updated_at <= that instant — agents created
// on cloud after this snapshot are not clobbered.
func pushAllAgents(ctx context.Context, gw *client.GatewayClient, agentsDir string) error {
	start := time.Now().UTC()
	items, err := buildSyncItems(agentsDir)
	if err != nil {
		return err
	}
	return gw.SyncAgents(ctx, items, true, start)
}

// pullAndApplyAgents applies the cloud agent mirror to local disk as a true
// bidirectional last-writer-wins (LWW) reconciliation:
//
//   - Tombstone (DeletedAt != nil): if the agent exists locally, delete its
//     definition files (mirroring handleDeleteAgent's removal set). Missing
//     locally → nothing to do.
//   - Live, missing locally → fully materialize (AGENT.md, PROFILE.yaml,
//     config.yaml, MEMORY.md, attached-skills manifest).
//   - Live, exists locally → LWW: overwrite from cloud only when the cloud
//     UpdatedAt is strictly newer than the local last-modified time; otherwise
//     keep the local edits (never clobber a locally-newer agent).
//
// After materializing/overwriting, the written files' mtimes are stamped to the
// cloud item's UpdatedAt so the next buildSyncItems reports that timestamp (not
// "now") — without this the freshly-written agent would falsely win the next
// LWW round and ping-pong. The pull function is injected for testability.
func pullAndApplyAgents(pull func() ([]client.SyncAgentItem, error), agentsDir string) error {
	items, err := pull()
	if err != nil {
		return err
	}
	for _, it := range items {
		// Validate the key before any path construction (path-traversal safety).
		if err := agents.ValidateAgentName(it.AgentKey); err != nil {
			log.Printf("agentsync: skipping pull of %q: invalid agent key: %v", it.AgentKey, err)
			continue
		}
		dir := filepath.Join(agentsDir, it.AgentKey)
		_, statErr := os.Stat(dir)
		existsLocally := statErr == nil

		if it.DeletedAt != nil {
			if existsLocally {
				deleteAgentDefinitionFiles(dir)
			}
			continue
		}

		if existsLocally {
			// LWW: only overwrite when cloud is strictly newer than local.
			if !it.UpdatedAt.After(agentLastModified(dir)) {
				continue // local newer or equal — keep local edits.
			}
		}

		materializeAgentFromItem(agentsDir, it)
	}
	return nil
}

// materializeAgentFromItem writes (or overwrites) all of an agent's definition
// files from a cloud sync item, then stamps their mtimes to it.UpdatedAt so the
// next push reports the cloud timestamp (LWW stability — no ping-pong).
func materializeAgentFromItem(agentsDir string, it client.SyncAgentItem) {
	// AGENT.md is MANDATORY — it is what makes the agent enumerable. Without
	// it the agent is invisible to ListAgents and the next full push would
	// soft-delete it on the cloud.
	if err := agents.WriteAgentPrompt(agentsDir, it.AgentKey, it.Prompt); err != nil {
		log.Printf("agentsync: write prompt for %q failed: %v", it.AgentKey, err)
		return
	}

	// PROFILE.yaml — presentation metadata (avatar/category/...).
	var blob agentProfileBlob
	if len(it.Profile) > 0 {
		if err := json.Unmarshal(it.Profile, &blob); err != nil {
			log.Printf("agentsync: pull of %q: profile decode: %v", it.AgentKey, err)
		}
	}
	// Avatar is the only profile field carrying a URL; validate it before
	// writing. On failure drop just the avatar (keep the rest of the agent).
	if err := agents.ValidateAvatarURL(blob.Avatar); err != nil {
		log.Printf("agentsync: pull of %q: dropping invalid avatar: %v", it.AgentKey, err)
		blob.Avatar = ""
	}
	profile := &agents.AgentProfile{
		Category:     blob.Category,
		Avatar:       blob.Avatar,
		Description:  blob.Description,
		GuidePrompts: blob.GuidePrompts,
		Examples:     blob.Examples,
	}
	if err := agents.WriteAgentProfile(agentsDir, it.AgentKey, profile); err != nil {
		log.Printf("agentsync: write profile for %q failed: %v", it.AgentKey, err)
	}

	// config.yaml — decode the same AgentConfigAPI shape the create handler
	// uses. Skip empty / JSON null / empty-object configs.
	if len(it.Config) > 0 && !isJSONNull(it.Config) && string(it.Config) != "{}" {
		var cfg agents.AgentConfigAPI
		if err := json.Unmarshal(it.Config, &cfg); err != nil {
			log.Printf("agentsync: pull of %q: config decode (skipped): %v", it.AgentKey, err)
		} else if err := agents.WriteAgentConfig(agentsDir, it.AgentKey, &cfg); err != nil {
			log.Printf("agentsync: write config for %q failed: %v", it.AgentKey, err)
		}
	}

	// memory — MEMORY.md.
	if it.Memory != nil && *it.Memory != "" {
		if err := agents.WriteAgentMemory(agentsDir, it.AgentKey, *it.Memory); err != nil {
			log.Printf("agentsync: write memory for %q failed: %v", it.AgentKey, err)
		}
	}

	// skills — attached-skills manifest. The blob mirrors api.Skills
	// ([]skills.SkillMeta); SetAttachedSkills wants the slug (fallback name).
	if len(it.Skills) > 0 && !isJSONNull(it.Skills) {
		var metas []skills.SkillMeta
		if err := json.Unmarshal(it.Skills, &metas); err != nil {
			log.Printf("agentsync: pull of %q: skills decode (skipped): %v", it.AgentKey, err)
		} else {
			names := make([]string, 0, len(metas))
			for _, m := range metas {
				ident := m.Slug
				if ident == "" {
					ident = m.Name
				}
				if ident != "" {
					names = append(names, ident)
				}
			}
			if len(names) > 0 {
				if err := agents.SetAttachedSkills(agentsDir, it.AgentKey, names); err != nil {
					log.Printf("agentsync: write skills for %q failed: %v", it.AgentKey, err)
				}
			}
		}
	}

	// Stamp mtimes to the cloud timestamp so this agent reports UpdatedAt ==
	// it.UpdatedAt on the next push (LWW no-op) rather than "now".
	stampAgentMtime(filepath.Join(agentsDir, it.AgentKey), it.UpdatedAt)
}

// stampAgentMtime sets the modification time of an agent's definition files
// (and the dir) to t. Best-effort — a failed Chtimes is logged-silently (the
// only consequence is one extra LWW round), never aborting the materialize.
func stampAgentMtime(dir string, t time.Time) {
	if t.IsZero() {
		return
	}
	for _, f := range []string{"AGENT.md", "config.yaml", "PROFILE.yaml", "_attached.yaml", "MEMORY.md"} {
		p := filepath.Join(dir, f)
		if _, err := os.Stat(p); err == nil {
			_ = os.Chtimes(p, t, t)
		}
	}
	_ = os.Chtimes(dir, t, t)
}

// deleteAgentDefinitionFiles removes an agent's definition files, mirroring
// handleDeleteAgent's removal set EXACTLY: AGENT.md, config.yaml, _attached.yaml,
// PROFILE.yaml plus the commands/ and skills/ dirs. Runtime state (MEMORY.md,
// sessions/) is preserved so a builtin can resurface with history intact. The
// dir itself is removed only when nothing remains. Best-effort: per-file errors
// are logged and do not abort the rest of the removal.
func deleteAgentDefinitionFiles(dir string) {
	for _, f := range []string{"AGENT.md", "config.yaml", "_attached.yaml", "PROFILE.yaml"} {
		p := filepath.Join(dir, f)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("agentsync: tombstone remove %q failed: %v", p, err)
		}
	}
	for _, d := range []string{"commands", "skills"} {
		p := filepath.Join(dir, d)
		if err := os.RemoveAll(p); err != nil {
			log.Printf("agentsync: tombstone remove %q failed: %v", p, err)
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}
