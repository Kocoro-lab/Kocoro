package koe

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/koe/reachy"
)

// Sentinel errors returned by ManualPlay / ManualStop so the control layer can map
// each to a distinct HTTP status without knowing the bridge internals:
//   - ErrUnknownMove       → 404 (name not in the bridge's advertised move set)
//   - ErrBridgeUnavailable → 503 (bridge disconnected or degraded)
//
// ErrCallActive is not raised here — the manual-play seam in cmd/koe.go decides it
// from the call-state source (the same one POST /call/text reads) and returns it so
// the control layer maps it to 409; it lives beside the others for that mapping.
var (
	ErrUnknownMove       = errors.New("unknown_move")
	ErrBridgeUnavailable = errors.New("bridge_unavailable")
	ErrCallActive        = errors.New("call_active")
)

// MotionStatus is the GET /motion/status snapshot (plan W2-1): the bridge's move
// catalog (from hello) plus the latest §8 heartbeat fields and the bridge_status
// state. JSON is snake_case so the runtime facade can merge it verbatim into the
// 20 Hz motion stream's `move` key and Desktop decodes it with convertFromSnakeCase.
type MotionStatus struct {
	Moves           []string `json:"moves"`
	CurrentMove     string   `json:"current_move"`
	IsListening     bool     `json:"is_listening"`
	BreathingActive bool     `json:"breathing_active"`
	BridgeState     string   `json:"bridge_state"`
}

// statusSnapshot caches the live fields from the most recent §8 status heartbeat.
// The bridge's status `data` object today carries motors/daemon/queue fields; when
// it also carries current_move/is_listening/breathing_active (spec §8) they are
// picked up here — absent fields stay zero-valued (json ignores unknown keys).
type statusSnapshot struct {
	CurrentMove     string `json:"current_move"`
	IsListening     bool   `json:"is_listening"`
	BreathingActive bool   `json:"breathing_active"`
}

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
	speechNodClip         = "yeah_nod"
	speechNodCooldown     = 6 * time.Second
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

	// statusSnap (statusSnapshot) and bridgeState (string) are updated by the Run
	// goroutine and read lock-free by Status()/ManualPlay from the control HTTP
	// goroutine.
	statusSnap  atomic.Value
	bridgeState atomic.Value

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
	speechStarted        chan struct{}
	successClipAvailable atomic.Bool
	speechNodAvailable   atomic.Bool
	lastSpeechNod        time.Time

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
		speechStarted:     make(chan struct{}, 1),
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

// ManualPlay plays a named move on operator command (POST /motion/play, dev/robot
// panel). Unlike Express it does NOT touch the express gate — a manual play is a
// direct user action, not a budgeted emotional gesture, so it never spends the
// ≤1/response express budget. The name is validated against the bridge's advertised
// move set (unknown → ErrUnknownMove); a disconnected or degraded bridge →
// ErrBridgeUnavailable. preempt is true so a manual play supersedes any in-flight
// move (play_move preempt semantics).
func (mc *MotionController) ManualPlay(name string) error {
	if !mc.bridgeReady() {
		return ErrBridgeUnavailable
	}
	if !mc.moveKnown(name) {
		return ErrUnknownMove
	}
	if err := mc.client.PlayMove(context.Background(), name, true); err != nil {
		// Dropped between the readiness check and the play.
		return ErrBridgeUnavailable
	}
	return nil
}

// ManualStop stops any in-flight move (POST /motion/stop). Bridge disconnected or
// degraded → ErrBridgeUnavailable.
func (mc *MotionController) ManualStop() error {
	if !mc.bridgeReady() {
		return ErrBridgeUnavailable
	}
	if err := mc.client.StopMoves(context.Background()); err != nil {
		return ErrBridgeUnavailable
	}
	return nil
}

// Status returns the GET /motion/status snapshot: the bridge's full move catalog
// (from hello — the complete list, NOT the express-gate-narrowed pool), the latest
// §8 heartbeat fields, and the current bridge_status state.
func (mc *MotionController) Status() MotionStatus {
	moves := []string{}
	if h := mc.client.Hello(); h != nil && len(h.Moves) > 0 {
		moves = append(moves, h.Moves...)
	}
	var snap statusSnapshot
	if v := mc.statusSnap.Load(); v != nil {
		snap = v.(statusSnapshot)
	}
	return MotionStatus{
		Moves:           moves,
		CurrentMove:     snap.CurrentMove,
		IsListening:     snap.IsListening,
		BreathingActive: snap.BreathingActive,
		BridgeState:     mc.currentBridgeState(),
	}
}

// bridgeReady reports whether a manual command can reach the bridge: the socket is
// handshaked AND the §10 watchdog has not marked it degraded.
func (mc *MotionController) bridgeReady() bool {
	return mc.client.IsConnected() && mc.currentBridgeState() != bridgeStateDegraded
}

// moveKnown reports whether name is in the bridge's advertised move set (hello).
func (mc *MotionController) moveKnown(name string) bool {
	h := mc.client.Hello()
	return h != nil && slices.Contains(h.Moves, name)
}

// currentBridgeState returns the last bridge_status emitted by Run ("" before Run
// has set one).
func (mc *MotionController) currentBridgeState() string {
	if v, ok := mc.bridgeState.Load().(string); ok {
		return v
	}
	return ""
}

// NewResponse resets the express ≤1/response budget (wired to response.created).
func (mc *MotionController) NewResponse() {
	mc.mu.Lock()
	mc.gate.NewResponse()
	mc.mu.Unlock()
}

// SpeechStarted schedules one restrained official nod when audible response
// playback begins. The latest-only channel and cooldown avoid mechanical
// repetition across tool preambles/follow-ups while the SDK HeadWobbler continues
// to provide fine-grained audio-reactive motion during the rest of the utterance.
func (mc *MotionController) SpeechStarted() {
	select {
	case mc.speechStarted <- struct{}{}:
	default:
	}
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
	// DOA reflection is a conversation reflex, not an idle room-noise motor loop.
	// The XVF can report fan/mechanical noise as speech; moving the head on those
	// samples creates more acoustic energy and feeds the detector again.
	if !mc.desiredListening.Load() {
		mc.doaCandidateHits = 0
		return
	}
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
	nodAvailable := false
	for _, move := range moves {
		if move == speechNodClip {
			nodAvailable = true
			break
		}
	}
	mc.speechNodAvailable.Store(nodAvailable)
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
		case <-mc.speechStarted:
			mc.playSpeechNod(ctx)
		}
	}
}

func (mc *MotionController) playSpeechNod(ctx context.Context) {
	if !mc.client.IsConnected() || !mc.speechNodAvailable.Load() {
		return
	}
	now := mc.now()
	if !mc.lastSpeechNod.IsZero() && now.Sub(mc.lastSpeechNod) < speechNodCooldown {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, motionRPCTimeout)
	err := mc.client.PlayMove(callCtx, speechNodClip, false)
	cancel()
	if err != nil {
		log.Printf("koe[event]: speech_nod clip=%s status=skipped reason=bridge_error err=%v", speechNodClip, err)
		return
	}
	mc.lastSpeechNod = now
	log.Printf("koe[event]: speech_nod clip=%s status=played", speechNodClip)
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
		mc.bridgeState.Store(s)
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
				var snap statusSnapshot
				if json.Unmarshal(ev.Data, &snap) == nil {
					mc.statusSnap.Store(snap)
				}
			}
		case <-ticker.C:
			if !mc.client.IsConnected() {
				mc.movesApplied.Store(false)
				mc.successClipAvailable.Store(false)
				mc.speechNodAvailable.Store(false)
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
