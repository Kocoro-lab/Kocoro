package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

// parseStatefulArg reads the optional "stateful" tool argument, tolerating the
// LLM emitting the JSON boolean as a string ("true"/"false") — a common model
// quirk. Returns (value, set, err): set is false when the arg is absent/nil.
// A value that is neither a bool nor a parseable bool-string is a validation
// error rather than being silently dropped to false. The silent drop was a
// real bug: an agent asked for context continuity emitted "stateful":"true",
// the bare `args["stateful"].(bool)` assertion failed, and the schedule was
// created stateful=false (fresh per run) despite the explicit request.
func parseStatefulArg(args map[string]any) (val bool, set bool, err error) {
	raw, present := args["stateful"]
	if !present || raw == nil {
		return false, false, nil
	}
	switch v := raw.(type) {
	case bool:
		return v, true, nil
	case string:
		b, perr := strconv.ParseBool(strings.TrimSpace(v))
		if perr != nil {
			return false, false, fmt.Errorf("stateful must be a boolean true/false; got %q", v)
		}
		return b, true, nil
	default:
		return false, false, fmt.Errorf("stateful must be a boolean true/false; got %T", raw)
	}
}

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
				"Storage: stateful=false (default) creates a brand-new session for every run and the LLM sees no prior schedule history, for both default and named agents. " +
				"Storage: stateful=true uses one dedicated accumulating session per schedule (agent:<name>:schedule:<id>, or schedule:<id> for the default agent), and each run sees that schedule's own prior runs. " +
				"Showing the user the results: when the user asks 'what did the schedule produce' or 'show me yesterday's run', call schedule_show yourself (it returns the last assistant turns of that run, sliced to the run's own message range). Never tell the user to run session_search themselves. " +
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
							"~/.shannon/sessions/ pool, and the reply is pushed back to the IM channel this conversation is happening in " +
							"(if this conversation reached you via Slack/Teams/another IM). " +
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
						"description": "Whether this schedule remembers across runs (applies to both the default and named agents). " +
							"false (default): each run starts in a brand-new session with no prior context — for digests, polling, daily reports, monitoring, and any task whose runs are independent. " +
							"true: all runs accumulate in ONE dedicated session and each run sees prior runs' conversation. Set true WHENEVER the task needs memory of earlier runs — INCLUDING when the prompt counts runs (\"the Nth time\", \"第几次\"), continues or builds on the last run, tracks progress over time, or references earlier results — as well as continuous research / a rolling standup / journal / ongoing project tracking. Rule of thumb: if the schedule's own prompt refers to prior runs or continuity in ANY way, you MUST set true, otherwise that prompt cannot work (each run would see an empty history).",
					},
					"broadcast": map[string]any{
						"type": "string",
						"enum": []string{"auto", "on", "off"},
						"description": "Optional. Controls whether the schedule's reply is pushed back to the IM channel it was created in (Slack / Teams / Lark / Feishu / LINE / ...) when the run finishes. " +
							"The push target is always the originating channel — a schedule can never deliver to any other channel, and schedules created outside an IM chat (Desktop/TUI/CLI) have no push target and always stay silent (results remain in the session). " +
							"Omit or \"auto\" (smart default, recommended): schedules created from an IM channel push back to that channel; others stay silent. " +
							"\"off\": even IM-created schedules stay silent — pick this when the user explicitly wants a quiet run. " +
							"\"on\": explicitly pin the push on for an IM-created schedule. " +
							"Important: do NOT default to \"off\" when the user hasn't expressed an opinion — let the smart default decide.",
					},
					"thread": map[string]any{
						"type": "string",
						"enum": []string{"auto", "on", "off"},
						"description": "Optional. Controls whether the IM push lands in a thread (one conversation) or as separate top-level messages. " +
							"Omit or \"auto\" (recommended): follows the task's memory mode — a stateful schedule collects its runs into one thread, a stateless one posts each run on its own. " +
							"\"on\": always collect runs in the same thread — pick when the user says they want everything in one place / one thread. " +
							"\"off\": always post each run separately at the top level — pick when the user says \"send it separately each time\" / don't bundle into a thread. " +
							"Note: platforms without real threads (LINE / WhatsApp / WeCom / Telegram) ignore this setting.",
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
						"description": "Change whether this schedule remembers across runs. Omit to leave unchanged. false = each run starts fresh in a new session; true = all runs accumulate in one dedicated session and each run sees prior history. Set true if the task must remember earlier runs (counting \"Nth time\", continuing from last run, progress tracking); false for independent runs. Applies to both default and named agents.",
					},
					"broadcast": map[string]any{
						"type": "string",
						"enum": []string{"auto", "on", "off"},
						"description": "Optional. Change the schedule's broadcast intent (whether the reply is pushed back to the IM channel the schedule was created in — never any other channel; non-IM-created schedules always stay silent). " +
							"Omit = leave the current setting unchanged. " +
							"\"auto\" = clear back to smart default (decided by the schedule's CreatedFromSource). " +
							"\"on\" / \"off\" = explicitly override.",
					},
					"thread": map[string]any{
						"type": "string",
						"enum": []string{"auto", "on", "off"},
						"description": "Optional. Change the IM thread-anchoring intent. " +
							"Omit = leave the current setting unchanged. " +
							"\"auto\" = follow the task's memory mode (stateful → one thread, stateless → separate each run). " +
							"\"on\" = always one thread; \"off\" = always separate top-level messages. " +
							"Platforms without real threads (LINE / WhatsApp / WeCom / Telegram) ignore this.",
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
		stateful, _, statefulErr := parseStatefulArg(args) // missing → false; tolerates "true"/"false" strings
		if statefulErr != nil {
			return agent.ValidationError(statefulErr.Error()), nil
		}
		// Parse broadcast enum. Absent or "auto" maps to nil (smart default).
		// Use the explicit "present?" check (not just truthy) so the LLM
		// passing a non-string value still surfaces as a validation error.
		var broadcast *bool
		if raw, present := args["broadcast"]; present && raw != nil {
			bStr, isStr := raw.(string)
			if !isStr {
				return agent.ValidationError(fmt.Sprintf("broadcast must be a string (\"auto\", \"on\", or \"off\"); got %T", raw)), nil
			}
			b, ok := schedule.ParseBroadcastEnum(bStr)
			if !ok {
				return agent.ValidationError(fmt.Sprintf("broadcast must be one of \"auto\", \"on\", \"off\"; got %q", bStr)), nil
			}
			broadcast = b
		}
		// Parse thread enum. Absent or "auto" maps to nil (follow session state).
		var thread *bool
		if raw, present := args["thread"]; present && raw != nil {
			tStr, isStr := raw.(string)
			if !isStr {
				return agent.ValidationError(fmt.Sprintf("thread must be a string (\"auto\", \"on\", or \"off\"); got %T", raw)), nil
			}
			t, ok := schedule.ParseThreadEnum(tStr)
			if !ok {
				return agent.ValidationError(fmt.Sprintf("thread must be one of \"auto\", \"on\", \"off\"; got %q", tStr)), nil
			}
			thread = t
		}
		// Capture per-call source for the broadcast gate's smart default.
		// Empty / not-in-ctx both map to "" — downstream treats both as
		// "unknown source" and the gate falls through to silent.
		createdFromSource, _ := agent.SourceFromContext(ctx)
		// Snapshot the run's inbound IM routing blob (if any) as the schedule's
		// proactive-delivery target. Empty for non-IM runs (Desktop/TUI/CLI),
		// in which case the eventual run never pushes to IM (origin-only
		// delivery — see daemon.broadcastReply).
		imStatusContext, _ := agent.IMStatusContextFromContext(ctx)
		id, err := t.manager.CreateWithOpts(agentName, cron, prompt, stateful, schedule.CreateOpts{
			Broadcast:         broadcast,
			CreatedFromSource: createdFromSource,
			IMStatusContext:   imStatusContext,
			Thread:            thread,
		})
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
			// Surface the remember-across-runs mode so the model can answer
			// "does this schedule remember previous runs?" and so the legacy
			// behavior change is discoverable from a listing, not just the
			// one-shot startup log: on = accumulates context, off = fresh each
			// run, off(legacy) = nil Stateful (created before the field existed)
			// which now also runs fresh — PATCH stateful=true to restore it.
			statefulTag := "off(legacy)"
			if s.Stateful != nil {
				if *s.Stateful {
					statefulTag = "on"
				} else {
					statefulTag = "off"
				}
			}
			fmt.Fprintf(&sb, "%s | agent=%s | cron=%s | enabled=%v | stateful=%s | sync=%s | %s%s\n",
				s.ID, agentDisplay, s.Cron, s.Enabled, statefulTag, s.SyncStatus, s.Prompt, ctxTag)
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
		if v, set, statefulErr := parseStatefulArg(args); statefulErr != nil {
			return agent.ValidationError(statefulErr.Error()), nil
		} else if set {
			opts.Stateful = &v
		}
		// Parse the optional broadcast enum. Absent → leave field unchanged.
		// Present → parseBroadcastEnum maps "auto"/"on"/"off" to *bool; the
		// BroadcastOpt wrapper distinguishes "leave alone" (opts.Broadcast == nil)
		// from "rewrite to nil/true/false" (opts.Broadcast != nil).
		if raw, present := args["broadcast"]; present && raw != nil {
			bStr, isStr := raw.(string)
			if !isStr {
				return agent.ValidationError(fmt.Sprintf("broadcast must be a string (\"auto\", \"on\", or \"off\"); got %T", raw)), nil
			}
			b, ok := schedule.ParseBroadcastEnum(bStr)
			if !ok {
				return agent.ValidationError(fmt.Sprintf("broadcast must be one of \"auto\", \"on\", \"off\"; got %q", bStr)), nil
			}
			opts.Broadcast = &schedule.BroadcastOpt{Value: b}
		}
		// Parse the optional thread enum. Absent → leave field unchanged.
		// Present → ParseThreadEnum maps "auto"/"on"/"off" to *bool; the
		// ThreadOpt wrapper distinguishes "leave alone" (opts.Thread == nil)
		// from "rewrite to nil/true/false" (opts.Thread != nil).
		if raw, present := args["thread"]; present && raw != nil {
			tStr, isStr := raw.(string)
			if !isStr {
				return agent.ValidationError(fmt.Sprintf("thread must be a string (\"auto\", \"on\", or \"off\"); got %T", raw)), nil
			}
			t, ok := schedule.ParseThreadEnum(tStr)
			if !ok {
				return agent.ValidationError(fmt.Sprintf("thread must be one of \"auto\", \"on\", \"off\"; got %q", tStr)), nil
			}
			opts.Thread = &schedule.ThreadOpt{Value: t}
		}
		// When no field is set, treat as a no-op success rather than an
		// error: a degenerate `{id, description}` call still produced a
		// well-formed response (the schedule exists, nothing needed to
		// change). Manager.Update has its own "no fields" guard so we
		// must short-circuit here before calling it.
		if opts.Cron == nil && opts.Prompt == nil && opts.Enabled == nil && opts.Stateful == nil && opts.Broadcast == nil && opts.Thread == nil {
			return agent.ToolResult{Content: fmt.Sprintf("Schedule %s unchanged (no fields specified).", id)}, nil
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
