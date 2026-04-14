package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// LoopAction tells the agent loop how to respond to a detection signal.
type LoopAction int

const (
	LoopContinue  LoopAction = iota // proceed normally
	LoopNudge                       // inject "try different approach" message
	LoopForceStop                   // force final response without tools
)

// ToolCallRecord tracks a single tool invocation for loop detection.
type ToolCallRecord struct {
	Name            string
	ArgsHash        string // hex-encoded hash of raw args
	TopicHash       string // hex-encoded hash of normalized args (web tools)
	ResultSig       string // domain signature from results (web tools)
	IsError         bool
	ErrorSig        string // first 100 chars of error for grouping
	IsSleep         bool   // bash command contains sleep
	IsNonActionable bool   // search returned no useful results (no matches, binary noise, errors)
}

// LoopDetector uses a sliding window of recent tool calls to detect stuck loops.
//
// Nine detection paths (checked in order, first match wins):
//   - ToolModeSwitch: visual tool after successful GUI-adjacent tool (applescript/browser)
//   - SuccessAfterError: visual tool after error recovery
//   - ConsecutiveDuplicate: back-to-back identical calls (catches web_search→web_search)
//   - ExactDuplicate: same name+argsHash spread across window (catches read→edit→read→edit→read)
//   - SameToolError: same tool returns errors N+ times in window
//   - FamilyNoProgress: tools in the same family, counted by topic similarity
//     (3 same-topic → nudge, 5 → stronger nudge, 7 → force stop)
//     Fallback: same-tool count when topic tracking unavailable (5 → nudge, 7 → force stop)
//   - SearchEscalation: trailing unproductive search-family calls
//     (5 unproductive → nudge, 8 unproductive → force stop)
//   - NoProgress: same tool called M+ times regardless of args (skip visual/search tools,
//     semi-repeatable tools like bash get a higher threshold)
//   - Sleep: bash commands containing sleep (2 → nudge, 4 → force stop)
//
// Response escalation: threshold = nudge, threshold+1 = force stop (consecutive), 2x threshold = force stop (others).
type LoopDetector struct {
	history     []ToolCallRecord
	historySize int

	consecDupThreshold   int // back-to-back identical calls (default 2)
	exactDupThreshold    int // spread-out identical calls in window (default 3)
	sameToolErrThreshold int
	noProgressThreshold  int

	repeatableTools          map[string]bool
	semiRepeatableTools     map[string]bool // higher NoProgress threshold (e.g. bash)
	semiRepeatableThreshold int             // nudge threshold for semi-repeatable tools
	// Note: force-stop = threshold*2 = 24 exceeds historySize (20), so the
	// NoProgress force-stop is intentionally unreachable for semi-repeatable
	// tools. The nudge budget escalation (maxNudges in loop.go) is the
	// backstop that converts accumulated nudges into a force-stop.

	// ToolModeSwitch detector state
	lastNonGUISuccess bool
	lastNonGUITool    string
	modeSwitchNudged  bool

	// SuccessAfterError detector state
	recentRecovery bool
	recoveredTool  string
}

// GUITools are tools that indicate GUI automation tasks.
// Used by both LoopDetector (exempt from NoProgress) and effectiveMaxIter (higher limit).
// Note: the literal "browser" key covers the legacy in-process browser tool.
// Real MCP playwright tool names (browser_navigate, browser_snapshot, …) are
// handled via isGUIToolName, which also prefix-matches "browser_".
var GUITools = map[string]bool{
	"screenshot": true, "computer": true, "applescript": true, "browser": true, "accessibility": true,
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
	"screenshot": true, "computer": true, "accessibility": true,
}

// repeatableGUITools extends visualTools with browser — multi-step browser workflows
// (navigate → snapshot → click → type → snapshot) naturally use many calls with
// different actions, so the no-progress fallback should not over-trigger.
// Topic-based detection (FamilyNoProgress) still catches same-URL loops.
var repeatableGUITools = map[string]bool{
	"screenshot": true, "computer": true, "accessibility": true, "browser": true,
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

// NewLoopDetector creates a detector with production defaults.
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		history:                 make([]ToolCallRecord, 0, 20),
		historySize:             20,
		consecDupThreshold:      2,
		exactDupThreshold:       3,
		sameToolErrThreshold:    4,
		noProgressThreshold:     8,
		repeatableTools:         repeatableGUITools,
		semiRepeatableTools:     semiRepeatableProdTools,
		semiRepeatableThreshold: 12,
	}
}

// Record adds a tool call to the sliding window.
func (ld *LoopDetector) Record(name, argsJSON string, isError bool, errMsg string, resultSig string, isNonActionable bool) {
	topicHash := ""
	if toolFamily(name) != "" {
		normalized := normalizeWebQuery(argsJSON)
		if normalized != "" {
			topicHash = hashArgs(normalized)
		}
	}
	rec := ToolCallRecord{
		Name:            name,
		ArgsHash:        hashArgs(argsJSON),
		TopicHash:       topicHash,
		ResultSig:       resultSig,
		IsError:         isError,
		ErrorSig:        truncateErrSig(errMsg, 100),
		IsSleep:         name == "bash" && isSleepCommand(argsJSON),
		IsNonActionable: isNonActionable,
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

// Check evaluates all detectors for the named tool.
// Returns the most severe action and an appropriate message.
func (ld *LoopDetector) Check(name string) (LoopAction, string) {
	if len(ld.history) < 2 {
		return LoopContinue, ""
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
	consecCount := 0
	for i := len(ld.history) - 1; i >= 0; i-- {
		if ld.history[i].Name != name || ld.history[i].ArgsHash != latestHash {
			break
		}
		consecCount++
	}
	if consecCount >= ld.consecDupThreshold+1 {
		return LoopForceStop, fmt.Sprintf(
			"You have called %s with identical arguments %d times in a row. Stop retrying and provide your answer now.", name, consecCount)
	}
	if consecCount >= ld.consecDupThreshold {
		return LoopNudge, fmt.Sprintf(
			"You've called %s %d times consecutively with identical arguments. The results won't change. Use the results you already have or try a different approach.", name, consecCount)
	}

	// 1b. Window-based exact duplicate — catches spread-out repeats
	// like read→edit→read→edit→read (same args appearing 3+ times in window).
	dupCount := 0
	if latestHash != "" {
		for _, rec := range ld.history {
			if rec.Name == name && rec.ArgsHash == latestHash {
				dupCount++
			}
		}
	}
	if dupCount >= ld.exactDupThreshold*2 {
		return LoopForceStop, fmt.Sprintf(
			"You have called %s with identical arguments %d times. Stop retrying and provide your answer now.", name, dupCount)
	}
	if dupCount >= ld.exactDupThreshold {
		return LoopNudge, fmt.Sprintf(
			"You've called %s %d times with identical arguments and similar results. Try a fundamentally different approach.", name, dupCount)
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
		return LoopForceStop, fmt.Sprintf(
			"Tool %s has failed %d times. Stop using it and provide your answer now.", name, errCount)
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

		if progressCount >= 7 {
			return LoopForceStop, familyNoProgressMessage(family, progressCount, familyCount, 2)
		}
		if progressCount >= 5 {
			return LoopNudge, familyNoProgressMessage(family, progressCount, familyCount, 1)
		}
		if progressCount >= 3 {
			return LoopNudge, familyNoProgressMessage(family, progressCount, familyCount, 0)
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
			if sameToolInFamily >= 7 {
				return LoopForceStop, fmt.Sprintf(
					"You have called %s %d times without meaningful progress. Provide your answer now.", name, sameToolInFamily)
			}
			if sameToolInFamily >= 5 {
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
		if unproductiveStreak >= 8 {
			return LoopForceStop, fmt.Sprintf(
				"You have made %d consecutive unproductive search calls. Stop searching and use what you have, or ask the user for guidance.", unproductiveStreak)
		}
		if unproductiveStreak >= 5 {
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
	if !isRepeatableToolName(ld.repeatableTools, name) && family != "search" {
		count := 0
		for _, rec := range ld.history {
			if rec.Name == name {
				count++
			}
		}
		threshold := ld.noProgressThreshold
		if ld.semiRepeatableTools[name] {
			threshold = ld.semiRepeatableThreshold
		}
		if count >= threshold*2 {
			return LoopForceStop, fmt.Sprintf(
				"You have called %s %d times without meaningful progress. Provide your answer now.", name, count)
		}
		if count >= threshold {
			return LoopNudge, fmt.Sprintf(
				"You've called %s %d times. Summarize what you've learned and try a different approach.", name, count)
		}
	}

	// 6. Sleep detector: bash commands containing sleep indicate polling/waiting
	sleepCount := 0
	for _, rec := range ld.history {
		if rec.IsSleep {
			sleepCount++
		}
	}
	if sleepCount >= 4 {
		return LoopForceStop, fmt.Sprintf(
			"You have used `sleep` in bash commands %d times. Stop polling and provide your answer now.", sleepCount)
	}
	if sleepCount >= 2 {
		return LoopNudge, fmt.Sprintf(
			"You've used `sleep` in bash commands %d times. Do not poll or wait in loops — diagnose the root cause, use a check command, or ask the user.", sleepCount)
	}

	return LoopContinue, ""
}

func hashArgs(args string) string {
	h := sha256.Sum256([]byte(args))
	return hex.EncodeToString(h[:8])
}

// sleepPattern matches `sleep` followed by a number, as a word boundary.
// Catches: "sleep 5", "sleep 1 && curl ...", "while true; do sleep 1; done"
// Avoids: "sleep.log", "sleeper"
var sleepPattern = regexp.MustCompile(`\bsleep\s+\d`)

// isSleepCommand checks whether a bash tool's JSON args contain a sleep command.
func isSleepCommand(argsJSON string) bool {
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return false
	}
	return sleepPattern.MatchString(args.Command)
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
			return fmt.Sprintf("You have made %d web calls with %d on the same topic. Return your collected results now.", familyCount, progressCount)
		case 1:
			return fmt.Sprintf("You've searched the same topic %d times. Summarize what you've found and present it to the user. Do not search again.", progressCount)
		default:
			return fmt.Sprintf("You've searched the same topic %d times with similar results. Use the results you already have or try a fundamentally different query.", progressCount)
		}
	case "browser", "gui":
		switch stage {
		case 2:
			return fmt.Sprintf("You have repeated the same UI action %d times across %d browser-family calls without the page state advancing. Report the current state to the user now.", progressCount, familyCount)
		case 1:
			return fmt.Sprintf("You've repeated the same UI action %d times without progress. Stop clicking — summarize the current page state for the user and wait for direction.", progressCount)
		default:
			return fmt.Sprintf("You've repeated the same UI action %d times with no observable change. Try a different selector, refresh the page, or step back and reassess the plan.", progressCount)
		}
	default:
		switch stage {
		case 2:
			return fmt.Sprintf("You have called tools in the same family %d times (%d on the same target) without progress. Provide your answer now.", familyCount, progressCount)
		case 1:
			return fmt.Sprintf("You've repeated the same action %d times without progress. Summarize what you have and report back to the user.", progressCount)
		default:
			return fmt.Sprintf("You've repeated the same action %d times with similar results. Change approach.", progressCount)
		}
	}
}
