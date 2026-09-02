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
	"gopkg.in/yaml.v3"
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
		// cwd is a device-local absolute path. Never upload it: a path valid on
		// this Mac can make the same agent unusable after another device pulls.
		syncConfig := *api.Config
		syncConfig.CWD = ""
		if !reflect.DeepEqual(syncConfig, agents.AgentConfigAPI{}) {
			if b, err := json.Marshal(&syncConfig); err == nil {
				config = b
			}
		}
	}
	syncedSkills, err := syncedAgentSkills(agentsDir, name, api.Skills)
	if err != nil {
		log.Printf("agentsync: skipping agent %q: attachment snapshot failed: %v", name, err)
		s.auditAgentSyncFailure(name, "attachment snapshot failed", err)
		return client.SyncAgentItem{}, false
	}

	return client.SyncAgentItem{
		AgentKey:    name,
		DisplayName: api.DisplayName,
		Prompt:      api.Prompt,
		Memory:      api.Memory,
		Config:      config,
		Skills:      syncedSkills,
		Profile:     profile,
		UpdatedAt:   agentLastModified(filepath.Join(agentsDir, name)).UTC(),
	}, true
}

// syncedAgentSkills serializes the attachment manifest itself, not only the
// subset that resolves against this device's installed skill inventory. A
// second device may not have installed a remotely attached skill yet; dropping
// that unresolved identifier from an outbound sync would turn local absence
// into a cross-device detach. Resolved entries retain their full metadata for
// compatibility, while unresolved entries round-trip by slug/name.
func syncedAgentSkills(agentsDir, agentName string, resolved []skills.SkillMeta) (json.RawMessage, error) {
	manifestPath := filepath.Join(agentsDir, agentName, "_attached.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	attached, err := agents.ReadAttachedSkills(agentsDir, agentName)
	if err != nil {
		return nil, err
	}
	byIdentifier := make(map[string]skills.SkillMeta, len(resolved)*2)
	for _, meta := range resolved {
		byIdentifier[meta.Slug] = meta
		byIdentifier[meta.Name] = meta
	}
	metas := make([]skills.SkillMeta, 0, len(attached))
	for _, identifier := range attached {
		if meta, ok := byIdentifier[identifier]; ok {
			metas = append(metas, meta)
			continue
		}
		meta := skills.SkillMeta{Name: identifier}
		if skills.ValidateSkillName(identifier) == nil {
			meta.Slug = identifier
		}
		metas = append(metas, meta)
	}
	b, err := json.Marshal(metas)
	if err != nil {
		return nil, err
	}
	return b, nil
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

// pushAllAgents builds the full local agent set and pushes it to Cloud. Deletes
// are reconciled (full_sync=true) ONLY once a clean startup pull has merged the
// cloud mirror into the local set (s.agentPullClean) — before that, the push is
// upsert-only so a push over a never-merged set can't soft-delete cloud-only
// agents the failed/never-run pull never brought down. The sync_started_at
// timestamp is captured BEFORE the local snapshot so Cloud's gated full_sync
// soft-delete only removes agents whose cloud updated_at <= that instant —
// agents created on cloud after this snapshot are not clobbered.
func (s *Server) pushAllAgents(ctx context.Context, gw *client.GatewayClient, agentsDir string) error {
	start := time.Now().UTC()
	items, err := s.buildSyncItems(agentsDir)
	if err != nil {
		return err
	}
	// Known residual TOCTOU: a principal transition landing between this Load
	// and the HTTP dispatch can still send full_sync=true under the new
	// account's key. Closing it fully would need a principal-generation lease
	// spanning the push (the integration-tools pattern); the window is
	// milliseconds and accepted.
	fullSync := s.agentPullClean.Load()
	res, err := gw.SyncAgents(ctx, items, fullSync, start)
	if err != nil {
		return err
	}
	// Surface the destructive count: a push that tombstones cloud agents should
	// never be silent (this is the observability that exposes an unexpected
	// mass-delete in the daemon log).
	if res != nil && res.SoftDeleted > 0 {
		log.Printf("agentsync: WARNING: push synced %d agent(s), soft-deleted %d on cloud (full_sync=%v)", res.Synced, res.SoftDeleted, fullSync)
	}
	return nil
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
//
// Only a SUCCESSFUL pull flips s.agentPullClean — until then every push (incl.
// pushes driven by later user edits, which run regardless of pull outcome) goes
// up as upsert-only so it can't soft-delete cloud-only agents. The flag is set
// BEFORE pullDone closes so the worker always observes the reconciled state.
//
// The restore is principal-guarded: a verified-principal transition landing
// while this pull is in flight means the merged local set belongs to the OLD
// principal — an unguarded Store(true) would overwrite the transition's reset
// (beginAgentSyncPrincipalTransition) and the post-pull full_sync push would go
// out under the NEW account's hot-swapped key carrying the old account's set.
// The new principal's own resync re-earns the license instead.
func (s *Server) runStartupAgentSync(pull func() ([]client.SyncAgentItem, error)) {
	if pull == nil {
		close(s.pullDone)
		return
	}
	samePrincipal := s.principalUnchangedGuard()
	items, pullErr := pull()
	restored := false
	if pullErr != nil {
		log.Printf("agentsync: startup pull failed: %v", pullErr)
	} else if !samePrincipal() {
		// Superseded while the pull was in flight: discard the old account's
		// mirror before it touches disk — materialized old-account agents
		// would ride the next full-sync push into the new account.
		log.Printf("agentsync: startup pull superseded by a principal transition; mirror discarded, full sync deferred to the new principal's resync")
	} else {
		s.applyPulledAgents(items)
		if samePrincipal() {
			s.agentPullClean.Store(true)
			restored = true
		} else {
			log.Printf("agentsync: startup pull superseded by a principal transition; full sync deferred to the new principal's resync")
		}
	}
	close(s.pullDone)
	if restored {
		s.triggerAgentSync()
	}
}

// principalUnchangedGuard snapshots the verified principal (id, epoch,
// verified-ness) and returns a func reporting whether it is still identical.
// With no AuthManager (legacy yaml-key platforms) no principal transition can
// occur, so the guard is constantly true. The epoch advances on EVERY verified-
// principal transition (including sign-out and forced same-account re-login),
// so a change-and-change-back cannot satisfy the guard.
func (s *Server) principalUnchangedGuard() func() bool {
	auth := s.auth
	if auth == nil {
		return func() bool { return true }
	}
	id, epoch, ok := auth.VerifiedPrincipal()
	return func() bool {
		curID, curEpoch, curOK := auth.VerifiedPrincipal()
		return curID == id && curEpoch == epoch && curOK == ok
	}
}

// beginAgentSyncPrincipalTransition is called from the verified-principal
// change handler (SetAuth). It synchronously closes the destructive-push gate:
// the startup pull's full-sync license was earned under the PREVIOUS principal,
// so after a sign-in / account switch / sign-out every push must degrade to
// upsert-only until a pull for the NEW principal has merged that account's
// cloud mirror — otherwise the first agent CRUD after the switch would push
// this device's local set with full_sync=true and soft-delete the new
// account's cloud-only agents.
//
// For a non-empty new principal it then re-runs the pull-then-push
// reconciliation asynchronously (never inside the auth mutation critical
// section — the pull is a network call). Isolated processes never resync:
// they suppress cloud background automation entirely.
func (s *Server) beginAgentSyncPrincipalTransition(current string) {
	s.agentPullClean.Store(false)
	if current == "" || s.isolated {
		return
	}
	go s.resyncAgentsForVerifiedPrincipal()
}

// resyncAgentsForVerifiedPrincipal is the production glue for the post-switch
// resync: it waits for the startup pull to settle (pullDone closes exactly
// once, success or failure), binds the pull to the currently verified
// principal epoch, and delegates to the testable core. A pull that outlives
// its principal (another switch while queued or in flight) must never restore
// the full-sync license, so the epoch is re-checked around the pull.
func (s *Server) resyncAgentsForVerifiedPrincipal() {
	<-s.pullDone
	auth := s.auth
	gw := s.cloudGateway()
	if auth == nil || gw == nil {
		return
	}
	id, epoch, ok := auth.VerifiedPrincipal()
	if !ok {
		return
	}
	s.resyncAgentsAfterPrincipalChange(
		// Deliberately not tied to the server lifecycle context: the pull is
		// bounded by the gateway HTTP client's own timeout and never blocks
		// Server.Shutdown; cancelling it on shutdown would buy nothing.
		func() ([]client.SyncAgentItem, error) { return gw.PullAgents(context.Background()) },
		func() bool {
			curID, curEpoch, curOK := auth.VerifiedPrincipal()
			return curOK && curID == id && curEpoch == epoch
		},
	)
}

// resyncAgentsAfterPrincipalChange mirrors runStartupAgentSync for a
// mid-process principal transition: pull the new principal's cloud mirror,
// and ONLY on a clean pull restore the full-sync license and queue one push
// (which reconciles local-wins agents up to the new account). Failure keeps
// pushes upsert-only — safe, never destructive. samePrincipal is re-checked
// between the fetch and the disk writes so a transition that landed while the
// pull was in flight discards the fetched mirror — old-account agents put on
// disk would ride the next full-sync push into the new account (the residual
// window is a mid-APPLY transition: local file writes, milliseconds, no
// rollback mechanism exists). A final check guards the license restore.
// Resyncs are serialized so back-to-back switches cannot interleave.
func (s *Server) resyncAgentsAfterPrincipalChange(pull func() ([]client.SyncAgentItem, error), samePrincipal func() bool) {
	s.agentResyncMu.Lock()
	defer s.agentResyncMu.Unlock()
	if !samePrincipal() {
		return
	}
	items, err := pull()
	if err != nil {
		log.Printf("agentsync: principal resync pull failed (pushes stay upsert-only): %v", err)
		return
	}
	if !samePrincipal() {
		return // superseded while the pull was in flight — discard the stale mirror
	}
	s.applyPulledAgents(items)
	if !samePrincipal() {
		return
	}
	s.agentPullClean.Store(true)
	s.triggerAgentSync()
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
// Callers that must interpose a check between the fetch and the disk writes
// (the principal-guarded resync/startup paths) call pull and applyPulledAgents
// separately instead.
func (s *Server) pullAndApplyAgents(pull func() ([]client.SyncAgentItem, error)) error {
	items, err := pull()
	if err != nil {
		return err
	}
	s.applyPulledAgents(items)
	return nil
}

// applyPulledAgents is the disk-write half of the pull reconciliation. Each
// per-agent critical section takes the SAME per-route lock the CRUD handlers
// use, so a pull write/delete never races handleCreate/Update/Delete:
//   - materialize/overwrite mirrors handleCreate/Update — wrapped in
//     LockRoute/UnlockRoute("agent:"+key).
//   - tombstone-delete mirrors handleDeleteAgent — calls SessionCache.Evict
//     (which does its OWN per-route locking; wrapping it in LockRoute would
//     self-deadlock on the same entry mutex) then removes the definition files.
//
// Per-agent failures are logged and never abort the rest of the mirror.
func (s *Server) applyPulledAgents(items []client.SyncAgentItem) {
	agentsDir := s.deps.AgentsDir
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

		syncSkillNames, writeSyncedSkills := decodeSyncedSkillNames(it.AgentKey, it.Skills)
		unlockSkills := s.lockSkillIdentifiers(syncSkillNames)

		// Materialize/overwrite under the same per-route lock the create/update
		// handlers take. Skill locks are acquired first, matching the global
		// delete path's slug -> route order, so a pull cannot reattach a skill
		// after its directory was removed.
		s.deps.SessionCache.LockRoute(routeKey)
		if _, statErr := os.Stat(dir); statErr == nil {
			// LWW: only overwrite when cloud is strictly newer than local.
			if !it.UpdatedAt.After(agentLastModified(dir)) {
				s.deps.SessionCache.UnlockRoute(routeKey)
				unlockSkills()
				continue // local newer or equal — keep local edits.
			}
		}
		materializeAgentFromItem(agentsDir, it, syncSkillNames, writeSyncedSkills,
			s.dropRegistryDeniedAlwaysAllow)
		s.deps.SessionCache.UnlockRoute(routeKey)
		unlockSkills()
	}
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
// sanitizePerms applies the registry-based always-allow drop
// (Server.dropRegistryDeniedAlwaysAllow) to the pulled permissions before the
// full-replace config write — a config pushed by an older device can carry a
// requires_approval integration grant the runtime will never honor. nil skips
// the dynamic filter (the static sanitize inside WriteAgentConfig still runs).
func materializeAgentFromItem(agentsDir string, it client.SyncAgentItem, skillNames []string, writeSyncedSkills bool, sanitizePerms func(*agents.AgentPermissionsConfig)) {
	writeFailed := false
	permsDropped := false

	// Capture the local cwd before any writes. It is device-local state: remote
	// config (including payloads produced by older daemons) must never replace
	// it, and a remote config clear must preserve it.
	localCWD := readDeviceLocalAgentCWD(agentsDir, it.AgentKey)

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

	// config.yaml — cloud remains authoritative for syncable fields, while cwd
	// is always restored from this device. Detect a CLEARED config SEMANTICALLY
	// (not by exact bytes): empty/JSON-null bytes, OR bytes that unmarshal to a
	// zero-value AgentConfigAPI. On a decode error, leave the existing config
	// untouched rather than wiping it silently.
	configPath := filepath.Join(agentsDir, it.AgentKey, "config.yaml")
	clearSyncedConfig := func() {
		if localCWD != "" {
			if err := agents.WriteAgentConfig(agentsDir, it.AgentKey, &agents.AgentConfigAPI{CWD: localCWD}); err != nil {
				log.Printf("agentsync: preserve local cwd for %q failed: %v", it.AgentKey, err)
				writeFailed = true
			}
			return
		}
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			log.Printf("agentsync: remove cleared config for %q failed: %v", it.AgentKey, err)
			writeFailed = true
		}
	}
	if len(it.Config) == 0 || isJSONNull(it.Config) {
		clearSyncedConfig()
	} else {
		var cfg agents.AgentConfigAPI
		if err := json.Unmarshal(it.Config, &cfg); err != nil {
			// Malformed-but-present config is NOT a clear signal — leave the
			// existing config untouched rather than wiping it silently.
			log.Printf("agentsync: pull of %q: config decode (existing kept): %v", it.AgentKey, err)
		} else {
			// Discard remote cwd even if this payload came from an older daemon
			// that still synced it, then merge this device's value back in.
			cfg.CWD = ""
			if reflect.DeepEqual(cfg, agents.AgentConfigAPI{}) {
				clearSyncedConfig()
			} else {
				if sanitizePerms != nil && cfg.Permissions != nil {
					before := len(cfg.Permissions.AlwaysAllowTools)
					sanitizePerms(cfg.Permissions)
					if len(cfg.Permissions.AlwaysAllowTools) != before {
						permsDropped = true
					}
				}
				// The STATIC sanitize inside WriteAgentConfig (legacy GUI
				// names) diverges the written config from Cloud's copy the
				// same way. Detect it up front via the documented contract:
				// SanitizeAgentPermissionsConfig returns the original pointer
				// when nothing needs dropping.
				if cleaned := agents.SanitizeAgentPermissionsConfig(cfg.Permissions); cleaned != cfg.Permissions {
					cfg.Permissions = cleaned
					permsDropped = true
				}
				cfg.CWD = localCWD
				if err := agents.WriteAgentConfig(agentsDir, it.AgentKey, &cfg); err != nil {
					log.Printf("agentsync: write config for %q failed: %v", it.AgentKey, err)
					writeFailed = true
				}
			}
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

	// skills — decoded before locking, then installation-filtered while the
	// matching slug and route locks are held. A malformed-but-present blob
	// leaves the existing manifest untouched.
	if writeSyncedSkills {
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

	// A sanitize drop makes the written config diverge from Cloud's copy, so
	// the mirror stamp below would be a lie: at an equal timestamp the
	// post-pull push is rejected by Cloud's strict-newer upsert and the stale
	// cloud row keeps reseeding other devices. Leave the LWW clock at "now"
	// (the drop is a real local mutation) so that push is strictly newer and
	// converges Cloud to the sanitized config. Convergence rides the single
	// post-pull push in runStartupAgentSync — if that push fails, the cloud
	// row stays stale until the next restart or agent edit (locally we are
	// correct either way: a "now" mtime wins the next pull's LWW check unless
	// this device's clock runs behind Cloud's UpdatedAt, which only costs a
	// harmless re-materialize-and-drop on the next startup).
	if !permsDropped {
		// Stamp mtimes to the cloud timestamp so this agent reports UpdatedAt
		// == it.UpdatedAt on the next push (LWW no-op) rather than "now".
		stampAgentMtime(filepath.Join(agentsDir, it.AgentKey), it.UpdatedAt)
	}
}

// readDeviceLocalAgentCWD reads only the active definition's config.yaml so
// preserving cwd does not depend on unrelated prompt/profile/skills parsing.
// It mirrors LoadAgent's user-definition-first, builtin-fallback resolution.
func readDeviceLocalAgentCWD(agentsDir, name string) string {
	dir := filepath.Join(agentsDir, name)
	if _, err := os.Stat(filepath.Join(dir, "AGENT.md")); err != nil {
		dir = filepath.Join(agentsDir, "_builtin", name)
		if _, err := os.Stat(filepath.Join(dir, "AGENT.md")); err != nil {
			return ""
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return ""
	}
	var cfg struct {
		CWD string `yaml:"cwd"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.CWD
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

func decodeSyncedSkillNames(agentKey string, raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, true
	}
	var metas []skills.SkillMeta
	if err := json.Unmarshal(raw, &metas); err != nil {
		log.Printf("agentsync: pull of %q: skills decode (skipped): %v", agentKey, err)
		return nil, false
	}
	names := make([]string, 0, len(metas))
	for _, meta := range metas {
		ident := meta.Slug
		if ident == "" {
			ident = meta.Name
		}
		if ident != "" {
			names = append(names, ident)
		}
	}
	return names, true
}
