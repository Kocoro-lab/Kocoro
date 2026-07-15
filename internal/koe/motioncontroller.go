package koe

import (
	"context"
	"log"
	"math"
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
	// Realtime can finish a dance tool call just before input transcription lands.
	// The tool dispatch runs off the event loop and waits briefly for that final
	// transcript; silence/capability questions still fail closed at the deadline.
	danceAuthorizationWait = 1500 * time.Millisecond

	// The robot-local DOA stream is sampled at 10 Hz. Three coherent samples avoid
	// turning on a click or one noisy estimate; the cooldown prevents a moving talker
	// from continuously preempting the primary motion queue.
	doaReflectionHits     = 3
	doaReflectionCooldown = 4 * time.Second
	doaCandidateTolerance = 15 * math.Pi / 180
	doaFrontDeadband      = 12 * math.Pi / 180
	lastSpeakerTTL        = 15 * time.Minute
	motionRPCTimeout      = 3 * time.Second
	taskCompleteClip      = "success1"
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
	danceAuthWait        time.Duration
	danceAuthNotify      chan struct{}

	// Reflection/event lanes are deliberately separate from the model express gate.
	// Voice/perception callbacks only publish latest state into these bounded queues;
	// one worker serializes bridge RPCs so audio and Realtime event loops never block.
	desiredListening     atomic.Bool
	listeningGeneration  atomic.Uint64
	reflectionNotify     chan struct{}
	doaLookAt            chan float64
	taskComplete         chan struct{}
	successClipAvailable atomic.Bool

	perceptionMu      sync.Mutex
	lastSpeakerAngle  float64
	lastSpeakerAt     time.Time
	doaCandidateAngle float64
	doaCandidateHits  int
	lastDOAReflection time.Time
	doaHitsRequired   int
	doaCooldown       time.Duration
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
		danceAuthWait:     danceAuthorizationWait,
		danceAuthNotify:   make(chan struct{}, 1),
		reflectionNotify:  make(chan struct{}, 1),
		doaLookAt:         make(chan float64, 1),
		taskComplete:      make(chan struct{}, 1),
		doaHitsRequired:   doaReflectionHits,
		doaCooldown:       doaReflectionCooldown,
	}
}

// IsConnected reports whether the bridge handshake is live.
func (mc *MotionController) IsConnected() bool { return mc.client.IsConnected() }

// BridgeDetails returns metadata from the currently-live system.hello. Empty
// values mean the bridge is disconnected or has not completed its handshake.
func (mc *MotionController) BridgeDetails() (proto, bridgeVersion string) {
	if hello := mc.client.Hello(); hello != nil {
		return hello.Proto, hello.BridgeVersion
	}
	return "", ""
}

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
		select {
		case mc.danceAuthNotify <- struct{}{}:
		default:
		}
	} else {
		mc.danceAuthorizedUntil = time.Time{}
		select {
		case <-mc.danceAuthNotify:
		default:
		}
	}
}

func (mc *MotionController) awaitDanceAuthorization(ctx context.Context) bool {
	mc.mu.Lock()
	authorized := !mc.danceAuthorizedUntil.IsZero() && !mc.now().After(mc.danceAuthorizedUntil)
	wait := mc.danceAuthWait
	notify := mc.danceAuthNotify
	mc.mu.Unlock()
	if authorized {
		return true
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	case <-notify:
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return !mc.danceAuthorizedUntil.IsZero() && !mc.now().After(mc.danceAuthorizedUntil)
}

// Express runs a gated body gesture: the gate picks a clip (≤1/response, cooldown,
// activity tier, availability), then the bridge plays it. A gate skip or a
// disconnected bridge returns a non-Expressed result — never an error the model
// must handle (a missed gesture is invisible; the conversation continues).
func (mc *MotionController) Express(ctx context.Context, intent string) ExpressResult {
	if intent == "dance" && !mc.awaitDanceAuthorization(ctx) {
		log.Printf("koe[express]: intent=dance status=skipped reason=not_explicit")
		return ExpressResult{Reason: "not_explicit"}
	}
	mc.mu.Lock()
	clip, ok, reason := mc.gate.Allow(intent)
	if ok && intent == "dance" {
		mc.danceAuthorizedUntil = time.Time{} // one explicit utterance authorizes one attempt
		select {
		case <-mc.danceAuthNotify:
		default:
		}
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

// ObserveVoiceState drives the model-free listening reflex. Only the listening
// state raises/animates the antennas; thinking, speaking, idle, and teardown all
// clear it. The callback is non-blocking and latest-only.
func (mc *MotionController) ObserveVoiceState(state string) {
	mc.desiredListening.Store(state == "listening")
	mc.signalListeningRefresh()
}

func (mc *MotionController) signalListeningRefresh() {
	mc.listeningGeneration.Add(1)
	select {
	case mc.reflectionNotify <- struct{}{}:
	default:
	}
}

// ObservePerception consumes the resident robot-local face/DOA stream. A fresh
// tracked face keeps ownership of the head in the daemon; only sustained speech
// with no tracked face becomes a DOA look-at reflex. The latest speaking angle is
// also retained for the deterministic task-complete event.
func (mc *MotionController) ObservePerception(snapshot PerceptionSnapshot) {
	now := snapshot.ObservedAt
	if now.IsZero() {
		now = mc.now()
	}

	mc.perceptionMu.Lock()
	defer mc.perceptionMu.Unlock()
	if !snapshot.DOA.Available || !snapshot.DOA.Fresh || !snapshot.DOA.SpeechDetected {
		mc.doaCandidateHits = 0
		return
	}

	angle := snapshot.DOA.Angle
	mc.lastSpeakerAngle = angle
	mc.lastSpeakerAt = now
	if snapshot.Face.Available && snapshot.Face.Fresh && snapshot.Face.Detected {
		mc.doaCandidateHits = 0
		return
	}
	if math.Abs(angle-math.Pi/2) <= doaFrontDeadband {
		mc.doaCandidateHits = 0
		return
	}
	if mc.doaCandidateHits == 0 || math.Abs(angle-mc.doaCandidateAngle) > doaCandidateTolerance {
		mc.doaCandidateAngle = angle
		mc.doaCandidateHits = 1
	} else {
		mc.doaCandidateAngle = (mc.doaCandidateAngle*float64(mc.doaCandidateHits) + angle) / float64(mc.doaCandidateHits+1)
		mc.doaCandidateHits++
	}
	if mc.doaCandidateHits < mc.doaHitsRequired || (!mc.lastDOAReflection.IsZero() && now.Sub(mc.lastDOAReflection) < mc.doaCooldown) {
		return
	}
	angle = mc.doaCandidateAngle
	mc.doaCandidateHits = 0
	mc.lastDOAReflection = now
	select {
	case <-mc.doaLookAt:
	default:
	}
	select {
	case mc.doaLookAt <- angle:
	default:
	}
}

// TriggerTaskComplete publishes the event-lane action. Multiple task completions
// that land while one success sequence is still being queued coalesce into one
// gesture instead of producing a burst of full-body motion.
func (mc *MotionController) TriggerTaskComplete() {
	select {
	case mc.taskComplete <- struct{}{}:
	default:
	}
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
	available := false
	for _, move := range moves {
		if move == taskCompleteClip {
			available = true
			break
		}
	}
	mc.successClipAvailable.Store(available)
	mc.movesApplied.Store(true)
}

func (mc *MotionController) runAutomaticLanes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-mc.reflectionNotify:
			mc.applyListening(ctx)
		case angle := <-mc.doaLookAt:
			mc.lookAtDOA(ctx, angle, "speech")
		case <-mc.taskComplete:
			mc.playTaskComplete(ctx)
		}
	}
}

func (mc *MotionController) applyListening(ctx context.Context) {
	for {
		generation := mc.listeningGeneration.Load()
		if !mc.client.IsConnected() {
			return // the connection state loop re-signals after the next handshake
		}
		callCtx, cancel := context.WithTimeout(ctx, motionRPCTimeout)
		err := mc.client.SetListening(callCtx, mc.desiredListening.Load())
		cancel()
		if err != nil {
			log.Printf("koe[reflex]: listening update skipped: %v", err)
			return
		}
		if generation == mc.listeningGeneration.Load() {
			return
		}
	}
}

func (mc *MotionController) lookAtDOA(ctx context.Context, angle float64, reason string) bool {
	if !mc.client.IsConnected() || math.Abs(angle-math.Pi/2) <= doaFrontDeadband {
		return false
	}
	// XVF3800/Pollen convention: 0=left, pi/2=front/back, pi=right.
	// motion.look_at uses +x=front and +y=left, hence (sin(a), cos(a)).
	x, y := math.Sin(angle), math.Cos(angle)
	callCtx, cancel := context.WithTimeout(ctx, motionRPCTimeout)
	err := mc.client.LookAtWorld(callCtx, x, y, 0)
	cancel()
	if err != nil {
		log.Printf("koe[reflex]: doa look-at skipped reason=%s: %v", reason, err)
		return false
	}
	log.Printf("koe[reflex]: doa look-at reason=%s angle_deg=%.1f", reason, angle*180/math.Pi)
	return true
}

func (mc *MotionController) playTaskComplete(ctx context.Context) {
	if !mc.client.IsConnected() {
		return
	}
	mc.perceptionMu.Lock()
	angle, observedAt := mc.lastSpeakerAngle, mc.lastSpeakerAt
	mc.perceptionMu.Unlock()
	if !observedAt.IsZero() && mc.now().Sub(observedAt) <= lastSpeakerTTL {
		mc.lookAtDOA(ctx, angle, "task_complete")
	}
	if !mc.successClipAvailable.Load() {
		log.Printf("koe[event]: task_complete status=skipped reason=success_clip_unavailable")
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, motionRPCTimeout)
	err := mc.client.PlayMove(callCtx, taskCompleteClip, false)
	cancel()
	if err != nil {
		log.Printf("koe[event]: task_complete clip=%s status=skipped reason=bridge_error err=%v", taskCompleteClip, err)
		return
	}
	log.Printf("koe[event]: task_complete clip=%s status=played", taskCompleteClip)
}

// Run drives the bridge client + the bridge_status state machine (connection +
// §10 heartbeat watchdog) until ctx is cancelled. The client's own Run goroutine
// owns dial/handshake/reconnect; this loop only observes and reports.
func (mc *MotionController) Run(ctx context.Context) {
	go mc.client.Run(ctx)
	go mc.runAutomaticLanes(ctx)

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
				mc.successClipAvailable.Store(false)
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
				mc.signalListeningRefresh()
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
