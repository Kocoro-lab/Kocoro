//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// ToolDef is an OpenAI Realtime function-tool definition.
type ToolDef struct {
	Type        string          `json:"type"` // always "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func obj(raw string) json.RawMessage { return json.RawMessage(raw) }

const doTaskParamsLegacy = `{"type":"object","properties":{"task":{"type":"string","description":"The task to perform, in the user's own words."},"agent":{"type":"string","description":"Optional: the agent the user named for this task, verbatim. Omit to use the bound agent."}},"required":["task"]}`

const doTaskParamsLedger = `{"type":"object","properties":{"task":{"type":"string","description":"The complete task to perform, in the user's own words."},"agent":{"type":"string","description":"Only when the user explicitly named an agent in this utterance; otherwise omit it."},"relationship":{"type":"string","enum":["new","follow_up"],"description":"new starts another independent task; follow_up refines or corrects an existing task. Omit only when genuinely unsure."},"task_id":{"type":"string","description":"For follow_up, the target task id from a prior result or get_status. Omit only when one running task is unambiguous."}},"required":["task"]}`

const cancelParamsLegacy = `{"type":"object","properties":{"reason":{"type":"string","enum":["user_cancel","interrupt"],"description":"Why the task is being cancelled."}},"required":[]}`

const cancelParamsLedger = `{"type":"object","properties":{"reason":{"type":"string","enum":["user_cancel","interrupt"],"description":"Why the task is being cancelled."},"task_id":{"type":"string","description":"The task id to cancel. Omit when exactly one task is running."}},"required":[]}`

// ToolDefs returns the voice tools. Enum'd where applicable; no parallel calls.
// end_call ends the whole conversation (dismiss / hang up) — the model judges the
// intent from the audio, which is more robust than matching the garbled input
// transcription; a tone plays and the call goes dormant (re-activate with a
// double-tap Option). This replaces the earlier local stop-word approach.
func ToolDefs() []ToolDef {
	doTaskParams, cancelParams := doTaskParamsLegacy, cancelParamsLegacy
	getStatusDesc := "Check whether a delegated task is still running."
	if TaskLedgerEnabled() {
		doTaskParams, cancelParams = doTaskParamsLedger, cancelParamsLedger
		getStatusDesc = "List every task created in this call with its task_id, latest request, and running/completed/failed/cancelled state."
	}
	defs := []ToolDef{
		{Type: "function", Name: "do_task",
			Description: "do_task — how you actually get things done: your own hands on a full computer. As Kocoro on Kocoro Desktop you can browse and research the web, read and write files, run code and calculate precisely, manage schedules, send email and messages, and run multi-step jobs. Answer directly from your own knowledge for stable public knowledge you already hold — concepts, how something works, math and science fundamentals, coding ideas, creative writing, small talk, and recapping what's already in this conversation or a context digest you hold; a tool round trip would only slow those down. Use do_task whenever the answer instead depends on something you cannot reliably supply from that knowledge alone: a real action or side effect, current or changing facts (news, a date, a price, the latest state of someone or something), the user's private information or system state, a specific fact you do not hold, any calculation beyond one obvious step, or content/results to show in Kocoro Desktop. Words like \"now\", \"current\", \"latest\", or \"today\" pin a question to the present moment and need do_task even when the topic is general knowledge. Judge by the nature of the information, not how sure you feel; never answer current facts, prices, or private details from memory or guess. Call it even when the request is vague or missing details — never quiz the user for them first: Kocoro already knows the user's own context (contacts, addresses, accounts, files, history), and the result will say if something is truly missing. Long or multi-part spoken requests still count: preserve the user's details and do the task instead of waiting for a follow-up like \"do it\". When you do call it, first say one short line before the tool call, in the language of the utterance, fitting the task naturally; vary the wording (我查一下 / 我来看看 / On it / Let me check) rather than repeating one stock phrase, and don't state the answer or result before it lands — but say it only when you are actually calling do_task, not when you can answer directly. Then speak the result in your own voice when it lands. What comes back to you is a short spoken line plus a context digest of the full answer: use the digest to answer recaps and follow-up questions directly, and call do_task again only for detail, action, or freshness beyond it — referring to that earlier work. The complete report stays in the session and on Kocoro Desktop; mention Kocoro Desktop only when there is genuinely more worth opening there (a long report, a table, code, or images), never as a routine sign-off.",
			Parameters:  obj(doTaskParams)},
		{Type: "function", Name: "cancel",
			Description: "Cancel the task that is currently running. Call it only when the user clearly and explicitly asked you to stop that task. Speech overheard mid-task that is ambiguous, off-topic, or possibly not addressed to you is NOT a cancel request — ignore it, or briefly confirm before cancelling if you think the user might have meant to stop.",
			Parameters:  obj(cancelParams)},
		{Type: "function", Name: "get_status",
			Description: getStatusDesc,
			Parameters:  obj(`{"type":"object","properties":{},"required":[]}`)},
		{Type: "function", Name: "control_app",
			Description: "Control only the Kocoro Desktop window or conversation shell: show, hide, start a new conversation, or open settings. Never use this to display, write, save, or update result content in Kocoro Desktop; use do_task for content/results.",
			Parameters:  obj(`{"type":"object","properties":{"action":{"type":"string","enum":["show","hide","new_conversation","open_settings"],"description":"The UI action to perform."}},"required":["action"]}`)},
		{Type: "function", Name: "switch_agent",
			Description: "Switch which specialist handles your real-work tasks for the rest of this conversation — only when the user explicitly names one; otherwise stay on the current agent.",
			Parameters:  obj(`{"type":"object","properties":{"agent":{"type":"string","description":"The agent the user named, verbatim."}},"required":["agent"]}`)},
		{Type: "function", Name: "end_call",
			Description: "End the whole voice conversation and go dormant — a hang up. Call this the moment the user clearly tells you to stop talking, dismisses you, or says goodbye: \"闭嘴\" \"够了\" \"停\"/\"停止\" \"别说了\" \"再见\" \"就这样\" \"没事了\" \"bye\" \"goodbye\" \"that's all\" \"stop\" \"黙れ\" \"やめて\" \"もういい\" — and similar. Judge it from what you actually heard, not just the on-screen transcription. After this, the call is over and the user must double-tap the Option key to talk again. Say NOTHING — do not acknowledge, do not ask to confirm; a short tone plays and the call ends. Do NOT call it for a topic change, a brief pause, or to stop one running task (that is `cancel`), and when you are unsure whether they meant to end, keep listening instead.",
			Parameters:  obj(`{"type":"object","properties":{},"required":[]}`)},
	}
	if TaskLedgerEnabled() {
		defs[0].Description += " The call returns immediately with status running and a task_id; the real result arrives later as a background task update. A running task never blocks you: keep conversing and call do_task again freely for another independent request or a follow-up, and never invent a result before the update lands."
	}
	return defs
}

// CallState holds the per-call mutable binding + in-flight tracker. burstID is
// fixed for the call; boundAgent changes via switch_agent; inFlight tracks the
// active do_task for get_status.
type CallState struct {
	mu             sync.Mutex
	burstID        string
	bound          string
	cwd            string
	foregroundHint *ForegroundHint
	inFlight       string
	inFlightN      int // concurrent do_task count — a follow-up ("change it to 6pm")
	// spawns a 2nd do_task goroutine while the 1st runs; the in-flight text must
	// survive until the LAST one clears, not the first.
	inFlightRoutes map[string]int
	// Call-scoped addressable task lineages. See ledger.go.
	tasks   map[string]*VoiceTask
	taskSeq int
}

func NewCallState(burstID, boundAgent string) *CallState {
	return &CallState{burstID: burstID, bound: boundAgent}
}

func (s *CallState) BoundAgent() string { s.mu.Lock(); defer s.mu.Unlock(); return s.bound }
func (s *CallState) setBound(a string)  { s.mu.Lock(); s.bound = a; s.mu.Unlock() }
func (s *CallState) BurstID() string    { s.mu.Lock(); defer s.mu.Unlock(); return s.burstID }

func (s *CallState) SetCallContext(ctx StartCallRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cwd = ctx.CWD
	if ctx.ForegroundHint != nil {
		hint := *ctx.ForegroundHint
		s.foregroundHint = &hint
	} else {
		s.foregroundHint = nil
	}
}

func (s *CallState) callContextLocked() (string, *ForegroundHint) {
	var hint *ForegroundHint
	if s.foregroundHint != nil {
		copy := *s.foregroundHint
		hint = &copy
	}
	return s.cwd, hint
}

// SetInFlight / ClearInFlight are exported because C's async do_task goroutine
// (NOT a blocking Dispatch) owns the in-flight lifecycle: set before delegating,
// clear when the result returns. get_status reads InFlight.
func (s *CallState) SetInFlight(t string) {
	s.mu.Lock()
	agent := s.bound
	threadID := s.burstID
	s.mu.Unlock()
	s.SetInFlightForRoute(t, agent, threadID)
}

func (s *CallState) SetInFlightForRoute(t, agent, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = t
	s.inFlightN++
	if s.inFlightRoutes == nil {
		s.inFlightRoutes = make(map[string]int)
	}
	s.inFlightRoutes[routeKeyFor(agent, threadID)]++
}

// SetInFlightForAgent is the main-lane compatibility helper used by existing
// callers and rollback tests.
func (s *CallState) SetInFlightForAgent(t, agent string) {
	s.SetInFlightForRoute(t, agent, s.BurstID())
}

func (s *CallState) ClearInFlight() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearOneInFlightLocked("")
}

func (s *CallState) ClearInFlightForRoute(agent, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearOneInFlightLocked(routeKeyFor(agent, threadID))
}

func (s *CallState) ClearInFlightForAgent(agent string) {
	s.ClearInFlightForRoute(agent, s.BurstID())
}

func (s *CallState) ClearAllInFlight() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = ""
	s.inFlightN = 0
	s.inFlightRoutes = nil
}

func (s *CallState) clearOneInFlightLocked(route string) {
	if s.inFlightN > 0 {
		s.inFlightN--
	}
	if len(s.inFlightRoutes) > 0 {
		if route == "" {
			for k := range s.inFlightRoutes {
				route = k
				break
			}
		}
		if n := s.inFlightRoutes[route]; n <= 1 {
			delete(s.inFlightRoutes, route)
		} else {
			s.inFlightRoutes[route] = n - 1
		}
	}
	if s.inFlightN == 0 {
		s.inFlight = "" // only idle once the last concurrent do_task has returned
		s.inFlightRoutes = nil
	}
}

func (s *CallState) InFlight() string { s.mu.Lock(); defer s.mu.Unlock(); return s.inFlight }

func (s *CallState) ActiveRouteKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inFlightRoutes) == 0 {
		if s.inFlightN == 0 {
			return nil
		}
		return []string{routeKeyFor(s.bound, s.burstID)}
	}
	keys := make([]string, 0, len(s.inFlightRoutes))
	for key := range s.inFlightRoutes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// burstRouteKey reconstructs the daemon route key for this call's burst,
// byte-identical to ComputeRouteKey (daemon runner.go:143/145) so cancel hits the
// live run. Source is the literal "koe"; the burst id is PathEscaped exactly as
// the daemon's sanitizeRouteValue does (the agent slug is used raw, matching the
// daemon). burst-ids MUST be URL-safe so this stays human-readable and equals the
// key Plan A Task 3 pins.
//
// CARRIER COUPLING (Plan A carrier-neutral constraint, honored via discoverability):
// the ":koe:" source segment is HARDCODED. This is correct today because koe is the
// ONLY carrier — the daemon receives these runs with req.Source=="koe", and
// sanitizeRouteValue("koe") is a no-op (already URL-safe). If a second carrier is
// ever added (e.g. "koe-reachy"), this MUST stop hardcoding "koe" and instead thread
// the actual source through CallState so it matches the daemon's
// sanitizeRouteValue(req.Source); otherwise cancel/get_status would compute the wrong
// key and silently no-op for that carrier. Full source-threading is deferred (YAGNI
// while koe is the only carrier).
func routeKeyFor(agent, threadID string) string {
	if agent == "" {
		return "default:koe:" + url.PathEscape(threadID)
	}
	return "agent:" + agent + ":koe:" + url.PathEscape(threadID)
}

func burstRouteKey(agent, burstID string) string { return routeKeyFor(agent, burstID) }

// ControlAppFunc is the Desktop-UI seam. Plan B leaves it nil (control_app then
// errors "not wired"); C/E supply the Desktop control channel.
type ControlAppFunc func(ctx context.Context, action string) error

// Dispatcher routes a realtime function call to link/resolver/state.
type Dispatcher struct {
	client     *DaemonClient
	resolver   *AgentResolver
	state      *CallState
	controlApp ControlAppFunc
}

func NewDispatcher(client *DaemonClient, resolver *AgentResolver, state *CallState, controlApp ControlAppFunc) *Dispatcher {
	return &Dispatcher{client: client, resolver: resolver, state: state, controlApp: controlApp}
}

// SayResult is the do_task function_call_output contract (spec §4): the spoken
// projection + a status + a capped context digest of the final answer — never the
// back-brain's tools/reasoning/transcript. spoken_summary is the canonical field;
// say stays as a compatibility alias for older prompt text/tests. Exported so C's
// async do_task goroutine consumes MapDoTaskOutcome's return.
type SayResult struct {
	SpokenSummary string `json:"spoken_summary,omitempty"`
	Say           string `json:"say,omitempty"`
	// Context is a digest of the full answer for the Realtime model's OWN use
	// (recaps and follow-up questions in the same call) — background context,
	// never read aloud verbatim.
	Context string `json:"context,omitempty"`
	// TaskID lets the native model address later follow-ups, status requests, and
	// cancellation without relying on text similarity.
	TaskID     string `json:"task_id,omitempty"`
	Status     string `json:"status"` // ok | failed | injected | clarify
	FailReason string `json:"fail_reason,omitempty"`
}

// defaultVoiceContextCap bounds the context digest attached to a completed
// do_task result. WORKLOAD: same-call recaps and follow-ups — with only the two
// spoken sentences, every "简单总结一下刚才的" costs another 10-60s do_task round
// trip (live 2026-07-02: 2 of 4 delegations in one call were re-fetch recaps).
// SYMPTOM if too low: follow-ups fall back to do_task again; if too high: the
// 32k audio-heavy Realtime session truncates older turns sooner. OVERRIDE:
// KOE_VOICE_CONTEXT_CAP.
const defaultVoiceContextCap = 800

// voiceContextDigest caps the reply into the SayResult context field; empty when
// it would add nothing over the spoken line.
func voiceContextDigest(reply, spoken string) string {
	r := strings.TrimSpace(reply)
	if r == "" || r == strings.TrimSpace(spoken) {
		return ""
	}
	limit := koeEnvInt("KOE_VOICE_CONTEXT_CAP", defaultVoiceContextCap)
	if limit <= 0 {
		return ""
	}
	runes := []rune(r)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return r
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// Dispatch executes one function call and returns its function_call_output bytes.
func (d *Dispatcher) Dispatch(ctx context.Context, name string, argsJSON []byte) ([]byte, error) {
	switch name {
	case "do_task":
		// do_task is async (deferred-ack): C calls PrepareDoTask + a goroutine +
		// MapDoTaskOutcome. Routing it through Dispatch would force the blocking
		// path that defeats the fast "在弄了" ack — fail loud.
		return nil, fmt.Errorf("do_task is async: use PrepareDoTask + goroutine + MapDoTaskOutcome, not Dispatch")
	case "cancel":
		var a struct {
			Reason string `json:"reason"`
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(argsJSON, &a)
		reason := a.Reason
		if reason == "" {
			reason = "user_cancel"
		}
		if TaskLedgerEnabled() {
			return d.cancelLedger(ctx, a.TaskID, reason)
		}
		// cancel keys off BoundAgent() (the persistent binding), so a do_task
		// delegated to a per-call agent OVERRIDE (PrepareDoTask resolves a different
		// slug WITHOUT setBound) would not be cancel-reachable by this path. INERT in
		// C-minimal — do_task is synchronous there, nothing to cancel mid-run; to be
		// addressed in C-full's async path (deferred F2).
		key := routeKeyFor(d.state.BoundAgent(), d.state.BurstID())
		keys := d.state.ActiveRouteKeys()
		if len(keys) == 0 {
			keys = []string{key}
		}
		for _, key := range keys {
			if err := d.client.Cancel(ctx, CancelRequest{RouteKey: key, Reason: reason}); err != nil {
				return mustJSON(map[string]string{"status": "failed", "error": err.Error()}), nil
			}
		}
		d.state.ClearAllInFlight()
		return mustJSON(map[string]string{"status": "ok"}), nil
	case "get_status":
		if TaskLedgerEnabled() {
			tasks := d.state.AllTasks()
			if len(tasks) == 0 {
				return mustJSON(map[string]string{"status": "idle"}), nil
			}
			type taskView struct {
				TaskID string `json:"task_id"`
				Task   string `json:"task"`
				State  string `json:"state"`
				Agent  string `json:"agent,omitempty"`
			}
			views := make([]taskView, 0, len(tasks))
			status := "idle"
			for _, task := range tasks {
				if task.State == TaskRunning {
					status = "running"
				}
				views = append(views, taskView{
					TaskID: task.ID, Task: task.Label, State: string(task.State), Agent: task.Agent,
				})
			}
			return mustJSON(map[string]any{"status": status, "tasks": views}), nil
		}
		running := d.state.InFlight()
		if running == "" {
			return mustJSON(map[string]string{"status": "idle"}), nil
		}
		return mustJSON(map[string]string{"status": "running", "task": running}), nil
	case "control_app":
		var a struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(argsJSON, &a); err != nil || a.Action == "" {
			return nil, fmt.Errorf("control_app requires an action")
		}
		if d.controlApp == nil {
			return mustJSON(map[string]string{"status": "failed", "error": "ui control not wired"}), nil
		}
		if err := d.controlApp(ctx, a.Action); err != nil {
			return mustJSON(map[string]string{"status": "failed", "error": err.Error()}), nil
		}
		return mustJSON(map[string]string{"status": "ok"}), nil
	case "switch_agent":
		var a struct {
			Agent string `json:"agent"`
		}
		if err := json.Unmarshal(argsJSON, &a); err != nil || a.Agent == "" {
			return nil, fmt.Errorf("switch_agent requires an agent")
		}
		res := d.resolver.Resolve(a.Agent)
		switch res.Status {
		case ResolveResolved:
			d.state.setBound(res.Slug)
			return mustJSON(map[string]string{"status": "ok", "agent": res.Slug}), nil
		case ResolveAmbiguous:
			return mustJSON(map[string]any{"status": "clarify", "candidates": res.Candidates}), nil
		default:
			return mustJSON(map[string]string{"status": "failed", "error": "no such agent"}), nil
		}
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// cancelLedger addresses one running lineage. An omitted id is safe only when a
// single target exists; cancelling every route on ambiguity would make a compound
// "stop that and start this" turn destructive.
func (d *Dispatcher) cancelLedger(ctx context.Context, taskID, reason string) ([]byte, error) {
	var target VoiceTask
	switch running := d.state.RunningTasks(); {
	case taskID != "":
		task, ok := d.state.TaskByID(taskID)
		if !ok || task.State != TaskRunning {
			return mustJSON(map[string]string{"status": "failed", "error": "no running task with that task_id"}), nil
		}
		target = task
	case len(running) == 1:
		target = running[0]
	case len(running) == 0:
		// Preserve main's blind-cancel recovery for rare daemon/ledger drift.
		key := routeKeyFor(d.state.BoundAgent(), d.state.BurstID())
		if err := d.client.Cancel(ctx, CancelRequest{RouteKey: key, Reason: reason}); err != nil {
			return mustJSON(map[string]string{"status": "failed", "error": err.Error()}), nil
		}
		return mustJSON(map[string]string{"status": "idle"}), nil
	default:
		views := make([]map[string]string, 0, len(running))
		for _, task := range running {
			views = append(views, map[string]string{"task_id": task.ID, "task": task.Label})
		}
		return mustJSON(map[string]any{"status": "clarify", "tasks": views}), nil
	}
	if err := d.client.Cancel(ctx, CancelRequest{
		RouteKey: routeKeyFor(target.Agent, target.ThreadID), Reason: reason,
	}); err != nil {
		return mustJSON(map[string]string{"status": "failed", "error": err.Error()}), nil
	}
	d.state.MarkCancelled(target.ID)
	return mustJSON(map[string]string{"status": "ok", "task_id": target.ID}), nil
}

// taskNamesAgent prevents a native model from silently selecting a specialist
// merely because the task resembles that agent's domain. The task field is
// contractually the user's own request, so an explicit user choice survives this
// containment check. KOE_AGENT_OVERRIDE_GUARD=0 restores trust-the-model behavior.
func taskNamesAgent(task string, names []string) bool {
	if !koeEnvBool("KOE_AGENT_OVERRIDE_GUARD", true) {
		return true
	}
	folded := strings.ToLower(task)
	folded = strings.NewReplacer("-", " ", "_", " ").Replace(folded)
	folded = strings.Join(strings.Fields(folded), " ")
	for _, name := range names {
		if name != "" && strings.Contains(folded, name) {
			return true
		}
	}
	return false
}

// PrepareDoTask resolves the optional per-call agent and builds the delegation
// request. It does NO network I/O — resolution is instant, so C can voice a
// clarify immediately or fast-ack ("在弄了") and delegate async. Returns:
//   - (req, task, nil, nil)  → delegate on task's stable route.
//   - (_, nil, clarify, nil) → voice clarification without delegating.
//   - (_, nil, nil, err)     → malformed call.
//
// lang selects the clarify language (see fallbackLang); the caller resolves it from
// the pinned koe language + the utterance before calling.
//
// Per-call agent override (Model 2): a named agent resolves for THIS task only;
// otherwise the persistent binding (CallState.bound) is used. PrepareDoTask does
// NOT set inFlight — C's goroutine owns that (SetInFlight before, ClearInFlight
// after) so get_status reflects the async delegation, not a blocking call.
func (d *Dispatcher) PrepareDoTask(argsJSON []byte, lang string) (DoTaskRequest, *VoiceTask, *SayResult, error) {
	var a struct {
		Task         string `json:"task"`
		Agent        string `json:"agent"`
		Relationship string `json:"relationship"`
		TaskID       string `json:"task_id"`
	}
	if err := json.Unmarshal(argsJSON, &a); err != nil || a.Task == "" {
		return DoTaskRequest{}, nil, nil, fmt.Errorf("do_task requires a task")
	}
	agent := d.state.BoundAgent()
	if a.Agent != "" {
		res := d.resolver.Resolve(a.Agent)
		switch res.Status {
		case ResolveResolved:
			if taskNamesAgent(a.Task, d.resolver.spokenNamesFor(a.Agent, res.Slug)) {
				agent = res.Slug
			} else {
				log.Printf("koe[task]: ignored unnamed agent override %q; using bound %q", a.Agent, agent)
			}
		case ResolveAmbiguous:
			if !taskNamesAgent(a.Task, d.resolver.spokenNamesFor(a.Agent, "")) {
				break
			}
			say := clarifyWhich(lang, res.Candidates)
			return DoTaskRequest{}, nil, &SayResult{Status: "clarify", SpokenSummary: say, Say: say}, nil
		default:
			if !taskNamesAgent(a.Task, d.resolver.spokenNamesFor(a.Agent, "")) {
				break
			}
			// Unknown named agent → ask rather than silently using the default.
			say := fallbackSay(lang, "clarify_unknown")
			return DoTaskRequest{}, nil, &SayResult{Status: "clarify", SpokenSummary: say, Say: say}, nil
		}
	}
	d.state.mu.Lock()
	burstID := d.state.burstID
	cwd, foregroundHint := d.state.callContextLocked()
	d.state.mu.Unlock()
	if !TaskLedgerEnabled() {
		return DoTaskRequest{Text: a.Task, Agent: agent, ThreadID: burstID, CWD: cwd, ForegroundHint: foregroundHint}, nil, nil, nil
	}

	// Relationship is native-model semantic evidence, resolved against the live
	// ledger. Ambiguous follow-ups never guess among several running tasks.
	var task *VoiceTask
	switch strings.ToLower(strings.TrimSpace(a.Relationship)) {
	case "follow_up", "followup", "follow-up":
		if resolved, ok := d.state.BeginFollowUp(a.TaskID, a.Task); ok {
			task = resolved
		} else {
			switch running := d.state.RunningTasks(); len(running) {
			case 1:
				task, _ = d.state.BeginFollowUp(running[0].ID, a.Task)
			case 0:
				task = d.state.BeginTask(a.Task, agent)
			default:
				say := clarifyWhichTask(lang, running)
				return DoTaskRequest{}, nil, &SayResult{Status: "clarify", SpokenSummary: say, Say: say}, nil
			}
		}
	case "new":
		task = d.state.BeginTask(a.Task, agent)
	default:
		// Keep main's existing merge bias when the model is unsure: a later P3
		// transaction groups same-turn parallel calls so they still split correctly.
		if running := d.state.RunningMainLaneTask(agent); running != nil {
			task, _ = d.state.BeginFollowUp(running.ID, a.Task)
		} else if running := d.state.RunningTasksForAgent(agent); len(running) == 1 {
			task, _ = d.state.BeginFollowUp(running[0].ID, a.Task)
		} else {
			task = d.state.BeginTask(a.Task, agent)
		}
	}
	return DoTaskRequest{
		Text: a.Task, Agent: task.Agent, ThreadID: task.ThreadID,
		CWD: cwd, ForegroundHint: foregroundHint,
	}, task, nil, nil
}

// MapDoTaskOutcome converts a delegation result (or transport error) into the
// say-contract output. Pure + exported so C's async goroutine reuses it without
// going through Dispatch. status ∈ ok|failed|injected. OutcomeInjected carries an
// empty say so the front brain doesn't double-speak (the original do_task voices
// the final result). lang selects the mechanical fallback language (transport
// failure / busy rejection / partial "incomplete" line); see fallbackLang. A fully
// completed outcome speaks the back-brain's own text and ignores lang; a partial
// one speaks the canned "incomplete" line in lang instead of its progress tail.
func MapDoTaskOutcome(out DoTaskOutcome, err error, lang string) SayResult {
	if err != nil {
		say := fallbackSay(lang, "transport_failed")
		return SayResult{Status: "failed", FailReason: err.Error(),
			SpokenSummary: say, Say: say}
	}
	switch out.Kind {
	case OutcomeCompleted:
		// A user-cancelled run carries no result to voice: its reply is the tail
		// of whatever was streaming when the run died (live 2026-07-02: the killed
		// run's progress line got read aloud right after the cancel). The model
		// already acknowledged the stop in its own words when the cancel tool
		// returned — status is all it needs here.
		if out.FailureCode == "user_cancelled" { // runstatus.CodeUserCancelled, mirrored (koe never imports daemon-side packages)
			return SayResult{Status: "cancelled", FailReason: out.FailureCode}
		}
		// A partial run (soft idle timeout / max-iter / force-stop, NOT
		// user_cancelled) returned only the tail of whatever was streaming when it
		// died — typically a tool preamble or progress line ("现在整理成结构化报告。").
		// Reply/SpokenSummary here is that untrustworthy fragment, so voicing it (or
		// seeding it as a recap digest) reads a progress narration aloud as if it
		// were the finished result — the same failure the user_cancelled guard above
		// fixed. Speak a safe status line instead, claim no completion, seed no
		// digest. Repro: internal/koe/tools_test.go TestMapDoTaskOutcomePartial*.
		if out.Partial {
			say := fallbackSay(lang, "incomplete")
			return SayResult{Status: "failed", SpokenSummary: say, Say: say, FailReason: out.FailureCode}
		}
		spoken := out.SpokenSummary
		if spoken == "" {
			spoken = out.Reply
		}
		return SayResult{Status: "ok", SpokenSummary: spoken, Say: spoken,
			Context: voiceContextDigest(out.Reply, spoken), FailReason: out.FailureCode}
	case OutcomeInjected:
		return SayResult{Status: "injected"}
	default: // OutcomeRejected
		say := fallbackSay(lang, "busy")
		return SayResult{Status: "failed", FailReason: out.Reason,
			SpokenSummary: say, Say: say}
	}
}
