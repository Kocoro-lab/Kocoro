package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"time"

	agentpkg "github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// ApprovalTimeout is the maximum time to wait for an approval response.
// After this, the tool call is denied and the agent loop continues.
const ApprovalTimeout = 5 * time.Minute

// ApprovalDecision represents the user's response to a tool approval request.
type ApprovalDecision string

const (
	DecisionAllow       ApprovalDecision = "allow"
	DecisionDeny        ApprovalDecision = "deny"
	DecisionAlwaysAllow ApprovalDecision = "always_allow"
)

// ApprovalRequestMeta carries identity/context fields that the approval bus
// payload needs to render an inbox card and click-through into the originating
// session. Passed to ApprovalBroker.Request alongside (tool, args) so callers
// don't grow positional parameters every time Desktop wants a new field.
type ApprovalRequestMeta struct {
	MessageID string
	SessionID string
	Source    string
	Channel   string
	ThreadID  string
	Agent     string
}

// pendingApproval tracks an in-flight approval request inside the broker. It is
// an alias of the shared pending-interaction entry specialized to
// ApprovalDecision; the `emitted` flag lets CancelAll skip cleanup events for
// requests whose approval_request was never published (sendFn failed before
// emission). See pending.go for the shared lifecycle.
type pendingApproval = pendingEntry[ApprovalDecision]

// ApprovalBroker mediates between the agent loop's OnApprovalNeeded and the WS
// client. It sends approval_request messages over WS and blocks until a
// matching approval_response arrives (or context is cancelled). The pending
// lifecycle (register / emit-guard / Resolve / CancelAll / timeout ordering) is
// the shared pendingCore; ApprovalBroker adds the approval-specific wire face
// (ApprovalRequest payload, non-interactive channel gating, Always-Allow cache).
type ApprovalBroker struct {
	*pendingCore[ApprovalDecision]
	toolAutoApprove map[string]bool // in-memory only, non-bash "always allow"
	sendFn          func(req ApprovalRequest) error
	onRequest       func(req ApprovalRequest)
	onAutoApprove   func(meta ApprovalRequestMeta, tool string) // called when a tool is auto-approved without prompting
	// persistenceDenied extends the static DisallowsAutoApproval name check
	// with registration-time schema data (integration tools marked
	// requires_approval). Installed at broker construction from
	// ServerDeps.ToolDisallowsAlwaysAllowPersistence; nil keeps the static
	// behavior (tests, brokers without a registry).
	persistenceDenied func(tool string) bool
}

// NewApprovalBroker creates a broker. sendFn sends an approval_request over WS.
// It must be reconnect-safe (e.g., a method on *Client, not a closure over a conn).
func NewApprovalBroker(sendFn func(req ApprovalRequest) error) *ApprovalBroker {
	return &ApprovalBroker{
		pendingCore:     newPendingCore[ApprovalDecision](),
		toolAutoApprove: make(map[string]bool),
		sendFn:          sendFn,
	}
}

// SetOnRequest sets a callback invoked after the approval request has been
// successfully sent to the transport (sendFn returned nil). Used to emit
// EventApprovalRequest to SSE subscribers with the fully-constructed request
// (including SessionID, Source, Title, Flags).
func (b *ApprovalBroker) SetOnRequest(fn func(req ApprovalRequest)) {
	b.onRequest = fn
}

// SetOnCleanup sets a callback invoked when a previously-emitted approval
// request is terminated by a daemon-originated path that does not pass
// through the external decision ingress (timeout, ctx cancel, CancelAll).
// Used to emit a synthetic EventApprovalResolved so Desktop dismisses the
// inbox card instead of leaving a stale entry in the ring buffer.
func (b *ApprovalBroker) SetOnCleanup(fn func(requestID string)) {
	b.onCleanup = fn
}

// SetAlwaysAllowPersistenceDenied installs the registry-backed lookup that
// reports tools refusing "Always Allow" persistence (integration tools whose
// Cloud schema carries requires_approval). It must be pure in-memory work —
// Request calls it on the approval hot path.
func (b *ApprovalBroker) SetAlwaysAllowPersistenceDenied(fn func(tool string) bool) {
	b.persistenceDenied = fn
}

// disallowsAlwaysAllow combines the static name deny-list with the dynamic
// schema-derived denial. It gates the UI flag, the in-memory auto-approve
// cache writes, and the cache reads (defense-in-depth).
func (b *ApprovalBroker) disallowsAlwaysAllow(tool string) bool {
	if agentpkg.DisallowsAutoApproval(tool) {
		return true
	}
	return b.persistenceDenied != nil && b.persistenceDenied(tool)
}

// SetOnAutoApprove sets a callback invoked when the broker auto-approves a tool
// without prompting (non-interactive IM channels). Used to emit EventApprovalAuto
// so the unattended execution is observable, mirroring the approval_auto notice
// the remote-run auto_approve path emits on its per-run SSE stream.
func (b *ApprovalBroker) SetOnAutoApprove(fn func(meta ApprovalRequestMeta, tool string)) {
	b.onAutoApprove = fn
}

// Request sends an approval_request and blocks until the response arrives
// or ctx is cancelled. Returns DecisionDeny if send fails or ctx is done.
//
// meta.MessageID must be the inbound claim's WS envelope ID — Cloud uses it
// to resolve the originating channel/thread for the approval card. Pass ""
// only from non-channel-routed paths (e.g. the local SSE dev server, where
// there is no Cloud claim and the approval flow stays in-process).
func (b *ApprovalBroker) Request(ctx context.Context, meta ApprovalRequestMeta, tool, args string) ApprovalDecision {
	// An explicit persisted grant is mirrored into this cache and is the only
	// way a deny-listed Computer Use request may run without an approval UI.
	// Tools that disallow persistence cannot enter the cache.
	if b.IsToolAutoApproved(tool) {
		return DecisionAllow
	}

	// A non-interactive channel has no human approval round-trip. Without the
	// explicit grant above, apply the unattended deny-list before the generic
	// no-UI auto-approval fallback.
	nonInteractive := IsNonInteractiveApprovalChannel(meta.Source)
	if nonInteractive && agentpkg.DisallowsUnattendedAutoApproval(tool) {
		log.Printf("approval: denying tool %q for non-interactive channel %q (disallows unattended auto-approval, no approval UI)", tool, meta.Source)
		return DecisionDeny
	}

	// Non-interactive IM channels (WeChat/WeCom/Discord/Telegram/voice) have no
	// Allow/Deny UI, and the cloud can't route an approval card to them — an
	// emitted request would stall until ApprovalTimeout and then deny, surfacing
	// as a truncated "(Response may be incomplete)". Auto-approve locally so the
	// agent can act. Hard-blocked/denied tools are already rejected upstream by
	// the permission engine before reaching the broker; only "ask" prompts land
	// here. See IsNonInteractiveApprovalChannel for the channel classification.
	if nonInteractive {
		// Route through the SAME unattended-approval gate as the remote-run
		// auto_approve path (remote_run.go OnApprovalNeeded), so the two paths stay
		// consistent: a tool on the DisallowsUnattendedAutoApproval denylist (e.g.
		// account deletion, payment auth) must never be blanket-approved just
		// because it arrived from a UI-less channel. A denied tool fails safe
		// immediately rather than stalling until ApprovalTimeout.
		log.Printf("approval: auto-approving tool %q for non-interactive channel %q (no approval UI)", tool, meta.Source)
		// Emit an observability notice: auto-approval bypasses the normal
		// approval_request flow, so this is the only controller-visible record
		// that an unattended tool ran on this channel.
		if b.onAutoApprove != nil {
			b.onAutoApprove(meta, tool)
		}
		return DecisionAllow
	}

	reqID := generateRequestID()
	presentationTitle, presentationArgs := approvalPresentation(tool, args)
	req := ApprovalRequest{
		MessageID: meta.MessageID,
		SessionID: meta.SessionID,
		Source:    meta.Source,
		Channel:   meta.Channel,
		ThreadID:  meta.ThreadID,
		RequestID: reqID,
		Tool:      tool,
		Title:     presentationTitle,
		Args:      presentationArgs,
		Agent:     meta.Agent,
	}
	// Policy hint for UI: tools that refuse always-allow persistence (static
	// deny-list, or integration tools marked requires_approval) cannot be
	// persisted. computer_use is intentionally persistable as the global
	// product grant.
	if b.disallowsAlwaysAllow(tool) {
		req.Flags = append(req.Flags, ApprovalFlagAlwaysAllowDisabled)
	}

	// The pending lifecycle (register → send → emit-guard → wait, with the
	// timeout / ctx / cancelAll ordering invariants) lives in pendingCore.run.
	return b.run(ctx, reqID, ApprovalTimeout, DecisionDeny,
		func() error { return b.sendFn(req) },
		func() {
			if b.onRequest != nil {
				b.onRequest(req)
			}
		},
	)
}

// CancelAll sends DecisionDeny to all pending approvals and clears the map,
// firing onCleanup (approval_resolved) for every already-emitted request.
// Called on WS disconnect to unblock all waiting goroutines. See
// pendingCore.cancelAll for the bus-ID ordering contract.
func (b *ApprovalBroker) CancelAll() { b.cancelAll(DecisionDeny) }

// SetToolAutoApprove marks a non-bash tool as auto-approved (in-memory only).
// Tools that refuse always-allow persistence (static deny-list or
// requires_approval integration schemas) are silently refused. Callers may
// still unconditionally invoke this after DecisionAlwaysAllow; the broker
// remains the authoritative gate.
func (b *ApprovalBroker) SetToolAutoApprove(tool string) {
	if b.disallowsAlwaysAllow(tool) {
		return
	}
	b.mu.Lock()
	b.toolAutoApprove[tool] = true
	b.mu.Unlock()
}

// IsToolAutoApproved checks if a tool has been auto-approved via "Always Allow".
// Defense-in-depth: even if the map somehow contains a non-persistable tool
// (e.g. from a future regression or a callsite bypassing SetToolAutoApprove),
// this gate refuses to honor it.
func (b *ApprovalBroker) IsToolAutoApproved(tool string) bool {
	if b.disallowsAlwaysAllow(tool) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.toolAutoApprove[tool]
}

// approvalRequestArgsCap caps the args length stored on the event-bus copy of
// approval_request. Bus payloads live in the ring buffer (in-memory, capped
// at ringSize events), so we keep args bounded; the wire copy sent to Cloud
// stays unredacted/unfixed because Slack/etc. need the full command to render.
const approvalRequestArgsCap = 1024

// approvalRequestTitleCap caps the title field on the bus payload. The title
// is parsed straight out of args.description — a model-controlled string —
// so a misbehaving / prompt-injected agent could otherwise smuggle long text
// or copied secrets through the title path that bypasses args' redaction.
// 200 bytes comfortably fits the "5-15 words" approval-description contract
// and matches the cap used by tool_status preview / args truncation.
const approvalRequestTitleCap = 200

// WireApprovalBusHooks installs the standard EventBus emitter hooks on b so
// approval_request / approval_resolved events flow through the same code path
// regardless of which broker created them (the cmd/daemon.go WS broker or the
// NewServer-owned approvalBroker that SSE per-request brokers inherit from).
//
// notify is fired on every daemon-originated cleanup (timeout / ctx cancel /
// WS disconnect) so Cloud clears the channel approval card (Feishu/Slack) the
// same way it does when Desktop resolves via POST /approval. Pass nil from
// tests that only care about the local bus; the cleanup emitter no-ops a nil
// notify.
func WireApprovalBusHooks(b *ApprovalBroker, bus *EventBus, notify func(ApprovalResolvedPayload) error) {
	if b == nil {
		return
	}
	b.SetOnRequest(makeApprovalRequestEmitter(bus))
	b.SetOnCleanup(makeApprovalCleanupEmitter(bus, notify))
	b.SetOnAutoApprove(makeApprovalAutoEmitter(bus))
}

// makeApprovalAutoEmitter returns a hook callable as ApprovalBroker.onAutoApprove
// that publishes EventApprovalAuto to bus so an unattended (non-interactive
// channel) tool execution is observable in the replay buffer and on Desktop —
// the counterpart to the approval_auto notice the remote-run path emits.
func makeApprovalAutoEmitter(bus *EventBus) func(meta ApprovalRequestMeta, tool string) {
	return func(meta ApprovalRequestMeta, tool string) {
		emitBusJSON(bus, EventApprovalAuto, map[string]any{
			"session_id": meta.SessionID,
			"agent":      meta.Agent,
			"tool":       tool,
			"source":     meta.Source,
			"channel":    meta.Channel,
			"reason":     "non_interactive_channel",
			"ts":         nowISO(),
		})
	}
}

// makeApprovalRequestEmitter returns a hook callable as ApprovalBroker.onRequest
// that publishes EventApprovalRequest to bus with the full payload Desktop
// needs to render an inbox card (request_id, session_id, agent, tool, title,
// source, channel, redacted/truncated args, flags, ts).
//
// `flags` is omitted from the payload when empty, matching the
// `json:"flags,omitempty"` semantics of the wire-side ApprovalRequest. A nil
// slice assigned into a map[string]any otherwise marshals as "flags": null,
// which would crash naive UI clients calling payload.flags.includes(...).
func makeApprovalRequestEmitter(bus *EventBus) func(req ApprovalRequest) {
	return func(req ApprovalRequest) {
		payload := map[string]any{
			"request_id": req.RequestID,
			"session_id": req.SessionID,
			"agent":      req.Agent,
			"tool":       req.Tool,
			"title":      redactAndTruncate(req.Title, approvalRequestTitleCap),
			"source":     req.Source,
			"channel":    req.Channel,
			"args":       redactAndTruncate(req.Args, approvalRequestArgsCap),
			"ts":         nowISO(),
		}
		if len(req.Flags) > 0 {
			payload["flags"] = req.Flags
		}
		emitBusJSON(bus, EventApprovalRequest, payload)
	}
}

// makeApprovalCleanupEmitter returns a hook callable as ApprovalBroker.onCleanup
// that publishes a synthetic EventApprovalResolved (decision=deny,
// resolved_by=daemon) so reconnecting Desktop clients dismiss the inbox card
// for an approval the daemon abandoned (timeout, ctx cancel, WS disconnect).
//
// When notify is non-nil it ALSO tells Cloud the approval was resolved so the
// gateway clears the channel card (Feishu/Slack) — without this, an approval
// the agent gave up on leaves a zombie card whose buttons never disappear.
// Primary value is the timeout and ctx-cancel paths, where the WS is still
// connected and the send actually reaches Cloud; the CancelAll-on-disconnect
// path is belt-and-suspenders only (the goroutine fires after the connection
// is already torn down, so the send almost always fails — Cloud's Redis TTL
// backstop is what clears the card there). The notify call runs on its own
// goroutine: onCleanup is invoked under the broker mutex by CancelAll, and a
// synchronous WS send there would block the lock (and every other approval) on
// network IO. Errors are ignored for the disconnect reason above.
func makeApprovalCleanupEmitter(bus *EventBus, notify func(ApprovalResolvedPayload) error) func(requestID string) {
	return func(requestID string) {
		emitBusJSON(bus, EventApprovalResolved, map[string]any{
			"request_id":  requestID,
			"decision":    string(DecisionDeny),
			"resolved_by": "daemon",
			"ts":          nowISO(),
		})
		if notify != nil {
			go func() {
				_ = notify(ApprovalResolvedPayload{
					RequestID:  requestID,
					Decision:   DecisionDeny,
					ResolvedBy: "daemon",
				})
			}()
		}
	}
}

// approvalTitle extracts the user-facing approval-card title from a tool's
// args JSON. Every tool whose RequiresApproval() returns true must declare a
// `description` field per internal/agent/approval_description.go; if args is
// not JSON or has no non-empty description we fall back to the tool name so
// the UI never renders a blank card.
func approvalTitle(tool, args string) string {
	var payload struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(args), &payload); err == nil {
		if title := strings.TrimSpace(payload.Description); title != "" {
			return title
		}
	}
	return tool
}

// approvalPresentation keeps the attended decision boundary useful without
// copying typed text, key content, scripts, AX values, or model descriptions
// into the approval transport/event ring. The executor retains the original
// args independently; only the user-facing card receives this projection.
func approvalPresentation(tool, args string) (title string, presentedArgs string) {
	presentedArgs = agentpkg.RedactGUIActivityArguments(tool, args)
	if presentedArgs == args {
		return approvalTitle(tool, args), args
	}
	var payload struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(presentedArgs), &payload) == nil && payload.Action != "" {
		return tool + ": " + payload.Action, presentedArgs
	}
	return tool, presentedArgs
}

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "apr_" + hex.EncodeToString(b)
}
