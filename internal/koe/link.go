//go:build darwin && cgo

// Package koe is the voice front-brain's process-local library: the HTTP link to
// the daemon back-brain, the agent name-resolution ladder, and the voice-tool
// schemas. It talks to the daemon over localhost JSON and never imports
// internal/daemon — the contract is the wire, not shared Go types.
package koe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DaemonClient is a localhost HTTP client for the daemon back-brain.
type DaemonClient struct {
	baseURL string
	// doTaskClient has NO timeout: a back-brain turn can run for minutes, so the
	// caller controls the lifetime via context (the Koe-process context, never the
	// realtime call's). controlClient is genuinely bounded — cancel/list are fast
	// localhost calls, 30s is a safety net against a wedged daemon; do_task stays
	// unbounded.
	doTaskClient  *http.Client
	controlClient *http.Client
}

// NewDaemonClient builds a client against e.g. "http://127.0.0.1:7533".
func NewDaemonClient(baseURL string) *DaemonClient {
	return &DaemonClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		doTaskClient:  &http.Client{Timeout: 0},                // unbounded; ctx-controlled
		controlClient: &http.Client{Timeout: 30 * time.Second}, // safety net for fast cancel/list
	}
}

// MintViaDaemon asks the daemon to mint an OpenAI Realtime ephemeral client
// secret on Koe's behalf (the via-daemon design — the front brain holds no
// long-lived credential; the daemon mints through Cloud with its own key). It
// returns the ephemeral "value" (ek_...). This is the production mint path that
// replaces C-minimal's direct dev-key mint. A fast localhost call → controlClient.
func (c *DaemonClient) MintViaDaemon(ctx context.Context, model string) (string, error) {
	body, _ := json.Marshal(map[string]any{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/koe/realtime/mint", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.controlClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon mint failed: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var mint struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &mint); err != nil || mint.Value == "" {
		return "", fmt.Errorf("daemon mint parse failed: %v body=%s", err, string(raw))
	}
	return mint.Value, nil
}

// FetchPersona pulls the small-tier-distilled spoken-persona context (who the
// user is, how to address them — derived from the user's instructions + memory)
// from the daemon, to append to Koe's base persona before the session.update.
// Best-effort: an empty result or any error means Koe uses its base persona only,
// never blocking the call.
func (c *DaemonClient) FetchPersona(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/koe/persona", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.controlClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon persona failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Persona string `json:"persona"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Persona, nil
}

// SendRealtimeUsage reports a realtime usage record (model, response_id, token
// details — built from a response.done event) to the daemon, which relays it to
// Cloud for server-side cost + quota. Fire-and-forget from the call loop: a usage
// POST failing must never interrupt the conversation. Koe never sees pricing.
func (c *DaemonClient) SendRealtimeUsage(ctx context.Context, usage json.RawMessage) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/koe/realtime/usage", bytes.NewReader(usage))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.controlClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon usage relay: HTTP %d", resp.StatusCode)
	}
	return nil
}

// DoTaskRequest is the subset of the daemon's POST /message body that Koe sends.
// Source is always "koe". ThreadID is the per-call burst id; Agent is the
// resolved slug ("" = daemon default).
type DoTaskRequest struct {
	Text           string          `json:"text"`
	Source         string          `json:"source"`
	Agent          string          `json:"agent,omitempty"`
	ThreadID       string          `json:"thread_id,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	ForegroundHint *ForegroundHint `json:"foreground_hint,omitempty"`
}

// ForegroundHint mirrors daemon.RunAgentRequest.foreground_hint without importing
// internal/daemon into the Koe package. Desktop sends it on /call/start so a
// spoken "this window/app" task can use the same AX/screenshot targeting path as
// the quick panel.
type ForegroundHint struct {
	PID      int    `json:"pid,omitempty"`
	AppName  string `json:"app_name,omitempty"`
	BundleID string `json:"bundle_id,omitempty"`
}

// OutcomeKind discriminates the polymorphic POST /message response.
type OutcomeKind int

const (
	OutcomeCompleted OutcomeKind = iota // a RunAgentResult with a reply
	OutcomeInjected                     // follow-up absorbed into a live run
	OutcomeRejected                     // queue_full / active_run_not_ready / cwd_conflict
)

// DoTaskOutcome carries exactly one meaningful payload, keyed by Kind.
type DoTaskOutcome struct {
	Kind          OutcomeKind
	Reply         string // Completed
	SpokenSummary string // Completed; voice-safe projection of Reply when present
	SessionID     string // Completed
	Agent         string // Completed
	Partial       bool   // Completed (soft force-stop)
	FailureCode   string // Completed (soft)
	Route         string // Injected / Rejected
	Reason        string // Rejected (queue_full|active_run_not_ready|cwd_conflict)
}

// DoTask POSTs a delegated task and blocks until the back-brain turn completes
// (or the follow-up is injected/rejected). It returns an error only for transport
// failures — a structured rejection is a normal OutcomeRejected, not an error.
func (c *DaemonClient) DoTask(ctx context.Context, req DoTaskRequest) (DoTaskOutcome, error) {
	if req.Source == "" {
		req.Source = "koe"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return DoTaskOutcome{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/message", bytes.NewReader(body))
	if err != nil {
		return DoTaskOutcome{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.doTaskClient.Do(httpReq)
	if err != nil {
		return DoTaskOutcome{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return DoTaskOutcome{}, err
	}

	var parsed struct {
		Reply         string `json:"reply"`
		SpokenSummary string `json:"spoken_summary"`
		SessionID     string `json:"session_id"`
		Agent         string `json:"agent"`
		Partial       bool   `json:"partial"`
		FailureCode   string `json:"failure_code"`
		Status        string `json:"status"`
		Route         string `json:"route"`
		Reason        string `json:"reason"`
		Error         string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return DoTaskOutcome{}, fmt.Errorf("decode POST /message response (status %d): %w; body=%s", resp.StatusCode, err, string(raw))
	}
	if parsed.Error != "" {
		return DoTaskOutcome{}, fmt.Errorf("daemon error (status %d): %s", resp.StatusCode, parsed.Error)
	}

	switch parsed.Status {
	case "":
		return DoTaskOutcome{
			Kind: OutcomeCompleted, Reply: parsed.Reply, SpokenSummary: parsed.SpokenSummary, SessionID: parsed.SessionID,
			Agent: parsed.Agent, Partial: parsed.Partial, FailureCode: parsed.FailureCode,
		}, nil
	case "injected", "retracted_before_delivery":
		return DoTaskOutcome{Kind: OutcomeInjected, Route: parsed.Route}, nil
	default: // "rejected" (and any future status) → treat as a structured rejection
		return DoTaskOutcome{Kind: OutcomeRejected, Route: parsed.Route, Reason: parsed.Reason}, nil
	}
}

// cancelReasons mirrors agenttypes.ParseCancelReason on the daemon (server.go:898).
// Validating client-side avoids a guaranteed 400 round-trip. The daemon accepts
// five reasons (the fifth, sibling_error, is missing from its own 400 message
// string but accepted by ParseCancelReason) — keep this list complete.
var cancelReasons = map[string]struct{}{
	"user_cancel": {}, "interrupt": {}, "background": {}, "idle_timeout": {}, "sibling_error": {},
}

// CancelRequest cancels the in-flight run on a route. RouteKey is the burst key
// (agent:<bound>:koe:<burst-id>). RestoreLast asks the daemon to slice the
// session back to before this run.
type CancelRequest struct {
	RouteKey    string `json:"route_key,omitempty"`
	Reason      string `json:"reason,omitempty"`
	RestoreLast bool   `json:"restore_last,omitempty"`
}

// Cancel POSTs /cancel. Returns an error for an unknown reason (caught locally),
// transport failure, or a non-2xx daemon response.
func (c *DaemonClient) Cancel(ctx context.Context, req CancelRequest) error {
	if req.Reason == "" {
		req.Reason = "user_cancel"
	}
	if _, ok := cancelReasons[req.Reason]; !ok {
		return fmt.Errorf("unknown cancel reason %q (want user_cancel|interrupt|background|idle_timeout)", req.Reason)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/cancel", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.controlClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel failed (status %d): %s", resp.StatusCode, string(raw))
	}
	return nil
}

// AgentSummary is the subset of GET /agents the resolver needs. Description is
// the localized blurb (locale → text); the resolver flattens it before matching.
type AgentSummary struct {
	Slug        string            `json:"name"`
	DisplayName string            `json:"display_name"`
	Description map[string]string `json:"description"`
}

// ListAgents fetches the daemon's agent registry for name resolution.
func (c *DaemonClient) ListAgents(ctx context.Context) ([]AgentSummary, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/agents", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.controlClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list agents failed (status %d): %s", resp.StatusCode, string(raw))
	}
	var parsed struct {
		Agents []AgentSummary `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Agents, nil
}
