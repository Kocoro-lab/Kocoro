package daemon

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
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
				if err := s.pushAllAgents(ctx, gw, s.deps.AgentsDir); err != nil {
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
//
// Each agent's read (LoadAgent + ToAPI/marshal) runs under the SAME per-route
// lock the CRUD handlers and pull take, so a push can't snapshot a cross-file-
// inconsistent agent (e.g. new AGENT.md + old PROFILE.yaml) while a concurrent
// handleUpdateAgent is mid-write. The lock is acquired per-agent (short critical
// section) and only AFTER the builtin-skip check.
func (s *Server) buildSyncItems(agentsDir string) ([]client.SyncAgentItem, error) {
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
		if item, ok := s.buildSyncItem(agentsDir, e.Name); ok {
			items = append(items, item)
		}
	}
	return items, nil
}

// buildSyncItem snapshots a single agent into a SyncAgentItem under the
// per-route lock so the read is internally consistent against concurrent CRUD.
func (s *Server) buildSyncItem(agentsDir, name string) (client.SyncAgentItem, bool) {
	routeKey := "agent:" + name
	s.deps.SessionCache.LockRoute(routeKey)
	defer s.deps.SessionCache.UnlockRoute(routeKey)

	a, err := agents.LoadAgent(agentsDir, name)
	if err != nil {
		log.Printf("agentsync: skipping agent %q: load failed: %v", name, err)
		return client.SyncAgentItem{}, false
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
		log.Printf("agentsync: skipping agent %q: marshal profile: %v", name, err)
		return client.SyncAgentItem{}, false
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

	return client.SyncAgentItem{
		AgentKey:    name,
		DisplayName: api.DisplayName,
		Prompt:      api.Prompt,
		Memory:      api.Memory,
		Config:      config,
		Skills:      skills,
		Profile:     profile,
		UpdatedAt:   agentLastModified(filepath.Join(agentsDir, name)).UTC(),
	}, true
}

// agentDefinitionFiles is the set of files whose mtimes drive the cross-device
// LWW clock. MEMORY.md is deliberately EXCLUDED: it is runtime state mutated by
// the memory_append tool during normal agent runs (a path that does NOT trigger
// a sync), so including it would make local memory churn look like a definition
// edit and silently drop genuine remote profile edits. sessions/ is excluded for
// the same reason. The stamped set (stampAgentMtime) is kept aligned with this.
var agentDefinitionFiles = []string{"AGENT.md", "config.yaml", "PROFILE.yaml", "_attached.yaml"}

// agentLastModified returns the latest ModTime among an agent's DEFINITION
// files, so cross-device LWW reflects the real local definition/presentation
// edit time rather than runtime memory churn or the push time. Falls back to the
// agent dir's own ModTime when no definition file is present.
func agentLastModified(dir string) time.Time {
	var latest time.Time
	for _, f := range agentDefinitionFiles {
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
func (s *Server) pushAllAgents(ctx context.Context, gw *client.GatewayClient, agentsDir string) error {
	start := time.Now().UTC()
	items, err := s.buildSyncItems(agentsDir)
	if err != nil {
		return err
	}
	return gw.SyncAgents(ctx, items, true, start)
}

// runStartupAgentSync runs the one-time startup agent pull, then unblocks the
// agentSyncWorker (always closes pullDone) and — ONLY after a clean pull —
// triggers exactly one full_sync push so local-wins agents (local-only /
// locally-newer) are reconciled up to Cloud.
//
// pull == nil means Cloud is unconfigured: close the gate so the worker doesn't
// hang, but do NOT trigger (a full_sync over an un-merged local set could
// wrongly soft-delete cloud agents). On pull FAILURE the gate is still closed
// but the trigger is likewise skipped, for the same safety reason. pullDone is
// closed BEFORE triggering so the worker can proceed past its gate.
func (s *Server) runStartupAgentSync(pull func() ([]client.SyncAgentItem, error)) {
	if pull == nil {
		close(s.pullDone)
		return
	}
	pullErr := s.pullAndApplyAgents(pull)
	if pullErr != nil {
		log.Printf("agentsync: startup pull failed: %v", pullErr)
	}
	close(s.pullDone)
	if pullErr == nil {
		s.triggerAgentSync()
	}
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
//
// Each per-agent critical section takes the SAME per-route lock the CRUD
// handlers use, so a pull write/delete never races handleCreate/Update/Delete:
//   - materialize/overwrite mirrors handleCreate/Update — wrapped in
//     LockRoute/UnlockRoute("agent:"+key).
//   - tombstone-delete mirrors handleDeleteAgent — calls SessionCache.Evict
//     (which does its OWN per-route locking; wrapping it in LockRoute would
//     self-deadlock on the same entry mutex) then removes the definition files.
func (s *Server) pullAndApplyAgents(pull func() ([]client.SyncAgentItem, error)) error {
	agentsDir := s.deps.AgentsDir
	items, err := pull()
	if err != nil {
		return err
	}
	for _, it := range items {
		// Validate the key before any path construction (path-traversal safety)
		// and before acquiring any lock.
		if err := agents.ValidateAgentName(it.AgentKey); err != nil {
			log.Printf("agentsync: skipping pull of %q: invalid agent key: %v", it.AgentKey, err)
			continue
		}
		dir := filepath.Join(agentsDir, it.AgentKey)
		routeKey := "agent:" + it.AgentKey

		if it.DeletedAt != nil {
			// Tombstone: mirror handleDeleteAgent EXACTLY. Evict the session
			// cache BEFORE removing files so a cloud-originated delete doesn't
			// pull AGENT.md out from under a cached route. Evict does its own
			// per-route locking — it MUST run OUTSIDE LockRoute (self-deadlock).
			// The file removal is then serialized on the per-route lock so it
			// can't interleave with handleCreate/Update/Delete on this agent.
			if _, statErr := os.Stat(dir); statErr == nil {
				s.deps.SessionCache.Evict(it.AgentKey)
				s.deps.SessionCache.LockRoute(routeKey)
				deleteAgentDefinitionFiles(dir)
				s.deps.SessionCache.UnlockRoute(routeKey)
			}
			continue
		}

		// Materialize/overwrite under the same per-route lock the create/update
		// handlers take. Lock per-item (not the whole loop) to keep critical
		// sections short.
		s.deps.SessionCache.LockRoute(routeKey)
		if _, statErr := os.Stat(dir); statErr == nil {
			// LWW: only overwrite when cloud is strictly newer than local.
			if !it.UpdatedAt.After(agentLastModified(dir)) {
				s.deps.SessionCache.UnlockRoute(routeKey)
				continue // local newer or equal — keep local edits.
			}
		}
		materializeAgentFromItem(agentsDir, it)
		s.deps.SessionCache.UnlockRoute(routeKey)
	}
	return nil
}

// materializeAgentFromItem writes (or overwrites) all of an agent's definition
// files from a cloud sync item, then stamps their mtimes to it.UpdatedAt so the
// next push reports the cloud timestamp (LWW stability — no ping-pong).
//
// It is best-effort per-file (logs + continues on a single write error). If ANY
// write failed the agent is half-written, so the mtime stamp is SKIPPED — the
// local mtime stays old and the next pull (cloud still strictly-newer) retries.
// Stamping a half-written agent to it.UpdatedAt would make the next pull see
// "equal, not strictly newer" and never retry until Cloud bumps UpdatedAt.
//
// Note: SyncAgentItem.DisplayName is intentionally NOT applied here — the
// display name is sourced from the config blob (AgentConfigAPI.DisplayName), so
// the top-level field would be redundant/conflicting.
func materializeAgentFromItem(agentsDir string, it client.SyncAgentItem) {
	writeFailed := false

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
		writeFailed = true
	}

	// config.yaml — decode the same AgentConfigAPI shape the create handler
	// uses. Overwrite is authoritative (cloud is strictly newer). Detect a
	// CLEARED config SEMANTICALLY (not by exact bytes): empty/JSON-null bytes,
	// OR bytes that unmarshal to a zero-value AgentConfigAPI (whitespace, "{}",
	// key reorder, trailing newline). On a real clear, remove the local file. On
	// a decode error, prefer leaving the existing config untouched + log rather
	// than wiping it silently (mirrors the skills clear-path robustness).
	configPath := filepath.Join(agentsDir, it.AgentKey, "config.yaml")
	if len(it.Config) == 0 || isJSONNull(it.Config) {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			log.Printf("agentsync: remove cleared config for %q failed: %v", it.AgentKey, err)
			writeFailed = true
		}
	} else {
		var cfg agents.AgentConfigAPI
		if err := json.Unmarshal(it.Config, &cfg); err != nil {
			// Malformed-but-present config is NOT a clear signal — leave the
			// existing config untouched rather than wiping it silently.
			log.Printf("agentsync: pull of %q: config decode (existing kept): %v", it.AgentKey, err)
		} else if reflect.DeepEqual(cfg, agents.AgentConfigAPI{}) {
			// Semantically empty → field was cleared on the originating device.
			if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
				log.Printf("agentsync: remove cleared config for %q failed: %v", it.AgentKey, err)
				writeFailed = true
			}
		} else if err := agents.WriteAgentConfig(agentsDir, it.AgentKey, &cfg); err != nil {
			log.Printf("agentsync: write config for %q failed: %v", it.AgentKey, err)
			writeFailed = true
		}
	}

	// memory — MEMORY.md. Empty/absent means the field was cleared → remove.
	if it.Memory == nil || *it.Memory == "" {
		if err := os.Remove(filepath.Join(agentsDir, it.AgentKey, "MEMORY.md")); err != nil && !os.IsNotExist(err) {
			log.Printf("agentsync: remove cleared memory for %q failed: %v", it.AgentKey, err)
			writeFailed = true
		}
	} else if err := agents.WriteAgentMemory(agentsDir, it.AgentKey, *it.Memory); err != nil {
		log.Printf("agentsync: write memory for %q failed: %v", it.AgentKey, err)
		writeFailed = true
	}

	// skills — attached-skills manifest. The blob mirrors api.Skills
	// ([]skills.SkillMeta); SetAttachedSkills wants the slug (fallback name).
	// An empty / JSON null / empty-array skills field means skills were cleared
	// → SetAttachedSkills([]) removes _attached.yaml (DeleteAttachedSkills).
	var skillNames []string
	skillsClear := true
	if len(it.Skills) > 0 && !isJSONNull(it.Skills) {
		var metas []skills.SkillMeta
		if err := json.Unmarshal(it.Skills, &metas); err != nil {
			// Malformed-but-present skills is NOT a clear signal — leave the
			// existing manifest untouched rather than wiping attached skills.
			log.Printf("agentsync: pull of %q: skills decode (skipped): %v", it.AgentKey, err)
			skillsClear = false
		} else {
			for _, m := range metas {
				ident := m.Slug
				if ident == "" {
					ident = m.Name
				}
				if ident != "" {
					skillNames = append(skillNames, ident)
				}
			}
		}
	}
	if skillsClear || len(skillNames) > 0 {
		if err := agents.SetAttachedSkills(agentsDir, it.AgentKey, skillNames); err != nil {
			log.Printf("agentsync: write skills for %q failed: %v", it.AgentKey, err)
			writeFailed = true
		}
	}

	// A partial write must leave the LWW clock STRICTLY BEFORE it.UpdatedAt so
	// the next pull still sees cloud as strictly-newer and retries the
	// half-written agent. Simply skipping the stamp is not enough: the files we
	// did rewrite carry mtime≈now (>= cloud's), which would make the next pull
	// see "not strictly newer" and never retry until Cloud bumps UpdatedAt.
	// Stamp definition files just before the cloud timestamp instead.
	if writeFailed {
		log.Printf("agentsync: pull of %q: partial write — backdating mtime so next pull retries", it.AgentKey)
		if !it.UpdatedAt.IsZero() {
			stampAgentMtime(filepath.Join(agentsDir, it.AgentKey), it.UpdatedAt.Add(-time.Second))
		}
		return
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
	// Stamp only the definition files (the LWW set) — MEMORY.md is runtime state
	// in its own lane and is deliberately not part of the LWW clock.
	for _, f := range agentDefinitionFiles {
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
