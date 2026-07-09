package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
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

// pendingApproval tracks an in-flight approval request inside the broker. The
// `emitted` flag lets CancelAll skip cleanup events for requests whose
// approval_request was never published (sendFn failed before emission).
type pendingApproval struct {
	ch      chan ApprovalDecision
	emitted bool
}

// ApprovalBroker mediates between the agent loop's OnApprovalNeeded and the WS
// client. It sends approval_request messages over WS and blocks until a
// matching approval_response arrives (or context is cancelled).
type ApprovalBroker struct {
	mu              sync.Mutex
	pending         map[string]*pendingApproval
	toolAutoApprove map[string]bool // in-memory only, non-bash "always allow"
	sendFn          func(req ApprovalRequest) error
	onRequest       func(req ApprovalRequest)
	onCleanup       func(requestID string) // daemon-originated terminal paths (timeout/ctx/CancelAll)
	onRegister      func(requestID string) // called when a pending entry is created
	onDeregister    func(requestID string) // called when a pending entry is cleaned up
	onAutoApprove   func(meta ApprovalRequestMeta, tool string) // called when a tool is auto-approved without prompting
}

// NewApprovalBroker creates a broker. sendFn sends an approval_request over WS.
// It must be reconnect-safe (e.g., a method on *Client, not a closure over a conn).
func NewApprovalBroker(sendFn func(req ApprovalRequest) error) *ApprovalBroker {
	return &ApprovalBroker{
		pending:         make(map[string]*pendingApproval),
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
	if b.IsToolAutoApproved(tool) {
		return DecisionAllow
	}

	// Non-interactive IM channels (WeChat/WeCom/Discord/Telegram/voice) have no
	// Allow/Deny UI, and the cloud can't route an approval card to them — an
	// emitted request would stall until ApprovalTimeout and then deny, surfacing
	// as a truncated "(Response may be incomplete)". Auto-approve locally so the
	// agent can act. Hard-blocked/denied tools are already rejected upstream by
	// the permission engine before reaching the broker; only "ask" prompts land
	// here. See IsNonInteractiveApprovalChannel for the channel classification.
	if IsNonInteractiveApprovalChannel(meta.Source) {
		// Route through the SAME unattended-approval gate as the remote-run
		// auto_approve path (remote_run.go OnApprovalNeeded), so the two paths stay
		// consistent: a tool on the DisallowsUnattendedAutoApproval denylist (e.g.
		// account deletion, payment auth) must never be blanket-approved just
		// because it arrived from a UI-less channel. The denylist is empty today,
		// so every current tool is still auto-approved; a denied one fails safe
		// (deny immediately rather than stall until ApprovalTimeout).
		if agentpkg.DisallowsUnattendedAutoApproval(tool) {
			log.Printf("approval: denying tool %q for non-interactive channel %q (disallows unattended auto-approval, no approval UI)", tool, meta.Source)
			return DecisionDeny
		}
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
	pa := &pendingApproval{ch: make(chan ApprovalDecision, 1)}

	b.mu.Lock()
	b.pending[reqID] = pa
	b.mu.Unlock()

	if b.onRegister != nil {
		b.onRegister(reqID)
	}

	defer func() {
		if b.onDeregister != nil {
			b.onDeregister(reqID)
		}
		b.mu.Lock()
		delete(b.pending, reqID)
		b.mu.Unlock()
	}()

	req := ApprovalRequest{
		MessageID: meta.MessageID,
		SessionID: meta.SessionID,
		Source:    meta.Source,
		Channel:   meta.Channel,
		ThreadID:  meta.ThreadID,
		RequestID: reqID,
		Tool:      tool,
		Title:     approvalTitle(tool, args),
		Args:      args,
		Agent:     meta.Agent,
	}
	// Policy hint for UI: tools in DisallowsAutoApproval cannot be persisted
	// as always-allow. The list is empty as of 2026-05-18, but the hint stays
	// available so clients can disable "Always Allow" for a future entry.
	if agentpkg.DisallowsAutoApproval(tool) {
		req.Flags = append(req.Flags, ApprovalFlagAlwaysAllowDisabled)
	}
	if err := b.sendFn(req); err != nil {
		return DecisionDeny
	}

	// Publish approval_request only AFTER sendFn succeeds so the bus never
	// shows a card for a request that never reached the foreground client.
	// onRequest + emitted=true run inside the broker mutex as a single
	// critical section: a concurrent CancelAll (WS disconnect mid-emit) is
	// either fully sequenced before us (we observe pa missing from pending
	// and skip emission so neither event reaches the bus) or fully sequenced
	// after us (it sees emitted=true and fires the matching cleanup).
	// Without this serialization, the race window between onRequest and
	// emitted=true could leak an orphan approval_request into the ring,
	// leaving a stale Desktop inbox card.
	b.mu.Lock()
	if _, stillPending := b.pending[reqID]; !stillPending {
		b.mu.Unlock()
		// Resolve or CancelAll terminated us between sendFn and now. Both
		// send to pa.ch under the same mutex before deleting from pending,
		// so a value is buffered; drain it for the correct decision.
		select {
		case decision := <-pa.ch:
			return decision
		default:
			return DecisionDeny
		}
	}
	if b.onRequest != nil {
		b.onRequest(req)
	}
	pa.emitted = true
	b.mu.Unlock()

	select {
	case decision := <-pa.ch:
		return decision
	case <-time.After(ApprovalTimeout):
		if b.claimForCleanup(reqID) {
			log.Printf("daemon: approval timeout for %s (tool=%s), denying", reqID, tool)
			if b.onCleanup != nil {
				b.onCleanup(reqID)
			}
			return DecisionDeny
		}
		// Lost the delete race to Resolve / CancelAll. Honor the real
		// decision they buffered onto pa.ch instead of producing a
		// conflicting deny/daemon terminal state.
		select {
		case decision := <-pa.ch:
			return decision
		default:
			return DecisionDeny
		}
	case <-ctx.Done():
		if b.claimForCleanup(reqID) {
			if b.onCleanup != nil {
				b.onCleanup(reqID)
			}
			return DecisionDeny
		}
		select {
		case decision := <-pa.ch:
			return decision
		default:
			return DecisionDeny
		}
	}
}

// claimForCleanup is the inverse of Resolve: it removes reqID from pending
// under the broker mutex and reports whether this caller is the one that
// won the delete race. Used by the timeout / ctx-cancel branches to ensure
// that the daemon-originated cleanup event is emitted only when no ingress
// (HTTP /approval, WS approval_response) has already claimed the request.
// Pairs with the at-most-one terminal-event contract documented on
// EventApprovalResolved.
func (b *ApprovalBroker) claimForCleanup(reqID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.pending[reqID]; !ok {
		return false
	}
	delete(b.pending, reqID)
	return true
}

// Resolve delivers a decision to a pending request. Returns true if this
// call is the one that terminated the request (i.e., the entry was still
// in pending); false if a prior Resolve / CancelAll / timeout / ctx-cancel
// already claimed it. Ingress callers (HTTP /approval, WS approval_response)
// use the return value to gate their bus emission so a daemon-cleanup path
// firing concurrently cannot produce a conflicting terminal event for the
// same request_id.
//
// beforeDeliver, when non-nil, runs UNDER the broker mutex AFTER the claim
// succeeds but BEFORE the decision is buffered onto pa.ch. Ingress callers
// pass an EventApprovalResolved emitter here so the bus event is guaranteed
// to be assigned an ID earlier than any event the Request goroutine — and
// the agent loop reading its decision — can emit after waking up. Without
// this serialization the Request goroutine can resume on another P in
// parallel with the bus emit (pa.ch is buffered cap-1 so the send is
// non-blocking and wakes the receiver immediately), letting an agent
// tool_status land on the bus with a lower ID than approval_resolved —
// Desktop would then dismiss the inbox card AFTER observing the next
// tool already running. Pass nil from non-ingress paths (tests, internal
// stubs) that do not produce a bus event.
//
// The decision is buffered onto pa.ch WHILE the broker mutex is held so a
// concurrent Request that races to acquire the lock after sendFn-success
// is guaranteed to see either (pa still in pending, emit normally) or
// (pa removed + decision ready to drain). Without that pairing the
// post-sendFn pa-missing check could race the ch send and return a stale
// DecisionDeny while the real decision goes to nobody.
func (b *ApprovalBroker) Resolve(requestID string, decision ApprovalDecision, beforeDeliver func()) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	pa, ok := b.pending[requestID]
	if !ok {
		return false
	}
	if beforeDeliver != nil {
		beforeDeliver()
	}
	select {
	case pa.ch <- decision:
	default:
	}
	delete(b.pending, requestID)
	return true
}

// CancelAll sends DecisionDeny to all pending requests and clears the map.
// Called on WS disconnect to unblock all waiting goroutines. Fires onCleanup
// for any pending entry whose approval_request had already been emitted so
// the corresponding inbox card on Desktop is dismissed.
//
// onCleanup is invoked under the broker mutex, BEFORE the deny is buffered
// onto pa.ch, for the same bus-ID ordering reason documented on Resolve:
// the Request goroutine becomes runnable on another P the instant the
// channel send completes, and any event its agent loop emits after seeing
// DecisionDeny must land on the bus with a higher ID than the cleanup
// approval_resolved. emitBusJSON only contends the bus mutex with
// non-blocking subscriber sends, so holding the broker mutex across emits
// here is bounded — even at 200K-context-era pending depths CancelAll
// keeps a single-digit number of in-flight approvals.
func (b *ApprovalBroker) CancelAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, pa := range b.pending {
		if pa.emitted && b.onCleanup != nil {
			b.onCleanup(id)
		}
		select {
		case pa.ch <- DecisionDeny:
		default:
		}
		delete(b.pending, id)
	}
}

// SetToolAutoApprove marks a non-bash tool as auto-approved (in-memory only).
// Tools in agentpkg.DisallowsAutoApproval are silently refused. The list is
// empty today, but callers may still unconditionally invoke this after
// DecisionAlwaysAllow; the broker remains the authoritative gate.
func (b *ApprovalBroker) SetToolAutoApprove(tool string) {
	if agentpkg.DisallowsAutoApproval(tool) {
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
	if agentpkg.DisallowsAutoApproval(tool) {
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

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "apr_" + hex.EncodeToString(b)
}
