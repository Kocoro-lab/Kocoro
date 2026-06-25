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

// DoTaskRequest is the subset of the daemon's POST /message body that Koe sends.
// Source is always "koe". ThreadID is the per-call burst id; Agent is the
// resolved slug ("" = daemon default).
type DoTaskRequest struct {
	Text     string `json:"text"`
	Source   string `json:"source"`
	Agent    string `json:"agent,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	CWD      string `json:"cwd,omitempty"`
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
	Kind        OutcomeKind
	Reply       string // Completed
	SessionID   string // Completed
	Agent       string // Completed
	Partial     bool   // Completed (soft force-stop)
	FailureCode string // Completed (soft)
	Route       string // Injected / Rejected
	Reason      string // Rejected (queue_full|active_run_not_ready|cwd_conflict)
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
		Reply       string `json:"reply"`
		SessionID   string `json:"session_id"`
		Agent       string `json:"agent"`
		Partial     bool   `json:"partial"`
		FailureCode string `json:"failure_code"`
		Status      string `json:"status"`
		Route       string `json:"route"`
		Reason      string `json:"reason"`
		Error       string `json:"error"`
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
			Kind: OutcomeCompleted, Reply: parsed.Reply, SessionID: parsed.SessionID,
			Agent: parsed.Agent, Partial: parsed.Partial, FailureCode: parsed.FailureCode,
		}, nil
	case "injected", "retracted_before_delivery":
		return DoTaskOutcome{Kind: OutcomeInjected, Route: parsed.Route}, nil
	default: // "rejected" (and any future status) → treat as a structured rejection
		return DoTaskOutcome{Kind: OutcomeRejected, Route: parsed.Route, Reason: parsed.Reason}, nil
	}
}
