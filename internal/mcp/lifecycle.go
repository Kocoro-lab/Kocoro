package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

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

	select {
	case <-ctx.Done():
		return ElicitationResult{}, ctx.Err()
	case <-ss.ctx.Done():
		return ElicitationResult{}, ss.ctx.Err()
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
