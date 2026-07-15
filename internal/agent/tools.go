package agent

import (
	"context"
	"sort"
	"sync"

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
	// SkillToolFilter, when non-nil, restricts the tools callable for the
	// remainder of this Run() call to exactly those named here (plus the
	// SkillExempt ones: use_skill/think/tool_search). A NON-NIL EMPTY slice is
	// meaningful — it restricts the skill to zero tools. A nil slice means no
	// restriction. Set by use_skill from the activated skill's allowed-tools
	// (nil when the field is absent, non-nil — possibly empty — when present).
	SkillToolFilter []string
	// SkillToolHint, when non-empty, contains a <system-reminder> text to
	// append to the tool_result content, guiding the LLM to restrict itself
	// to the allowed tools. Works alongside SkillToolFilter (which provides
	// execution-time denial).
	SkillToolHint string
	// InternalOnly marks a daemon-synthesized tool_result that must be
	// written to the LLM transcript (so the tool_use/tool_result pairing
	// the API requires stays intact) but MUST NOT be pushed to SSE/WS
	// clients. Used for the [output_truncated] recovery prompt — that text
	// is addressed to the LLM ("you got cut off, break the work smaller"),
	// not to the human user. Without this flag, clients render it as a red
	// error card and users think the run failed.
	//
	// Field is intentionally untagged for JSON — it never crosses the wire.
	InternalOnly bool `json:"-"`
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

// CancelableMidTurn is an optional interface a tool can implement to opt
// into mid-turn cancellation. When the user submits a new message while a
// tool is running (CancelReason=Interrupt path), the daemon honors this
// signal:
//
//   - tools that DO implement CancelableMidTurn() returning true are
//     aborted via ctx cancellation; their goroutine is expected to return
//     promptly via ctx-aware HTTP / subprocess / file IO.
//   - tools that do NOT implement this interface (or return false) keep
//     running until natural completion; the queued user message becomes
//     the next turn's prompt after this tool finishes.
//
// Default is "block" (safer): the queued submit only fires after the
// in-flight tool resolves. Only proven-safe pure-read or trivially
// cancellable tools should opt in.
//
// Explicit opt-in list (see internal/tools/cancelable_optin.go):
// file_read, glob, grep, directory_list, think, system_info,
// memory_recall, session_search, list_my_published_files, tool_search,
// use_skill, schedule_list, plus pdf/docx/xlsx/pptx text extractors
// (subprocess-based, respond cleanly to ctx cancel).
//
// retract_published_file and http are deliberately NOT cancelable: HTTP
// methods may be non-idempotent (POST/DELETE) and aborting mid-flight
// can leave the remote system in an inconsistent state.
type CancelableMidTurn interface {
	CancelableMidTurn() bool
}

// IsCancelableMidTurn reports whether a tool opts into mid-turn cancel.
// Returns false (block) by default — the safer choice.
//
// Two layers: (1) explicit CancelableMidTurn interface implementation,
// (2) a centralized name-based allowlist for builtin tools that we
// deliberately decline to modify file-by-file. The allowlist is the
// pragmatic path for shipping mid-turn cancel without touching ~30 tool
// source files; the interface remains the durable mechanism (third-party / MCP
// tools can opt in directly).
func IsCancelableMidTurn(t Tool) bool {
	if c, ok := t.(CancelableMidTurn); ok {
		return c.CancelableMidTurn()
	}
	if t == nil {
		return false
	}
	return isBuiltinCancelable(t.Info().Name)
}

// builtinCancelableMidTurn lists builtin tools that may be aborted via
// ctx cancellation when the user submits a new message mid-tool. Anything
// not in this list (and not implementing CancelableMidTurn) is treated as
// blocking — the queued message waits for natural tool completion.
//
// Hard-blocked tools (deliberately omitted):
//   - bash, file_write, file_edit, archive_extract, process (state-mutating)
//   - publish_to_web, generate_image, edit_image, retract_published_file
//     (paid quota / non-idempotent network)
//   - cloud_delegate, memory_append (remote / file-lock-protected state)
//   - http (verb-polymorphic; aborting POST/DELETE risks inconsistent
//     remote state)
//   - schedule_create, schedule_update, schedule_remove (modifies plist)
//   - accessibility, applescript, screenshot, computer, clipboard,
//     notify, browser, wait_for, ghostty (GUI side effects)
var builtinCancelableMidTurn = map[string]struct{}{
	"file_read":               {},
	"glob":                    {},
	"grep":                    {},
	"directory_list":          {},
	"think":                   {},
	"system_info":             {},
	"memory_recall":           {},
	"session_search":          {},
	"list_my_published_files": {},
	"tool_search":             {},
	"use_skill":               {},
	"schedule_list":           {},
	"archive_inspect":         {},
	"pdf_to_text":             {},
	"docx_to_text":            {},
	"xlsx_to_text":            {},
	"pptx_to_text":            {},
}

func isBuiltinCancelable(name string) bool {
	if name == "" {
		return false
	}
	_, ok := builtinCancelableMidTurn[name]
	return ok
}

// autoApprovalDenyList is the canonical set of tools that REFUSE to be
// persisted into a user-facing "always allow" list (per-agent or global).
// AutoApprovalDenyList returns a copy for cross-package consistency tests;
// DisallowsAutoApproval is the runtime check.
//
// As of 2026-05-18 this list is intentionally empty: publish_to_web /
// generate_image / edit_image used to be on it because of paid + permanent
// CDN concerns. The product decision was to treat them as ordinary
// approval-required tools — fresh prompt the first time, "always allow"
// persists for future calls. The path-allowlist and basename-blocklist
// guards in `internal/tools/publish_to_web.go` are still in place as
// independent protection.
//
// The plumbing (DisallowsAutoApproval + all call sites) is preserved as a
// hook for a future tool that genuinely cannot be persisted (account
// deletion, payment authorization, etc.). See unattendedAutoApprovalDenyList
// below for the parallel unattended-only gate — also currently empty.
var autoApprovalDenyList = []string{}

// unattendedAutoApprovalDenyList is the set of tools that scheduled
// (unattended) agent runs MUST NOT auto-approve. As of 2026-05-18 this list
// is empty: the product call (same one that emptied autoApprovalDenyList)
// chose to treat publish_to_web / generate_image / edit_image as ordinary
// approval-required tools across all paths — if a user adds them to
// always-allow, that consent extends to scheduled / watcher / heartbeat
// invocations too, no separate gate.
//
// The plumbing (DisallowsUnattendedAutoApproval + every handler that calls
// it) is preserved so a future tool that genuinely cannot run unattended
// (e.g. payment authorization, account deletion) can be added here without
// rewriting callers. Empty for now.
var unattendedAutoApprovalDenyList = []string{}

// AutoApprovalDenyList returns a copy of the tools that disallow being
// persisted into a user-facing always-allow list. Exposed for consistency
// tests that verify the agents package's persistence gate stays aligned
// with the runtime gate.
//
// This is the "attended" gate (user is at the keyboard and explicitly opted
// into skipping prompts). For the unattended scheduled-run gate, see
// DisallowsUnattendedAutoApproval / unattendedAutoApprovalDenyList.
func AutoApprovalDenyList() []string {
	out := make([]string, len(autoApprovalDenyList))
	copy(out, autoApprovalDenyList)
	return out
}

// DisallowsAutoApproval reports tools that refuse "always allow" persistence.
// These tools may still be approved once, but the persistence pathway (the
// "Always Allow" button in Desktop, hand-edited config.yaml entries) is
// refused at multiple layers.
//
// Currently empty — see autoApprovalDenyList for the policy decision.
// Scheduled-run gating is INTENTIONALLY separate, since attended consent
// ("I'm clicking always-allow right now") is a different surface than
// unattended consent ("this cron fires at 3am unsupervised").
func DisallowsAutoApproval(toolName string) bool {
	for _, denied := range autoApprovalDenyList {
		if toolName == denied {
			return true
		}
	}
	return false
}

// DisallowsUnattendedAutoApproval reports tools that MUST NOT be
// auto-approved by scheduled or otherwise-unattended agent runs, even if
// the user has them in an always-allow list. Compare with
// DisallowsAutoApproval, which gates attended ("I'm watching") consent.
//
// Caller: scheduleHandler.OnApprovalNeeded. Other unattended paths added in
// the future (RemoteTrigger auto-run, system-level retries, etc.) should
// route through this check before auto-approving.
func DisallowsUnattendedAutoApproval(toolName string) bool {
	for _, denied := range unattendedAutoApprovalDenyList {
		if toolName == denied {
			return true
		}
	}
	return false
}

// UnattendedAutoApprovalDenyList returns a copy of the unattended deny list.
// Exposed for tests and any future debugging UI that wants to surface the
// guard policy to operators.
func UnattendedAutoApprovalDenyList() []string {
	out := make([]string, len(unattendedAutoApprovalDenyList))
	copy(out, unattendedAutoApprovalDenyList)
	return out
}

// ToolSummary is a lightweight name+description pair for deferred tool listings.
type ToolSummary struct {
	Name        string
	Description string
}

type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]Tool),
	}
}

func (r *ToolRegistry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Info().Name
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

func (r *ToolRegistry) Clone() *ToolRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := NewToolRegistry()
	for _, name := range r.order {
		tool := r.tools[name]
		clone.tools[name] = tool
		clone.order = append(clone.order, name)
	}
	return clone
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

func (r *ToolRegistry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		tools = append(tools, r.tools[name])
	}
	return tools
}

// Remove removes a tool from the registry by name.
func (r *ToolRegistry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Len returns the number of registered tools.
func (r *ToolRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// FilterByAllow returns a new registry containing only the named tools.
// Tools not found are silently skipped.
func (r *ToolRegistry) FilterByAllow(allow []string) *ToolRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	schemas := make([]client.Tool, 0, len(r.order))
	for _, name := range r.order {
		schemas = append(schemas, buildToolSchema(r.tools[name]))
	}
	return schemas
}

// SummaryList returns name+description for all registered tools.
func (r *ToolRegistry) SummaryList() []ToolSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	local, mcp, gw := r.partitionBySourceLocked()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	local, mcp, gw := r.partitionBySourceLocked()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, mcp, _ := r.partitionBySourceLocked()
	return mcp
}

// partitionBySource groups tool names by their source category.
func (r *ToolRegistry) partitionBySource() (local, mcp, gw []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.partitionBySourceLocked()
}

func (r *ToolRegistry) partitionBySourceLocked() (local, mcp, gw []string) {
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
