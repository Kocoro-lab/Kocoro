package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// LoopAction tells the agent loop how to respond to a detection signal.
type LoopAction int

const (
	LoopContinue  LoopAction = iota // proceed normally
	LoopNudge                       // inject "try different approach" message
	LoopForceStop                   // force final response without tools
)

const loopForceStopOutcomeCaveat = "Further tool calls are blocked for this run. The stop itself is not evidence of success. Preserve completed results; treat unverified completion or side effects as partial, blocked, or outcome unknown."

const (
	readOnlyStableNudgeAt    = 3
	readOnlyStableBlockAfter = 5
	pingPongNudgeAt          = 6
	pingPongBlockAfter       = 8
	webStableNudgeAt         = 5
	webStableBlockAfter      = 7
)

func loopForceStopNote(detail string) string {
	return strings.TrimSpace(detail) + " " + loopForceStopOutcomeCaveat
}

// ToolCallRecord tracks a single tool invocation for loop detection.
type ToolCallRecord struct {
	Name            string
	ArgsHash        string // hex-encoded hash of raw args
	TopicHash       string // hex-encoded hash of normalized args (web tools)
	ResultSig       string // domain signature from results (web tools)
	OutcomeSig      string // stable signature of the complete tool outcome
	IsReadOnly      bool   // identical read args may still make progress when outcomes change
	TrustsProgress  bool   // tool explicitly guarantees distinct outcomes prove bounded state-machine progress
	IsError         bool
	ErrorSig        string // first 100 chars of error for grouping
	IsNonActionable bool   // search returned no useful results (no matches, binary noise, errors)
	// IsEmptyThinkInput is true only for `think` tool calls whose parsed
	// args yield an empty / whitespace-only `thought`. Used by Check() to
	// force-stop after two consecutive ritual think({}) calls (see plan
	// 2026-05-14-thinking-blocks-alignment.md Phase 0). Always false for
	// other tools, malformed JSON, or non-empty thought values.
	IsEmptyThinkInput bool
}

// BoundedProgressTool opts a stateful tool into strict distinct-outcome
// progress detection. Implementations must represent a bounded state machine
// where every successful call advances that state and returns durable evidence
// of the new state. Arbitrary external create/send/delete tools must not opt in:
// fresh provider ids alone do not make repeated mutations safe.
type BoundedProgressTool interface {
	TrustsDistinctOutcomeProgress() bool
}

// LoopDetector uses a sliding window of recent tool calls to detect stuck loops.
//
// Nine detection paths (checked in order, first match wins):
//   - ToolModeSwitch: visual tool after successful GUI-adjacent tool (applescript/browser)
//   - SuccessAfterError: visual tool after error recovery
//   - ConsecutiveDuplicate: back-to-back identical calls (catches web_search→web_search)
//   - ExactDuplicate: same name+argsHash spread across window (catches read→edit→read→edit→read)
//   - SameToolError: same tool returns errors N+ times in window
//   - FamilyNoProgress: tools in the same family, counted by topic or result
//     similarity (stable web results → nudge at 5 and block call 8 before
//     dispatch; same-topic calls with changing results use the broader 5/8/12 budget)
//     Fallback: same-tool count when topic tracking unavailable (8 → nudge, 12 → force stop)
//   - SearchEscalation: trailing unproductive search-family calls
//     (7 unproductive → nudge, 12 unproductive → force stop)
//   - NoProgress: same tool called M+ times regardless of args (skip visual/search tools,
//     semi-repeatable tools like bash get a higher threshold)
//
// Response escalation: threshold = nudge, threshold+1 = force stop (consecutive), 2x threshold = force stop (others).
//
// Removed 2026-05: a `Sleep` detector (bash commands containing `sleep N` →
// nudge/force-stop) produced false positives on legitimate parallel-sleep
// tasks. Real polling is now covered by ExactDup (same command repeated),
// NoProgress (many bash calls without progress), and the idle watchdog
// (silent long-running commands). See the post-mortem in plan
// 2026-05-14-thinking-blocks-alignment.md.
type LoopDetector struct {
	history     []ToolCallRecord
	historySize int

	consecDupThreshold   int // back-to-back identical calls (default 2)
	exactDupThreshold    int // spread-out identical calls in window (default 3)
	sameToolErrThreshold int
	noProgressThreshold  int

	repeatableTools         map[string]bool
	semiRepeatableTools     map[string]bool // higher NoProgress threshold (e.g. bash)
	semiRepeatableThreshold int             // nudge threshold for semi-repeatable tools
	// Note: force-stop = threshold*2 = 24 exceeds historySize (20), so the
	// NoProgress force-stop is intentionally unreachable for semi-repeatable
	// tools. The nudge budget escalation (maxNudges in loop.go) is the
	// backstop that converts accumulated nudges into a force-stop.

	// batchTolerant lists tools whose NoProgress nudge is gated on an
	// args-uniqueness ratio. When ≥50% of same-name calls in the window
	// carry distinct argsHash, the detector treats the stream as a
	// legitimate batch/enumeration rather than a stuck loop. Populated at
	// construction time from bash + the runtime's MCP tool names. Only
	// this set gets the relaxation; the generic NoProgress path for
	// think/http/file_*/grep/glob stays fully active.
	batchTolerant map[string]bool

	// ToolModeSwitch detector state
	lastNonGUISuccess bool
	lastNonGUITool    string
	modeSwitchNudged  bool

	// SuccessAfterError detector state
	recentRecovery bool
	recoveredTool  string
}

// GUITools are tools that indicate GUI automation tasks.
// Used by LoopDetector to recognize GUI observation/action workflows.
// Note: the literal "browser" key covers the legacy in-process browser tool.
// Real MCP playwright tool names (browser_navigate, browser_snapshot, …) are
// handled via isGUIToolName, which also prefix-matches "browser_".
var GUITools = map[string]bool{
	"screenshot": true, "computer": true, "computer_use": true, "applescript": true, "browser": true, "accessibility": true,
}

// isGUIToolName reports whether a tool name belongs to the GUI automation
// family, including playwright MCP tools that share the "browser_" prefix.
func isGUIToolName(name string) bool {
	if GUITools[name] {
		return true
	}
	return strings.HasPrefix(name, "browser_")
}

// visualTools are tools used purely for visual verification (screenshots, mouse/keyboard).
// Separate from GUITools because applescript/browser return structured data results.
// Used by the mode-switch detector to distinguish data tools from visual verification.
var visualTools = map[string]bool{
	"screenshot": true, "computer": true, "computer_use": true, "accessibility": true,
}

// repeatableGUITools extends visualTools with browser — multi-step browser workflows
// (navigate → snapshot → click → type → snapshot) naturally use many calls with
// different actions, so the no-progress fallback should not over-trigger.
// Topic-based detection (FamilyNoProgress) still catches same-URL loops.
var repeatableGUITools = map[string]bool{
	"screenshot": true, "computer": true, "computer_use": true, "accessibility": true, "browser": true,
}

// dupExemptTools lists tools where every call is inherently independent
// and duplicate counting (consecutive or windowed) is always a false
// positive. Unlike repeatableGUITools (where polling spin is still a real
// concern caught by ExactDup), these tools have zero side effects and
// zero cost model — calling them N times produces N identical outputs
// with no state change.
//
//   - use_skill: idempotent metadata loader (internal/tools/skill.go).
//     Loading the same SKILL.md ×N is harmless. Production audit log had
//     9 force-stops at iter=3 on use_skill same-args ×3 (2026-04-21).
//
// Adding a tool here is a stronger exemption than repeatableGUITools —
// think carefully before extending.
//
// IMMUTABLE AT RUNTIME: this map is shared across all LoopDetector
// instances. Do NOT mutate it after package init. Modify only by
// editing the literal below.
var dupExemptTools = map[string]bool{
	"use_skill": true,
}

// windowDupExemptTools contains observation tools whose identical arguments
// do not imply identical state when another action occurred between calls.
// Consecutive duplicates remain protected by the stricter detector above, so
// snapshot-only polling still stops while snapshot → click → snapshot remains
// a valid state-observation workflow.
var windowDupExemptTools = map[string]bool{
	"browser_snapshot": true,
}

// isRepeatableToolName reports whether a tool naturally repeats across a
// workflow and should be exempt from the generic NoProgress detectors. It
// checks the configured repeatable set plus a "browser_" prefix so playwright
// MCP tools (browser_navigate, browser_snapshot, …) match without having to
// enumerate every one.
func isRepeatableToolName(set map[string]bool, name string) bool {
	if set[name] {
		return true
	}
	return strings.HasPrefix(name, "browser_")
}

// semiRepeatableProdTools lists tools that legitimately appear many times
// in multi-step scripting workflows (fetch → process → install → build)
// but should NOT be fully exempt from the NoProgress detector because
// real loops also live in bash. The exact-dup, same-error, and sleep
// detectors still catch genuine stuck loops at their existing thresholds.
var semiRepeatableProdTools = map[string]bool{
	"bash": true,
}

// batchTolerantTools lists tools whose NoProgress nudge is gated on argument
// uniqueness: when >=50% of same-name calls in the window carry distinct
// argsHash, the run is treated as legitimate batch enumeration and exempted
// (the exact-dup / same-error detectors still catch genuine spins). http is
// here so an agent batch-disabling many items (POST /skills/disabled with a
// distinct body each — e.g. clearing a 100+ longbridge skill family) is not
// flagged as a no-progress loop; identical-args polling stays caught because
// it fails the uniqueness gate.
var batchTolerantTools = map[string]bool{
	"http": true,
}

// readVerbs and writeVerbs classify MCP tool names by the conventional
// verb word. Only tools whose primary verb (position 0, 1, or 2 after
// tokenizing on _ or -) is in readVerbs AND whose first three tokens
// contain NO writeVerb are eligible for batch-tolerance. The write
// blacklist is the defensive half of the heuristic: names like
// lookup_and_delete_all_records, get_or_create_item,
// find_and_remove_entry would otherwise sneak through on the position-0
// read-verb match, despite an obvious destructive suffix. Anything
// unmatched stays under the count-based NoProgress guard because a loop
// of write calls with unique arguments is exactly what NoProgress
// defends against (the permission engine does not gate MCP calls, and
// MCPTool.RequiresApproval() is always false).
var readVerbs = map[string]bool{
	"get":      true,
	"list":     true,
	"search":   true,
	"query":    true,
	"fetch":    true,
	"read":     true,
	"describe": true,
	"find":     true,
	"count":    true,
	"head":     true,
	"show":     true,
	"resolve":  true,
	"lookup":   true,
	"inspect":  true,
}

var writeVerbs = map[string]bool{
	// Destructive/creative verbs.
	"create":  true,
	"delete":  true,
	"update":  true,
	"remove":  true,
	"insert":  true,
	"append":  true,
	"archive": true,
	"modify":  true,
	"rename":  true,
	"replace": true,
	"drop":    true,
	"prune":   true,
	"clear":   true,
	// Data transfer / mutation verbs.
	"send":    true,
	"move":    true,
	"upload":  true,
	"write":   true,
	"push":    true,
	"publish": true,
	"submit":  true,
	"post":    true,
	// Key-value / property verbs (common in GitHub/Linear/Notion/Slack
	// MCP servers for compound names like get_and_set_properties).
	"add":   true,
	"set":   true,
	"patch": true,
	"put":   true,
	// Ambiguous execution verbs (could SELECT or INSERT — fail-closed).
	"execute": true,
	"run":     true,
}

// isReadMCPName reports whether an MCP tool name looks like a read-only
// operation. Tokenizes on both '_' and '-', then checks the first three
// tokens: accepts iff ≥1 read verb is present AND 0 write verbs are
// present. Matching is case-insensitive. Handles:
//
//   - direct prefix:          list_calendars, get_events
//   - 2-token namespaced:     notion_list_pages, API-query-data-source
//   - 3-token namespaced:     google_gmail_search_messages
//   - compound-verb rejects:  lookup_and_delete_all_records,
//     get_or_create_item, find_and_remove_entry
//
// Fail-closed: ambiguous names (run_* / execute_* — could be SELECT or
// INSERT) go through writeVerbs so the count-based guard stays engaged.
// Names whose verb sits at position 3 or later are treated as writes.
// mintedResourceNouns are resources a "get" typically MINTS or CONSUMES
// rather than reads: fetching one changes server state (a fresh token, an
// acquired lock, a burned nonce). A read-verb name touching one of these is
// treated as mutating so the loop detector applies the tighter side-effect
// budget (force at call 4, not the read-only 6). Deliberately narrow:
// ambiguous nouns (session, key, url alone) stay read-only — the regression
// corpus in loopdetect_readname_regression_test.go pins both directions.
var mintedResourceNouns = map[string]bool{
	"token": true, "tokens": true, "lock": true, "locks": true,
	"lease": true, "leases": true, "nonce": true, "otp": true,
	"credential": true, "credentials": true, "secret": true, "secrets": true,
	"ticket": true, "tickets": true,
}

// consumedWorkItemNouns complete the queue-consuming "next <work item>"
// pattern (get_next_job dequeues; get_next_page paginates and stays read).
var consumedWorkItemNouns = map[string]bool{
	"task": true, "job": true, "message": true, "item": true,
}

// urlMinterQualifiers complete the presigned-URL pattern: get_upload_url /
// get_signed_url mint a capability, while a bare get_url stays read-only.
var urlMinterQualifiers = map[string]bool{
	"upload": true, "signed": true, "presigned": true,
}

func isReadMCPName(name string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '_' || r == '-'
	})
	limit := len(tokens)
	if limit > 3 {
		limit = 3
	}
	hasRead := false
	for i := 0; i < limit; i++ {
		if writeVerbs[tokens[i]] {
			return false
		}
		if readVerbs[tokens[i]] {
			hasRead = true
		}
	}
	if !hasRead {
		return false
	}
	// Mint/consume layer scans EVERY token — the resource noun can sit past
	// position 3 (get_upload_url, google_auth_get_access_token).
	for i, tok := range tokens {
		if mintedResourceNouns[tok] {
			return false
		}
		if tok == "next" && i+1 < len(tokens) && consumedWorkItemNouns[tokens[i+1]] {
			return false
		}
		if urlMinterQualifiers[tok] && i+1 < len(tokens) && tokens[i+1] == "url" {
			return false
		}
	}
	return true
}

// NewLoopDetector creates a detector with production defaults.
//
// Threshold policy (v2, 2026-04-22): values are tuned for Claude 4.X
// self-recovery behavior. The previous (3.5-era) defaults were 2/3/4/8
// and produced frequent false positives — newer models reliably notice
// they're stuck and switch approach without external intervention.
// Raised values trade off slightly later detection of genuine spin for
// dramatically fewer false-positive nudges/force-stops on legitimate
// iterative workflows (refactor loops, multi-source research, form
// fills). The unit tests assert the relationships, not the absolute
// numbers — so retuning is a one-line change here.
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		history:                 make([]ToolCallRecord, 0, 20),
		historySize:             20,
		consecDupThreshold:      3,  // v2: 2 → 3 (was over-strict for re-search/re-fetch)
		exactDupThreshold:       5,  // v2: 3 → 5 (refactor read→edit→read iteration is common)
		sameToolErrThreshold:    6,  // v2: 4 → 6 (cross-args retry needs more headroom)
		noProgressThreshold:     12, // v2: 8 → 12 (legitimate research uses many same-tool calls)
		repeatableTools:         repeatableGUITools,
		semiRepeatableTools:     semiRepeatableProdTools,
		semiRepeatableThreshold: 16, // v2: 12 → 16 (bash multi-step scripting)
		batchTolerant:           batchTolerantTools,
	}
}

// Record adds a tool call to the sliding window.
func (ld *LoopDetector) Record(name, argsJSON string, isError bool, errMsg string, resultSig string, isNonActionable bool) {
	ld.RecordOutcome(name, argsJSON, isError, errMsg, resultSig, "[legacy-static-outcome]", false, isNonActionable)
}

// RecordOutcome adds a tool call together with the execution outcome used to
// distinguish a stuck observation loop from legitimate polling. Exact repeated
// mutations deliberately remain argument-based: repeating the same write is
// dangerous even when a provider returns fresh ids or timestamps. The generic
// same-tool counter recognizes changing outcomes only for read-only tools or
// explicit BoundedProgressTool state machines, and then requires every argument
// and every successful outcome in the window to be distinct.
func (ld *LoopDetector) RecordOutcome(name, argsJSON string, isError bool, errMsg string, resultSig string, outcomeSig string, isReadOnly bool, isNonActionable bool) {
	ld.recordOutcome(name, argsJSON, isError, errMsg, resultSig, outcomeSig, isReadOnly, false, isNonActionable)
}

func (ld *LoopDetector) recordOutcome(name, argsJSON string, isError bool, errMsg string, resultSig string, outcomeSig string, isReadOnly bool, trustsProgress bool, isNonActionable bool) {
	topicHash := ""
	if toolFamily(name) != "" {
		normalized := normalizeWebQuery(argsJSON)
		if normalized != "" {
			topicHash = hashArgs(normalized)
		}
	}
	rec := ToolCallRecord{
		Name:              name,
		ArgsHash:          hashArgs(argsJSON),
		TopicHash:         topicHash,
		ResultSig:         resultSig,
		OutcomeSig:        outcomeSig,
		IsReadOnly:        isReadOnly,
		TrustsProgress:    trustsProgress,
		IsError:           isError,
		ErrorSig:          truncateErrSig(errMsg, 100),
		IsNonActionable:   isNonActionable,
		IsEmptyThinkInput: isEmptyThinkInput(name, argsJSON),
	}
	ld.history = append(ld.history, rec)
	if len(ld.history) > ld.historySize {
		ld.history = ld.history[len(ld.history)-ld.historySize:]
	}

	// Track non-visual tool success for mode-switch detection.
	// Uses visualTools (not GUITools) because applescript/browser return structured data.
	if !visualTools[name] {
		if isError {
			ld.lastNonGUISuccess = false
			ld.lastNonGUITool = ""
		} else {
			ld.lastNonGUISuccess = true
			ld.lastNonGUITool = name
			ld.modeSwitchNudged = false
		}
	}

	// Track error recovery for SuccessAfterError detection
	if !isError && !visualTools[name] {
		// Check if this tool had a previous error in the window with different args
		hasEarlierError := false
		for _, rec := range ld.history[:len(ld.history)-1] { // exclude the just-recorded entry
			if rec.Name == name && rec.IsError && rec.ArgsHash != ld.history[len(ld.history)-1].ArgsHash {
				hasEarlierError = true
				break
			}
		}
		if hasEarlierError {
			ld.recentRecovery = true
			ld.recoveredTool = name
		} else if name != ld.recoveredTool {
			// Moving to different non-visual tool → reset recovery state
			ld.recentRecovery = false
			ld.recoveredTool = ""
		}
	}
}

// CheckBefore evaluates only patterns that can be proven from completed prior
// outcomes. It is intentionally limited to read-only observations: mutating
// calls have separate replay/idempotency admission, and predicting an unseen
// read result too early would hide legitimate progress. A stable observation
// gets one warning window before the sixth identical call is blocked.
func (ld *LoopDetector) CheckBefore(name, argsJSON string, isReadOnly bool) (LoopAction, string) {
	if !isReadOnly || dupExemptTools[name] || len(ld.history) == 0 {
		return LoopContinue, ""
	}
	argsHash := hashArgs(argsJSON)
	if count := stableReadOnlyActionTail(ld.history, name, argsHash); count >= readOnlyStableBlockAfter {
		return LoopForceStop, fmt.Sprintf(
			"Blocked %s before execution: the same read-only action returned an identical outcome %d times and exhausted its warning window.",
			name, count)
	}
	if pingPongNoProgressCount(ld.history) >= pingPongBlockAfter &&
		pingPongWouldContinue(ld.history, name, argsHash) {
		return LoopForceStop, fmt.Sprintf(
			"Blocked %s before execution: it would continue an outcome-stable two-action loop after %d completed calls.",
			name, pingPongBlockAfter)
	}
	if toolFamily(name) == "web" {
		if count := stableWebResultTail(ld.history); count >= webStableBlockAfter {
			return LoopForceStop, fmt.Sprintf(
				"Blocked %s before execution: %d consecutive web calls returned the same sources despite changed arguments.",
				name, count)
		}
	}
	return LoopContinue, ""
}

// ShouldDisableSpeculation prevents a streamed read from executing before the
// provider's full batch is available whenever the prior history is already in
// a warned no-progress state. The normal batch admission can then veto every
// sibling atomically before any tool starts.
func (ld *LoopDetector) ShouldDisableSpeculation() bool {
	if len(ld.history) == 0 {
		return false
	}
	last := ld.history[len(ld.history)-1]
	if stableReadOnlyActionTail(ld.history, last.Name, last.ArgsHash) >= readOnlyStableNudgeAt {
		return true
	}
	if pingPongNoProgressCount(ld.history) >= pingPongNudgeAt {
		return true
	}
	return toolFamily(last.Name) == "web" &&
		stableWebResultTail(ld.history) >= webStableNudgeAt
}

// Check evaluates all detectors for the named tool.
// Returns the most severe action and an appropriate message.
func (ld *LoopDetector) Check(name string) (LoopAction, string) {
	if len(ld.history) < 2 {
		return LoopContinue, ""
	}

	// 0a. Empty-think force-stop. Two consecutive `think` calls whose args
	// parsed to an empty/whitespace `thought` mean the model's reasoning has
	// already landed in a native thinking content block; further empty
	// think({}) calls are a no-op ritual that burns LLM round-trips. Fire
	// before any other detector so the agent never spins on this pattern.
	// Streak is broken by a non-think tool OR a think call with content.
	// See plan 2026-05-14-thinking-blocks-alignment.md Phase 0.3.
	if name == "think" {
		h := ld.history
		n := len(h)
		if n >= 2 &&
			h[n-1].Name == "think" && h[n-1].IsEmptyThinkInput &&
			h[n-2].Name == "think" && h[n-2].IsEmptyThinkInput {
			return LoopForceStop, loopForceStopNote("Two consecutive `think` calls had empty input. Further empty reasoning calls cannot add evidence.")
		}
	}

	// 0. Mode switch: visual tool used right after successful GUI-adjacent tool
	// (applescript, browser). Only fire for GUI-adjacent tools where visual
	// verification is likely redundant. Don't fire after file_read, bash, etc.
	if visualTools[name] && ld.lastNonGUISuccess && !ld.modeSwitchNudged && isGUIToolName(ld.lastNonGUITool) {
		ld.modeSwitchNudged = true
		return LoopNudge, fmt.Sprintf(
			"Your previous non-GUI tool call (%s) returned a success result. Visual verification is likely unnecessary — consider whether you can summarize the result and stop.", ld.lastNonGUITool)
	}

	// 0b. Success after error: agent recovered from error but continues verifying
	if visualTools[name] && ld.recentRecovery {
		ld.recentRecovery = false
		return LoopNudge, fmt.Sprintf(
			"You recovered from the earlier %s error and the retry succeeded. The successful result is your confirmation — proceed to your final answer.", ld.recoveredTool)
	}

	// Find latest argsHash for this tool (must be called right after Record).
	var latestHash string
	for i := len(ld.history) - 1; i >= 0; i-- {
		if ld.history[i].Name == name {
			latestHash = ld.history[i].ArgsHash
			break
		}
	}

	// 1a. Consecutive exact duplicate — catches back-to-back identical calls
	// like web_search→web_search. Does NOT fire for read→edit→read patterns
	// because the intervening edit breaks the consecutive run.
	// IsError-aware (added 2026-04-21):
	//
	//   Rule 1 (tail-success skip): if the most recent call succeeded AND the
	//   run had any error, the model has just recovered. Skip this detector —
	//   ExactDup across the full window still catches sustained spin, and
	//   punishing a successful retry is strictly worse than a false negative.
	//
	//   Rule 2 (all-errors 2x): if every call in the run is an error, treat
	//   it as legitimate retry and double the threshold (4 nudge, 5 force-stop).
	//   Flaky Playwright selectors race page-load timing — 3 fails is normal.
	//
	//   Otherwise (all-success, or mixed ending in error): original strict
	//   threshold (2 nudge, 3 force-stop).
	consecCount := 0
	consecErrCount := 0
	consecValidationErrCount := 0
	consecSameOutcomeCount := 0
	latestOutcome := ld.history[len(ld.history)-1].OutcomeSig
	outcomeTailOpen := latestOutcome != ""
	for i := len(ld.history) - 1; i >= 0; i-- {
		if ld.history[i].Name != name || ld.history[i].ArgsHash != latestHash {
			break
		}
		consecCount++
		if outcomeTailOpen && ld.history[i].OutcomeSig == latestOutcome {
			consecSameOutcomeCount++
		} else {
			outcomeTailOpen = false
		}
		if ld.history[i].IsError {
			consecErrCount++
			if isValidationErrorSig(ld.history[i].ErrorSig) {
				consecValidationErrCount++
			}
		}
	}
	// dupExemptTools (use_skill) are pure idempotent loaders — skip both
	// ConsecutiveDup and ExactDup checks entirely.
	// recovered = tail-success after any error → model has just recovered;
	// skip both 1a and 1b (see Rule 1 comment above).
	recovered := consecCount > 0 &&
		!ld.history[len(ld.history)-1].IsError &&
		consecErrCount > 0
	exactRecovered := latestRecoveredAfterSameArgsErrors(ld.history, name, latestHash)

	// 1a-pre. Deterministic validation-error short-circuit. Same tool +
	// same args + every result tagged as `[validation error]` × 3 means
	// the model is asking for the same impossible thing — retrying with
	// identical args cannot succeed. Skip the all-errors 2x budget below
	// (which is calibrated for flaky retries, not parameter-shape
	// mistakes) and force-stop immediately.
	if !dupExemptTools[name] && consecValidationErrCount >= 3 && consecValidationErrCount == consecCount {
		return LoopForceStop, loopForceStopNote(fmt.Sprintf(
			"Tool %s rejected your arguments %d times in a row with the same validation error: %q. "+
				"Retrying with identical arguments cannot succeed.",
			name, consecCount, ld.history[len(ld.history)-1].ErrorSig))
	}

	if !dupExemptTools[name] && consecCount > 0 && !recovered {
		threshold := ld.consecDupThreshold
		if consecErrCount == consecCount {
			threshold = ld.consecDupThreshold * 2 // Rule 2: all-errors budget
		}
		effectiveCount := consecCount
		if ld.history[len(ld.history)-1].IsReadOnly && consecErrCount == 0 {
			effectiveCount = consecSameOutcomeCount
		}
		if ld.history[len(ld.history)-1].IsReadOnly && consecErrCount == 0 {
			if effectiveCount >= readOnlyStableBlockAfter+1 {
				return LoopForceStop, loopForceStopNote(fmt.Sprintf(
					"You called %s with identical arguments and outcomes %d times in a row without new evidence.", name, effectiveCount))
			}
			if effectiveCount == readOnlyStableNudgeAt {
				return LoopNudge, fmt.Sprintf(
					"You've called %s %d times consecutively without a changed outcome. Inspect the latest result first. If it completes the task, use it and answer now; otherwise try a different approach.", name, effectiveCount)
			}
		} else if effectiveCount >= threshold+1 {
			return LoopForceStop, loopForceStopNote(fmt.Sprintf(
				"You called %s with identical arguments and outcomes %d times in a row without new evidence.", name, effectiveCount))
		} else if effectiveCount >= threshold {
			return LoopNudge, fmt.Sprintf(
				"You've called %s %d times consecutively without a changed outcome. Inspect the latest result first. If it completes the task, use it and answer now; otherwise try a different approach.", name, effectiveCount)
		}
	}

	// 1a-post. Ping-pong no-progress: A -> B -> A -> B with stable outcomes
	// on both sides. This catches loops that evade same-tool consecutive checks
	// while preserving productive alternation whenever either observation
	// changes. Six calls nudge; pre-dispatch admission blocks the ninth call
	// after one recovery window. Ten completed calls are a defensive fallback
	// for callers that use the detector without admission.
	if count := pingPongNoProgressCount(ld.history); count >= pingPongBlockAfter+2 {
		return LoopForceStop, loopForceStopNote(fmt.Sprintf(
			"You alternated between the same two tool actions %d times without a changed outcome.", count))
	} else if count == pingPongNudgeAt {
		return LoopNudge, fmt.Sprintf(
			"You've alternated between the same two tool actions %d times without a changed outcome. Use the evidence already returned or change strategy.", count)
	}

	// 1b. Window-based exact duplicate — catches spread-out repeats
	// like read→edit→read→edit→read (same args appearing 3+ times in window).
	// Rule 1 (tail-success skip) also applies here: skip if model just recovered.
	//
	// IsError-aware (added 2026-04-21): if every same-args repeat in the
	// window is an error, treat as flaky-retry recovery pattern (browser_click
	// → snapshot → wait_for → browser_click → …) and double the threshold.
	// Any success in the set means the tool sometimes works and continuing
	// identical calls is real spin — strict threshold applies.
	dupCount := 0
	dupErrCount := 0
	if latestHash != "" {
		for _, rec := range ld.history {
			if rec.Name == name && rec.ArgsHash == latestHash {
				dupCount++
				if rec.IsError {
					dupErrCount++
				}
			}
		}
	}
	dupSameOutcomeCount := 0
	if latestOutcome != "" {
		for i := len(ld.history) - 1; i >= 0; i-- {
			rec := ld.history[i]
			if rec.Name != name || rec.ArgsHash != latestHash {
				continue
			}
			if rec.OutcomeSig != latestOutcome {
				break
			}
			dupSameOutcomeCount++
		}
	}
	if !dupExemptTools[name] && !windowDupExemptTools[name] && !exactRecovered {
		threshold := ld.exactDupThreshold
		if dupCount > 0 && dupErrCount == dupCount {
			threshold = ld.exactDupThreshold * 2 // all-errors budget
		}
		effectiveCount := dupCount
		if ld.history[len(ld.history)-1].IsReadOnly && dupErrCount == 0 {
			effectiveCount = dupSameOutcomeCount
		}
		if effectiveCount >= threshold*2 {
			return LoopForceStop, loopForceStopNote(fmt.Sprintf(
				"You called %s with identical arguments and outcomes %d times without new evidence.", name, effectiveCount))
		}
		isReadOnlyLatest := ld.history[len(ld.history)-1].IsReadOnly && dupErrCount == 0
		if (!isReadOnlyLatest && effectiveCount >= threshold) ||
			(isReadOnlyLatest && effectiveCount == threshold && consecCount < effectiveCount) {
			return LoopNudge, fmt.Sprintf(
				"You've called %s %d times without a changed outcome. Inspect the latest result first. If it completes the task, use it and answer now; otherwise try a fundamentally different approach.", name, effectiveCount)
		}
	}

	// 2. Same tool error detector: same tool returning errors
	errCount := 0
	var lastErr string
	for _, rec := range ld.history {
		if rec.Name == name && rec.IsError {
			errCount++
			lastErr = rec.ErrorSig
		}
	}
	if errCount >= ld.sameToolErrThreshold*2 {
		return LoopForceStop, loopForceStopNote(fmt.Sprintf(
			"Tool %s failed %d times and no reliable recovery is available in this run.", name, errCount))
	}
	if errCount >= ld.sameToolErrThreshold {
		return LoopNudge, fmt.Sprintf(
			"Tool %s has failed %d times with: %s. Do NOT retry this tool. Use a different approach or ask the user.", name, errCount, lastErr)
	}

	// 3. Family no-progress: web tools in the same family, counted by topic similarity.
	// Tiered escalation: 3 same-topic → nudge, 5 → stronger nudge, 7 → force stop.
	family := toolFamily(name)
	if family != "" {
		latestTopic := ""
		latestResult := ""
		for i := len(ld.history) - 1; i >= 0; i-- {
			if toolFamily(ld.history[i].Name) == family {
				if latestTopic == "" && ld.history[i].TopicHash != "" {
					latestTopic = ld.history[i].TopicHash
				}
				if latestResult == "" && ld.history[i].ResultSig != "" {
					latestResult = ld.history[i].ResultSig
				}
				if latestTopic != "" && latestResult != "" {
					break
				}
			}
		}

		// Browser/gui families legitimately mix different tool names in one
		// workflow (navigate → click → type → upload) while still sharing the
		// same page URL/result signature. Scoping those families to the SAME
		// tool name avoids false positives on healthy multi-step interaction.
		// Web/search families keep the broader family-level counting so
		// alternating search/fetch loops still nudge early.
		scopeSameName := family == "browser" || family == "gui"
		familyCount := 0
		sameTopicCount := 0
		sameResultCount := 0
		for _, rec := range ld.history {
			if toolFamily(rec.Name) != family {
				continue
			}
			familyCount++
			if scopeSameName && rec.Name != name {
				continue
			}
			if latestTopic != "" && rec.TopicHash == latestTopic {
				sameTopicCount++
			}
			if latestResult != "" && rec.ResultSig == latestResult {
				sameResultCount++
			}
		}

		progressCount := sameTopicCount
		if sameResultCount > progressCount {
			progressCount = sameResultCount
		}
		if family == "web" {
			if stableCount := stableWebResultTail(ld.history); stableCount >= webStableBlockAfter+1 {
				return LoopForceStop, loopForceStopNote(fmt.Sprintf(
					"You made %d consecutive web calls with changed arguments, but they returned the same sources.", stableCount))
			}
		}

		// For repeatable tools (browser_*, screenshot, computer_use, accessibility, computer),
		// a stable result_sig is a weak "no progress" signal: SPA workflows and
		// form fills legitimately share the same URL across many operations.
		// When the strong topic-based signal is absent (no prior same-topic
		// collisions beyond the current call itself), use a single force-stop
		// threshold at 15 and skip intermediate nudges — nudges here would stack
		// with the rolling-window escalation (loop.go's nudges window) and kill
		// long but legitimate form fills.
		//
		// Non-repeatable families and repeatable tools with actual topic-signal
		// collisions still use the original 3/5/7 path.
		isRepeatable := isRepeatableToolName(ld.repeatableTools, name)
		// sameTopicCount includes the current call itself whenever latestTopic is
		// non-empty. Treat "self only" as no strong topic signal — the detector
		// should only use the stricter topic-based thresholds when prior calls
		// collide on the same normalized topic.
		topicCollisions := sameTopicCount
		if latestTopic != "" && topicCollisions > 0 {
			topicCollisions--
		}
		repeatableResultOnly := isRepeatable && topicCollisions == 0

		if repeatableResultOnly {
			if progressCount >= 15 {
				return LoopForceStop, loopForceStopNote(familyNoProgressMessage(family, progressCount, familyCount, 2))
			}
			// Below 15: silent. No nudge tier — see rationale above.
		} else {
			// v2 (2026-04-22): raised from 3/5/7 → 5/8/12. Multi-source research
			// (3 different queries on the same topic) is a legitimate pattern;
			// the old thresholds nudged the model immediately on a 3rd query.
			if progressCount >= 12 {
				return LoopForceStop, loopForceStopNote(familyNoProgressMessage(family, progressCount, familyCount, 2))
			}
			if progressCount >= 8 {
				return LoopNudge, familyNoProgressMessage(family, progressCount, familyCount, 1)
			}
			if progressCount >= 5 {
				return LoopNudge, familyNoProgressMessage(family, progressCount, familyCount, 0)
			}
		}

		// Fallback for families without topic/result tracking (e.g., GUI tools
		// where normalizer can't extract topics from script/screenshot args).
		// Count same-tool occurrences as a proxy for lack of progress.
		// Skip repeatable tools and search-family tools (search has its own
		// dedicated unproductive-streak detector below).
		if progressCount == 0 && !isRepeatableToolName(ld.repeatableTools, name) && family != "search" {
			sameToolInFamily := 0
			for _, rec := range ld.history {
				if rec.Name == name {
					sameToolInFamily++
				}
			}
			// v2 (2026-04-22): raised from 5/7 → 8/12. This fallback fires only
			// when topic/result tracking was empty (e.g. file_* / grep / glob
			// where there's no URL or web topic to dedupe by). Real research
			// sessions can hit a single tool 6-10 times legitimately.
			if sameToolInFamily >= 12 {
				return LoopForceStop, loopForceStopNote(fmt.Sprintf(
					"You called %s %d times without meaningful progress.", name, sameToolInFamily))
			}
			if sameToolInFamily >= 8 {
				return LoopNudge, fmt.Sprintf(
					"You've called %s %d times. Consider whether you're making progress or stuck in a loop.", name, sameToolInFamily)
			}
		}
	}

	// 4. Search escalation: count trailing unproductive search-family calls.
	// A productive search (actionable results) resets the streak. Only
	// non-actionable calls (no matches, errors, binary noise) count.
	if family == "search" {
		unproductiveStreak := 0
		for i := len(ld.history) - 1; i >= 0; i-- {
			rec := ld.history[i]
			if toolFamily(rec.Name) != "search" {
				break // non-search tool breaks the streak
			}
			if !rec.IsNonActionable {
				break // productive search resets the streak
			}
			unproductiveStreak++
		}
		// v2 (2026-04-22): raised from 5/8 → 7/12. Rare-information lookups
		// (e.g. "find this obscure error string") legitimately need many
		// query variants before finding a hit.
		if unproductiveStreak >= 12 {
			return LoopForceStop, loopForceStopNote(fmt.Sprintf(
				"You made %d consecutive unproductive search calls without finding useful evidence.", unproductiveStreak))
		}
		if unproductiveStreak >= 7 {
			return LoopNudge, fmt.Sprintf(
				"You've made %d search calls without finding useful results. Reconsider your approach — try different search terms, check if the file/pattern exists, or ask the user for guidance.", unproductiveStreak)
		}
	}

	// 5. No progress detector: same tool called too many times.
	// Search-family tools are excluded because productive repository exploration
	// often uses many grep/glob calls with different arguments.
	// Semi-repeatable tools (e.g. bash) get a higher threshold because
	// legitimate multi-step scripting uses many distinct calls, but they
	// are NOT fully exempt — the exact-dup, same-error, and sleep
	// detectors still catch real loops at their own thresholds.
	//
	// Batch-tolerant tools (http) additionally get a uniqueness gate: when
	// ≥50% of same-name calls carry distinct argsHash, treat the stream as
	// legitimate enumeration (e.g. paging an API, polling distinct URLs) and
	// fall through to the remaining detectors. Read-only tools and explicit
	// BoundedProgressTool state machines earn the same relief only with stronger
	// evidence: every same-name call in the window succeeded, and both its
	// arguments and complete outcome are unique. Stable/empty outcomes and
	// arbitrary write tools therefore stay under this guard.
	if !isRepeatableToolName(ld.repeatableTools, name) && family != "search" {
		count := 0
		seen := make(map[string]struct{}, ld.historySize)
		for _, rec := range ld.history {
			if rec.Name == name {
				count++
				seen[rec.ArgsHash] = struct{}{}
			}
		}
		threshold := ld.noProgressThreshold
		if ld.semiRepeatableTools[name] {
			threshold = ld.semiRepeatableThreshold
		}
		batchGated := ld.batchTolerant[name] && count > 0 && len(seen)*2 >= count
		progressGated := count >= threshold && hasStrictOutcomeProgress(ld.history, name)
		if !batchGated && !progressGated {
			if count >= threshold*2 {
				return LoopForceStop, loopForceStopNote(fmt.Sprintf(
					"You called %s %d times without meaningful progress.", name, count))
			}
			if count >= threshold {
				return LoopNudge, fmt.Sprintf(
					"You've called %s %d times. Summarize what you've learned and try a different approach.", name, count)
			}
		}
	}

	// Real polling patterns are caught by the generic detectors above:
	//   - exact-args repeats (`sleep N && check_status` x N times) → ExactDup
	//   - many bash calls without progress → NoProgress (12 threshold)
	//   - silent hang on a long-running command → idle watchdog (90s/540s)
	// A dedicated `sleep` count produced false positives on legitimate
	// parallel-sleep tasks (a user requested "run 3 sleep commands in
	// parallel" and the model's first iteration tripped the 4-count
	// threshold — see plan 2026-05-14-thinking-blocks-alignment.md
	// post-mortem). Removed 2026-05.
	return LoopContinue, ""
}

// hasStrictOutcomeProgress recognizes a dependent/enumerated sequence without
// trusting a volatile receipt for repeated mutations. Every same-name record in
// the active window must be a success with a new argument identity and a new
// complete-outcome identity. Any duplicate, missing evidence, or error keeps the
// ordinary NoProgress thresholds active.
func hasStrictOutcomeProgress(history []ToolCallRecord, name string) bool {
	seenArgs := make(map[string]struct{})
	seenOutcomes := make(map[string]struct{})
	count := 0
	for _, record := range history {
		if record.Name != name {
			continue
		}
		count++
		if record.IsError || (!record.IsReadOnly && !record.TrustsProgress) || record.ArgsHash == "" || record.OutcomeSig == "" {
			return false
		}
		if _, duplicate := seenArgs[record.ArgsHash]; duplicate {
			return false
		}
		if _, duplicate := seenOutcomes[record.OutcomeSig]; duplicate {
			return false
		}
		seenArgs[record.ArgsHash] = struct{}{}
		seenOutcomes[record.OutcomeSig] = struct{}{}
	}
	return count > 1
}

func stableReadOnlyActionTail(history []ToolCallRecord, name, argsHash string) int {
	if len(history) == 0 {
		return 0
	}
	latestOutcome := ""
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		record := history[i]
		if record.Name != name || record.ArgsHash != argsHash || !record.IsReadOnly || record.IsError || record.OutcomeSig == "" {
			break
		}
		if latestOutcome == "" {
			latestOutcome = record.OutcomeSig
		} else if record.OutcomeSig != latestOutcome {
			break
		}
		count++
	}
	return count
}

func stableWebResultTail(history []ToolCallRecord) int {
	if len(history) == 0 {
		return 0
	}
	latestResult := ""
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		record := history[i]
		if toolFamily(record.Name) != "web" ||
			!record.IsReadOnly || record.IsError || record.ResultSig == "" {
			break
		}
		if latestResult == "" {
			latestResult = record.ResultSig
		} else if record.ResultSig != latestResult {
			break
		}
		count++
	}
	return count
}

func pingPongWouldContinue(history []ToolCallRecord, name, argsHash string) bool {
	if len(history) < 2 {
		return false
	}
	expected := history[len(history)-2]
	return expected.Name == name && expected.ArgsHash == argsHash
}

func pingPongNoProgressCount(history []ToolCallRecord) int {
	if len(history) < 4 {
		return 0
	}
	last := history[len(history)-1]
	previous := history[len(history)-2]
	if sameToolAction(last, previous) {
		return 0
	}

	outcomeSet := [2]bool{}
	outcomes := [2]string{}
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		record := history[i]
		slot := count % 2
		expected := last
		if slot == 1 {
			expected = previous
		}
		if !sameToolAction(record, expected) {
			break
		}
		// Alternating failures use the existing error recovery budgets, which
		// deliberately allow flaky calls more room than successful no-progress
		// observations. Do not let the ping-pong path bypass that policy.
		if record.IsError {
			return 0
		}
		if record.IsReadOnly && !record.IsError {
			if record.OutcomeSig == "" {
				return 0
			}
			if outcomeSet[slot] && outcomes[slot] != record.OutcomeSig {
				return 0
			}
			outcomeSet[slot] = true
			outcomes[slot] = record.OutcomeSig
		}
		count++
	}
	if count < 4 || count%2 != 0 {
		return 0
	}
	return count
}

func sameToolAction(left, right ToolCallRecord) bool {
	return left.Name == right.Name && left.ArgsHash == right.ArgsHash
}

func isLoopReadOnlyCall(tool Tool, name, argsJSON string) bool {
	if tool == nil {
		return false
	}
	if readOnly, ok := tool.(ReadOnlyChecker); ok && readOnly.IsReadOnlyCall(argsJSON) {
		return true
	}
	if source, ok := tool.(ToolSourcer); ok {
		switch source.ToolSource() {
		case SourceMCP:
			return name == "browser_snapshot" || isReadMCPName(name)
		case SourceGateway, SourceIntegration:
			return false
		}
	}
	if name == "browser_snapshot" {
		return true
	}
	// Local argument-aware tools such as bash and process deliberately keep
	// IsReadOnlyCall false to prevent speculative execution, while their
	// materiality checker can still prove an individual poll is observational.
	if material, ok := tool.(MaterialSideEffectChecker); ok {
		return !material.HasMaterialSideEffect(argsJSON)
	}
	return false
}

// latestRecoveredAfterSameArgsErrors reports whether the latest same-name,
// same-args call is the first success after a recent same-args error streak.
// Intervening different tools do not break this recovery pattern for ExactDup:
// browser_click(e1,error) → browser_snapshot → browser_click(e1,success) is
// still a legitimate retry recovery, not spread-out spin.
func latestRecoveredAfterSameArgsErrors(history []ToolCallRecord, name, latestHash string) bool {
	if len(history) == 0 || latestHash == "" {
		return false
	}
	latest := history[len(history)-1]
	if latest.Name != name || latest.ArgsHash != latestHash || latest.IsError {
		return false
	}

	sawError := false
	for i := len(history) - 2; i >= 0; i-- {
		rec := history[i]
		if rec.Name != name || rec.ArgsHash != latestHash {
			continue
		}
		if rec.IsError {
			sawError = true
			continue
		}
		// An earlier same-args success means the recovery already happened.
		return sawError
	}
	return sawError
}

// isValidationErrorSig reports whether a result's error signature was produced
// by `agent.ValidationError(...)`, which prefixes its content with the literal
// "[validation error] " marker. Matching the prefix lets the loop detector
// distinguish deterministic "your arguments are wrong" failures from
// transient/flaky failures (network, permission denied, file moved by editor)
// that legitimately benefit from the all-errors 2x retry budget.
//
// Why prefix-only and not keyword matching: keyword heuristics ("missing",
// "required", "invalid") risk false positives on staleness messages
// ("File has been modified... read it again") and on tool-specific business
// errors. The prefix is set in exactly one place (tools.go ValidationError)
// and is byte-stable across releases.
func isValidationErrorSig(sig string) bool {
	return strings.HasPrefix(sig, "[validation error]")
}

func hashArgs(args string) string {
	h := sha256.Sum256([]byte(args))
	return hex.EncodeToString(h[:8])
}

func truncateErrSig(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

// familyNoProgressMessage returns a family-appropriate nudge/force-stop
// message for the FamilyNoProgress detector. The earlier wording was borrowed
// from the search family ("searched the same topic", "fundamentally different
// query") and surfaced verbatim for browser/gui callers when the detector
// was extended to cover "browser_*", producing misleading guidance like
// "You've searched..." for a series of browser_click calls. stage maps to
// the threshold tier: 0 = 3-hit initial nudge, 1 = 5-hit stronger nudge,
// 2 = 7-hit force stop.
func familyNoProgressMessage(family string, progressCount, familyCount, stage int) string {
	switch family {
	case "search", "web":
		switch stage {
		case 2:
			return fmt.Sprintf("You made %d web calls with %d on the same topic without obtaining new evidence.", familyCount, progressCount)
		case 1:
			return fmt.Sprintf("You've searched the same topic %d times. Summarize what you've found and present it to the user. Do not search again.", progressCount)
		default:
			return fmt.Sprintf("You've searched the same topic %d times with similar results. Use the results you already have or try a fundamentally different query.", progressCount)
		}
	case "browser", "gui":
		switch stage {
		case 2:
			return fmt.Sprintf("You repeated the same UI action %d times across %d browser-family calls without the page state advancing.", progressCount, familyCount)
		case 1:
			return fmt.Sprintf("You've repeated the same UI action %d times without progress. Stop clicking — summarize the current page state for the user and wait for direction.", progressCount)
		default:
			return fmt.Sprintf("You've repeated the same UI action %d times with no observable change. Try a different selector, refresh the page, or step back and reassess the plan.", progressCount)
		}
	default:
		switch stage {
		case 2:
			return fmt.Sprintf("You called tools in the same family %d times (%d on the same target) without progress.", familyCount, progressCount)
		case 1:
			return fmt.Sprintf("You've repeated the same action %d times without progress. Summarize what you have and report back to the user.", progressCount)
		default:
			return fmt.Sprintf("You've repeated the same action %d times with similar results. Change approach.", progressCount)
		}
	}
}

// isEmptyThinkInput reports whether a tool call is a `think` invocation whose
// args parse to an empty / whitespace-only `thought` field. False for any
// other tool name, for malformed JSON, and for non-empty thought values.
//
// Matches the soft-error trigger in internal/tools/think.go so the Check()
// force-stop rule fires on exactly the calls the tool itself treats as
// no-op hints. See plan 2026-05-14-thinking-blocks-alignment.md Phase 0.
func isEmptyThinkInput(name, argsJSON string) bool {
	if name != "think" {
		return false
	}
	var args struct {
		Thought string `json:"thought"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// Malformed JSON is a different failure mode (think.go returns a
		// hard error for it). Don't conflate with the empty-thought signal.
		return false
	}
	return strings.TrimSpace(args.Thought) == ""
}
