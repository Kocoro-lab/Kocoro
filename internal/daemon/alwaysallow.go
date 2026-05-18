package daemon

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

// HandleAlwaysAllowDecision is the single entry point for persisting a
// DecisionAlwaysAllow click. Called by both the SSE handler (Desktop, this
// package) and the WS handler (Cloud channel, cmd/daemon.go) so the two
// transports cannot drift.
//
// Decision matrix:
//
//  1. bash + always-ask prefix (pip install, rm -rf, python -c, git push
//     --force ...): never persisted at any level. User gets a one-time
//     allow plus a notice. Runtime gate (checkPermissionAndApproval) also
//     refuses to honor always_allow_tools entries for these — see
//     internal/agent/loop.go.
//
//  2. bash + safe command + named agent: tool-level persistence to the
//     agent's permissions.always_allow_tools. All future bash calls from
//     this agent skip approval (except always-ask gated ones).
//     broker.SetToolAutoApprove is also set for immediate effect.
//
//  3. bash + safe command + default agent (agentName == ""): tool-level
//     persistence to the GLOBAL permissions.always_allow_tools list (via
//     persistGlobalToolAlwaysAllow). Applies to every agent including the
//     default. Replaced the legacy command-string allowed_commands path so
//     non-technical users get "click once, never asked again" semantics.
//
//  4. Non-bash + named agent: tool-level per-agent persistence.
//
//  5. Non-bash + default agent: tool-level GLOBAL persistence (same global
//     list bash uses). Required because the SSE handler recreates the
//     broker per request, so broker.SetToolAutoApprove alone evaporates.
//     Tools in DisallowsAutoApproval are refused at this entry plus at
//     PersistAgentAlwaysAllow, broker, and the runtime gate in loop.go. The
//     list is empty as of 2026-05-18.
func HandleAlwaysAllowDecision(deps *ServerDeps, broker *ApprovalBroker, agentName, tool, args string) {
	if tool == "bash" {
		handleBashAlwaysAllow(deps, broker, agentName, args)
		return
	}
	// Non-bash. PersistAgentAlwaysAllow does its own DisallowsAutoApproval gate
	// (notice + false return) for both the per-agent and the "no agent context"
	// sub-paths.
	if agentName != "" {
		PersistAgentAlwaysAllow(deps, agentName, tool)
		broker.SetToolAutoApprove(tool)
		return
	}
	// Default agent: SSE handler creates a fresh ApprovalBroker per request
	// (server.go), so broker.SetToolAutoApprove alone evaporates after the
	// current message. Persist to GLOBAL permissions.always_allow_tools so
	// the runtime gate honors the click on every subsequent request — same
	// mechanism PR 6 wired for bash. Tools in DisallowsAutoApproval remain
	// rejected at every gate (write-time + broker + runtime).
	if agent.DisallowsAutoApproval(tool) {
		emitAlwaysAllowNotice(deps, "warn", NoticeCodeHighRiskNotPersistable, tool,
			"This tool always requires fresh approval and cannot be saved as always-allow. Allowed for this call only.")
		log.Printf("daemon: always-allow rejected for non-persistable tool (default agent): %s", tool)
		return
	}
	persistGlobalToolAlwaysAllow(deps, broker, tool)
}

func handleBashAlwaysAllow(deps *ServerDeps, broker *ApprovalBroker, agentName, args string) {
	cmd := permissions.ExtractField(args, "command")
	if cmd == "" {
		return
	}
	if permissions.IsAlwaysAskPrefix(cmd) {
		emitAlwaysAllowNotice(deps, "warn", NoticeCodeBashAlwaysAskNotPersisted, "bash",
			"Allowed for this turn. Not saved (high-risk command — runs that fetch and execute code or destroy data must be approved each call).")
		log.Printf("daemon: always-allow rejected for high-risk prefix: %s", cmd)
		return
	}
	if agentName != "" {
		// PR 5: tool-level always-allow for bash on a named agent. From now
		// on every safe bash command from this agent skips approval;
		// always-ask gated commands still prompt (runtime defense in loop.go).
		PersistAgentAlwaysAllow(deps, agentName, "bash")
		broker.SetToolAutoApprove("bash")
		return
	}
	// PR 6: Default agent (no per-agent config to write to) — use the GLOBAL
	// tool-level always_allow_tools list. Non-technical users who never
	// create named agents now also benefit from "click once, never asked
	// again" for bash. always-ask high-risk gates still prompt every call
	// regardless of what's in the global list (runtime defense + write-time
	// rejection both block them).
	persistGlobalToolAlwaysAllow(deps, broker, "bash")
}

// persistGlobalToolAlwaysAllow writes a tool name to the global
// permissions.always_allow_tools list, updates the in-memory config mirror,
// and sets the broker's in-memory flag for immediate session effect.
// Used by both bash (default agent) and non-bash + default agent paths so a
// fresh-per-request SSE broker doesn't make "Always Allow" evaporate.
// Callers must reject DisallowsAutoApproval tools before invoking this.
func persistGlobalToolAlwaysAllow(deps *ServerDeps, broker *ApprovalBroker, tool string) {
	if err := config.AppendGlobalAlwaysAllowTool(deps.ShannonDir, tool); err != nil {
		log.Printf("daemon: failed to persist global always-allow for %s: %v", tool, err)
		emitAlwaysAllowNotice(deps, "warn", NoticeCodePersistFailed, tool,
			"Click honored for this session, but could not save to config — you may be prompted again after a daemon restart.")
		// Still flip the broker so the rest of this session honors the click.
		broker.SetToolAutoApprove(tool)
		return
	}
	deps.WriteLock()
	perms := &deps.Config.Permissions
	found := false
	for _, t := range perms.AlwaysAllowTools {
		if t == tool {
			found = true
			break
		}
	}
	if !found {
		perms.AlwaysAllowTools = append(perms.AlwaysAllowTools, tool)
	}
	deps.WriteUnlock()
	broker.SetToolAutoApprove(tool)
	log.Printf("daemon: always-allow persisted to global always_allow_tools: %s", tool)
}

// PersistAgentAlwaysAllow is the per-agent tool-level write-through for the
// always-allow set. Called from HandleAlwaysAllowDecision for non-bash tools
// (PR 3) and for bash on named agents (PR 5). Also callable directly by
// other code that needs the same gates.
func PersistAgentAlwaysAllow(deps *ServerDeps, agentName, tool string) bool {
	if deps == nil || tool == "" {
		return false
	}
	if agent.DisallowsAutoApproval(tool) {
		emitAlwaysAllowNotice(deps, "warn", NoticeCodeHighRiskNotPersistable, tool,
			"This tool always requires fresh approval and cannot be saved as always-allow. Allowed for this call only.")
		log.Printf("daemon: always-allow rejected for non-persistable tool: %s", tool)
		return false
	}
	if agentName == "" {
		// Default-agent / non-routed sources have no per-agent config.yaml to
		// write to. The caller will still set the broker's in-memory flag so
		// the rest of this session honors the click.
		log.Printf("daemon: always-allow (session-only, no agent context): %s", tool)
		return false
	}
	if err := agents.AppendAlwaysAllowTool(deps.AgentsDir, agentName, tool); err != nil {
		if errors.Is(err, agents.ErrToolNotPersistable) {
			// Defense-in-depth: the persistence layer also rejects high-risk
			// tools. Should be unreachable given the check above, but the two
			// gates are independent on purpose.
			emitAlwaysAllowNotice(deps, "warn", NoticeCodeHighRiskNotPersistable, tool,
				"This tool always requires fresh approval and cannot be saved as always-allow. Allowed for this call only.")
			log.Printf("daemon: always-allow rejected by persistence layer: agent=%s tool=%s", agentName, tool)
			return false
		}
		emitAlwaysAllowNotice(deps, "warn", NoticeCodePersistFailed, tool,
			"Click honored for this session, but could not save to agent config — you may be prompted again after a daemon restart.")
		log.Printf("daemon: always-allow persist failed: agent=%s tool=%s err=%v", agentName, tool, err)
		return false
	}
	log.Printf("daemon: always-allow persisted: agent=%s tool=%s", agentName, tool)
	return true
}

// NoticeCode identifies a stable, i18n-friendly notice category. UI clients
// map these to localized strings; the daemon never sends translated UI text.
// `message` on the payload is an English fallback for clients that don't yet
// recognize the code.
const (
	// NoticeCodeHighRiskNotPersistable is sent when the user clicks "Always
	// Allow" on a tool in agent.DisallowsAutoApproval. The click is honored
	// once but cannot be saved at any persistence layer. The list is empty as
	// of 2026-05-18, but the notice code stays for future entries.
	NoticeCodeHighRiskNotPersistable = "high_risk_not_persistable"
	// NoticeCodeBashAlwaysAskNotPersisted is sent when the user clicks
	// "Always Allow" on a bash command matching permissions.alwaysAskPrefixes
	// (pip install / rm -rf / python -c / git push --force / ...). Allowed
	// once but not saved (and never auto-allowed in the future).
	NoticeCodeBashAlwaysAskNotPersisted = "bash_always_ask_not_persisted"
	// NoticeCodePersistFailed is sent when the user clicks "Always Allow" on
	// an otherwise-allowed tool but the filesystem write failed. The click
	// is still honored for the current session via the broker.
	NoticeCodePersistFailed = "persist_failed"
)

// AlwaysAllowNoticePayload is the structured shape of EventApprovalNotice
// payloads emitted by the always-allow flow. UI clients should:
//  1. Look up `Code` in their localized message table.
//  2. Fall back to `Message` (English) if the code is unknown or untranslated.
//  3. Use `Tool` to interpolate the tool name into the localized template.
//
// Older clients that only read `severity` + `message` continue to work —
// `code` and `tool` are omitempty and additive.
type AlwaysAllowNoticePayload struct {
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Message  string `json:"message"`
}

// emitAlwaysAllowNotice publishes an EventApprovalNotice. Best-effort —
// silent no-op when EventBus is unavailable (one-shot / test paths). The
// `message` argument is the English fallback used by older clients; `code`
// is the stable identifier the UI uses for localization.
func emitAlwaysAllowNotice(deps *ServerDeps, severity, code, tool, message string) {
	if deps == nil || deps.EventBus == nil {
		return
	}
	payload, err := json.Marshal(AlwaysAllowNoticePayload{
		Severity: severity,
		Code:     code,
		Tool:     tool,
		Message:  message,
	})
	if err != nil {
		return
	}
	deps.EventBus.Emit(Event{Type: EventApprovalNotice, Payload: payload})
}
