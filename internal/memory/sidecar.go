package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type SidecarState int32

const (
	StateStopped SidecarState = iota
	StateStarting
	StateReady
	StateRestarting
	StateDegraded
)

func (s SidecarState) String() string {
	return [...]string{"stopped", "starting", "ready", "restarting", "degraded"}[s]
}

// ErrTLMNotFound is the sentinel returned when neither memory.tlm_path nor
// PATH yields a usable sidecar binary. Service treats it as terminal — no
// restart loop, just status=Unavailable.
var ErrTLMNotFound = errors.New("memory: tlm binary not found in PATH and memory.tlm_path empty")

// ErrReadyCeilingExceeded is returned by WaitReady when the internal deadline
// fires before the sidecar reports ready=true. Supervisor uses this to
// distinguish startup timeout from other health-check failures.
var ErrReadyCeilingExceeded = errors.New("memory: sidecar ready ceiling exceeded")

// ReadyTimeoutError wraps ErrReadyCeilingExceeded with the last /health
// observation captured during the WaitReady poll window. Supervisor uses
// LastCompatibility + LastSubCode to classify the failure (e.g.
// tlm_binary_too_old vs generic startup_timeout) and to populate the
// repair_needed detail block on GET /status. errors.Is(err, ErrReadyCeilingExceeded)
// stays true via Unwrap.
type ReadyTimeoutError struct {
	LastCompatibility string // "", "unknown", "incompatible", "compatible"
	LastSubCode       string // populated when LastCompatibility=="incompatible"
	LastBundleVersion string // populated when /health surfaced one before deadline
}

func (e *ReadyTimeoutError) Error() string {
	if e.LastCompatibility == "incompatible" && e.LastSubCode != "" {
		return "memory: sidecar ready ceiling exceeded (incompatible: " + e.LastSubCode + ")"
	}
	return "memory: sidecar ready ceiling exceeded"
}

func (e *ReadyTimeoutError) Unwrap() error { return ErrReadyCeilingExceeded }

// Failure reason strings surfaced via Supervisor.onDegraded and stored in
// Service.disabledReason for the GET /status memory field.
const (
	ReasonBinaryMissing      = "tlm_binary_missing"
	ReasonExecError          = "tlm_exec_error"
	ReasonHealthFailed       = "tlm_health_failed"
	ReasonStartupTimeout     = "startup_timeout"
	ReasonRepeatedCrash      = "repeated_crash"
	ReasonCloudMisconfigured = "cloud_misconfigured"
	// ReasonBundleSchemaMismatch is set when WaitReady consistently observes
	// compatibility="incompatible" with sub_code in {"no_manifest","version_out_of_range"}.
	// Most common cause: a tlm binary older than the bundle's manifest
	// schema — the dataclass can't unmarshal newer fields. Kocoro Desktop
	// consumes this via GET /status memory.reason and prompts the user to
	// re-run its on-demand tlm install.
	ReasonBundleSchemaMismatch = "tlm_binary_too_old"
)

// Sidecar is the managed child-process handle for the local memory sidecar.
// Daemon-only: only Service.Start (in daemon mode) constructs and operates a
// Sidecar. CLI/TUI use AttachPolicy instead — they never spawn.
//
// Concurrency model:
//   - Spawn launches a single dedicated goroutine that owns the only
//     exec.Cmd.Wait() call for that child. It stores the result in waitErr
//     under mu and closes cmdDone.
//   - Sidecar.Wait and Sidecar.Shutdown never call cmd.Wait() themselves;
//     they synchronize on <-cmdDone. Both callers may run concurrently
//     (e.g. supervisor's Wait while Service.Stop calls Shutdown during
//     daemon repair-and-restart) without racing on cmd field access or
//     double-Waiting the same exec.Cmd.
//   - mu protects all read/write of cmd and cmdDone. Snapshots are taken
//     under the lock, then blocking ops happen unlocked.
type Sidecar struct {
	cfg      Config
	extraArg []string // test injection: prefix args before "serve --socket --bundle-root"
	state    atomic.Int32

	mu        sync.Mutex
	cmd       *exec.Cmd
	cmdDone   chan struct{} // closed exactly once when the spawned child has been reaped
	waitErr   error         // valid only after cmdDone is closed; protected by mu
	cmdReaped atomic.Bool   // set right before cmdDone is closed
}

// NewSidecar builds a Sidecar bound to cfg. extraArg lets tests prepend
// positional args (e.g. a python script path when TLMPath="python3").
// Production callers pass nil.
func NewSidecar(cfg Config, extraArg []string) *Sidecar {
	return &Sidecar{cfg: cfg, extraArg: extraArg}
}

func (s *Sidecar) Status() SidecarState { return SidecarState(s.state.Load()) }

func (s *Sidecar) resolveBinary() (string, error) {
	if s.cfg.TLMPath != "" {
		// Honor explicit configuration. Existence is checked at exec time;
		// returning the path here lets ErrTLMNotFound surface from Start().
		if _, err := os.Stat(s.cfg.TLMPath); err == nil {
			return s.cfg.TLMPath, nil
		}
		// If the configured path is "python3" or a bare command, fall through
		// to PATH lookup so test rigs work.
		if p, err := exec.LookPath(s.cfg.TLMPath); err == nil {
			return p, nil
		}
		return "", ErrTLMNotFound
	}
	p, err := exec.LookPath("tlm")
	if err != nil {
		return "", ErrTLMNotFound
	}
	return p, nil
}

// Spawn starts the sidecar child process. Removes any stale socket first.
// Sets PGID so Shutdown can SIGTERM the whole process group. Launches a
// dedicated goroutine that owns the only exec.Cmd.Wait() call for this child;
// Sidecar.Wait and Sidecar.Shutdown both synchronize on the resulting
// cmdDone channel.
func (s *Sidecar) Spawn(ctx context.Context) error {
	bin, err := s.resolveBinary()
	if err != nil {
		return err
	}
	if s.cfg.SocketPath != "" {
		_ = os.Remove(s.cfg.SocketPath)
	}
	args := append([]string{}, s.extraArg...)
	args = append(args, "serve", "--socket", s.cfg.SocketPath, "--bundle-root", s.cfg.BundleRoot)
	cmd := exec.Command(bin, args...)
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.cmdDone = done
	s.waitErr = nil
	s.mu.Unlock()
	s.cmdReaped.Store(false)
	s.state.Store(int32(StateStarting))
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.waitErr = err
		s.mu.Unlock()
		s.cmdReaped.Store(true)
		close(done)
	}()
	return nil
}

// Wait blocks until the child exits. The Supervisor uses this to detect
// crashes and trigger backoff + re-spawn. Returns nil if cmd was never
// started. Safe to call concurrently with Shutdown — both synchronize on
// the shared cmdDone channel; only the dedicated goroutine in Spawn ever
// calls exec.Cmd.Wait().
func (s *Sidecar) Wait() error {
	s.mu.Lock()
	done := s.cmdDone
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitErr
}

// WaitReady polls /health every 500ms until the sidecar is operational,
// ctx is canceled, or ceiling elapses (whichever first).
//
// "Operational" is defined as ready=true OR compatibility="unknown".
// compatibility="unknown" means the sidecar process is alive and responsive
// but has no bundle loaded yet. Returning nil here lets onReady fire so the
// puller loop starts and downloads the first bundle. During that window,
// Service.Query delegates to the sidecar which returns an error envelope
// (no data), ClassifyHTTP returns ClassUnavailable, and the tool falls back
// to session_search + MEMORY.md — correct degraded behavior, not data loss.
// Only compatibility="incompatible" (wrong bundle schema) or a silent process
// should keep the gate open.
//
// On deadline, returns *ReadyTimeoutError carrying the most recent /health
// snapshot so the supervisor can classify schema-mismatch lockouts (typed
// reason tlm_binary_too_old) and surface a repair_needed block on GET /status.
func (s *Sidecar) WaitReady(ctx context.Context, ceiling time.Duration) error {
	c := NewClient(s.cfg.SocketPath, 1*time.Second)
	deadline := time.Now().Add(ceiling)
	var lastCompat, lastSub, lastVer string
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return &ReadyTimeoutError{
				LastCompatibility: lastCompat,
				LastSubCode:       lastSub,
				LastBundleVersion: lastVer,
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		h, err := c.Health(probeCtx)
		cancel()
		if err == nil {
			if h.Compatibility != "" {
				lastCompat = h.Compatibility
			}
			if sub := h.Error.SubCode(); sub != "" {
				lastSub = sub
			}
			if h.BundleVersion != "" {
				lastVer = h.BundleVersion
			}
			if h.Ready || h.Compatibility == "unknown" {
				s.state.Store(int32(StateReady))
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Shutdown sends SIGTERM to the process group, waits up to grace for the
// dedicated Wait-goroutine to reap, then SIGKILLs if needed. Best-effort
// socket unlink + state reset at the end.
//
// Idempotent and concurrency-safe:
//   - Safe to call after Wait() has already reaped the child, or twice in
//     a row (cmd/cmdDone snapshot under mu; subsequent calls see nil
//     pointers and return immediately).
//   - Safe to call concurrently with Sidecar.Wait — both block on the same
//     cmdDone channel and only the Spawn-launched goroutine ever calls
//     exec.Cmd.Wait.
//
// Production trigger that motivated the concurrency fix: Kocoro Desktop
// repair-and-restart path stops the daemon (Service.Stop → Shutdown) while
// the supervisor goroutine may be blocked in Wait().
func (s *Sidecar) Shutdown(grace time.Duration) error {
	s.mu.Lock()
	cmd := s.cmd
	done := s.cmdDone
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if s.cmdReaped.Load() {
		s.mu.Lock()
		if s.cfg.SocketPath != "" {
			_ = os.Remove(s.cfg.SocketPath)
		}
		s.state.Store(int32(StateStopped))
		s.cmd = nil
		s.cmdDone = nil
		s.mu.Unlock()
		return nil
	}
	terminateProcessTree(cmd.Process, done, grace)
	s.mu.Lock()
	if s.cfg.SocketPath != "" {
		_ = os.Remove(s.cfg.SocketPath)
	}
	s.state.Store(int32(StateStopped))
	s.cmd = nil
	s.cmdDone = nil
	s.mu.Unlock()
	return nil
}

// AttachPolicy probes /health on the given socket. Never spawns. CLI/TUI use
// this — they get (ready, _) and decide whether to enable the memory tool or
// fall back. err is reserved for unexpected probe failures worth logging
// (currently always returned as nil; reserved for future use).
func AttachPolicy(ctx context.Context, socket string) (bool, error) {
	c := NewClient(socket, 1*time.Second)
	pctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	h, err := c.Health(pctx)
	if err != nil {
		return false, nil
	}
	return h.Ready, nil
}

// Spawner is the lifecycle interface the supervisor needs. *Sidecar
// already satisfies it; using an interface keeps the supervisor unit-testable
// without spawning real processes.
//
// Shutdown is required by the restart loop: when WaitReady times out without
// the child becoming ready, the supervisor calls Shutdown on the stuck child
// before re-Spawn. Without this the failed-startup process stays alive bound
// to a stale socket while subsequent Spawn calls unlink+rebind it, leaving an
// orphan process tree (production case 2026-05-22: five zombie tlm serve PIDs).
type Spawner interface {
	Spawn(ctx context.Context) error
	WaitReady(ctx context.Context, ceiling time.Duration) error
	Wait() error
	Shutdown(grace time.Duration) error
}

// Supervisor drives the spawn → wait-ready → wait → backoff → re-spawn loop
// for a Spawner. Spec §3.6: cold-start failures are recoverable — a failed
// first WaitReady is treated identically to a runtime crash (counted toward
// the budget, retried with backoff). After SidecarRestartMax attempts are
// exhausted the supervisor returns StateDegraded.
type Supervisor struct {
	sp             Spawner
	maxAttempts    int
	onReady        func()
	onDegraded     func(reason string, attempts int, detail map[string]any)
	onIncompatible func()
	readyTimeout   time.Duration
	shutdownGrace  time.Duration
	testBackoff    func(int) time.Duration // override for tests; production uses backoffSec

	incompatibleHandled bool // one-shot guard for the self-heal pull/retry hook
}

// NewSupervisor builds a Supervisor with sane defaults (10s ready timeout).
// Pass nil for onReady if the caller doesn't need a transition hook. Use
// SetReadyTimeout to honor a configured memory.sidecar_ready_timeout.
func NewSupervisor(sp Spawner, maxAttempts int, onReady func()) *Supervisor {
	return &Supervisor{
		sp:            sp,
		maxAttempts:   maxAttempts,
		onReady:       onReady,
		readyTimeout:  10 * time.Second,
		shutdownGrace: 1 * time.Second,
	}
}

// SetReadyTimeout overrides the default 10s WaitReady ceiling. Service.Start
// calls this with cfg.SidecarReadyTimeout so operator-tuned values from
// memory.sidecar_ready_timeout actually flow through.
func (s *Supervisor) SetReadyTimeout(d time.Duration) {
	if d > 0 {
		s.readyTimeout = d
	}
}

// SetShutdownGrace overrides the default 1s Shutdown grace used between
// restart attempts. Production rarely needs this; tests use a small value.
func (s *Supervisor) SetShutdownGrace(d time.Duration) {
	if d > 0 {
		s.shutdownGrace = d
	}
}

// SetOnDegraded registers a callback invoked once when the restart budget is
// exhausted OR when a confirmed schema-mismatch short-circuit fires. reason
// is one of the Reason* constants; attempts is the total spawn attempts made;
// detail carries optional fields (e.g. compatibility/sub_code/bundle_version
// when reason == ReasonBundleSchemaMismatch). Not called on ctx cancel.
func (s *Supervisor) SetOnDegraded(fn func(reason string, attempts int, detail map[string]any)) {
	s.onDegraded = fn
}

// SetOnIncompatible registers a one-shot self-heal hook. When WaitReady
// reports sustained incompatible_bundle (no_manifest or version_out_of_range),
// the supervisor invokes this callback exactly once across the lifetime of
// the Run call — intended for Service to refresh the bundle via the puller.
// On the next iteration, if the sidecar is still incompatible, the supervisor
// short-circuits to StateDegraded with ReasonBundleSchemaMismatch instead of
// burning the remaining restart budget.
func (s *Supervisor) SetOnIncompatible(fn func()) {
	s.onIncompatible = fn
}

func (s *Supervisor) backoff(n int) time.Duration {
	if s.testBackoff != nil {
		return s.testBackoff(n)
	}
	// Exponential: 1s, 2s, 4s, ...
	return time.Duration(1<<n) * time.Second
}

// Run drives the lifecycle loop. Returns the terminal state:
//   - StateDegraded if maxAttempts is exhausted, or if a confirmed
//     schema-mismatch lockout short-circuits the loop
//   - StateStopped if ctx is canceled before exhaustion
//
// onReady is invoked each time WaitReady succeeds (so the caller can flip
// service status to Ready and start the puller goroutine on first ready).
// The restart counter resets to 0 if the sidecar stays Ready continuously
// for ≥5 minutes (transient blip vs flapping — spec §4.3).
//
// Failed-startup children are Shutdown before the next Spawn so the orphan
// process+socket pair can't accumulate. Wait-then-Shutdown is a no-op via
// the Sidecar.cmdReaped guard.
func (s *Supervisor) Run(ctx context.Context) SidecarState {
	attempt := 0
	lastReason := ReasonRepeatedCrash
	var lastDetail map[string]any
	for attempt < s.maxAttempts {
		if ctx.Err() != nil {
			return StateStopped
		}
		spawnErr := s.sp.Spawn(ctx)
		spawnedAlive := false
		shortCircuit := false
		if spawnErr != nil {
			if errors.Is(spawnErr, ErrTLMNotFound) {
				lastReason = ReasonBinaryMissing
			} else {
				lastReason = ReasonExecError
			}
		} else {
			spawnedAlive = true
			waitErr := s.sp.WaitReady(ctx, s.readyTimeout)
			if waitErr == nil {
				lastReason = ReasonRepeatedCrash
				lastDetail = nil
				readyAt := time.Now()
				if s.onReady != nil {
					s.onReady()
				}
				_ = s.sp.Wait() // blocks until child exits
				spawnedAlive = false
				if time.Since(readyAt) >= 5*time.Minute {
					attempt = 0
				}
			} else if errors.Is(waitErr, ErrReadyCeilingExceeded) {
				lastReason = ReasonStartupTimeout
				var rte *ReadyTimeoutError
				if errors.As(waitErr, &rte) {
					lastDetail = map[string]any{
						"compatibility":  rte.LastCompatibility,
						"sub_code":       rte.LastSubCode,
						"bundle_version": rte.LastBundleVersion,
					}
					if rte.LastCompatibility == "incompatible" &&
						(rte.LastSubCode == "no_manifest" || rte.LastSubCode == "version_out_of_range") {
						lastReason = ReasonBundleSchemaMismatch
					}
				}
			} else {
				lastReason = ReasonHealthFailed
			}
		}
		// Kill the still-alive failed-startup child before the next iteration.
		if spawnedAlive {
			_ = s.sp.Shutdown(s.shutdownGrace)
		}
		// One-time self-heal + short-circuit on confirmed schema mismatch.
		if lastReason == ReasonBundleSchemaMismatch {
			if !s.incompatibleHandled && s.onIncompatible != nil {
				s.incompatibleHandled = true
				s.onIncompatible()
				// Fall through to attempt++ + backoff, then retry once.
			} else {
				// Self-heal already attempted (or no hook). Short-circuit
				// rather than burn the remaining attempt budget — a stale
				// tlm binary will not become compatible by being restarted.
				shortCircuit = true
			}
		}
		attempt++
		if shortCircuit {
			break
		}
		select {
		case <-ctx.Done():
			return StateStopped
		case <-time.After(s.backoff(attempt)):
		}
	}
	if s.onDegraded != nil {
		s.onDegraded(lastReason, attempt, lastDetail)
	}
	return StateDegraded
}
