package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// --- cache-cost model -----------------------------------------------------
//
// A provider matches the request prefix against its cache and pays cache WRITE
// for everything from the first divergent byte to the end. So the cost of a
// history rewrite is NOT the size of the edit — it is the size of the suffix
// behind it.
//
// turnCacheWriteBytes reproduces that: it hashes each message the way
// gateway.go's logCacheDebug does (per-message, in order), finds the first
// index whose bytes changed since the previous turn, and charges every byte
// from there to the end. Appended messages at the tail are charged too — those
// are unavoidable and are what a healthy turn pays.

func messageBytes(t *testing.T, m client.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return b
}

func snapshotTurn(t *testing.T, msgs []client.Message) [][]byte {
	t.Helper()
	out := make([][]byte, len(msgs))
	for i, m := range msgs {
		out[i] = messageBytes(t, m)
	}
	return out
}

// turnCacheWriteBytes returns the bytes this turn must re-write into the cache,
// and whether the rewrite started inside the shared prefix (a "break") rather
// than at the tail.
func turnCacheWriteBytes(prev, cur [][]byte) (writeBytes int, brokePrefix bool) {
	firstDiff := len(prev)
	for i := 0; i < len(prev) && i < len(cur); i++ {
		if string(prev[i]) != string(cur[i]) {
			firstDiff = i
			brokePrefix = true
			break
		}
	}
	for i := firstDiff; i < len(cur); i++ {
		writeBytes += len(cur[i])
	}
	return writeBytes, brokePrefix
}

// --- simulated browser session -------------------------------------------

const (
	simTurns   = 20
	simObsCap  = defaultBrowserObservationMaxChars // 24000 runes, the capture cap
	simRawPage = 57000                             // a raw page snapshot, larger than the cap
)

// appendBrowserTurn adds one iteration's worth of history: the assistant's
// tool_use plus the user's tool_result carrying a capture-capped observation.
// Mirrors what the real loop accumulates (loop.go applies truncateObservation
// at capture, before any windowing).
func appendBrowserTurn(msgs []client.Message, i int) []client.Message {
	toolID := fmt.Sprintf("tc%02d", i)
	observation := truncateObservation(
		strings.Repeat(fmt.Sprintf("page-%02d ", i), simRawPage/8),
		simObsCap,
	)
	return append(msgs,
		client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "tool_use", ID: toolID, Name: "browser_snapshot", Input: json.RawMessage(`{"x":1}`)},
			}),
		},
		client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(toolID, observation, false),
			}),
		},
	)
}

type simResult struct {
	mode            ObservationTriggerMode
	breaks          int
	cacheWriteBytes int
	finalCtxBytes   int
	fullObsAtEnd    int
}

// runSession replays a simTurns-iteration browser loop under one trigger mode,
// calling the SAME applyObservationWindow the agent loop calls.
//
// lastAssistantAt is held recent so the cache is warm throughout — an active
// browser task, which is the workload where the old policy bled.
func runSession(t *testing.T, mode ObservationTriggerMode) simResult {
	t.Helper()

	cfg := ObservationWindowConfig{Mode: mode}
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}

	var prev [][]byte
	res := simResult{mode: mode}

	for turn := 0; turn < simTurns; turn++ {
		msgs = appendBrowserTurn(msgs, turn)

		// Warm cache: the model answered seconds ago, as in a live loop.
		lastAssistantAt := time.Now().Add(-2 * time.Second)
		applyObservationWindow(msgs, defaultObservationWindow, lastAssistantAt, cfg)

		cur := snapshotTurn(t, msgs)
		if prev != nil {
			wb, broke := turnCacheWriteBytes(prev, cur)
			res.cacheWriteBytes += wb
			if broke {
				res.breaks++
			}
		}
		prev = cur
	}

	for _, b := range prev {
		res.finalCtxBytes += len(b)
	}
	for turn := 0; turn < simTurns; turn++ {
		content := browserResultContent(t, msgs, fmt.Sprintf("tc%02d", turn))
		if !isObservationStubContent(content) {
			res.fullObsAtEnd++
		}
	}
	return res
}

// TestCompareObservationTriggerModes is the measurement, not an assertion of
// taste: it replays the same 20-iteration browser session under every trigger
// policy and reports what each costs in prompt-cache writes.
func TestCompareObservationTriggerModes(t *testing.T) {
	modes := []ObservationTriggerMode{
		ObservationTriggerEveryTurn,
		ObservationTriggerBudget,
		ObservationTriggerHybrid,
		ObservationTriggerColdCache,
		ObservationTriggerOff,
	}

	results := make(map[ObservationTriggerMode]simResult, len(modes))
	t.Logf("20-iteration browser session, warm cache throughout, keep=%d, capture cap=%d runes",
		defaultObservationWindow, simObsCap)
	t.Logf("%-12s %8s %14s %14s %10s", "mode", "breaks", "cacheWriteKB", "finalCtxKB", "fullObs")
	for _, m := range modes {
		r := runSession(t, m)
		results[m] = r
		t.Logf("%-12s %8d %14.1f %14.1f %10d",
			m, r.breaks,
			float64(r.cacheWriteBytes)/1024,
			float64(r.finalCtxBytes)/1024,
			r.fullObsAtEnd)
	}

	every := results[ObservationTriggerEveryTurn]
	hybrid := results[ObservationTriggerHybrid]
	budget := results[ObservationTriggerBudget]
	off := results[ObservationTriggerOff]

	// The finding this whole change rests on: per-turn evaluation breaks the
	// prefix on nearly every iteration.
	if every.breaks < simTurns-defaultObservationWindow-1 {
		t.Fatalf("every_turn broke the prefix only %d times in %d turns; the "+
			"per-turn drift this change targets is not being reproduced",
			every.breaks, simTurns)
	}

	// Off never rewrites history, so it is the floor for cache writes and the
	// ceiling for context. It is the control, not a candidate.
	if off.breaks != 0 {
		t.Fatalf("off mode broke the prefix %d times; it must never rewrite history", off.breaks)
	}

	// Budget and hybrid must both cut breaks by an order of magnitude.
	for _, r := range []simResult{budget, hybrid} {
		if r.breaks >= every.breaks/2 {
			t.Fatalf("%s broke %d times vs every_turn's %d; expected far fewer",
				r.mode, r.breaks, every.breaks)
		}
		if r.cacheWriteBytes >= every.cacheWriteBytes {
			t.Fatalf("%s wrote %d cache bytes vs every_turn's %d; expected fewer",
				r.mode, r.cacheWriteBytes, every.cacheWriteBytes)
		}
	}

	// Bounded growth is the reason budget/hybrid exist rather than just off:
	// they must still hold the final context well under the unclipped size.
	if hybrid.finalCtxBytes >= off.finalCtxBytes {
		t.Fatalf("hybrid final context %d is not smaller than off's %d; the "+
			"aggregate backstop is not binding",
			hybrid.finalCtxBytes, off.finalCtxBytes)
	}

	t.Logf("every_turn -> hybrid: breaks %d -> %d, cache writes %.1fKB -> %.1fKB (%.1fx less)",
		every.breaks, hybrid.breaks,
		float64(every.cacheWriteBytes)/1024, float64(hybrid.cacheWriteBytes)/1024,
		float64(every.cacheWriteBytes)/float64(hybrid.cacheWriteBytes))
}

// TestEveryTurnBreaksPrefixOnEachIteration isolates the regression this change
// prevents: under the old policy the divergence point advances every turn.
func TestEveryTurnBreaksPrefixOnEachIteration(t *testing.T) {
	cfg := ObservationWindowConfig{Mode: ObservationTriggerEveryTurn}
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}

	var prev [][]byte
	brokenTurns := 0
	for turn := 0; turn < simTurns; turn++ {
		msgs = appendBrowserTurn(msgs, turn)
		applyObservationWindow(msgs, defaultObservationWindow, time.Now(), cfg)
		cur := snapshotTurn(t, msgs)
		if prev != nil {
			if _, broke := turnCacheWriteBytes(prev, cur); broke {
				brokenTurns++
			}
		}
		prev = cur
	}

	// Turns 1..keep produce no stub yet; every turn after that does.
	wantAtLeast := simTurns - defaultObservationWindow - 1
	if brokenTurns < wantAtLeast {
		t.Fatalf("every_turn broke the prefix on %d turns, want >= %d", brokenTurns, wantAtLeast)
	}
}

// --- trigger predicate contracts ------------------------------------------

func TestObservationWindow_WarmCacheUnderBudgetIsNoOp(t *testing.T) {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	// Two observations: well under the aggregate cap and inside the window.
	msgs = appendBrowserTurn(msgs, 0)
	msgs = appendBrowserTurn(msgs, 1)
	before, _ := json.Marshal(msgs)

	n := applyObservationWindow(msgs, defaultObservationWindow, time.Now(),
		ObservationWindowConfig{Mode: ObservationTriggerHybrid})

	after, _ := json.Marshal(msgs)
	if n != 0 || string(before) != string(after) {
		t.Fatalf("warm cache under budget must not touch a byte (stubbed=%d)", n)
	}
}

func TestObservationWindow_ColdCacheClearsForFree(t *testing.T) {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 6; i++ {
		msgs = appendBrowserTurn(msgs, i)
	}

	// Two hours idle: past any provider prompt-cache TTL, so the prefix is
	// being rewritten regardless and clearing costs nothing extra.
	n := applyObservationWindow(msgs, defaultObservationWindow,
		time.Now().Add(-2*time.Hour),
		ObservationWindowConfig{Mode: ObservationTriggerColdCache})

	if want := 6 - defaultObservationWindow; n != want {
		t.Fatalf("cold cache stubbed %d observations, want %d", n, want)
	}
}

func TestObservationWindow_ColdCacheModeHoldsWhileWarm(t *testing.T) {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 6; i++ {
		msgs = appendBrowserTurn(msgs, i)
	}

	n := applyObservationWindow(msgs, defaultObservationWindow, time.Now(),
		ObservationWindowConfig{Mode: ObservationTriggerColdCache})

	if n != 0 {
		t.Fatalf("cold_cache mode stubbed %d observations while the cache was warm", n)
	}
}

func TestObservationWindow_BudgetFiresOnAggregateNotCount(t *testing.T) {
	// Count alone must not fire: four observations exceed keep=3 but a small
	// aggregate cap is what decides.
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 4; i++ {
		msgs = appendBrowserTurn(msgs, i)
	}

	huge := ObservationWindowConfig{
		Mode:              ObservationTriggerBudget,
		AggregateCapRunes: 10_000_000,
	}
	if n := applyObservationWindow(msgs, defaultObservationWindow, time.Now(), huge); n != 0 {
		t.Fatalf("budget mode stubbed %d observations while far under cap", n)
	}

	tight := ObservationWindowConfig{
		Mode:              ObservationTriggerBudget,
		AggregateCapRunes: 1_000,
	}
	if n := applyObservationWindow(msgs, defaultObservationWindow, time.Now(), tight); n != 1 {
		t.Fatalf("budget mode over cap stubbed %d observations, want 1", n)
	}
}

func TestObservationWindow_AggregateExcludesAlreadyStubbed(t *testing.T) {
	// Without this the trigger stays latched forever: reclaimed bytes would
	// keep counting toward the cap they were meant to relieve.
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 6; i++ {
		msgs = appendBrowserTurn(msgs, i)
	}

	full := observationAggregateRunes(msgs)
	filterOldObservations(msgs, defaultObservationWindow)
	afterStub := observationAggregateRunes(msgs)

	if afterStub >= full {
		t.Fatalf("aggregate after stubbing is %d, not below the original %d", afterStub, full)
	}
	if afterStub > defaultObservationWindow*simObsCap {
		t.Fatalf("aggregate %d exceeds the %d retained full observations", afterStub, defaultObservationWindow)
	}
}

func TestObservationWindow_OffNeverClears(t *testing.T) {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 10; i++ {
		msgs = appendBrowserTurn(msgs, i)
	}
	before, _ := json.Marshal(msgs)

	n := applyObservationWindow(msgs, defaultObservationWindow,
		time.Now().Add(-99*time.Hour),
		ObservationWindowConfig{Mode: ObservationTriggerOff})

	after, _ := json.Marshal(msgs)
	if n != 0 || string(before) != string(after) {
		t.Fatalf("off mode must never clear (stubbed=%d)", n)
	}
}

func TestObservationWindow_DisabledWindowWinsOverAnyMode(t *testing.T) {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 8; i++ {
		msgs = appendBrowserTurn(msgs, i)
	}
	before, _ := json.Marshal(msgs)

	for _, mode := range []ObservationTriggerMode{
		ObservationTriggerEveryTurn, ObservationTriggerColdCache,
		ObservationTriggerBudget, ObservationTriggerHybrid,
	} {
		if n := applyObservationWindow(msgs, 0, time.Now().Add(-99*time.Hour),
			ObservationWindowConfig{Mode: mode}); n != 0 {
			t.Fatalf("keep=0 must disable the window, but %s stubbed %d", mode, n)
		}
	}
	after, _ := json.Marshal(msgs)
	if string(before) != string(after) {
		t.Fatal("keep=0 changed bytes")
	}
}

func TestObservationWindow_ZeroValueConfigResolvesToDefaults(t *testing.T) {
	// Callers that set only Mode (or nothing) must land on the documented
	// defaults rather than a zero cap that fires on every byte.
	var zero ObservationWindowConfig
	def := DefaultObservationWindowConfig()

	if zero.mode() != def.Mode {
		t.Fatalf("zero mode resolved to %q, want %q", zero.mode(), def.Mode)
	}
	// The DEFAULT cap is raised to the headroom floor so it stays coherent as
	// keep is tuned; an explicit cap is honored verbatim.
	wantDefault := (defaultObservationWindow + minObservationHeadroom) * defaultBrowserObservationMaxChars
	if got := zero.aggregateCapFor(defaultObservationWindow); got != wantDefault {
		t.Fatalf("default cap at keep=%d resolved to %d, want the headroom floor %d",
			defaultObservationWindow, got, wantDefault)
	}
	if def.AggregateCapRunes != 0 {
		t.Fatalf("constructor default aggregate cap = %d, want unresolved zero sentinel", def.AggregateCapRunes)
	}
	if got := def.aggregateCapFor(defaultObservationWindow); got != wantDefault {
		t.Fatalf("constructor default cap at keep=%d resolved to %d, want %d",
			defaultObservationWindow, got, wantDefault)
	}
	explicit := ObservationWindowConfig{AggregateCapRunes: 1_000}
	if got := explicit.aggregateCapFor(defaultObservationWindow); got != 1_000 {
		t.Fatalf("explicit cap was overridden to %d; an operator's value must stand", got)
	}
	if zero.coldCacheGap() != defaultObservationColdCacheGapMinutes*time.Minute {
		t.Fatalf("zero cold-cache gap resolved to %v", zero.coldCacheGap())
	}
}

func TestObservationWindow_NestedImageResultCountsTowardBudgetAndKeepsImage(t *testing.T) {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < 4; i++ {
		toolID := fmt.Sprintf("img%02d", i)
		text := strings.Repeat(fmt.Sprintf("page-%02d ", i), 100)
		msgs = append(msgs,
			client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{{
				Type: "tool_use", ID: toolID, Name: "browser_snapshot", Input: json.RawMessage(`{}`),
			}})},
			client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlockWithImages(toolID, text, []client.ContentBlock{{
					Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="},
				}}, false),
			})},
		)
	}

	if got := filterOldBrowserImages(msgs, 1); got != 3 {
		t.Fatalf("aged out %d nested images, want 3", got)
	}
	if got := observationAggregateRunes(msgs); got == 0 {
		t.Fatal("nested text+image observations did not count toward the aggregate budget")
	}
	cfg := ObservationWindowConfig{Mode: ObservationTriggerBudget, AggregateCapRunes: 1}
	if got := applyObservationWindow(msgs, 1, time.Now(), cfg); got != 3 {
		t.Fatalf("stubbed %d nested observations, want 3", got)
	}
	if got := toolResultImageCount(msgs); got != 1 {
		t.Fatalf("image aging plus observation clearing left %d images, want 1", got)
	}
	for i := 0; i < 4; i++ {
		text := browserResultContent(t, msgs, fmt.Sprintf("img%02d", i))
		if i < 3 && !isObservationStubContent(text) {
			t.Fatalf("nested observation %d was not stubbed: %q", i, text)
		}
		if i == 3 && isObservationStubContent(text) {
			t.Fatal("most recent nested observation was stubbed")
		}
	}
}

func TestObservationWindow_NoAssistantYetTreatedAsCold(t *testing.T) {
	// A zero lastAssistantAt means nothing has been cached to protect.
	cfg := ObservationWindowConfig{Mode: ObservationTriggerColdCache}
	if !observationCacheIsCold(time.Time{}, cfg) {
		t.Fatal("zero lastAssistantAt must count as cold")
	}
}

func TestValidObservationTriggerMode(t *testing.T) {
	for _, ok := range []string{"every_turn", "cold_cache", "budget", "hybrid", "off"} {
		if !ValidObservationTriggerMode(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "EVERY_TURN", "always", "cold", "budgeted"} {
		if ValidObservationTriggerMode(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

// TestObservationWindow_RetainedContentIdenticalAcrossModes proves the change
// is a scheduling change only: whatever a mode does clear, it clears to the
// same bytes the old policy produced.
func TestObservationWindow_RetainedContentIdenticalAcrossModes(t *testing.T) {
	build := func() []client.Message {
		msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
		for i := 0; i < 8; i++ {
			msgs = appendBrowserTurn(msgs, i)
		}
		return msgs
	}

	legacy := build()
	filterOldObservations(legacy, defaultObservationWindow)
	want, _ := json.Marshal(legacy)

	// Cold cache fires the same pass; the resulting bytes must match exactly.
	cold := build()
	applyObservationWindow(cold, defaultObservationWindow,
		time.Now().Add(-2*time.Hour),
		ObservationWindowConfig{Mode: ObservationTriggerColdCache})
	got, _ := json.Marshal(cold)

	if string(want) != string(got) {
		t.Fatal("cold_cache produced different retained bytes than the legacy pass")
	}
}

// TestKeepSensitivitySweep measures what the retained-observation count
// actually buys. The count window is the SECOND bound on observation bytes —
// each observation is already capped at capture (browserObsMaxChars), so the
// worst case is keep x cap regardless. This reports whether the count is
// carrying its weight or just adding a knob.
func TestKeepSensitivitySweep(t *testing.T) {
	modes := []ObservationTriggerMode{ObservationTriggerEveryTurn, ObservationTriggerHybrid}
	keeps := []int{1, 3, 5, 8}

	t.Logf("20-iteration browser session, warm cache, capture cap=%d runes", simObsCap)
	t.Logf("%-11s %5s %8s %14s %13s %9s", "mode", "keep", "breaks", "cacheWriteKB", "finalCtxKB", "fullObs")
	for _, mode := range modes {
		for _, keep := range keeps {
			r := runSessionWithKeep(t, mode, keep)
			t.Logf("%-11s %5d %8d %14.1f %13.1f %9d",
				mode, keep, r.breaks,
				float64(r.cacheWriteBytes)/1024,
				float64(r.finalCtxBytes)/1024,
				r.fullObsAtEnd)
		}
	}
}

// runSessionWithKeep is runSession with the window size as a parameter.
func runSessionWithKeep(t *testing.T, mode ObservationTriggerMode, keep int) simResult {
	t.Helper()

	cfg := ObservationWindowConfig{Mode: mode}
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}

	var prev [][]byte
	res := simResult{mode: mode}
	for turn := 0; turn < simTurns; turn++ {
		msgs = appendBrowserTurn(msgs, turn)
		applyObservationWindow(msgs, keep, time.Now().Add(-2*time.Second), cfg)
		cur := snapshotTurn(t, msgs)
		if prev != nil {
			wb, broke := turnCacheWriteBytes(prev, cur)
			res.cacheWriteBytes += wb
			if broke {
				res.breaks++
			}
		}
		prev = cur
	}
	for _, b := range prev {
		res.finalCtxBytes += len(b)
	}
	for turn := 0; turn < simTurns; turn++ {
		if !isObservationStubContent(browserResultContent(t, msgs, fmt.Sprintf("tc%02d", turn))) {
			res.fullObsAtEnd++
		}
	}
	return res
}
