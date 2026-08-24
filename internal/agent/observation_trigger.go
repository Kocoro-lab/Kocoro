package agent

import (
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// Observation-window trigger policy.
//
// filterOldObservations bounds how much browser/GUI page text the loop
// re-sends every iteration. WHAT it keeps (the most recent Keep observations
// at full fidelity) is not in question — WHEN it runs is.
//
// A sliding window evaluated every iteration rewrites history mid-prefix on
// every turn: each new observation pushes one older observation into the stub
// set, so the first divergent byte moves forward one slot per turn and the
// provider re-writes the whole suffix behind it. Prompt-cache write is priced
// ABOVE fresh input (Luna 1.25x vs 1.0x) and 12.5x above a cache read, so the
// bytes that should have cost 0.1x cost 1.25x instead.
//
// The trigger modes below are the candidate policies. They differ only in
// when the same clearing pass fires.
type ObservationTriggerMode string

const (
	// ObservationTriggerEveryTurn evaluates the window on every iteration.
	// This is the historical behavior, kept so its cost stays measurable
	// against the alternatives rather than only describable.
	ObservationTriggerEveryTurn ObservationTriggerMode = "every_turn"

	// ObservationTriggerColdCache fires only once the gap since the last
	// assistant response proves the provider's prompt cache has expired.
	// Clearing a cold prefix costs nothing extra because that prefix was
	// going to be rewritten regardless. Mirrors the discipline in
	// timebasedcompact.go.
	ObservationTriggerColdCache ObservationTriggerMode = "cold_cache"

	// ObservationTriggerBudget fires when accumulated observation text
	// crosses an aggregate cap, then clears in one batch. Trades many small
	// suffix rewrites for one large one.
	ObservationTriggerBudget ObservationTriggerMode = "budget"

	// ObservationTriggerHybrid fires on either signal: a cold cache (free)
	// or the aggregate budget (bounded growth backstop).
	ObservationTriggerHybrid ObservationTriggerMode = "hybrid"

	// ObservationTriggerOff never clears. Observations stay bounded only by
	// the per-observation capture cap (browserObsMaxChars), with context
	// pressure left to compaction.
	ObservationTriggerOff ObservationTriggerMode = "off"
)

// defaultObservationAggregateCapRunes is the accumulated browser-observation
// budget that fires budget/hybrid mode.
//
//   - Workload: a long browser/GUI loop that snapshots after each action.
//     Each observation is already capped at defaultBrowserObservationMaxChars
//     (24000 runes), so this admits roughly 5 full-fidelity observations
//     before batching.
//   - Symptom when it binds: several observations become stubs in one turn
//     (one cache break) instead of one per turn (a break every turn).
//   - Override: agent.observation_window_trigger.aggregate_cap_runes.
const defaultObservationAggregateCapRunes = 120_000

// minObservationHeadroom is how many further observations must fit between a
// clearing pass and the next one. It exists because the aggregate cap and the
// retained count interact: right after a pass the aggregate is already up to
// keep x browserObsMaxChars, so a cap near that product leaves no gap and the
// trigger fires again on the very next observation — silently degenerating
// into the per-turn behavior this policy exists to remove. Measured at
// keep=5 with a 120K cap: 15 breaks in 20 turns, byte-identical to every_turn.
//
// Same shape as the compaction trigger/landing hysteresis band.
//
//   - Workload: any operator who raises agent.observation_window above the
//     default 3, which the config explicitly permits.
//   - Symptom when it binds: the effective cap is larger than configured, so
//     more observation text accumulates before a batch (~4 observations
//     between passes at any keep).
//   - Override: none directly; raise aggregate_cap_runes past the floor to
//     widen the band further.
const minObservationHeadroom = 4

// defaultObservationColdCacheGapMinutes is the idle gap after which the
// prompt cache is treated as expired for cold_cache/hybrid mode.
//
//   - Workload: an interactive session the user steps away from. Provider
//     prompt-cache TTLs top out at one hour, so a gap at or past this value
//     means the prefix is refetched from scratch either way.
//   - Symptom when it binds: a long-idle session sheds observation bytes on
//     its first turn back, with no cache penalty.
//   - Override: agent.observation_window.cold_cache_gap_minutes.
//
// Deliberately NOT read from TimeBasedCompactConfig.GapThresholdMinutes: that
// config gates whether tool_result CLEARING is enabled at all, a separate
// operator decision from how the observation window is scheduled.
const defaultObservationColdCacheGapMinutes = 60

// ObservationWindowConfig selects the observation-window trigger policy.
type ObservationWindowConfig struct {
	// Mode selects the trigger. Empty resolves to the package default.
	Mode ObservationTriggerMode
	// AggregateCapRunes is the budget/hybrid trigger threshold. Non-positive
	// resolves to defaultObservationAggregateCapRunes.
	AggregateCapRunes int
	// ColdCacheGapMinutes is the cold_cache/hybrid idle threshold.
	// Non-positive resolves to defaultObservationColdCacheGapMinutes.
	ColdCacheGapMinutes int
}

// DefaultObservationWindowConfig is hybrid: take the free clearing
// opportunity when the cache is cold, and keep an aggregate backstop so an
// uninterrupted browser loop cannot grow without bound.
func DefaultObservationWindowConfig() ObservationWindowConfig {
	return ObservationWindowConfig{
		Mode:                ObservationTriggerHybrid,
		ColdCacheGapMinutes: defaultObservationColdCacheGapMinutes,
	}
}

func (c ObservationWindowConfig) mode() ObservationTriggerMode {
	if c.Mode == "" {
		return DefaultObservationWindowConfig().Mode
	}
	return c.Mode
}

// aggregateCapFor resolves the trigger threshold for a given window size.
// The floor keeps minObservationHeadroom observations' worth of room above
// what a pass leaves behind, so the trigger cannot re-fire immediately at a
// larger keep.
func (c ObservationWindowConfig) aggregateCapFor(keep, observationMaxRunes int) int {
	// An explicitly configured cap is honored verbatim, floor included. An
	// operator who sets a deliberately tight value is choosing frequent
	// clearing with its cache cost; silently widening it would be the same
	// "resolved to a value you did not choose" failure the mode validation
	// exists to prevent. The headroom floor protects only the DEFAULT, which
	// nobody chose and which must stay coherent as keep is tuned.
	if c.AggregateCapRunes > 0 {
		return c.AggregateCapRunes
	}
	cap := defaultObservationAggregateCapRunes
	if observationMaxRunes <= 0 {
		observationMaxRunes = defaultBrowserObservationMaxChars
	}
	if keep > 0 {
		if floor := (keep + minObservationHeadroom) * observationMaxRunes; floor > cap {
			cap = floor
		}
	}
	return cap
}

func (c ObservationWindowConfig) coldCacheGap() time.Duration {
	mins := c.ColdCacheGapMinutes
	if mins <= 0 {
		mins = defaultObservationColdCacheGapMinutes
	}
	return time.Duration(mins) * time.Minute
}

// ValidObservationTriggerMode reports whether s names a known mode. Used by
// config validation so a typo fails loudly at startup instead of silently
// resolving to a default the operator did not choose.
func ValidObservationTriggerMode(s string) bool {
	switch ObservationTriggerMode(s) {
	case ObservationTriggerEveryTurn,
		ObservationTriggerColdCache,
		ObservationTriggerBudget,
		ObservationTriggerHybrid,
		ObservationTriggerOff:
		return true
	}
	return false
}

// observationAggregateRunes sums the rune length of every full-fidelity
// (non-stub) browser/GUI text observation currently in history. Already
// stubbed observations are excluded: they are what a previous pass already
// reclaimed, so counting them would keep the trigger latched forever.
func observationAggregateRunes(messages []client.Message) int {
	ids, _ := collectObservationToolUseIDs(messages)
	if len(ids) == 0 {
		return 0
	}
	obsIDs := make(map[string]bool, len(ids))
	for _, id := range ids {
		obsIDs[id] = true
	}

	total := 0
	for _, m := range messages {
		if m.Role != "user" || !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type != "tool_result" || !obsIDs[b.ToolUseID] {
				continue
			}
			s, ok := observationToolResultText(b)
			if !ok || isObservationStubContent(s) {
				continue
			}
			total += utf8.RuneCountInString(s)
		}
	}
	return total
}

// observationWindowShouldFire decides whether the clearing pass runs this
// iteration, and returns the reason for observability.
func observationWindowShouldFire(
	messages []client.Message,
	keep int,
	lastAssistantAt time.Time,
	observationMaxRunes int,
	cfg ObservationWindowConfig,
) (reason string, fire bool) {
	switch cfg.mode() {
	case ObservationTriggerOff:
		return "off", false

	case ObservationTriggerEveryTurn:
		return "every_turn", true

	case ObservationTriggerColdCache:
		if observationCacheIsCold(lastAssistantAt, cfg) {
			return "cold_cache", true
		}
		return "warm_cache", false

	case ObservationTriggerBudget:
		if observationAggregateRunes(messages) > cfg.aggregateCapFor(keep, observationMaxRunes) {
			return "budget", true
		}
		return "under_budget", false

	case ObservationTriggerHybrid:
		if observationCacheIsCold(lastAssistantAt, cfg) {
			return "cold_cache", true
		}
		if observationAggregateRunes(messages) > cfg.aggregateCapFor(keep, observationMaxRunes) {
			return "budget", true
		}
		return "warm_under_budget", false
	}
	// Unreachable for validated config; treated as the default policy rather
	// than silently disabling the window.
	return "unknown_mode", true
}

// observationCacheIsCold reports whether enough wall-clock has passed since
// the last assistant response that the provider prompt cache has expired.
//
// A zero lastAssistantAt means no assistant response has landed in this loop
// yet, so there is no warm prefix to protect and clearing is free.
func observationCacheIsCold(lastAssistantAt time.Time, cfg ObservationWindowConfig) bool {
	if lastAssistantAt.IsZero() {
		return true
	}
	return time.Since(lastAssistantAt) >= cfg.coldCacheGap()
}

// applyObservationWindow runs the browser-observation clearing pass under the
// configured trigger policy. Returns the number of observations stubbed.
//
// keep and the clearing behavior are unchanged from filterOldObservations —
// this only decides whether that pass runs on this iteration.
func applyObservationWindow(
	messages []client.Message,
	keep int,
	lastAssistantAt time.Time,
	cfg ObservationWindowConfig,
) int {
	return applyObservationWindowWithCaptureCap(
		messages, keep, lastAssistantAt, defaultBrowserObservationMaxChars, cfg,
	)
}

// applyObservationWindowWithCaptureCap keeps the derived aggregate threshold
// coherent with the actual per-observation capture cap. This matters when an
// operator raises tools.browser_result_truncation above its default: after a
// clearing pass, retained observations still leave the configured headroom.
func applyObservationWindowWithCaptureCap(
	messages []client.Message,
	keep int,
	lastAssistantAt time.Time,
	observationMaxRunes int,
	cfg ObservationWindowConfig,
) int {
	if keep <= 0 {
		return 0
	}
	if _, fire := observationWindowShouldFire(
		messages, keep, lastAssistantAt, observationMaxRunes, cfg,
	); !fire {
		return 0
	}
	return filterOldObservations(messages, keep)
}
