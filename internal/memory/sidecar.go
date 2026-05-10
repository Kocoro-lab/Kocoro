package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
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

// Failure reason strings surfaced via Supervisor.onDegraded and stored in
// Service.disabledReason for the GET /status memory field.
const (
	ReasonBinaryMissing       = "tlm_binary_missing"
	ReasonExecError           = "tlm_exec_error"
	ReasonHealthFailed        = "tlm_health_failed"
	ReasonStartupTimeout      = "startup_timeout"
	ReasonRepeatedCrash       = "repeated_crash"
	ReasonCloudMisconfigured  = "cloud_misconfigured"
)

// Sidecar is the managed child-process handle for the local memory sidecar.
// Daemon-only: only Service.Start (in daemon mode) constructs and operates a
// Sidecar. CLI/TUI use AttachPolicy instead — they never spawn.
type Sidecar struct {
	cfg      Config
	extraArg []string // test injection: prefix args before "serve --socket --bundle-root"
	cmd      *exec.Cmd
	state    atomic.Int32
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
// Sets PGID so Shutdown can SIGTERM the whole process group.
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	s.cmd = cmd
	s.state.Store(int32(StateStarting))
	return nil
}

// Wait blocks until the child exits. The Supervisor uses this to detect
// crashes and trigger backoff + re-spawn. Returns nil if cmd was never started.
func (s *Sidecar) Wait() error {
	if s.cmd == nil {
		return nil
	}
	return s.cmd.Wait()
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
func (s *Sidecar) WaitReady(ctx context.Context, ceiling time.Duration) error {
	c := NewClient(s.cfg.SocketPath, 1*time.Second)
	deadline := time.Now().Add(ceiling)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return ErrReadyCeilingExceeded
		}
		probeCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		h, err := c.Health(probeCtx)
		cancel()
		if err == nil && (h.Ready || h.Compatibility == "unknown") {
			s.state.Store(int32(StateReady))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Shutdown sends SIGTERM to the process group, waits up to grace, then SIGKILLs
// if needed. Best-effort socket unlink at the end.
func (s *Sidecar) Shutdown(grace time.Duration) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(s.cmd.Process.Pid)
	if err != nil {
		// Fallback: signal the process directly.
		pgid = s.cmd.Process.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(1 * time.Second):
		}
	}
	if s.cfg.SocketPath != "" {
		_ = os.Remove(s.cfg.SocketPath)
	}
	s.state.Store(int32(StateStopped))
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
type Spawner interface {
	Spawn(ctx context.Context) error
	WaitReady(ctx context.Context, ceiling time.Duration) error
	Wait() error
}

// Supervisor drives the spawn → wait-ready → wait → backoff → re-spawn loop
// for a Spawner. Spec §3.6: cold-start failures are recoverable — a failed
// first WaitReady is treated identically to a runtime crash (counted toward
// the budget, retried with backoff). After SidecarRestartMax attempts are
// exhausted the supervisor returns StateDegraded.
type Supervisor struct {
	sp           Spawner
	maxAttempts  int
	onReady      func()
	onDegraded   func(reason string, attempts int)
	readyTimeout time.Duration
	testBackoff  func(int) time.Duration // override for tests; production uses backoffSec
}

// NewSupervisor builds a Supervisor with sane defaults (10s ready timeout).
// Pass nil for onReady if the caller doesn't need a transition hook. Use
// SetReadyTimeout to honor a configured memory.sidecar_ready_timeout.
func NewSupervisor(sp Spawner, maxAttempts int, onReady func()) *Supervisor {
	return &Supervisor{
		sp:           sp,
		maxAttempts:  maxAttempts,
		onReady:      onReady,
		readyTimeout: 10 * time.Second,
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

// SetOnDegraded registers a callback invoked once when the restart budget is
// exhausted. reason is one of the Reason* constants; attempts is the total
// spawn attempts made. Not called on ctx cancel (StateStopped).
func (s *Supervisor) SetOnDegraded(fn func(reason string, attempts int)) {
	s.onDegraded = fn
}

func (s *Supervisor) backoff(n int) time.Duration {
	if s.testBackoff != nil {
		return s.testBackoff(n)
	}
	// Exponential: 1s, 2s, 4s, ...
	return time.Duration(1<<n) * time.Second
}

// Run drives the lifecycle loop. Returns the terminal state:
//   - StateDegraded if maxAttempts is exhausted without sustained readiness
//   - StateStopped if ctx is canceled before exhaustion
//
// onReady is invoked each time WaitReady succeeds (so the caller can flip
// service status to Ready and start the puller goroutine on first ready).
// The restart counter resets to 0 if the sidecar stays Ready continuously
// for ≥5 minutes (transient blip vs flapping — spec §4.3).
func (s *Supervisor) Run(ctx context.Context) SidecarState {
	attempt := 0
	lastReason := ReasonRepeatedCrash
	for attempt < s.maxAttempts {
		if ctx.Err() != nil {
			return StateStopped
		}
		spawnErr := s.sp.Spawn(ctx)
		if spawnErr != nil {
			if errors.Is(spawnErr, ErrTLMNotFound) {
				lastReason = ReasonBinaryMissing
			} else {
				lastReason = ReasonExecError
			}
		} else {
			waitErr := s.sp.WaitReady(ctx, s.readyTimeout)
			if waitErr == nil {
				lastReason = ReasonRepeatedCrash
				readyAt := time.Now()
				if s.onReady != nil {
					s.onReady()
				}
				_ = s.sp.Wait() // blocks until child exits
				if time.Since(readyAt) >= 5*time.Minute {
					attempt = 0
				}
			} else if errors.Is(waitErr, ErrReadyCeilingExceeded) {
				lastReason = ReasonStartupTimeout
			} else {
				lastReason = ReasonHealthFailed
			}
		}
		attempt++
		select {
		case <-ctx.Done():
			return StateStopped
		case <-time.After(s.backoff(attempt)):
		}
	}
	if s.onDegraded != nil {
		s.onDegraded(lastReason, attempt)
	}
	return StateDegraded
}
