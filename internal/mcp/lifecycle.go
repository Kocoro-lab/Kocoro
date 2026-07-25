package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// defaultElicitationTimeout bounds how long a tool call waits for the client to
// answer an elicitation/create prompt (e.g. an approval confirmation).
//
//   - Workload: an interactive user deciding whether to allow a single tool
//     call — 5 minutes matches calendar_request_permission's TCC-dialog budget.
//   - Symptom when it binds: a client that received the prompt but never answers
//     and never disconnects would otherwise pin the tool goroutine forever; on
//     timeout the elicitation fails closed (approval denied) and the tool call
//     returns a normal JSON-RPC denial, so no goroutine leaks.
//   - Override: SHANNON_MCP_ELICITATION_TIMEOUT accepts a Go duration string
//     (e.g. "30s", "10m"); a non-positive or unparseable value keeps the default.
const defaultElicitationTimeout = 5 * time.Minute

func elicitationTimeout() time.Duration {
	if v := os.Getenv("SHANNON_MCP_ELICITATION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultElicitationTimeout
}

type lifecycleContextKey struct{}

type lifecycleContext struct {
	session       *serverSession
	progressToken json.RawMessage
}

func withLifecycleSession(ctx context.Context, session *serverSession) context.Context {
	return context.WithValue(ctx, lifecycleContextKey{}, lifecycleContext{session: session})
}

func withProgressToken(ctx context.Context, token json.RawMessage) context.Context {
	state, _ := ctx.Value(lifecycleContextKey{}).(lifecycleContext)
	state.progressToken = append(json.RawMessage(nil), token...)
	return context.WithValue(ctx, lifecycleContextKey{}, state)
}

// ReportProgress sends an MCP progress notification for the current tool call.
// It returns false when the caller did not provide a progress token, the
// session has closed, or the notification could not be written.
func ReportProgress(ctx context.Context, progress, total float64, message string) bool {
	state, _ := ctx.Value(lifecycleContextKey{}).(lifecycleContext)
	if state.session == nil || len(state.progressToken) == 0 || ctx.Err() != nil {
		return false
	}
	return state.session.sendProgress(state.progressToken, progress, total, message) == nil
}

// RequestElicitation asks an MCP client for structured, non-secret user input.
// It is available only when the client advertised form elicitation support.
func RequestElicitation(
	ctx context.Context,
	message string,
	requestedSchema map[string]any,
) (ElicitationResult, error) {
	state, _ := ctx.Value(lifecycleContextKey{}).(lifecycleContext)
	if state.session == nil {
		return ElicitationResult{}, errors.New("MCP lifecycle session is unavailable")
	}
	return state.session.requestElicitation(ctx, ElicitationParams{
		Mode:            "form",
		Message:         message,
		RequestedSchema: requestedSchema,
	})
}

func (ss *serverSession) sendProgress(
	token json.RawMessage,
	progress, total float64,
	message string,
) error {
	if len(token) == 0 {
		return errors.New("progress token is missing")
	}
	return ss.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": ProgressParams{
			ProgressToken: token,
			Progress:      progress,
			Total:         total,
			Message:       message,
		},
	})
}

func (ss *serverSession) requestElicitation(
	ctx context.Context,
	params ElicitationParams,
) (ElicitationResult, error) {
	if !ss.canElicit() {
		return ElicitationResult{}, errors.New("client does not support form elicitation")
	}

	idValue := fmt.Sprintf("server-%d", ss.nextID.Add(1))
	idBytes, _ := json.Marshal(idValue)
	id := json.RawMessage(idBytes)
	responseCh := make(chan inboundMessage, 1)

	ss.pendingMu.Lock()
	ss.pending[string(id)] = responseCh
	ss.pendingMu.Unlock()
	defer func() {
		ss.pendingMu.Lock()
		delete(ss.pending, string(id))
		ss.pendingMu.Unlock()
	}()

	if err := ss.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "elicitation/create",
		"params":  params,
	}); err != nil {
		return ElicitationResult{}, err
	}

	timer := time.NewTimer(elicitationTimeout())
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ElicitationResult{}, ctx.Err()
	case <-ss.ctx.Done():
		return ElicitationResult{}, ss.ctx.Err()
	case <-timer.C:
		return ElicitationResult{}, errors.New("elicitation timed out waiting for client response")
	case msg := <-responseCh:
		if msg.Error != nil {
			return ElicitationResult{}, fmt.Errorf(
				"elicitation failed (%d): %s",
				msg.Error.Code,
				msg.Error.Message,
			)
		}
		var result ElicitationResult
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return ElicitationResult{}, fmt.Errorf("invalid elicitation response: %w", err)
		}
		return result, nil
	}
}
