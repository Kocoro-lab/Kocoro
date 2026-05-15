package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

// ServerTool wraps a server-side tool schema and proxies execution
// through the gateway's /api/v1/tools/{name}/execute endpoint.
type ServerTool struct {
	schema  client.ServerToolSchema
	gateway *client.GatewayClient
}

func NewServerTool(schema client.ServerToolSchema, gateway *client.GatewayClient) *ServerTool {
	return &ServerTool{schema: schema, gateway: gateway}
}

func (t *ServerTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        t.schema.Name,
		Description: t.schema.Description,
		Parameters:  t.schema.Parameters,
	}
}

func (t *ServerTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args map[string]any
	if argsJSON != "" && argsJSON != "{}" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return agent.ToolResult{
				Content: fmt.Sprintf("invalid arguments: %v", err),
				IsError: true,
			}, nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}

	resp, err := t.gateway.ExecuteTool(ctx, t.schema.Name, args, "")
	if err != nil {
		msg := err.Error()
		prefix := classifyServerError(msg)
		return agent.ToolResult{
			Content: fmt.Sprintf("%sserver tool error: %v", prefix, err),
			IsError: true,
		}, nil
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
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			TotalTokens:  totalTokens,
			CostUSD:      u.CostUSD,
			Units:        u.Units,
			UnitType:     u.UnitType,
		}
		agent.EmitUsage(ctx, agent.TurnUsage{
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			TotalTokens:  totalTokens,
			CostUSD:      u.CostUSD,
			// Gateway tool calls are not LLM calls from the driving model's
			// perspective — leave LLMCalls=0 so session LLMCalls stays clean.
			Model: model,
		})
	}

	if resp.Error != nil && *resp.Error != "" {
		content := *resp.Error
		if !resp.Success {
			content = appendLadder(content, resp.Metadata)
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

// RequiresApproval returns false — the server enforces its own access control.
func (t *ServerTool) RequiresApproval() bool { return false }

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
func (t *ServerTool) ToolSource() agent.ToolSource { return agent.SourceGateway }

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
