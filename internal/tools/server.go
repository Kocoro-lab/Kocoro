package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// ladderHeader marks the start of a fallback-ladder block appended to a failed
// tool_result content. It also doubles as the idempotence sentinel: when the
// cloud has already embedded its own ladder in `error`, we don't append a
// second one. See buildFallbackLadder.
const ladderHeader = "Provider attempts:"

// ladderDetailMaxLen caps each provider's detail line at this many runes after
// secret redaction. Mirrors the cap audit.truncate uses for tool args/preview,
// balancing diagnostic value vs LLM context burn.
const ladderDetailMaxLen = 200

// ServerTool wraps a server-side tool schema and proxies execution to the
// gateway. Gateway tools hit /api/v1/tools/{name}/execute; integration tools
// (see NewIntegrationTool) hit /api/v1/integrations/tools/{name}/execute. Only
// the execute function and the declared source differ — the argument
// stripping, error classification, usage accounting and result shaping are
// identical, so both share Run.
type ServerTool struct {
	schema                client.ServerToolSchema
	gateway               *client.GatewayClient
	execute               func(ctx context.Context, name string, args map[string]any) (*client.ToolExecuteResponse, error)
	source                agent.ToolSource
	integrationGeneration uint64
}

// NewServerTool builds a gateway tool (allowlisted server-side tools such as
// web_search / web_fetch), executed via /api/v1/tools/{name}/execute.
func NewServerTool(schema client.ServerToolSchema, gateway *client.GatewayClient) *ServerTool {
	return &ServerTool{
		schema:  schema,
		gateway: gateway,
		execute: func(ctx context.Context, name string, args map[string]any) (*client.ToolExecuteResponse, error) {
			return gateway.ExecuteTool(ctx, name, args, "")
		},
		source: agent.SourceGateway,
	}
}

// NewIntegrationTool builds a third-party integration tool (Notion/Slack/…),
// executed via /api/v1/integrations/tools/{name}/execute. Cloud resolves the
// caller's connection from the API key and enforces its own access control, so
// like a gateway tool it does not require local approval.
func NewIntegrationTool(schema client.ServerToolSchema, gateway *client.GatewayClient) *ServerTool {
	generation := uint64(0)
	if gateway != nil {
		generation, _ = gateway.IntegrationGeneration()
	}
	return NewIntegrationToolForGeneration(schema, gateway, generation)
}

// NewIntegrationToolForGeneration binds a Cloud-returned schema to the exact
// verified-principal generation used by its list request. Captured tools held by
// an old AgentLoop or registry clone remain fail-closed after auth mutation.
func NewIntegrationToolForGeneration(
	schema client.ServerToolSchema,
	gateway *client.GatewayClient,
	generation uint64,
) *ServerTool {
	return &ServerTool{
		schema:  schema,
		gateway: gateway,
		execute: func(ctx context.Context, name string, args map[string]any) (*client.ToolExecuteResponse, error) {
			requestID := ""
			idempotencyKey := ""
			if execution, ok := agent.SideEffectExecutionFromContext(ctx); ok {
				requestID = execution.ExecutionID
				idempotencyKey = execution.IdempotencyKey
			} else if invocation, ok := agent.ToolInvocationFromContext(ctx); ok {
				requestID = invocation.ToolUseID
			}
			var resp *client.ToolExecuteResponse
			var err error
			for attempt, delay := range []time.Duration{0, 250 * time.Millisecond, 750 * time.Millisecond} {
				if delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return nil, ctx.Err()
					case <-timer.C:
					}
				}
				resp, err = gateway.ExecuteIntegrationToolWithIdentityForGeneration(
					ctx, name, args, requestID, idempotencyKey, generation,
				)
				if err == nil || requestID == "" || attempt == 2 || !integrationCallCanRetryWithSameIdentity(err) {
					return resp, err
				}
			}
			return resp, err
		},
		source:                agent.SourceIntegration,
		integrationGeneration: generation,
	}
}

// billing_error and call_in_progress mean Cloud already owns this request id.
// A bounded in-call retry can finish the same ledger entry without repeating
// the provider operation. Returning a retryable error to the model would be
// unsafe because a new tool call receives a different request id.
func integrationCallCanRetryWithSameIdentity(err error) bool {
	var integrationErr *client.IntegrationToolAPIError
	if !errors.As(err, &integrationErr) {
		return false
	}
	return integrationErr.Code == "billing_error" || integrationErr.Code == "call_in_progress"
}

func (t *ServerTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        t.schema.Name,
		Description: t.schema.Description,
		Parameters:  t.schema.Parameters,
		Required:    requiredFieldsFromSchema(t.schema.Parameters),
	}
}

func (t *ServerTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	info := t.Info()
	if result, valid := agent.ValidateToolArgumentPresence(info, argsJSON); !valid {
		return result, nil
	}
	var args map[string]any
	if argsJSON != "" && argsJSON != "{}" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}

	// Defense against LLM over-generalization: the model frequently
	// hallucinates fields it learned from other tools' schemas (e.g. the
	// `description` field that every approval-card-bearing local tool
	// declares gets injected into server-tool calls too, where the cloud
	// schema doesn't declare it). Strip anything the gateway's schema
	// doesn't list so the wire payload stays minimal, audit logs don't
	// show phantom args, and cloud-side reject paths aren't tripped on
	// older cloud versions without the reserved-daemon-fields whitelist.
	stripFieldsNotInSchema(args, t.schema.Parameters)
	materialSideEffect := t.HasMaterialSideEffect(argsJSON)
	if err := ctx.Err(); err != nil {
		result := agent.ToolResult{
			Content: fmt.Sprintf("server tool call cancelled before dispatch: %v", err),
			IsError: true,
		}
		if materialSideEffect {
			result = withKnownNoEffect(result)
		}
		return result, nil
	}

	resp, err := t.execute(ctx, t.schema.Name, args)
	if err != nil {
		if materialSideEffect {
			return t.materialErrorResult(err), nil
		}
		return t.nonMaterialErrorResult(err), nil
	}

	// Convert server-reported usage (xAI Grok tokens for x_search, SerpAPI
	// queries for web_search, etc.) into an agent-level ToolUsage. Populated
	// on the ToolResult so the audit logger can attribute cost per call; also
	// emitted via context so the per-run usage accumulator picks it up.
	// Server populates resp.Usage when the underlying provider returns billing
	// info; older servers leave it nil and this is a no-op.
	var toolUsage *agent.ToolUsage
	if resp.Usage != nil {
		u := resp.Usage
		// The gateway currently returns a flat `tokens` count (synthetic for
		// SERP tools, real input+output sum for x_search). If explicit
		// input/output breakdowns are present, prefer them; else collapse
		// `tokens` into TotalTokens so the accumulator still sees the volume.
		totalTokens := u.TotalTokens
		if totalTokens == 0 {
			totalTokens = u.Tokens
		}
		if totalTokens == 0 {
			totalTokens = u.InputTokens + u.OutputTokens
		}
		model := u.Model
		if model == "" {
			model = u.CostModel
		}
		toolUsage = &agent.ToolUsage{
			Provider:     u.Provider,
			Model:        model,
			CostModel:    u.CostModel,
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			TotalTokens:  totalTokens,
			CostUSD:      u.CostUSD,
			Units:        u.Units,
			UnitType:     u.UnitType,
		}
		agent.EmitUsage(ctx, agent.TurnUsage{
			Provider:     u.Provider,
			CostModel:    u.CostModel,
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			TotalTokens:  totalTokens,
			CostUSD:      u.CostUSD,
			// Gateway tool calls are not LLM calls from the driving model's
			// perspective — leave LLMCalls=0 so session LLMCalls stays clean.
			Model:    model,
			Units:    u.Units,
			UnitType: u.UnitType,
		})
	}

	if resp.Error != nil && *resp.Error != "" {
		content := *resp.Error
		if !resp.Success {
			content = appendLadder(content, resp.Metadata)
		}
		if !materialSideEffect && looksLikeRemoteValidationError(content) {
			result := agent.ValidationError(strings.TrimPrefix(content, "[validation error] "))
			result.Usage = toolUsage
			return result, nil
		}
		return agent.ToolResult{Content: content, IsError: true, Usage: toolUsage}, nil
	}

	if !resp.Success {
		content := appendLadder("tool execution failed", resp.Metadata)
		return agent.ToolResult{Content: content, IsError: true, Usage: toolUsage}, nil
	}

	// Prefer pre-formatted text from backend; fall back to raw JSON output
	if resp.Text != nil && *resp.Text != "" {
		return agent.ToolResult{Content: *resp.Text, Usage: toolUsage}, nil
	}
	if len(resp.Output) == 0 || string(resp.Output) == "null" {
		return agent.ToolResult{Content: "no output", Usage: toolUsage}, nil
	}
	return agent.ToolResult{Content: string(resp.Output), Usage: toolUsage}, nil
}

func (t *ServerTool) reconnectInstruction() string {
	provider := strings.TrimSpace(t.schema.Provider)
	switch strings.ToLower(provider) {
	case "x", "twitter":
		provider = "X"
	case "shopify":
		provider = "Shopify"
	case "notion":
		provider = "Notion"
	case "figma":
		provider = "Figma"
	case "slack":
		provider = "Slack"
	case "":
		return "Reconnect the integration in Kocoro Settings → MCP Servers before retrying."
	}
	return fmt.Sprintf("Reconnect %s in Kocoro Settings → MCP Servers → %s before retrying.", provider, provider)
}

func (t *ServerTool) nonMaterialErrorResult(err error) agent.ToolResult {
	if t.source == agent.SourceGateway {
		msg := err.Error()
		prefix := classifyServerError(msg)
		return agent.ToolResult{
			Content: fmt.Sprintf("%sserver tool error: %v", prefix, err),
			IsError: true,
		}
	}
	var staleGeneration *client.StaleIntegrationGenerationError
	if errors.As(err, &staleGeneration) {
		return withKnownNoEffect(agent.BusinessError(
			"integration tool is no longer authorized for the current signed-in principal; rediscover the live integration tools before retrying",
		))
	}
	var integrationErr *client.IntegrationToolAPIError
	if errors.As(err, &integrationErr) {
		return t.integrationAPIErrorResult(integrationErr, false)
	}
	var dispatchErr *client.ToolDispatchError
	if errors.As(err, &dispatchErr) {
		return agent.TransientError(fmt.Sprintf("integration tool %s transport failed: %v", t.schema.Name, err))
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		msg := fmt.Sprintf("server tool %s returned %d", t.schema.Name, apiErr.StatusCode)
		if apiErr.StatusCode >= http.StatusInternalServerError || apiErr.StatusCode == http.StatusTooManyRequests {
			return agent.TransientError(msg)
		}
		return agent.BusinessError(msg)
	}
	return agent.TransientError(fmt.Sprintf("integration tool %s failed: %v", t.schema.Name, err))
}

func (t *ServerTool) materialErrorResult(err error) agent.ToolResult {
	var staleGeneration *client.StaleIntegrationGenerationError
	if errors.As(err, &staleGeneration) {
		return withKnownNoEffect(agent.BusinessError(
			"integration tool was invalidated before dispatch because the signed-in principal changed; rediscover the live integration tools before retrying",
		))
	}
	var integrationErr *client.IntegrationToolAPIError
	if errors.As(err, &integrationErr) {
		if integrationErr.Code == "call_in_progress" {
			return externalOutcomeUnknown(fmt.Sprintf(
				"External tool outcome UNKNOWN: the original %s request is still in progress after bounded same-identity polling. Do not resend it or create a new durable request identity; wait or verify its state.",
				t.schema.Name,
			))
		}
		if integrationCodeMayHaveEffect(integrationErr.Code) {
			return externalOutcomeUnknown(fmt.Sprintf(
				"External tool outcome UNKNOWN: Cloud returned %d (%s) after receiving %s. The external action may have taken effect; verify before retrying.",
				integrationErr.StatusCode, integrationErr.Code, t.schema.Name,
			))
		}
		if integrationCodeProvesNoEffect(integrationErr.Code) ||
			integrationStatusProvesNoEffect(integrationErr.StatusCode) {
			return withKnownNoEffect(t.integrationAPIErrorResult(integrationErr, true))
		}
		// Billing may fail after the provider committed, and provider transport
		// failures do not prove the action was rejected. Any structured error
		// without explicit no-effect evidence therefore remains conservative.
		return externalOutcomeUnknown(fmt.Sprintf(
			"External tool outcome UNKNOWN: Cloud returned %d (%s) after receiving %s. The external action may have taken effect; verify before retrying.",
			integrationErr.StatusCode, integrationErr.Code, t.schema.Name,
		))
	}
	var dispatchErr *client.ToolDispatchError
	if errors.As(err, &dispatchErr) {
		if dispatchErr.MayHaveDispatched {
			return externalOutcomeUnknown(fmt.Sprintf(
				"External tool outcome UNKNOWN: %s may have been dispatched, but no complete response arrived. The external action may have taken effect; verify before retrying.",
				t.schema.Name,
			))
		}
		msg := fmt.Sprintf("external tool %s failed before dispatch", t.schema.Name)
		if dispatchErr.Retryable {
			return withKnownNoEffect(agent.TransientError(msg))
		}
		return withKnownNoEffect(agent.BusinessError(msg))
	}

	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		msg := fmt.Sprintf("external tool %s returned %d", t.schema.Name, apiErr.StatusCode)
		if apiErr.Body != "" {
			msg += ": " + apiErr.Body
		}
		switch apiErr.StatusCode {
		case http.StatusRequestTimeout:
			return externalOutcomeUnknown(fmt.Sprintf(
				"External tool outcome UNKNOWN: Cloud returned %d after receiving %s. The external action may have taken effect; verify before retrying.",
				apiErr.StatusCode,
				t.schema.Name,
			))
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return withKnownNoEffect(agent.ValidationError(msg))
		case http.StatusUnauthorized, http.StatusForbidden:
			return withKnownNoEffect(agent.PermissionError(msg))
		case http.StatusNotFound:
			return withKnownNoEffect(agent.BusinessError(msg))
		case http.StatusConflict:
			return externalOutcomeUnknown(fmt.Sprintf(
				"External tool outcome UNKNOWN: Cloud returned %d after receiving %s. The external action may have taken effect; verify before retrying.",
				apiErr.StatusCode,
				t.schema.Name,
			))
		case http.StatusTooManyRequests:
			return withKnownNoEffect(agent.TransientError(msg))
		}
		if apiErr.StatusCode >= http.StatusInternalServerError {
			return externalOutcomeUnknown(fmt.Sprintf(
				"External tool outcome UNKNOWN: Cloud returned %d after receiving %s. The external action may have taken effect; verify before retrying.",
				apiErr.StatusCode,
				t.schema.Name,
			))
		}
		prefix := classifyServerError(msg)
		return agent.ToolResult{Content: prefix + "server tool error: " + msg, IsError: true}
	}

	return externalOutcomeUnknown(fmt.Sprintf(
		"External tool outcome UNKNOWN: %s failed without dispatch-phase evidence. The external action may have taken effect; verify before retrying.",
		t.schema.Name,
	))
}

func integrationCodeMayHaveEffect(code string) bool {
	switch code {
	case "billing_error", "provider_error", "outcome_unknown":
		return true
	default:
		return false
	}
}

func integrationStatusProvesNoEffect(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func integrationCodeProvesNoEffect(code string) bool {
	switch code {
	case "not_connected", "auth_expired", "integration_limit_exceeded",
		"provider_unavailable", "feature_disabled", "tool_not_allowed",
		"idempotency_conflict":
		return true
	default:
		return false
	}
}

func (t *ServerTool) integrationAPIErrorResult(apiErr *client.IntegrationToolAPIError, material bool) agent.ToolResult {
	msg := fmt.Sprintf("integration tool %s returned %d", t.schema.Name, apiErr.StatusCode)
	switch apiErr.Code {
	case "not_connected":
		return agent.BusinessError(msg + ": the account is not connected. " + t.reconnectInstruction())
	case "auth_expired":
		return agent.BusinessError(msg + ": authorization expired. " + t.reconnectInstruction())
	case "integration_limit_exceeded":
		return agent.BusinessError(msg + ": the integration usage limit was reached")
	case "idempotency_conflict":
		return agent.ValidationError(msg + ": the request id was reused with different tool arguments")
	case "billing_error":
		return agent.BusinessError(msg + ": usage recording is still pending; do not issue a new tool call")
	case "provider_unavailable":
		return agent.TransientError(msg + ": the provider is temporarily unavailable")
	case "outcome_unknown":
		if material {
			return externalOutcomeUnknown("External tool outcome UNKNOWN: " + msg)
		}
		return agent.BusinessError(msg + ": the provider result or charge could not be confirmed; do not repeat automatically")
	case "call_in_progress":
		if material {
			return agent.BusinessError(msg + ": the original request is still in progress; wait or query its state, and do not resend the action")
		}
		return agent.BusinessError(msg + ": the original request is still in progress; do not issue a new tool call")
	case "feature_disabled":
		return agent.BusinessError(msg + ": integrations are disabled")
	case "tool_not_allowed":
		return agent.PermissionError(msg + ": the integration tool is not allowed")
	}
	if apiErr.StatusCode >= http.StatusInternalServerError || apiErr.StatusCode == http.StatusTooManyRequests {
		return agent.TransientError(msg)
	}
	if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
		return agent.PermissionError(msg)
	}
	if apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnprocessableEntity {
		return agent.ValidationError(msg)
	}
	if material && apiErr.StatusCode == http.StatusConflict {
		return externalOutcomeUnknown("External tool outcome UNKNOWN: " + msg)
	}
	return agent.BusinessError(msg)
}

func withKnownNoEffect(result agent.ToolResult) agent.ToolResult {
	result.SideEffectKnownNoEffect = true
	return result
}

func externalOutcomeUnknown(content string) agent.ToolResult {
	return agent.ToolResult{Content: content, IsError: true, SideEffectOutcomeUnknown: true}
}

// RequiresApproval returns false — the server enforces its own access control.
func (t *ServerTool) RequiresApproval() bool { return false }

// HasMaterialSideEffect keeps explicitly reviewed observational gateway tools
// out of the durable write journal. Gateway jobs that persist provider state,
// and integrations without reliable read-only annotations, remain material.
func (t *ServerTool) HasMaterialSideEffect(string) bool {
	if t.source == agent.SourceIntegration {
		if t.schema.MaterialSideEffect != nil {
			return *t.schema.MaterialSideEffect
		}
		return true
	}
	policy, registered := gatewayToolPolicies[t.schema.Name]
	return !registered || !policy.noMaterialSideEffect
}

// IsConcurrencySafeCall allows trusted observational integration schemas to
// batch while keeping unannotated and mutating integrations serial.
func (t *ServerTool) IsConcurrencySafeCall(string) bool {
	return t.source == agent.SourceIntegration &&
		t.schema.MaterialSideEffect != nil &&
		!*t.schema.MaterialSideEffect
}

// classifyServerError returns the appropriate error prefix based on the error
// message, so the agent loop's error-handling instructions can guide the model
// to retry transient failures instead of fabricating explanations.
//
// Status-code markers (returned NNN) are checked before free-text transient
// keywords so that a 4xx response body mentioning "timeout" (e.g. validation
// "timeout must be <= 30") is not mis-tagged as transient and retried.
func classifyServerError(msg string) string {
	lower := strings.ToLower(msg)
	// Status-code classification first — the HTTP status is authoritative.
	if strings.Contains(lower, "returned 401") || strings.Contains(lower, "returned 403") {
		return "[permission error] "
	}
	if strings.Contains(lower, "returned 400") || strings.Contains(lower, "returned 422") {
		return "[validation error] "
	}
	if strings.Contains(lower, "returned 429") ||
		strings.Contains(lower, "returned 502") ||
		strings.Contains(lower, "returned 503") ||
		strings.Contains(lower, "returned 504") {
		return "[transient error] "
	}
	// Keyword fallback for network-layer failures that have no HTTP status
	// (connection refused/reset, DNS, timeouts before the server responded).
	for _, sig := range []string{
		"rate limit", "timeout", "timed out", "connection refused",
		"connection reset", "eof", "unavailable",
	} {
		if strings.Contains(lower, sig) {
			return "[transient error] "
		}
	}
	return ""
}

// ToolSource implements agent.ToolSourcer for deterministic tool ordering.
func (t *ServerTool) ToolSource() agent.ToolSource { return t.source }

// IntegrationGenerationCurrent is true for non-integration tools and for an
// integration schema bound to the GatewayClient's live verified principal.
// Overlay extraction and MCP health rebuilds use it to avoid re-advertising
// stale tools even though Run also enforces the same boundary before dispatch.
func (t *ServerTool) IntegrationGenerationCurrent() bool {
	if t == nil || t.source != agent.SourceIntegration {
		return true
	}
	return t.gateway != nil &&
		t.gateway.IsIntegrationGenerationCurrent(t.integrationGeneration)
}

func requiredFieldsFromSchema(parameters map[string]any) []string {
	if parameters == nil {
		return nil
	}
	raw, ok := parameters["required"]
	if !ok {
		return nil
	}
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		required := make([]string, 0, len(values))
		for _, value := range values {
			if name, ok := value.(string); ok && name != "" {
				required = append(required, name)
			}
		}
		return required
	default:
		return nil
	}
}

func looksLikeRemoteValidationError(content string) bool {
	lower := strings.ToLower(content)
	for _, marker := range []string{
		"validation error",
		"missing required argument",
		"missing required parameter",
		"unexpected keyword argument",
		"input should be",
		"pydantic.dev",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// appendLadder returns base with a fallback-ladder block appended when one can
// be built from metadata. Idempotent: if base already contains the ladder
// header, returns base unchanged so cloud-side ladders (e.g. future
// web_fetch.py revisions that embed the ladder in error themselves) aren't
// duplicated.
func appendLadder(base string, metadata map[string]any) string {
	if strings.Contains(base, ladderHeader) {
		return base
	}
	ladder := buildFallbackLadder(metadata)
	if ladder == "" {
		return base
	}
	return base + "\n\n" + ladder
}

// buildFallbackLadder converts the cloud-side metadata["attempts"] array into
// a multi-line failure summary so the LLM sees per-provider root causes rather
// than only the last fallback's error. Returns "" when attempts contributes
// nothing useful (no failed entries, or only mid-state / "not configured"
// skipped entries).
//
// The cloud encodes attempts as a list of objects like
//
//	{"provider": "firecrawl", "status": "failed",  "error": "Firecrawl error: 403: ..."}
//	{"provider": "exa",       "status": "failed",  "error": "Exa API returned no content"}
//	{"provider": "python",    "status": "sparse_fallback"}                 // mid-state
//	{"provider": "exa",       "status": "skipped", "reason": "not configured"}
//
// Each emitted line is `audit.RedactSecrets`-sanitized first, then truncated
// to ladderDetailMaxLen runes — the order matters so secrets straddling the
// truncation boundary aren't half-chopped past the regex.
func buildFallbackLadder(metadata map[string]any) string {
	raw, ok := metadata["attempts"].([]any)
	if !ok || len(raw) == 0 {
		return ""
	}
	var lines []string
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status, _ := m["status"].(string)
		var detail string
		switch status {
		case "failed":
			if e, ok := m["error"].(string); ok && e != "" {
				detail = e
			} else if r, ok := m["reason"].(string); ok && r != "" {
				detail = r
			} else {
				detail = status
			}
		case "skipped":
			r, _ := m["reason"].(string)
			// "not configured" is housekeeping noise; everything else
			// (rate-limited, domain blocklisted, quota exhausted) gives the
			// agent something to act on.
			if r == "" || r == "not configured" {
				continue
			}
			detail = r
		default:
			// success / attempted / sparse_fallback are not real failures.
			continue
		}
		provider, _ := m["provider"].(string)
		if provider == "" {
			provider = "?"
		}
		safe := truncateLadderDetail(audit.RedactSecrets(detail))
		lines = append(lines, fmt.Sprintf("- %s %s: %s", provider, status, safe))
	}
	if len(lines) == 0 {
		return ""
	}
	return ladderHeader + "\n" + strings.Join(lines, "\n")
}

func truncateLadderDetail(s string) string {
	r := []rune(s)
	if len(r) <= ladderDetailMaxLen {
		return s
	}
	return string(r[:ladderDetailMaxLen]) + "..."
}

// stripFieldsNotInSchema removes any args keys that the server tool's schema
// does not declare under properties. Conservative: when the schema is nil,
// empty, or doesn't expose properties as a map, args is left untouched (better
// to send a possibly-extra field than to silently drop a legitimate one whose
// schema we couldn't parse).
func stripFieldsNotInSchema(args map[string]any, schemaParams map[string]any) {
	if len(args) == 0 || len(schemaParams) == 0 {
		return
	}
	propsAny, ok := schemaParams["properties"]
	if !ok {
		return
	}
	props, ok := propsAny.(map[string]any)
	if !ok {
		return
	}
	for k := range args {
		if _, declared := props[k]; !declared {
			delete(args, k)
		}
	}
}
