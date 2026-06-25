package koe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
			Description: "Delegate a real task to the Kocoro back-brain (which has full tools and memory). Use for anything that does work: scheduling, lookups, edits, multi-step jobs. Optionally name a specific agent.",
			Parameters:  obj(`{"type":"object","properties":{"task":{"type":"string","description":"The task to perform, in the user's own words."},"agent":{"type":"string","description":"Optional: the agent the user named for this task, verbatim (e.g. \"金融\", \"finance\"). Omit to use the bound agent."}},"required":["task"]}`)},
		{Type: "function", Name: "cancel",
			Description: "Cancel the task that is currently running.",
			Parameters:  obj(`{"type":"object","properties":{"reason":{"type":"string","enum":["user_cancel","interrupt"],"description":"Why the task is being cancelled."}},"required":[]}`)},
		{Type: "function", Name: "get_status",
			Description: "Check whether a delegated task is still running.",
			Parameters:  obj(`{"type":"object","properties":{},"required":[]}`)},
		{Type: "function", Name: "control_app",
			Description: "Control the Kocoro desktop window or conversation.",
			Parameters:  obj(`{"type":"object","properties":{"action":{"type":"string","enum":["show","hide","new_conversation","open_settings"],"description":"The UI action to perform."}},"required":["action"]}`)},
		{Type: "function", Name: "switch_agent",
			Description: "Change which back-brain agent handles future tasks until told otherwise.",
			Parameters:  obj(`{"type":"object","properties":{"agent":{"type":"string","description":"The agent the user named, verbatim."}},"required":["agent"]}`)},
	}
}

// CallState holds the per-call mutable binding + in-flight tracker. burstID is
// fixed for the call; boundAgent changes via switch_agent; inFlight tracks the
// active do_task for get_status.
type CallState struct {
	mu       sync.Mutex
	burstID  string
	bound    string
	inFlight string
}

func NewCallState(burstID, boundAgent string) *CallState {
	return &CallState{burstID: burstID, bound: boundAgent}
}

func (s *CallState) BoundAgent() string { s.mu.Lock(); defer s.mu.Unlock(); return s.bound }
func (s *CallState) setBound(a string)  { s.mu.Lock(); s.bound = a; s.mu.Unlock() }
func (s *CallState) BurstID() string    { s.mu.Lock(); defer s.mu.Unlock(); return s.burstID }

// SetInFlight / ClearInFlight are exported because C's async do_task goroutine
// (NOT a blocking Dispatch) owns the in-flight lifecycle: set before delegating,
// clear when the result returns. get_status reads InFlight.
func (s *CallState) SetInFlight(t string) { s.mu.Lock(); s.inFlight = t; s.mu.Unlock() }
func (s *CallState) ClearInFlight()       { s.mu.Lock(); s.inFlight = ""; s.mu.Unlock() }
func (s *CallState) InFlight() string     { s.mu.Lock(); defer s.mu.Unlock(); return s.inFlight }

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

// SayResult is the do_task function_call_output contract (spec §4): only the
// spoken sentence + a status, never the back-brain's tools/reasoning/transcript.
// Exported so C's async do_task goroutine consumes MapDoTaskOutcome's return.
type SayResult struct {
	Say        string `json:"say"`
	Status     string `json:"status"` // ok | failed | injected | clarify
	FailReason string `json:"fail_reason,omitempty"`
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
		if err := d.client.Cancel(ctx, CancelRequest{RouteKey: key, Reason: reason}); err != nil {
			return mustJSON(map[string]string{"status": "failed", "error": err.Error()}), nil
		}
		d.state.ClearInFlight()
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
				Say: "你是指哪个 agent？" + joinHuman(res.Candidates)}, nil
		default:
			// Unknown named agent → ask rather than silently using the default.
			return DoTaskRequest{}, &SayResult{Status: "clarify",
				Say: "我没找到这个 agent，你是指哪一个？"}, nil
		}
	}
	return DoTaskRequest{Text: a.Task, Agent: agent, ThreadID: d.state.BurstID()}, nil, nil
}

// MapDoTaskOutcome converts a delegation result (or transport error) into the
// say-contract output. Pure + exported so C's async goroutine reuses it without
// going through Dispatch. status ∈ ok|failed|injected. OutcomeInjected carries an
// empty say so the front brain doesn't double-speak (the original do_task voices
// the final result).
func MapDoTaskOutcome(out DoTaskOutcome, err error) SayResult {
	if err != nil {
		return SayResult{Status: "failed", FailReason: err.Error(),
			Say: "抱歉，刚才没能完成，连接出了点问题。"}
	}
	switch out.Kind {
	case OutcomeCompleted:
		status := "ok"
		if out.Partial {
			status = "failed"
		}
		return SayResult{Status: status, Say: out.Reply, FailReason: out.FailureCode}
	case OutcomeInjected:
		return SayResult{Status: "injected"}
	default: // OutcomeRejected
		return SayResult{Status: "failed", FailReason: out.Reason,
			Say: "现在有点忙，稍等一下再说一次好吗？"}
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
