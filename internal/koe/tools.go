//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"fmt"
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

// ToolDefs returns the five (and only five) voice tools. Enum'd where applicable;
// no parallel calls. Voice end-of-call is NOT a tool — Koe detects stop-words
// locally (spec §5).
func ToolDefs() []ToolDef {
	return []ToolDef{
		{Type: "function", Name: "do_task",
			Description: "do_task — how you actually get things done: your own hands on a full computer. As Kocoro on Kocoro Desktop you can browse and research the web, read and write files, run code and calculate precisely, manage schedules, send email and messages, and run multi-step jobs. Reach for it for ANYTHING whose answer needs the world outside this conversation — real data, a current fact, a date or price, system state, a real action, any calculation beyond one obvious step, or content/results to show in Kocoro Desktop — never answer those from memory or guess. Only conversation-internal one-step replies (small talk, restating an earlier result, trivial arithmetic like 1+1) are answered without it. Call it even when the request is vague or missing details — never quiz the user for them first: Kocoro already knows the user's own context (contacts, addresses, accounts, files, history), and the result will say if something is truly missing. Long or multi-part spoken requests still count: preserve the user's details and do the task instead of waiting for a follow-up like \"do it\". The moment you call it, say exactly one short acknowledgement before the tool call, in the language of the utterance; vary the wording naturally (我来处理 / 我看看 / On it / Let me check), never include an answer, number, fact, or step, and no second sentence. Then speak the result in your own voice when it lands. What comes back to you is a short spoken line plus a context digest of the full answer: use the digest to answer recaps and follow-up questions directly, and call do_task again only for detail, action, or freshness beyond it — referring to that earlier work. The complete report stays in the session and on Kocoro Desktop; mention Kocoro Desktop only when there is genuinely more worth opening there (a long report, a table, code, or images), never as a routine sign-off.",
			Parameters:  obj(`{"type":"object","properties":{"task":{"type":"string","description":"The task to perform, in the user's own words."},"agent":{"type":"string","description":"Optional: the agent the user named for this task, verbatim. Omit to use the bound agent."}},"required":["task"]}`)},
		{Type: "function", Name: "cancel",
			Description: "Cancel the task that is currently running. Call it only when the user clearly and explicitly asked you to stop that task. Speech overheard mid-task that is ambiguous, off-topic, or possibly not addressed to you is NOT a cancel request — ignore it, or briefly confirm before cancelling if you think the user might have meant to stop.",
			Parameters:  obj(`{"type":"object","properties":{"reason":{"type":"string","enum":["user_cancel","interrupt"],"description":"Why the task is being cancelled."}},"required":[]}`)},
		{Type: "function", Name: "get_status",
			Description: "Check whether a delegated task is still running.",
			Parameters:  obj(`{"type":"object","properties":{},"required":[]}`)},
		{Type: "function", Name: "control_app",
			Description: "Control only the Kocoro Desktop window or conversation shell: show, hide, start a new conversation, or open settings. Never use this to display, write, save, or update result content in Kocoro Desktop; use do_task for content/results.",
			Parameters:  obj(`{"type":"object","properties":{"action":{"type":"string","enum":["show","hide","new_conversation","open_settings"],"description":"The UI action to perform."}},"required":["action"]}`)},
		{Type: "function", Name: "switch_agent",
			Description: "Switch which specialist handles your real-work tasks for the rest of this conversation — only when the user explicitly names one; otherwise stay on the current agent.",
			Parameters:  obj(`{"type":"object","properties":{"agent":{"type":"string","description":"The agent the user named, verbatim."}},"required":["agent"]}`)},
	}
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
	s.mu.Unlock()
	s.SetInFlightForAgent(t, agent)
}

func (s *CallState) SetInFlightForAgent(t, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = t
	s.inFlightN++
	if s.inFlightRoutes == nil {
		s.inFlightRoutes = make(map[string]int)
	}
	s.inFlightRoutes[burstRouteKey(agent, s.burstID)]++
}

func (s *CallState) ClearInFlight() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearOneInFlightLocked("")
}

func (s *CallState) ClearInFlightForAgent(agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearOneInFlightLocked(burstRouteKey(agent, s.burstID))
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
		return []string{burstRouteKey(s.bound, s.burstID)}
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
func burstRouteKey(agent, burstID string) string {
	if agent == "" {
		return "default:koe:" + url.PathEscape(burstID)
	}
	return "agent:" + agent + ":koe:" + url.PathEscape(burstID)
}

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
	Context    string `json:"context,omitempty"`
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
		}
		_ = json.Unmarshal(argsJSON, &a)
		// cancel keys off BoundAgent() (the persistent binding), so a do_task
		// delegated to a per-call agent OVERRIDE (PrepareDoTask resolves a different
		// slug WITHOUT setBound) would not be cancel-reachable by this path. INERT in
		// C-minimal — do_task is synchronous there, nothing to cancel mid-run; to be
		// addressed in C-full's async path (deferred F2).
		key := burstRouteKey(d.state.BoundAgent(), d.state.BurstID())
		reason := a.Reason
		if reason == "" {
			reason = "user_cancel"
		}
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

// PrepareDoTask resolves the optional per-call agent and builds the delegation
// request. It does NO network I/O — resolution is instant, so C can voice a
// clarify immediately or fast-ack ("在弄了") and delegate async. Returns:
//   - (req, nil, nil)   → send req to DoTask in a goroutine, then MapDoTaskOutcome.
//   - (_, clarify, nil) → voice clarify now; do NOT delegate (ambiguous/unknown agent).
//   - (_, nil, err)     → malformed call.
//
// Per-call agent override (Model 2): a named agent resolves for THIS task only;
// otherwise the persistent binding (CallState.bound) is used. PrepareDoTask does
// NOT set inFlight — C's goroutine owns that (SetInFlight before, ClearInFlight
// after) so get_status reflects the async delegation, not a blocking call.
func (d *Dispatcher) PrepareDoTask(argsJSON []byte) (DoTaskRequest, *SayResult, error) {
	var a struct {
		Task  string `json:"task"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(argsJSON, &a); err != nil || a.Task == "" {
		return DoTaskRequest{}, nil, fmt.Errorf("do_task requires a task")
	}
	agent := d.state.BoundAgent()
	if a.Agent != "" {
		res := d.resolver.Resolve(a.Agent)
		switch res.Status {
		case ResolveResolved:
			agent = res.Slug
		case ResolveAmbiguous:
			return DoTaskRequest{}, &SayResult{Status: "clarify",
				SpokenSummary: "你是指哪个 agent？" + joinHuman(res.Candidates),
				Say:           "你是指哪个 agent？" + joinHuman(res.Candidates)}, nil
		default:
			// Unknown named agent → ask rather than silently using the default.
			return DoTaskRequest{}, &SayResult{Status: "clarify",
				SpokenSummary: "我没找到这个 agent，你是指哪一个？",
				Say:           "我没找到这个 agent，你是指哪一个？"}, nil
		}
	}
	d.state.mu.Lock()
	burstID := d.state.burstID
	cwd, foregroundHint := d.state.callContextLocked()
	d.state.mu.Unlock()
	return DoTaskRequest{Text: a.Task, Agent: agent, ThreadID: burstID, CWD: cwd, ForegroundHint: foregroundHint}, nil, nil
}

// MapDoTaskOutcome converts a delegation result (or transport error) into the
// say-contract output. Pure + exported so C's async goroutine reuses it without
// going through Dispatch. status ∈ ok|failed|injected. OutcomeInjected carries an
// empty say so the front brain doesn't double-speak (the original do_task voices
// the final result).
func MapDoTaskOutcome(out DoTaskOutcome, err error) SayResult {
	if err != nil {
		return SayResult{Status: "failed", FailReason: err.Error(),
			SpokenSummary: "抱歉，刚才没能完成，连接出了点问题。",
			Say:           "抱歉，刚才没能完成，连接出了点问题。"}
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
		status := "ok"
		if out.Partial {
			status = "failed"
		}
		spoken := out.SpokenSummary
		if spoken == "" {
			spoken = out.Reply
		}
		return SayResult{Status: status, SpokenSummary: spoken, Say: spoken,
			Context: voiceContextDigest(out.Reply, spoken), FailReason: out.FailureCode}
	case OutcomeInjected:
		return SayResult{Status: "injected"}
	default: // OutcomeRejected
		return SayResult{Status: "failed", FailReason: out.Reason,
			SpokenSummary: "现在有点忙，稍等一下再说一次好吗？",
			Say:           "现在有点忙，稍等一下再说一次好吗？"}
	}
}

// joinHuman renders candidate slugs into a spoken "A 还是 B" choice.
func joinHuman(slugs []string) string {
	switch len(slugs) {
	case 0:
		return ""
	case 1:
		return slugs[0]
	default:
		out := slugs[0]
		for _, s := range slugs[1:] {
			out += " 还是 " + s
		}
		return out
	}
}
