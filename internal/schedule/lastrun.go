package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LastRunSummary is the resolved view of a schedule's most recent run.
// Returned by SummarizeLastRun; consumed by tools/schedule_show and the
// HTTP /schedules/{id}/last-run endpoint.
type LastRunSummary struct {
	LastRunAt *time.Time    `json:"last_run_at,omitempty"`
	SessionID string        `json:"session_id"`
	AgentName string        `json:"agent_name"`
	Turns     []TurnSummary `json:"turns"`
}

// TurnSummary is one assistant turn from the resolved session, with text
// extracted from whatever content shape the message used (string or
// content-block array). Tool-use / tool-result blocks are skipped.
type TurnSummary struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

const lastRunTurnRuneCap = 800 // each assistant turn truncated to keep tool output bounded

// SummarizeLastRun loads the session file referenced by sched.LastRunSessionID,
// slices it to sched.LastRunMessage{Start,End}Index, and returns up to
// maxTurns assistant turns from the tail of that slice. Returns an empty
// summary (no error) when the schedule has never run.
//
// Why slice by index range: a sticky schedule's session accumulates many
// runs, so its tail could mix this run with earlier ones. The index range
// isolates exactly the turns this run wrote. (For a fresh run the session
// holds only this run, so the range spans the whole session.) Both scopes
// use dedicated sessions — never the user's interactive chat — so the slice
// can no longer pick up an unrelated chat reply.
//
// Legacy fallback: when both Start and End are 0 (e.g. row was stamped
// before the index fields existed), we fall back to "show the whole
// session's tail." Fine for a fresh session; for a legacy accumulating
// session it may span runs, but it's better than returning empty.
//
// Cross-file invariant: the (0,0) sentinel is safe only because
// runner.go pre-appends a user message for every req.Source != "" run
// (including schedule), so turnBase.msgCount is captured AFTER the
// append and is therefore >= 1 when MessageStartIndex is stamped. If a
// future refactor lets a real schedule run produce MessageStartIndex == 0,
// this fallback silently regresses to scanning the whole session tail.
// Touch points to keep aligned: internal/daemon/runner.go pre-loop user
// append (look for `preLoopUserAppended`) and `captureTurnBaseline`.
//
// shannonDir is the root (~/.shannon) — we resolve the per-agent or default
// sessions directory under it.
func SummarizeLastRun(sched Schedule, shannonDir string, maxTurns int) (LastRunSummary, error) {
	out := LastRunSummary{
		SessionID: sched.LastRunSessionID,
		AgentName: sched.Agent,
		LastRunAt: sched.LastRunAt,
		// Always non-nil so JSON marshals as [] not null. Swift Codable on
		// the Desktop bridge declares the field as [ScheduleLastRunTurn]
		// (non-optional), which would fail to decode on null.
		Turns: []TurnSummary{},
	}
	if sched.LastRunSessionID == "" {
		return out, nil
	}
	if maxTurns <= 0 {
		maxTurns = 5
	}
	if maxTurns > 20 {
		maxTurns = 20
	}

	sessDir := filepath.Join(shannonDir, "sessions")
	if sched.Agent != "" {
		sessDir = filepath.Join(shannonDir, "agents", sched.Agent, "sessions")
	}
	path := filepath.Join(sessDir, sched.LastRunSessionID+".json")

	data, err := os.ReadFile(path)
	if err != nil {
		// The recorded last-run session was deleted (e.g. the user removed it
		// from the chat list). Treat it as "no last run to show" — the same
		// empty shape returned when a schedule has never run — instead of
		// surfacing an error. The schedule itself is unaffected: its next run
		// recreates a session and re-points LastRunSessionID. Genuine read
		// errors (permissions, I/O) still propagate.
		if os.IsNotExist(err) {
			// Return the exact never-ran shape (no id, no timestamp, empty turns)
			// so every consumer — all of which key off an empty SessionID —
			// renders the same neutral state.
			out.SessionID = ""
			out.LastRunAt = nil
			return out, nil
		}
		return out, fmt.Errorf("session file %s: %w", sched.LastRunSessionID, err)
	}

	var raw struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return out, fmt.Errorf("parse session %s: %w", sched.LastRunSessionID, err)
	}

	startIdx, endIdx := sched.LastRunMessageStartIndex, sched.LastRunMessageEndIndex
	if startIdx == 0 && endIdx == 0 {
		startIdx, endIdx = 0, len(raw.Messages)
	}
	if startIdx < 0 {
		startIdx = 0
	}
	if endIdx > len(raw.Messages) {
		endIdx = len(raw.Messages)
	}
	if startIdx >= endIdx {
		return out, nil
	}
	slice := raw.Messages[startIdx:endIdx]

	var allAssistant []TurnSummary
	for _, m := range slice {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		if msg.Role != "assistant" {
			continue
		}
		text := extractAssistantText(msg.Content)
		if text == "" {
			continue
		}
		if utf8RuneCount(text) > lastRunTurnRuneCap {
			text = truncRunes(text, lastRunTurnRuneCap) + " …"
		}
		allAssistant = append(allAssistant, TurnSummary{Role: msg.Role, Text: text})
	}

	if len(allAssistant) > maxTurns {
		allAssistant = allAssistant[len(allAssistant)-maxTurns:]
	}
	if allAssistant != nil {
		// Preserve the pre-initialized empty slice when no assistant turns
		// were extracted, so JSON stays "turns":[] (see init above).
		out.Turns = allAssistant
	}
	return out, nil
}

func extractAssistantText(c json.RawMessage) string {
	var s string
	if err := json.Unmarshal(c, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, strings.TrimSpace(b.Text))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
