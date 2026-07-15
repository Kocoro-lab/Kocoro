package koe

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/koe/reachy"
)

// Bridge status states (mirror control.go bridge_status / spec §3-§4).
const (
	bridgeStateConnecting = "connecting"
	bridgeStateConnected  = "connected"
	bridgeStateDegraded   = "degraded"
)

// reachyHeartbeatInterval is the bridge's status-heartbeat cadence (spec §8, 2s).
// reachyHeartbeatMisses is spec §10: this many consecutive missed heartbeats mark
// the bridge degraded. WORKLOAD: a bridge whose socket is alive but the motion loop
// is wedged (motors dead). SYMPTOM if too low: one dropped heartbeat false-degrades;
// too high: slow to notice. OVERRIDE: the fields on MotionController (tests set
// tiny values; a power user could lift them if a slow bridge false-degrades).
const (
	reachyHeartbeatInterval = 2 * time.Second
	reachyHeartbeatMisses   = 3
	// ASR completion and the response tool call normally land within a few seconds.
	// Bound the authorization so an abandoned dance request cannot leak into a
	// later turn; a new non-dance transcript clears it immediately as well.
	danceAuthorizationWindow = 12 * time.Second
)

// MotionController owns the reachy motion-bridge client + the express gate for a
// carrier with a body. It is the ExpressFunc seam (Express runs a gated gesture
// through the bridge), keeps the gate's clip pool in sync with the bridge's move
// set (spec §5), and drives bridge_status from the connection + a §10 heartbeat
// watchdog. Single-owned: the gate is lock-free by contract, so this serializes all
// gate access (the dispatch goroutine's Express/NewResponse vs the Run goroutine's
// clip filtering) under mu.
type MotionController struct {
	client *reachy.Client
	gate   *ExpressGate

	mu sync.Mutex // serializes gate access

	onBridgeStatus    func(string)  // nil-safe; drives control.go EmitBridgeStatus
	pollInterval      time.Duration // how often Run checks connection + watchdog
	heartbeatInterval time.Duration // bridge status cadence (× misses = stale threshold)
	misses            int

	now          func() time.Time
	movesApplied atomic.Bool

	danceAuthorizedUntil time.Time
}

// NewMotionController builds a controller for the motion bridge at socketPath.
// Call Run to start the client + status state machine.
func NewMotionController(socketPath string, tier ActivityTier, onBridgeStatus func(string)) *MotionController {
	return &MotionController{
		client:            reachy.NewClient(socketPath),
		gate:              NewExpressGate(tier),
		onBridgeStatus:    onBridgeStatus,
		pollInterval:      reachyHeartbeatInterval,
		heartbeatInterval: reachyHeartbeatInterval,
		misses:            reachyHeartbeatMisses,
		now:               time.Now,
	}
}

// IsConnected reports whether the bridge handshake is live.
func (mc *MotionController) IsConnected() bool { return mc.client.IsConnected() }

// MovesApplied reports whether the clip pool has been narrowed to the bridge move
// set at least once since the current connection came up.
func (mc *MotionController) MovesApplied() bool { return mc.movesApplied.Load() }

// ObserveUserTranscript updates the server-side dance authorization for the
// current user turn. It stores no transcript and logs no content. Every new
// non-request clears the grant; an explicit request grants one bounded attempt.
func (mc *MotionController) ObserveUserTranscript(transcript string) {
	allowed := explicitDanceRequest(transcript)
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if allowed {
		mc.danceAuthorizedUntil = mc.now().Add(danceAuthorizationWindow)
	} else {
		mc.danceAuthorizedUntil = time.Time{}
	}
}

// Express runs a gated body gesture: the gate picks a clip (≤1/response, cooldown,
// activity tier, availability), then the bridge plays it. A gate skip or a
// disconnected bridge returns a non-Expressed result — never an error the model
// must handle (a missed gesture is invisible; the conversation continues).
func (mc *MotionController) Express(ctx context.Context, intent string) ExpressResult {
	mc.mu.Lock()
	if intent == "dance" && (mc.danceAuthorizedUntil.IsZero() || mc.now().After(mc.danceAuthorizedUntil)) {
		mc.mu.Unlock()
		log.Printf("koe[express]: intent=dance status=skipped reason=not_explicit")
		return ExpressResult{Reason: "not_explicit"}
	}
	clip, ok, reason := mc.gate.Allow(intent)
	if ok && intent == "dance" {
		mc.danceAuthorizedUntil = time.Time{} // one explicit utterance authorizes one attempt
	}
	mc.mu.Unlock()
	if !ok {
		log.Printf("koe[express]: intent=%s status=skipped reason=%s", intent, reason)
		return ExpressResult{Reason: reason}
	}
	if err := mc.client.PlayMove(ctx, clip, false); err != nil {
		// Bridge dropped between the gate decision and the play — the budget is spent
		// but the gesture didn't happen; report a skip, don't surface the error.
		log.Printf("koe[express]: intent=%s clip=%s status=skipped reason=not_connected", intent, clip)
		return ExpressResult{Reason: "not_connected"}
	}
	log.Printf("koe[express]: intent=%s clip=%s status=expressed", intent, clip)
	return ExpressResult{Expressed: true, Clip: clip}
}

// NewResponse resets the express ≤1/response budget (wired to response.created).
func (mc *MotionController) NewResponse() {
	mc.mu.Lock()
	mc.gate.NewResponse()
	mc.mu.Unlock()
}

// Close stops the bridge client.
func (mc *MotionController) Close() { mc.client.Close() }

// applyMoves narrows the express clip pool to the bridge's current move set
// (spec §5 hello.moves × intentClips).
func (mc *MotionController) applyMoves() {
	var moves []string
	if h := mc.client.Hello(); h != nil {
		moves = h.Moves
	}
	mc.mu.Lock()
	mc.gate.SetAvailableMoves(moves)
	mc.mu.Unlock()
	mc.movesApplied.Store(true)
}

// Run drives the bridge client + the bridge_status state machine (connection +
// §10 heartbeat watchdog) until ctx is cancelled. The client's own Run goroutine
// owns dial/handshake/reconnect; this loop only observes and reports.
func (mc *MotionController) Run(ctx context.Context) {
	go mc.client.Run(ctx)

	ticker := time.NewTicker(mc.pollInterval)
	defer ticker.Stop()

	state := ""
	var lastStatus time.Time
	setState := func(s string) {
		if s == state {
			return
		}
		state = s
		if mc.onBridgeStatus != nil {
			mc.onBridgeStatus(s)
		}
	}
	staleThreshold := func() time.Duration { return mc.heartbeatInterval * time.Duration(mc.misses) }
	setState(bridgeStateConnecting)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-mc.client.Events():
			if !ok {
				return
			}
			if ev.Event == reachy.EventStatus {
				lastStatus = mc.now()
			}
		case <-ticker.C:
			if !mc.client.IsConnected() {
				mc.movesApplied.Store(false)
				setState(bridgeStateConnecting)
				continue
			}
			// First tick after the handshake lands: sync the clip pool to the bridge
			// move set and open the heartbeat grace window.
			if state == bridgeStateConnecting {
				mc.applyMoves()
				lastStatus = mc.now()
				setState(bridgeStateConnected)
				// Wake the robot so its motors are live and it comes alive (idle breathing +
				// speech wobble + express). The daemon is launched --no-wake-up-on-start
				// (§16), so torque is off until we ask; wake is idempotent across reconnects.
				// Fire-and-forget off the state loop (Wake is a blocking RPC).
				go func() { _ = mc.client.Wake(ctx) }()
			}
			// §10 watchdog: heartbeats stalled → degraded; resumed → connected.
			if !lastStatus.IsZero() {
				if mc.now().Sub(lastStatus) > staleThreshold() {
					setState(bridgeStateDegraded)
				} else if state == bridgeStateDegraded {
					setState(bridgeStateConnected)
				}
			}
		}
	}
}
