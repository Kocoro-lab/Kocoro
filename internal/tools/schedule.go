package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

// scheduleAudienceDisclaimer is appended to every ScheduleTool description so
// the LLM treats these as its own tools rather than user-typed commands. The
// 我帮你取消 example is bilingual on purpose — Chinese users in the wild were
// the original failure mode the line addresses.
const scheduleAudienceDisclaimer = "Audience: this tool is for YOU to call, not a command the user can type. Never tell the user to 'use schedule_remove' or 'call schedule_show' — just say what you'll do (e.g. '我帮你取消') and call the tool yourself."

type ScheduleTool struct {
	manager    *schedule.Manager
	action     string
	shannonDir string // root for resolving session files in schedule_show
}

func NewScheduleTools(mgr *schedule.Manager, shannonDir string) []agent.Tool {
	return []agent.Tool{
		&ScheduleTool{manager: mgr, action: "create", shannonDir: shannonDir},
		&ScheduleTool{manager: mgr, action: "list", shannonDir: shannonDir},
		&ScheduleTool{manager: mgr, action: "update", shannonDir: shannonDir},
		&ScheduleTool{manager: mgr, action: "remove", shannonDir: shannonDir},
		&ScheduleTool{manager: mgr, action: "show", shannonDir: shannonDir},
	}
}

func (t *ScheduleTool) Info() agent.ToolInfo {
	switch t.action {
	case "create":
		return agent.ToolInfo{
			Name: "schedule_create",
			Description: "Create a scheduled task that runs an agent on a cron schedule. Supports full cron syntax (ranges, steps, lists). " +
				"Storage: for a NAMED agent, every run appends turns to that agent's single ongoing session file (one file, growing). " +
				"For the default agent, every run creates a brand-new session file under ~/.shannon/sessions/. " +
				"Showing the user the results: when the user asks 'what did the schedule produce' or 'show me yesterday's run', call schedule_show yourself (it returns the last assistant turns of that run, sliced to the run's own message range so you don't see unrelated interactive chat from the shared session). Never tell the user to run session_search themselves. " +
				scheduleAudienceDisclaimer +
				agent.DescriptionGuidance,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": map[string]any{
						"type": "string",
						"description": "Agent name (from ~/.shannon/agents/). " +
							"When you're handling a conversation as a named agent (sticky context shows `Agent: <name>`), " +
							"pass that name so future runs use the same persona AND the user can find results via session_search inside the same agent. " +
							"When you're handling a conversation as the default agent (sticky context shows `Agent: default`), " +
							"pass an empty string — runs will execute under the default agent identity, results land in the global " +
							"~/.shannon/sessions/ pool, and the reply broadcasts to whichever channels Cloud has bound to the default agent " +
							"(including the current Slack/IM channel if that's how this conversation reached you). " +
							"Treat default and named agents symmetrically — neither is 'rare', the choice follows the current conversation identity.",
					},
					"cron": map[string]any{"type": "string", "description": "5-field cron expression (minute hour day month weekday). Supports */5, 1-5, 1,3,5."},
					"prompt": map[string]any{
						"type": "string",
						"description": "The instruction template applied verbatim on each scheduled fire. " +
							"Write it as a complete, self-contained user-message the agent could execute with zero additional context — the agent gets no memory that this is a scheduled run vs an interactive ask. " +
							"Restate the user's intent as an actionable instruction; do not just echo keywords from the user's request. " +
							"BAD: user asks '每分钟说一次你好' → prompt='你好' (agent receives a bare greeting and replies 'how can I help?' — the run produces nothing useful). " +
							"GOOD: user asks '每分钟说一次你好' → prompt='请生成一句简短的「你好」问候作为本次定时任务的输出,只说一句,不要追问' (agent has a clear deliverable, run produces a useful artifact).",
					},
					"stateful": map[string]any{
						"type":    "boolean",
						"default": false,
						"description": "Only meaningful for named agents (ignored for the default agent). " +
							"false (default, recommended): each run starts with no prior conversation history — best for digests, polling, daily reports, monitoring, and any task where runs are independent. " +
							"true: each run sees the conversation from prior runs — only choose this when the user explicitly wants the agent to remember and build on previous runs (e.g. continuous research, ongoing project tracking with follow-up questions).",
					},
					"description": agent.DescriptionFieldSpec,
				},
			},
			Required: []string{"cron", "prompt", "description"},
		}
	case "list":
		return agent.ToolInfo{
			Name: "schedule_list",
			Description: "List all locally scheduled tasks with their status. " +
				scheduleAudienceDisclaimer,
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		}
	case "update":
		return agent.ToolInfo{
			Name: "schedule_update",
			Description: "Update an existing scheduled task. " +
				scheduleAudienceDisclaimer +
				agent.DescriptionGuidance,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":      map[string]any{"type": "string", "description": "Schedule ID"},
					"cron":    map[string]any{"type": "string", "description": "New cron expression"},
					"prompt":  map[string]any{"type": "string", "description": "New prompt"},
					"enabled": map[string]any{"type": "boolean", "description": "Enable or disable"},
					"stateful": map[string]any{
						"type":        "boolean",
						"description": "Change history-preservation behaviour for named-agent schedules. Omit to leave unchanged. false = each run starts fresh; true = each run sees prior history. Has no effect on default-agent schedules.",
					},
					"description": agent.DescriptionFieldSpec,
				},
			},
			Required: []string{"id", "description"},
		}
	case "remove":
		return agent.ToolInfo{
			Name: "schedule_remove",
			Description: "Remove a scheduled task. " +
				scheduleAudienceDisclaimer +
				agent.DescriptionGuidance,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "description": "Schedule ID to remove"},
					"description": agent.DescriptionFieldSpec,
				},
			},
			Required: []string{"id", "description"},
		}
	case "show":
		return agent.ToolInfo{
			Name: "schedule_show",
			Description: "Show the most recent run of a scheduled task. Returns when it last fired plus a summary of the last assistant turns from that run. Use this when the user asks what a schedule produced (e.g. 'what did my daily report say' or 'show me the last run'); do not push the user to call session_search themselves. " +
				scheduleAudienceDisclaimer +
				agent.DescriptionGuidance,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "description": "Schedule ID (from schedule_list)."},
					"max_turns":   map[string]any{"type": "integer", "default": 5, "minimum": 1, "maximum": 20, "description": "How many most-recent assistant turns to include. Defaults to 5; clamped to 1-20."},
					"description": agent.DescriptionFieldSpec,
				},
			},
			Required: []string{"id", "description"},
		}
	}
	return agent.ToolInfo{}
}

func (t *ScheduleTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError("invalid args: " + err.Error()), nil
	}
	switch t.action {
	case "create":
		agentName, agentExplicit := args["agent"].(string)
		// When the LLM omits the agent arg entirely, default to the caller's
		// own agent so results stay reachable via session_search inside the
		// same agent. "agent": "" (explicit empty) still means default agent.
		if !agentExplicit {
			if ctxAgent, ok := agent.AgentNameFromContext(ctx); ok {
				agentName = ctxAgent
			}
		}
		cron, _ := args["cron"].(string)
		prompt, _ := args["prompt"].(string)
		description, _ := args["description"].(string)
		// Validate every Required field with ValidationError so the loop
		// detector's [validation error] short-circuit can stop a model
		// stuck retrying the same missing-field call. Required for
		// schedule_create: cron, prompt, description.
		if cron == "" {
			return agent.ValidationError("cron is required"), nil
		}
		if prompt == "" {
			return agent.ValidationError("prompt is required"), nil
		}
		if description == "" {
			return agent.ValidationError("description is required"), nil
		}
		stateful, _ := args["stateful"].(bool) // missing → false (Go zero value, matches HTTP/CLI default)
		id, err := t.manager.Create(agentName, cron, prompt, stateful)
		if err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		// Capture and save the current conversation context so the agent
		// can understand the task background when the schedule fires.
		if ctxMsgs := extractConversationContext(ctx); len(ctxMsgs) > 0 {
			if saveErr := t.manager.SaveContext(id, ctxMsgs); saveErr != nil {
				return agent.ToolResult{Content: fmt.Sprintf("Schedule created: %s (warning: failed to save context: %v)", id, saveErr)}, nil
			}
		}
		msg := fmt.Sprintf("Schedule created: %s", id)
		if w := t.triggerConflictWarning(agentName); w != "" {
			msg += "\n" + w
		}
		return agent.ToolResult{Content: msg}, nil
	case "list":
		list, err := t.manager.List()
		if err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		if len(list) == 0 {
			return agent.ToolResult{Content: "No scheduled tasks."}, nil
		}
		var sb strings.Builder
		for _, s := range list {
			agentDisplay := s.Agent
			if agentDisplay == "" {
				agentDisplay = "(default)"
			}
			ctxTag := ""
			if t.manager.HasContext(s.ID) {
				ctxTag = " [ctx]"
			}
			fmt.Fprintf(&sb, "%s | agent=%s | cron=%s | enabled=%v | sync=%s | %s%s\n",
				s.ID, agentDisplay, s.Cron, s.Enabled, s.SyncStatus, s.Prompt, ctxTag)
		}
		return agent.ToolResult{Content: sb.String()}, nil
	case "update":
		id, _ := args["id"].(string)
		description, _ := args["description"].(string)
		// Required for schedule_update: id, description.
		if id == "" {
			return agent.ValidationError("id is required"), nil
		}
		if description == "" {
			return agent.ValidationError("description is required"), nil
		}
		opts := &schedule.UpdateOpts{}
		if v, ok := args["cron"].(string); ok {
			opts.Cron = &v
		}
		if v, ok := args["prompt"].(string); ok {
			opts.Prompt = &v
		}
		if v, ok := args["enabled"].(bool); ok {
			opts.Enabled = &v
		}
		if v, ok := args["stateful"].(bool); ok {
			opts.Stateful = &v
		}
		if opts.Cron == nil && opts.Prompt == nil && opts.Enabled == nil && opts.Stateful == nil {
			return agent.ValidationError("at least one of cron, prompt, enabled, or stateful is required"), nil
		}
		if err := t.manager.Update(id, opts); err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		msg := fmt.Sprintf("Schedule %s updated.", id)
		// Look up the updated schedule to resolve its agent name before
		// checking for trigger conflicts. Best-effort: lookup errors are
		// silently swallowed since warnings are additive-only.
		if sched, err := t.manager.Get(id); err == nil && sched != nil {
			if w := t.triggerConflictWarning(sched.Agent); w != "" {
				msg += "\n" + w
			}
		}
		return agent.ToolResult{Content: msg}, nil
	case "remove":
		id, _ := args["id"].(string)
		description, _ := args["description"].(string)
		// Required for schedule_remove: id, description.
		if id == "" {
			return agent.ValidationError("id is required"), nil
		}
		if description == "" {
			return agent.ValidationError("description is required"), nil
		}
		if err := t.manager.Remove(id); err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("Schedule %s removed.", id)}, nil
	case "show":
		id, _ := args["id"].(string)
		description, _ := args["description"].(string)
		// Required for schedule_show: id, description.
		if id == "" {
			return agent.ValidationError("id is required"), nil
		}
		if description == "" {
			return agent.ValidationError("description is required"), nil
		}
		sched, err := t.manager.Get(id)
		if err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		maxTurns := 5
		if v, ok := args["max_turns"].(float64); ok && v > 0 {
			maxTurns = int(v)
		}
		summary, err := schedule.SummarizeLastRun(*sched, t.shannonDir, maxTurns)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("schedule_show: %v", err), IsError: true}, nil
		}
		if summary.SessionID == "" {
			return agent.ToolResult{Content: fmt.Sprintf("Schedule %s has not run yet.", id)}, nil
		}
		var sb strings.Builder
		if summary.LastRunAt != nil {
			fmt.Fprintf(&sb, "Schedule %s last ran at %s (session %s).\n", id, summary.LastRunAt.Format(time.RFC3339), summary.SessionID)
		} else {
			fmt.Fprintf(&sb, "Schedule %s (session %s):\n", id, summary.SessionID)
		}
		if len(summary.Turns) == 0 {
			sb.WriteString("(session has no assistant turns yet)")
		} else {
			for i, turn := range summary.Turns {
				fmt.Fprintf(&sb, "\n--- assistant turn %d ---\n%s\n", i+1, turn.Text)
			}
		}
		return agent.ToolResult{Content: sb.String()}, nil
	}
	return agent.ToolResult{Content: "unknown action", IsError: true}, nil
}

func (t *ScheduleTool) RequiresApproval() bool {
	switch t.action {
	case "list", "show":
		return false
	}
	return true
}

func (t *ScheduleTool) IsReadOnlyCall(string) bool {
	switch t.action {
	case "list", "show":
		return true
	}
	return false
}

// triggerConflictWarning returns a user-facing warning line (with the leading
// "⚠️ Note:" marker) when the named agent has a non-zero heartbeat AND an
// enabled schedule referencing it. Returns an empty string on no conflict, on
// an empty agent name, or when lookups fail — this is visibility only, never
// a hard error.
func (t *ScheduleTool) triggerConflictWarning(agentName string) string {
	if agentName == "" {
		return ""
	}
	shanDir := config.ShannonDir()
	if shanDir == "" {
		return ""
	}
	agentsDir := filepath.Join(shanDir, "agents")

	list, err := t.manager.List()
	if err != nil {
		return ""
	}
	refs := make([]agents.ScheduleRef, 0, len(list))
	for _, s := range list {
		refs = append(refs, agents.ScheduleRef{ID: s.ID, Agent: s.Agent, Enabled: s.Enabled})
	}
	warnings := agents.DetectTriggerConflicts(agentsDir, agentName, refs)
	if len(warnings) == 0 {
		return ""
	}
	return "⚠️ Note: " + warnings[0]
}

// extractConversationContext pulls a compact context from the live
// conversation snapshot. It keeps only plain-text user/assistant messages
// and skips system, tool_use, and tool_result messages. At most the last
// 20 messages are kept, with total text capped at 8000 runes.
func extractConversationContext(ctx context.Context) []schedule.ContextMessage {
	snapshotFn := agent.ConversationSnapshotFromContext(ctx)
	if snapshotFn == nil {
		return nil
	}
	messages := snapshotFn()
	if len(messages) == 0 {
		return nil
	}

	// Filter: keep only plain-text user/assistant messages.
	//
	// For block content we concatenate ONLY the text blocks, never
	// tool_result blocks. MessageContent.Text() merges tool_result payloads
	// into its output, and those payloads can include spill file paths
	// (~/.shannon/tmp/…) or other internal infrastructure text that must
	// never surface as "conversation context".
	var filtered []schedule.ContextMessage
	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		var text string
		if msg.Content.HasBlocks() {
			var sb strings.Builder
			for _, b := range msg.Content.Blocks() {
				if b.Type == "text" && b.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(b.Text)
				}
			}
			text = strings.TrimSpace(sb.String())
		} else {
			text = strings.TrimSpace(msg.Content.Text())
		}
		if text == "" {
			continue
		}
		filtered = append(filtered, schedule.ContextMessage{
			Role:    msg.Role,
			Content: text,
		})
	}

	// Keep only the most recent 20 messages.
	const maxMessages = 20
	if len(filtered) > maxMessages {
		filtered = filtered[len(filtered)-maxMessages:]
	}

	// Cap total text at 8000 runes (not bytes — Chinese is 3 bytes/char, so
	// a byte budget would give ~2666 effective chars). Drop oldest first.
	const maxChars = 8000
	totalChars := 0
	for _, m := range filtered {
		totalChars += utf8.RuneCountInString(m.Content)
	}
	for totalChars > maxChars && len(filtered) > 1 {
		totalChars -= utf8.RuneCountInString(filtered[0].Content)
		filtered = filtered[1:]
	}

	return filtered
}
