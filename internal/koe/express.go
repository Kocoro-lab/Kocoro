package koe

import (
	"log"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// Expression layer (§19): the model's only expressive tool is express{intent}, a
// small enum of emotional intents. This file is the deterministic gate + the
// intent→clip resolver — pure logic, no realtime/audio deps (tagless).
//
// The gate is the house-style safety net: over-calling ("演上头") is bounded hard
// (≤1 per response, a cooldown, and an activity-tier budget); under-calling is
// left to persona guidance, because the reflex + event lanes already carry the
// "alive" baseline.

// ActivityTier is the user's "活跃度" setting mapped to the express budget.
type ActivityTier int

const (
	ActivityQuiet    ActivityTier = iota // model lane off — only the reflex/event lanes
	ActivityStandard                     // default budget
	ActivityLively                       // cooldown halved (reflex reinforcement lives elsewhere)
)

const (
	expressCooldownStandard = 18 * time.Second
	expressCooldownLively   = 9 * time.Second
)

// expressIntentOrder is the tool-schema enum (§19: small — the realtime 16k
// instructions+tools budget). dance is an intent, not its own tool.
var expressIntentOrder = []string{
	"happy", "excited", "curious", "attentive", "thinking",
	"surprised", "sad", "sorry", "confused", "proud", "dance",
}

// intentClips maps each intent to a weighted variant pool of recorded-move clips
// (§10 layer 4 "动作数据包"). Starter mapping over the real emotions/dances
// datasets; the exact entries are implementation-time tuning (§19). Server-side
// validation gates on these keys (an illegal intent name is dropped, not played).
var intentClips = map[string][]string{
	"happy":     {"cheerful1", "laughing1", "laughing2", "loving1", "relief1"},
	"excited":   {"enthusiastic1", "enthusiastic2", "electric1", "no_excited1"},
	"curious":   {"curious1", "inquiring1", "inquiring2", "inquiring3", "side_glance_flick"},
	"attentive": {"attentive1", "attentive2", "uh_huh_tilt", "understanding1", "yeah_nod"},
	"thinking":  {"thoughtful1", "thoughtful2", "chin_lead", "uncertain1"},
	"surprised": {"surprised1", "surprised2", "amazed1", "oops1"},
	"sad":       {"sad1", "sad2", "downcast1", "lonely1"},
	"sorry":     {"shy1", "resigned1", "oops2", "uncomfortable1"},
	"confused":  {"confused1", "lost1", "incomprehensible2"},
	"proud":     {"proud1", "proud2", "proud3", "success1", "success2"},
	"dance":     {"dance1", "dance2", "dance3", "groovy_sway_and_roll", "side_to_side_sway"},
}

// ExpressIntents returns the intent enum (a copy) for the tool schema + validation.
func ExpressIntents() []string {
	out := make([]string, len(expressIntentOrder))
	copy(out, expressIntentOrder)
	return out
}

// ExpressGate is the Koe-side deterministic gate for express{intent}. Not
// goroutine-safe: the realtime dispatch loop (via MotionController) owns it
// single-threaded and serializes Allow / NewResponse / SetAvailableMoves.
type ExpressGate struct {
	tier     ActivityTier
	cooldown time.Duration
	lastFire time.Time
	fired    bool // an express already fired in the current response (≤1/response)
	lastClip map[string]string
	// clips is this gate's live intent→clip pool: a copy of intentClips, narrowed
	// to what the bridge actually exposes via SetAvailableMoves (spec §5 hello.moves).
	clips map[string][]string
	pick  func(n int) int
	now   func() time.Time
}

// ExpressOption configures an ExpressGate (clock/picker injection for tests).
type ExpressOption func(*ExpressGate)

// WithClock overrides the time source.
func WithClock(f func() time.Time) ExpressOption { return func(g *ExpressGate) { g.now = f } }

// WithPicker overrides the variant selector (returns an index in [0,n)).
func WithPicker(f func(n int) int) ExpressOption { return func(g *ExpressGate) { g.pick = f } }

// NewExpressGate builds a gate for an activity tier.
func NewExpressGate(tier ActivityTier, opts ...ExpressOption) *ExpressGate {
	g := &ExpressGate{
		tier:     tier,
		cooldown: cooldownForTier(tier),
		lastClip: make(map[string]string),
		clips:    cloneClipMap(intentClips),
		pick:     func(n int) int { return rand.Intn(n) },
		now:      time.Now,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func cooldownForTier(t ActivityTier) time.Duration {
	if t == ActivityLively {
		return expressCooldownLively
	}
	return expressCooldownStandard
}

// NewResponse resets the per-response budget; call it when a new model response
// starts so the next response may express again.
func (g *ExpressGate) NewResponse() { g.fired = false }

// Allow decides whether an express intent fires now. On success it returns the
// resolved clip name; otherwise ("", false, reason) with reason ∈
// invalid_intent | quiet | over_budget | cooldown. A skip is silent (no motion,
// no interruption of speech).
func (g *ExpressGate) Allow(intent string) (clip string, ok bool, reason string) {
	clips, known := g.clips[intent]
	if !known || len(clips) == 0 {
		return "", false, "invalid_intent"
	}
	if g.tier == ActivityQuiet {
		return "", false, "quiet"
	}
	if g.fired {
		return "", false, "over_budget"
	}
	now := g.now()
	if !g.lastFire.IsZero() && now.Sub(g.lastFire) < g.cooldown {
		return "", false, "cooldown"
	}
	clip = g.resolveVariant(intent, clips)
	g.fired = true
	g.lastFire = now
	g.lastClip[intent] = clip
	return clip, true, ""
}

// SetAvailableMoves narrows each intent's clip pool to the moves the bridge
// actually exposes (spec §5 hello.moves). A clip absent from the current dataset
// would make the bridge reject the play with unknown_move AFTER the gate already
// spent the response budget + cooldown — a silent no-op — so it is dropped here
// (logged). An intent whose entire pool is missing goes dark (Allow →
// invalid_intent). An empty move set means "not connected / unknown" and restores
// the full mapping — the unfiltered default, never "nothing allowed". Always
// re-derived from the canonical intentClips so reconnects are idempotent.
func (g *ExpressGate) SetAvailableMoves(moves []string) {
	if len(moves) == 0 {
		g.clips = cloneClipMap(intentClips)
		return
	}
	avail := make(map[string]bool, len(moves))
	for _, m := range moves {
		avail[m] = true
	}
	filtered := make(map[string][]string, len(intentClips))
	var dropped []string
	for intent, pool := range intentClips {
		kept := make([]string, 0, len(pool))
		for _, c := range pool {
			if avail[c] {
				kept = append(kept, c)
			} else {
				dropped = append(dropped, c)
			}
		}
		if len(kept) > 0 {
			filtered[intent] = kept
		}
	}
	g.clips = filtered
	if len(dropped) > 0 {
		sort.Strings(dropped)
		log.Printf("koe[express]: %d clip(s) not in the bridge move set, dropped from intent pools: %s",
			len(dropped), strings.Join(dropped, ", "))
	}
}

func cloneClipMap(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(src))
	for k, v := range src {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// resolveVariant picks a clip for the intent, avoiding the one it played last for
// that intent (Cozmo animation-trigger pattern: same intent twice ≠ same clip).
func (g *ExpressGate) resolveVariant(intent string, clips []string) string {
	last := g.lastClip[intent]
	candidates := clips
	if last != "" && len(clips) > 1 {
		candidates = make([]string, 0, len(clips))
		for _, c := range clips {
			if c != last {
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = clips
	}
	return candidates[g.pick(len(candidates))%len(candidates)]
}
