package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

// Sub-agent cost control defaults. Sub-agents are expensive (each spawns a
// full LLM session), so we apply tighter limits than the parent agent.
const (
	SubAgentMaxTokens     = 8_000 // CC uses 8K; BQ p99 is ~5K tokens
	SubAgentMaxIterations = 15    // Tighter than parent's default 25
)

type SubAgentTool struct {
	agentsDir string              // path to ~/.shannon/agents/
	runner    SubAgentRunner      // injected by daemon/TUI runner per-run
	baseReg   *agent.ToolRegistry // parent's tool registry; child clones from this
	lastUsage *agent.ToolUsage    // set after each Run(); read by loop via UsageReporter
	onStatus  func(id, agentType, description, status, toolName, toolArgs string, tokens int, taskID string) // TUI status callback
}

type subagentArgs struct {
	Prompt      string `json:"prompt"`
	Description string `json:"description"`
	AgentType   string `json:"agent_type"`
	TaskID      string `json:"task_id,omitempty"`
}

func (t *SubAgentTool) Info() agent.ToolInfo {
	desc := "Launch a named agent to handle a task autonomously using LOCAL tools (file read/write, bash, grep, etc). " +
		"Each sub-agent runs in its own context — it cannot see this conversation. " +
		"Results are returned as a concise summary, keeping your context clean.\n\n" +
		"USE THIS TOOL (not cloud_delegate) when:\n" +
		"- The task involves LOCAL files — subagent can read/write local files, cloud_delegate CANNOT\n" +
		"- The user asks for 2+ separate tasks — one sub-agent per task, all in a SINGLE response (parallel)\n" +
		"- A task needs reading files then producing analysis or reports\n" +
		"- Deep exploration of local codebase or documents\n\n" +
		"Skip only for a single quick action (one file, one search, one question).\n\n" +
		"The sub-agent cannot spawn further sub-agents."

	agentList := t.listAvailableAgents()
	if agentList != "" {
		desc += "\n\nAvailable agent_type values:\n" + agentList
	} else {
		desc += "\n\nNo agents configured. Create agents under ~/.shannon/agents/<name>/AGENT.md."
	}

	return agent.ToolInfo{
		Name:        "subagent",
		Description: desc,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "Complete task description. Include all context — the sub-agent cannot see the parent conversation.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Short 3-5 word summary",
				},
				"agent_type": map[string]any{
					"type":        "string",
					"description": "Named agent to use (e.g., 'scout', 'worker', 'checker'). Must be a configured agent.",
				},
				"task_id": map[string]any{
					"type":        "string",
					"description": "Task ID to link this sub-agent to (from task_create). Enables unified progress tracking in the UI.",
				},
			},
		},
		Required: []string{"prompt", "description", "agent_type"},
	}
}

func (t *SubAgentTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args subagentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Prompt == "" {
		return agent.ValidationError("prompt is required"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("description is required"), nil
	}
	if args.AgentType == "" {
		return agent.ValidationError("agent_type is required — specify a named agent (e.g., 'scout', 'worker')"), nil
	}
	if t.runner == nil {
		return agent.ToolResult{Content: "subagent runner not configured — this tool requires runtime wiring by the daemon or TUI", IsError: true}, nil
	}

	// Load and validate the named agent
	ag, err := agents.LoadAgent(t.agentsDir, args.AgentType)
	if err != nil {
		available := t.listAvailableAgents()
		msg := fmt.Sprintf("agent %q not found: %v", args.AgentType, err)
		if available != "" {
			msg += "\n\nAvailable agents:\n" + available
		}
		return agent.ValidationError(msg), nil
	}

	// Build child tool registry: clone parent, remove subagent, apply agent filters
	if t.baseReg == nil {
		return agent.ToolResult{Content: "subagent base registry not configured", IsError: true}, nil
	}
	childReg := t.buildChildRegistry(ag)

	// Notify TUI of sub-agent start
	callID := fmt.Sprintf("subagent-%s-%d", args.AgentType, time.Now().UnixNano())
	if t.onStatus != nil {
		t.onStatus(callID, args.AgentType, args.Description, "started", "", "", 0, args.TaskID)
	}

	// Create bridge handler for child → parent event forwarding + approval.
	// Must be unconditional: onStatus may be nil (daemon/one-shot), but the
	// handler is still needed for OnApprovalNeeded to auto-approve child tools.
	bridgeHandler := &subAgentBridgeHandler{
		callID:    callID,
		agentType: args.AgentType,
		desc:      args.Description,
		taskID:    args.TaskID,
		onStatus:  t.onStatus,
	}

	// Delegate to the runner
	result := t.runner(ctx, ag, args.Prompt, childReg, bridgeHandler)

	// Notify TUI of sub-agent completion
	if t.onStatus != nil {
		if result.Err != nil {
			t.onStatus(callID, args.AgentType, args.Description, "error", "", "", result.TotalTokens, args.TaskID)
		} else {
			t.onStatus(callID, args.AgentType, args.Description, "completed", "", "", result.TotalTokens, args.TaskID)
		}
	}

	// Record child usage for UsageReporter interface
	t.lastUsage = &agent.ToolUsage{
		InputTokens:  result.InputTokens,
		OutputTokens: result.OutputTokens,
		TotalTokens:  result.TotalTokens,
		LLMCalls:     result.LLMCalls,
		CostUSD:      result.CostUSD,
	}

	if result.Err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("Sub-agent \"%s\" failed: %v", args.Description, result.Err),
			IsError: true,
		}, nil
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Sub-agent \"%s\" completed", args.Description)
	if result.TotalTokens > 0 {
		fmt.Fprintf(&summary, " (%d tokens, %d LLM calls)", result.TotalTokens, result.LLMCalls)
	}
	summary.WriteString(".\n\n")
	summary.WriteString(result.Output)

	return agent.ToolResult{Content: summary.String(), Usage: t.lastUsage}, nil
}

func (t *SubAgentTool) buildChildRegistry(ag *agents.Agent) *agent.ToolRegistry {
	reg := t.baseReg.Clone()
	reg.Remove("subagent")

	if ag.Config != nil && ag.Config.Tools != nil {
		if len(ag.Config.Tools.Allow) > 0 {
			reg = reg.FilterByAllow(ag.Config.Tools.Allow)
		} else if len(ag.Config.Tools.Deny) > 0 {
			reg = reg.FilterByDeny(ag.Config.Tools.Deny)
		}
	}
	return reg
}

// SetRunner injects the runner. Called by daemon/TUI per-run.
func (t *SubAgentTool) SetRunner(r SubAgentRunner) {
	t.runner = r
}

// SetBaseRegistry updates the parent tool registry snapshot.
func (t *SubAgentTool) SetBaseRegistry(reg *agent.ToolRegistry) {
	t.baseReg = reg
}

// SetAgentsDir updates the agents directory.
func (t *SubAgentTool) SetAgentsDir(dir string) {
	t.agentsDir = dir
}

// SetOnStatus sets a callback for sub-agent lifecycle events (TUI progress display).
func (t *SubAgentTool) SetOnStatus(fn func(id, agentType, description, status, toolName, toolArgs string, tokens int, taskID string)) {
	t.onStatus = fn
}

// subAgentBridgeHandler forwards child loop tool events to the parent's onStatus callback.
type subAgentBridgeHandler struct {
	callID    string
	agentType string
	desc      string
	taskID    string
	onStatus  func(id, agentType, description, status, toolName, toolArgs string, tokens int, taskID string)
	tokens    int
}

func (h *subAgentBridgeHandler) OnToolCall(name string, args string) {
	if h.onStatus != nil {
		h.onStatus(h.callID, h.agentType, h.desc, "tool_call", name, args, h.tokens, h.taskID)
	}
}

func (h *subAgentBridgeHandler) OnToolResult(name string, args string, result agent.ToolResult, elapsed time.Duration) {
}
func (h *subAgentBridgeHandler) OnText(text string)                                                 {}
func (h *subAgentBridgeHandler) OnStreamDelta(delta string)                                         {}
func (h *subAgentBridgeHandler) OnApprovalNeeded(tool string, args string) bool                     { return true }
func (h *subAgentBridgeHandler) OnUsage(usage agent.TurnUsage)                                      {}
func (h *subAgentBridgeHandler) OnCloudAgent(agentID string, status string, message string)         {}
func (h *subAgentBridgeHandler) OnCloudProgress(completed int, total int)                           {}
func (h *subAgentBridgeHandler) OnCloudPlan(planType string, content string, needsReview bool)      {}

func (t *SubAgentTool) RequiresApproval() bool { return false }


// IsReadOnlyCall returns true so that multiple subagent calls issued in the
// same LLM turn are batched and executed concurrently by partitionToolCalls.
// Each child loop runs in full isolation (own context, cloned registry),
// so parallel execution is safe regardless of what the child does internally.
func (t *SubAgentTool) IsReadOnlyCall(string) bool { return true }

// --- agent listing helpers ---

func (t *SubAgentTool) listAvailableAgents() string {
	if t.agentsDir == "" {
		return ""
	}
	seen := make(map[string]bool)
	var lines []string
	scanAgentDir(t.agentsDir, seen, &lines)
	scanAgentDir(filepath.Join(t.agentsDir, "_builtin"), seen, &lines)
	return strings.Join(lines, "\n")
}

func scanAgentDir(dir string, seen map[string]bool, lines *[]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		if seen[name] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name, "AGENT.md"))
		if err != nil {
			continue
		}
		seen[name] = true
		firstLine := extractFirstContentLine(string(data))
		if firstLine != "" {
			*lines = append(*lines, fmt.Sprintf("- %s: %s", name, firstLine))
		} else {
			*lines = append(*lines, fmt.Sprintf("- %s", name))
		}
	}
}

func extractFirstContentLine(md string) string {
	for line := range strings.SplitSeq(md, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "---") {
			continue
		}
		if len(line) > 120 {
			return line[:120] + "..."
		}
		return line
	}
	return ""
}
