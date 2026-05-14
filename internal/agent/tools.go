package agent

import (
	"context"
	"sort"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type ToolInfo struct {
	Name               string
	Description        string
	Parameters         map[string]any
	Required           []string
	MaxResultSizeChars int
}

const (
	DefaultMaxToolResultSizeChars = 50000
	UnlimitedToolResultSizeChars  = -1
)

type ImageBlock struct {
	MediaType string // e.g. "image/png"
	Data      string // base64-encoded
}

// ErrorCategory classifies the nature of a tool failure so the agent
// can make informed retry decisions.
type ErrorCategory string

const (
	// ErrCategoryTransient indicates a timeout or network error. Retry may help.
	ErrCategoryTransient ErrorCategory = "transient"
	// ErrCategoryValidation indicates the tool arguments were invalid. Fix before retrying.
	ErrCategoryValidation ErrorCategory = "validation"
	// ErrCategoryBusiness indicates a policy or constraint violation. Do not retry.
	ErrCategoryBusiness ErrorCategory = "business"
	// ErrCategoryPermission indicates access was denied. Escalate to user.
	ErrCategoryPermission ErrorCategory = "permission"
)

// ToolSource classifies the origin of a tool for deterministic ordering.
type ToolSource string

const (
	SourceLocal   ToolSource = "local"
	SourceMCP     ToolSource = "mcp"
	SourceGateway ToolSource = "gateway"
)

// ToolSourcer is an optional interface tools implement to declare their origin.
// Tools that don't implement this are classified as SourceLocal.
type ToolSourcer interface {
	ToolSource() ToolSource
}

type ToolResult struct {
	Content       string
	IsError       bool
	ErrorCategory ErrorCategory // empty when IsError is false
	IsRetryable   bool          // true only for transient errors
	Images        []ImageBlock
	CloudResult   bool // true when result is a cloud deliverable (bypass LLM summarization)
	// Usage optionally reports per-call cost for this tool. Gateway tools
	// whose server returns billing info (x_search → xAI tokens, web_search
	// → SerpAPI query count) populate this so the audit logger can write a
	// cost breakdown per tool call. nil when the tool does not bill per call.
	Usage *ToolUsage
	// ContentBlocks, when non-nil, carries structured output (e.g. tool_reference
	// blocks from tool_search) that loop.go passes through verbatim as
	// tool_result content when the gateway/model supports the protocol.
	// When nil, loop.go falls back to the Content string path.
	ContentBlocks []client.ContentBlock
	// SkillToolFilter, when non-nil, restricts the tool schemas sent to
	// the LLM for the remainder of this Run() call. Only tools whose names
	// appear in this list (plus use_skill itself) will be visible. Set by
	// use_skill when the activated skill declares allowed-tools.
	SkillToolFilter []string
	// SkillToolHint, when non-empty, contains a <system-reminder> text to
	// append to the tool_result content, guiding the LLM to restrict itself
	// to the allowed tools. Works alongside SkillToolFilter (which provides
	// execution-time denial).
	SkillToolHint string
}

// ToolUsage is ToolResult's per-call cost breakdown. Mirrors client.ToolUsage
// (see internal/client/gateway.go) so tool implementations depending on agent
// don't need the client import.
type ToolUsage struct {
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	CostUSD      float64
	Units        int
	UnitType     string
}

// TransientError returns a ToolResult for timeout/network failures where retry may help.
func TransientError(msg string) ToolResult {
	return ToolResult{
		Content:       "[transient error] " + msg,
		IsError:       true,
		ErrorCategory: ErrCategoryTransient,
		IsRetryable:   true,
	}
}

// ValidationError returns a ToolResult for invalid tool arguments.
func ValidationError(msg string) ToolResult {
	return ToolResult{
		Content:       "[validation error] " + msg,
		IsError:       true,
		ErrorCategory: ErrCategoryValidation,
	}
}

// BusinessError returns a ToolResult for policy/constraint violations that must not be retried.
func BusinessError(msg string) ToolResult {
	return ToolResult{
		Content:       "[business error] " + msg,
		IsError:       true,
		ErrorCategory: ErrCategoryBusiness,
	}
}

// PermissionError returns a ToolResult for access denied scenarios requiring escalation.
func PermissionError(msg string) ToolResult {
	return ToolResult{
		Content:       "[permission error] " + msg,
		IsError:       true,
		ErrorCategory: ErrCategoryPermission,
	}
}

type Tool interface {
	Info() ToolInfo
	Run(ctx context.Context, args string) (ToolResult, error)
	RequiresApproval() bool
}

// NativeToolProvider is an optional interface for tools that use a provider's
// native tool schema (e.g., Anthropic's computer_20251124) instead of the
// standard function-calling format.
type NativeToolProvider interface {
	NativeToolDef() *client.NativeToolDef
}

// SafeChecker is an optional interface tools can implement to indicate
// certain invocations are safe and don't need approval.
type SafeChecker interface {
	IsSafeArgs(argsJSON string) bool
}

// SafeCheckerWithContext is like SafeChecker but receives the call context,
// allowing tools to use session-scoped CWD for path-based safety checks.
type SafeCheckerWithContext interface {
	IsSafeArgsWithContext(ctx context.Context, argsJSON string) bool
}

// ReadOnlyChecker is an optional interface for tools that can classify
// individual calls as read-only based on arguments.
// If args parsing fails, implementations MUST return false (fail-closed).
type ReadOnlyChecker interface {
	IsReadOnlyCall(argsJSON string) bool
}

// SkillExempt is an optional interface tools can implement to opt out of skill
// allowed-tools enforcement. Reserved for pure-infrastructure tools that have
// no I/O side effects and are needed for the skill mechanism itself or for
// reasoning hygiene (e.g. think, tool_search, use_skill). Tools that touch the
// filesystem, network, or external services MUST NOT implement this — those
// remain subject to per-skill restriction.
type SkillExempt interface {
	SkillExempt() bool
}

// IsSkillExempt reports whether a tool opts out of skill restriction.
func IsSkillExempt(t Tool) bool {
	if e, ok := t.(SkillExempt); ok {
		return e.SkillExempt()
	}
	return false
}

// autoApprovalDenyList is the canonical set of tools that require a fresh
// human decision for every call. AutoApprovalDenyList returns a copy for
// cross-package consistency tests; DisallowsAutoApproval is the runtime check.
var autoApprovalDenyList = []string{"publish_to_web", "generate_image", "edit_image"}

// AutoApprovalDenyList returns a copy of the tools that disallow any form of
// auto-approval (session always-allow, agent always-allow, global auto-approve,
// unattended/scheduled runs). Exposed for consistency tests that verify the
// agents package's persistence gate stays aligned with the runtime gate.
func AutoApprovalDenyList() []string {
	out := make([]string, len(autoApprovalDenyList))
	copy(out, autoApprovalDenyList)
	return out
}

// DisallowsAutoApproval reports tools that require a fresh human decision for
// every call. These tools may still be approved once, but global auto-approve,
// unattended runs, and session-level "always allow" must not cover them.
func DisallowsAutoApproval(toolName string) bool {
	for _, denied := range autoApprovalDenyList {
		if toolName == denied {
			return true
		}
	}
	return false
}

// ToolSummary is a lightweight name+description pair for deferred tool listings.
type ToolSummary struct {
	Name        string
	Description string
}

type ToolRegistry struct {
	tools map[string]Tool
	order []string
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]Tool),
	}
}

func (r *ToolRegistry) Register(t Tool) {
	name := t.Info().Name
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

func (r *ToolRegistry) Clone() *ToolRegistry {
	clone := NewToolRegistry()
	for _, name := range r.order {
		tool := r.tools[name]
		clone.tools[name] = tool
		clone.order = append(clone.order, name)
	}
	return clone
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Has reports whether a tool with the given name is registered. Used by
// prompt builders (e.g. agent/loop.go operationalRules) that want to
// conditionally surface a tool's documentation only when the tool itself
// is live in this run. Nil-safe — a nil registry contains no tools.
func (r *ToolRegistry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.tools[name]
	return ok
}

func (r *ToolRegistry) All() []Tool {
	tools := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		tools = append(tools, r.tools[name])
	}
	return tools
}

// Remove removes a tool from the registry by name.
func (r *ToolRegistry) Remove(name string) {
	if _, ok := r.tools[name]; !ok {
		return
	}
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

// Names returns the ordered list of tool names.
func (r *ToolRegistry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Len returns the number of registered tools.
func (r *ToolRegistry) Len() int {
	return len(r.tools)
}

// FilterByAllow returns a new registry containing only the named tools.
// Tools not found are silently skipped.
func (r *ToolRegistry) FilterByAllow(allow []string) *ToolRegistry {
	filtered := NewToolRegistry()
	for _, name := range allow {
		if t, ok := r.tools[name]; ok {
			filtered.Register(t)
		}
	}
	return filtered
}

// FilterByDeny returns a new registry with the named tools removed.
func (r *ToolRegistry) FilterByDeny(deny []string) *ToolRegistry {
	denySet := make(map[string]struct{}, len(deny))
	for _, name := range deny {
		denySet[name] = struct{}{}
	}
	filtered := NewToolRegistry()
	for _, name := range r.order {
		if _, blocked := denySet[name]; !blocked {
			filtered.Register(r.tools[name])
		}
	}
	return filtered
}

func (r *ToolRegistry) Schemas() []client.Tool {
	schemas := make([]client.Tool, 0, len(r.order))
	for _, name := range r.order {
		schemas = append(schemas, buildToolSchema(r.tools[name]))
	}
	return schemas
}

// SummaryList returns name+description for all registered tools.
func (r *ToolRegistry) SummaryList() []ToolSummary {
	summaries := make([]ToolSummary, 0, len(r.order))
	for _, name := range r.order {
		info := r.tools[name].Info()
		summaries = append(summaries, ToolSummary{Name: info.Name, Description: info.Description})
	}
	return summaries
}

// FullSchemas returns complete client.Tool schemas for the named tools.
// Unknown names are silently skipped.
func (r *ToolRegistry) FullSchemas(names []string) []client.Tool {
	schemas := make([]client.Tool, 0, len(names))
	for _, name := range names {
		if t, ok := r.tools[name]; ok {
			schemas = append(schemas, buildToolSchema(t))
		}
	}
	return schemas
}

// SortedSchemas returns tool schemas in deterministic order:
// local tools (alpha) → MCP tools (alpha) → gateway tools (alpha).
func (r *ToolRegistry) SortedSchemas() []client.Tool {
	local, mcp, gw := r.partitionBySource()
	sort.Strings(local)
	sort.Strings(mcp)
	sort.Strings(gw)

	schemas := make([]client.Tool, 0, len(r.order))
	for _, group := range [][]string{local, mcp, gw} {
		for _, name := range group {
			schemas = append(schemas, buildToolSchema(r.tools[name]))
		}
	}
	return schemas
}

// buildToolSchema converts a Tool into a client.Tool schema definition.
func buildToolSchema(t Tool) client.Tool {
	if native, ok := t.(NativeToolProvider); ok {
		def := native.NativeToolDef()
		if def != nil {
			return client.Tool{
				Type:            def.Type,
				Name:            def.Name,
				DisplayWidthPx:  def.DisplayWidthPx,
				DisplayHeightPx: def.DisplayHeightPx,
			}
		}
	}
	info := t.Info()
	params := info.Parameters
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if info.Required != nil {
		params["required"] = info.Required
	}
	return client.Tool{
		Type: "function",
		Function: client.FunctionDef{
			Name:        info.Name,
			Description: info.Description,
			Parameters:  params,
		},
	}
}

// SortedNames returns tool names in the same deterministic order as SortedSchemas.
func (r *ToolRegistry) SortedNames() []string {
	local, mcp, gw := r.partitionBySource()
	sort.Strings(local)
	sort.Strings(mcp)
	sort.Strings(gw)

	names := make([]string, 0, len(r.order))
	names = append(names, local...)
	names = append(names, mcp...)
	names = append(names, gw...)
	return names
}

// MCPNames returns the names of all MCP-origin tools in the registry. Used by
// the loop detector to mark MCP tools as batch-tolerant so legitimate
// enumerations (e.g. iterating over distinct database UUIDs) do not trip the
// NoProgress nudge on count alone.
func (r *ToolRegistry) MCPNames() []string {
	_, mcp, _ := r.partitionBySource()
	return mcp
}

// partitionBySource groups tool names by their source category.
func (r *ToolRegistry) partitionBySource() (local, mcp, gw []string) {
	for _, name := range r.order {
		t := r.tools[name]
		if sourcer, ok := t.(ToolSourcer); ok {
			switch sourcer.ToolSource() {
			case SourceMCP:
				mcp = append(mcp, name)
			case SourceGateway:
				gw = append(gw, name)
			default:
				local = append(local, name)
			}
		} else {
			local = append(local, name)
		}
	}
	return
}
