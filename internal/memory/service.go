package memory

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type ServiceStatus int32

const (
	StatusDisabled ServiceStatus = iota
	StatusInitializing
	StatusReady
	StatusDegraded
	StatusUnavailable
)

func (s ServiceStatus) String() string {
	return [...]string{"disabled", "initializing", "ready", "degraded", "unavailable"}[s]
}

// Service is the orchestrator that daemon code and the memory_recall tool
// talk to. It owns the sidecar lifecycle (in daemon mode; CLI/TUI use
// AttachPolicy + NewServiceAttached instead) and coordinates the bundle
// puller goroutine. Tool fallback is triggered whenever Status() != Ready.
type Service struct {
	cfg      Config
	audit    AuditLogger
	sidecar  *Sidecar
	puller   *Puller
	client   *Client
	status   atomic.Int32
	cancel   context.CancelFunc
	attached bool // true for NewServiceAttached path; never spawns

	// Phase 2.5b: reason tracking for MemoryProviderStatus.
	disabledReason  atomic.Pointer[string]
	restartAttempts atomic.Int32

	// 2026-05-22: typed detail for the GET /status memory.detail block —
	// carries {compatibility, sub_code, bundle_version} from the supervisor's
	// last WaitReady observation. Surfaced as repair_needed when reason is
	// ReasonBundleSchemaMismatch so Kocoro Desktop can prompt for tlm reinstall.
	disabledDetail atomic.Pointer[map[string]any]

	// pullNow is a buffered(1) channel that wakes the puller for an
	// out-of-schedule bundle check. NotifySyncDone sends to it; nil on
	// NewServiceAttached (CLI/TUI never runs a puller loop).
	pullNow chan struct{}

	// Test injection: extra positional args prepended to "serve --socket
	// --bundle-root" so unit tests can run a fake binary (e.g. python3 with
	// a script path). Production callers leave this nil.
	testExtraSpawnArgs []string
}

// NewService builds the daemon-mode Service that owns sidecar lifecycle.
func NewService(cfg Config, audit AuditLogger) *Service {
	return &Service{cfg: cfg, audit: audit, pullNow: make(chan struct{}, 1)}
}

// NewServiceAttached builds a Service for the CLI/TUI attach-only path.
// AttachPolicy must have already confirmed a reachable sidecar before this
// is constructed; the returned Service never spawns.
func NewServiceAttached(cfg Config, audit AuditLogger) *Service {
	return &Service{cfg: cfg, audit: audit, attached: true}
}

func (s *Service) Status() ServiceStatus { return ServiceStatus(s.status.Load()) }

func (s *Service) logAudit(ev string, fields map[string]any) {
	if s.audit != nil {
		s.audit.Log(ev, fields)
	}
}

// setDisabledReason stores r as the current failure reason. Pass "" to clear.
func (s *Service) setDisabledReason(r string) {
	if r == "" {
		s.disabledReason.Store(nil)
	} else {
		s.disabledReason.Store(&r)
	}
}

// setDisabledDetail stores the supervisor's last WaitReady observation as
// the repair_needed payload. Pass nil to clear.
func (s *Service) setDisabledDetail(d map[string]any) {
	if d == nil {
		s.disabledDetail.Store(nil)
		return
	}
	cp := make(map[string]any, len(d))
	for k, v := range d {
		cp[k] = v
	}
	s.disabledDetail.Store(&cp)
}

// MemoryProviderStatus returns the atomic snapshot of the memory feature
// state for inclusion in the daemon GET /status response.
func (s *Service) MemoryProviderStatus() MemoryStatus {
	st := ServiceStatus(s.status.Load())
	switch st {
	case StatusReady, StatusInitializing:
		return MemoryStatus{Provider: "enabled"}
	case StatusDisabled:
		return MemoryStatus{Provider: "disabled"}
	default: // StatusUnavailable, StatusDegraded
		ms := MemoryStatus{
			Provider: "disabled",
			Reason:   s.disabledReason.Load(),
		}
		if st == StatusDegraded {
			ms.Detail = map[string]any{
				"restart_attempts": int(s.restartAttempts.Load()),
			}
			// Attach the supervisor's last WaitReady observation as a
			// repair_needed block when the failure is a schema-mismatch
			// lockout. Desktop reads this to drive its on-demand tlm install.
			if reason := s.disabledReason.Load(); reason != nil && *reason == ReasonBundleSchemaMismatch {
				if d := s.disabledDetail.Load(); d != nil && len(*d) > 0 {
					ms.Detail["repair_needed"] = *d
				}
			}
		}
		return ms
	}
}

// NotifySyncDone wakes the puller for an out-of-schedule bundle check after
// a successful session sync. Non-blocking: if a wakeup is already pending
// the call is a no-op. Safe to call from any goroutine.
func (s *Service) NotifySyncDone() {
	if s.pullNow == nil {
		return
	}
	select {
	case s.pullNow <- struct{}{}:
	default:
	}
}

// tlmAvailable reports whether the configured (or PATH-resolved) sidecar
// binary is callable. A bare command name (e.g. "tlm" or "python3" in tests)
// is resolved via exec.LookPath; an absolute path is checked via os.Stat.
func (s *Service) tlmAvailable() bool {
	if s.cfg.TLMPath != "" {
		if _, err := os.Stat(s.cfg.TLMPath); err == nil {
			return true
		}
		if _, err := exec.LookPath(s.cfg.TLMPath); err == nil {
			return true
		}
		return false
	}
	_, err := exec.LookPath("tlm")
	return err == nil
}

// Start runs the cold-path gates from spec §3.6 (steps 1-3) and, if all
// gates pass, spawns the supervisor goroutine that owns sidecar lifecycle
// and (in cloud mode) the bundle puller.
//
// All failure modes are silent: the function returns nil even when the
// service is Unavailable or Disabled. Callers check Status() to decide
// whether to proceed.
func (s *Service) Start(ctx context.Context) error {
	if s.cfg.Provider == "disabled" || s.cfg.Provider == "" {
		s.status.Store(int32(StatusDisabled))
		return nil
	}
	if !s.tlmAvailable() {
		s.setDisabledReason(ReasonBinaryMissing)
		s.status.Store(int32(StatusUnavailable))
		s.logAudit("memory_tlm_missing", map[string]any{"tlm_path_set": s.cfg.TLMPath != ""})
		return nil
	}
	if s.cfg.Provider == "cloud" {
		if s.cfg.Endpoint == "" || s.cfg.APIKey == "" {
			s.setDisabledReason(ReasonCloudMisconfigured)
			s.status.Store(int32(StatusUnavailable))
			s.logAudit("memory_cloud_misconfigured", map[string]any{
				"endpoint_resolved": s.cfg.Endpoint != "",
				"api_key_present":   s.cfg.APIKey != "",
			})
			return nil
		}
	}
	s.status.Store(int32(StatusInitializing))

	// Cold-start bootstrap (cloud-mode only): sidecar reports ready=false
	// until a bundle exists, but the puller only starts from onReady. Break
	// the cycle by pulling synchronously here when `current` is missing,
	// dangling, or points at a bundle whose manifest.json is unreadable as
	// JSON. This catches the "missing/corrupt" case only — manifests that
	// parse as JSON but contain newer fields the local tlm binary's dataclass
	// can't unmarshal look readable here and must be handled by the
	// supervisor's onIncompatible self-heal path (real schema-mismatch fix).
	if s.cfg.Provider == "cloud" {
		if !currentBundleReadable(s.cfg.BundleRoot) {
			boot := NewPuller(s.cfg, nil, s.audit)
			if tickErr := boot.tick(ctx); tickErr != nil {
				s.logAudit("memory_bootstrap_pull_failed", map[string]any{"reason": tickErr.Error()})
			} else {
				s.logAudit("memory_bootstrap_pull_ok", map[string]any{})
			}
		}
	}

	// Spawn the supervisor goroutine. It owns the full spawn →
	// wait-ready → wait → backoff loop. Cold-start failures (failed first
	// WaitReady) are treated identically to runtime crashes — no daemon
	// restart required to recover from a slow-disk first boot.
	s.sidecar = NewSidecar(s.cfg, s.testExtraSpawnArgs)
	supCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	var pullerOnce sync.Once
	onReady := func() {
		s.status.Store(int32(StatusReady))
		s.client = NewClient(s.cfg.SocketPath, s.cfg.ClientRequestTimeout)
		if s.cfg.Provider == "cloud" {
			pullerOnce.Do(func() {
				s.puller = NewPuller(s.cfg, s.sidecar, s.audit)
				go s.runPullerLoop(supCtx)
			})
		}
	}

	sup := NewSupervisor(s.sidecar, s.cfg.SidecarRestartMax, onReady)
	sup.SetReadyTimeout(s.cfg.SidecarReadyTimeout)
	sup.SetOnDegraded(func(reason string, attempts int, detail map[string]any) {
		s.restartAttempts.Store(int32(attempts))
		s.setDisabledReason(reason)
		s.setDisabledDetail(detail)
		s.status.Store(int32(StatusDegraded))
		fields := map[string]any{
			"reason":           reason,
			"restart_attempts": attempts,
		}
		for k, v := range detail {
			fields[k] = v
		}
		s.logAudit("memory_sidecar_degraded", fields)
	})
	// One-shot self-heal hook: cloud-mode only. When the supervisor sees a
	// sustained incompatible_bundle/(no_manifest|version_out_of_range) it
	// invokes this callback once, then on continued mismatch short-circuits
	// to StateDegraded with ReasonBundleSchemaMismatch — so we burn one
	// puller round-trip, not the full restart budget. Local-mode users have
	// no upstream to pull from so we skip the hook (supervisor will
	// short-circuit on first detection instead of retrying pointlessly).
	if s.cfg.Provider == "cloud" {
		sup.SetOnIncompatible(func() {
			s.logAudit("memory_self_heal_attempt", map[string]any{
				"trigger": "incompatible_bundle",
			})
			boot := NewPuller(s.cfg, nil, s.audit)
			if err := boot.tick(supCtx); err != nil {
				s.logAudit("memory_self_heal_failed", map[string]any{"reason": err.Error()})
			} else {
				s.logAudit("memory_self_heal_ok", map[string]any{})
			}
		})
	}
	go func() {
		sup.Run(supCtx)
		// Status and reason already set by onDegraded.
		// StateStopped (ctx cancel via Stop()) needs no action.
	}()
	return nil
}

// currentBundleReadable reports whether <bundleRoot>/current resolves to a
// directory that contains a manifest.json the Go side can JSON-decode. It
// returns false when:
//   - the `current` path is missing or a dangling symlink
//   - the target is not a directory
//   - manifest.json is missing
//   - manifest.json contents are not valid JSON
//
// It does NOT validate that the manifest is loadable by the local tlm
// binary's dataclass. The production 2026-05-22 lockout had a JSON-valid
// manifest carrying newer fields the Apr 21 binary couldn't accept; that
// case looks readable here and is the supervisor's onIncompatible path.
func currentBundleReadable(bundleRoot string) bool {
	currentPath := filepath.Join(bundleRoot, "current")
	st, err := os.Stat(currentPath) // follows the symlink
	if err != nil || !st.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(currentPath, "manifest.json"))
	if err != nil {
		return false
	}
	var probe map[string]any
	return json.Unmarshal(data, &probe) == nil
}

// runPullerLoop runs the 24h bundle pull ticker. Honors
// BundlePullStartupDelay, exits on ctx cancel. Cloud-mode only (caller
// gates this in Start).
func (s *Service) runPullerLoop(ctx context.Context) {
	if s.cfg.BundlePullStartupDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.BundlePullStartupDelay):
		}
	}
	if err := s.puller.tick(ctx); err != nil {
		s.logAudit("memory_reload_failed", map[string]any{"reason": err.Error()})
	}
	interval := s.cfg.BundlePullInterval
	if interval <= 0 {
		return // misconfigured; no recurring ticks
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.puller.tick(ctx); err != nil {
				s.logAudit("memory_reload_failed", map[string]any{"reason": err.Error()})
			}
		case <-s.pullNow:
			if err := s.puller.tick(ctx); err != nil {
				s.logAudit("memory_reload_failed", map[string]any{"reason": err.Error()})
			}
		}
	}
}

// Query is the only entry point the memory_recall tool needs. Returns
// ClassUnavailable whenever the service is not Ready (so the tool falls
// back instead of erroring).
func (s *Service) Query(ctx context.Context, intent QueryIntent) (*ResponseEnvelope, ErrorClass, error) {
	if s.Status() != StatusReady || s.client == nil {
		return nil, ClassUnavailable, nil
	}
	return s.client.Query(ctx, intent)
}

// Stop cancels the supervisor + puller goroutines and shuts the sidecar
// down within the configured grace period. Best-effort — daemon shutdown
// does not block on this.
func (s *Service) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.sidecar != nil {
		return s.sidecar.Shutdown(s.cfg.SidecarShutdownGrace)
	}
	return nil
}
