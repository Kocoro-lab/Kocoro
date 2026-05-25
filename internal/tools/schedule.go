package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

type ScheduleTool struct {
	manager *schedule.Manager
	action  string
}

func NewScheduleTools(mgr *schedule.Manager) []agent.Tool {
	return []agent.Tool{
		&ScheduleTool{manager: mgr, action: "create"},
		&ScheduleTool{manager: mgr, action: "list"},
		&ScheduleTool{manager: mgr, action: "update"},
		&ScheduleTool{manager: mgr, action: "remove"},
	}
}

func (t *ScheduleTool) Info() agent.ToolInfo {
	switch t.action {
	case "create":
		return agent.ToolInfo{
			Name: "schedule_create",
			Description: "Create a scheduled task that runs a shan agent on a cron schedule. Supports full cron syntax (ranges, steps, lists). Each run saves its result as a session (searchable via session_search)." +
				agent.DescriptionGuidance,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent":       map[string]any{"type": "string", "description": "Agent name (from ~/.shannon/agents/). Empty for default agent."},
					"cron":        map[string]any{"type": "string", "description": "5-field cron expression (minute hour day month weekday). Supports */5, 1-5, 1,3,5."},
					"prompt":      map[string]any{"type": "string", "description": "The prompt to send to the agent on each run."},
					"description": agent.DescriptionFieldSpec,
				},
			},
			Required: []string{"cron", "prompt", "description"},
		}
	case "list":
		return agent.ToolInfo{
			Name:        "schedule_list",
			Description: "List all locally scheduled tasks with their status.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}
	case "update":
		return agent.ToolInfo{
			Name: "schedule_update",
			Description: "Update an existing scheduled task." +
				agent.DescriptionGuidance,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "description": "Schedule ID"},
					"cron":        map[string]any{"type": "string", "description": "New cron expression"},
					"prompt":      map[string]any{"type": "string", "description": "New prompt"},
					"enabled":     map[string]any{"type": "boolean", "description": "Enable or disable"},
					"description": agent.DescriptionFieldSpec,
				},
			},
			Required: []string{"id", "description"},
		}
	case "remove":
		return agent.ToolInfo{
			Name: "schedule_remove",
			Description: "Remove a scheduled task." +
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
	}
	return agent.ToolInfo{}
}

func (t *ScheduleTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: "invalid args: " + err.Error(), IsError: true}, nil
	}
	switch t.action {
	case "create":
		agentName, _ := args["agent"].(string)
		cron, _ := args["cron"].(string)
		prompt, _ := args["prompt"].(string)
		if cron == "" || prompt == "" {
			return agent.ToolResult{Content: "cron and prompt are required", IsError: true}, nil
		}
		// LLM-created schedules default to stateless (same as HTTP/CLI default).
		// If callers need stateful schedules, surface a "stateful" arg here.
		id, err := t.manager.Create(agentName, cron, prompt, false)
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
		if id == "" {
			return agent.ToolResult{Content: "id is required", IsError: true}, nil
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
		if opts.Cron == nil && opts.Prompt == nil && opts.Enabled == nil {
			return agent.ToolResult{Content: "at least one of cron, prompt, or enabled is required", IsError: true}, nil
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
		if id == "" {
			return agent.ToolResult{Content: "id is required", IsError: true}, nil
		}
		if err := t.manager.Remove(id); err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("Schedule %s removed.", id)}, nil
	}
	return agent.ToolResult{Content: "unknown action", IsError: true}, nil
}

func (t *ScheduleTool) RequiresApproval() bool {
	return t.action != "list"
}

func (t *ScheduleTool) IsReadOnlyCall(string) bool {
	return t.action == "list"
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
