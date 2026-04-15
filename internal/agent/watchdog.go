package agent

import (
	"context"
	"errors"
	"time"
)

// ErrHardIdleTimeout is the cancel cause attached when the watchdog aborts
// a run at its hard threshold. Callers use errors.Is(context.Cause(ctx), …)
// to distinguish from a user-initiated cancel.
var ErrHardIdleTimeout = errors.New("agent: hard idle timeout exceeded")

// defaultWatchdogTick is how often the watchdog samples the phase tracker.
// Small enough to be responsive, large enough not to thrash. Tests can
// override via runWatchdogWithTick.
const defaultWatchdogTick = 1 * time.Second

// runWatchdog spawns a goroutine that polls the phase tracker and fires
// onSoft / onHard when an idle-counted phase exceeds the configured
// thresholds. Returns a stop func — defer it right after the call so every
// Run() exit path releases the goroutine.
//
// Semantics:
//
//   - softTimeout == 0 disables the soft-status event.
//   - hardTimeout == 0 disables cancellation (visibility-only mode). This
//     is the default in Slice 3 rollout so production gets watchdog events
//     without any new cancel risk.
//   - Only phases where p.CountsAsIdle() returns true are measured.
//   - Soft fires at most once per phase transition (identified by the
//     tracker's seq number). A transition — including re-entering the same
//     phase type — re-arms soft.
//   - If tracker.Invalid() is ever observed, the watchdog disables itself
//     for the remainder of the run (silently, no events, no cancel).
func runWatchdog(
	parent context.Context,
	tracker *phaseTracker,
	softTimeout, hardTimeout time.Duration,
	onSoft func(phase TurnPhase, idle time.Duration),
	onHard func(phase TurnPhase, idle time.Duration),
	cancelCause func(error),
) (stop func()) {
	return runWatchdogWithTick(parent, tracker, softTimeout, hardTimeout,
		defaultWatchdogTick, onSoft, onHard, cancelCause)
}

func runWatchdogWithTick(
	parent context.Context,
	tracker *phaseTracker,
	softTimeout, hardTimeout, tick time.Duration,
	onSoft func(phase TurnPhase, idle time.Duration),
	onHard func(phase TurnPhase, idle time.Duration),
	cancelCause func(error),
) (stop func()) {
	if tracker == nil || (softTimeout <= 0 && hardTimeout <= 0) {
		return func() {}
	}
	if tick <= 0 {
		tick = defaultWatchdogTick
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()

		// Dedup soft firing by transition seq, not by phase type. Re-entering
		// the same phase bumps seq and re-arms soft naturally.
		var (
			lastSeqSeen int64 = -1
			softFired   bool
		)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tracker.Invalid() {
					// Tracker lost integrity; disable ourselves for the rest
					// of the run rather than act on untrustworthy data.
					return
				}
				phase, idle, seq := tracker.Current()
				if seq != lastSeqSeen {
					lastSeqSeen = seq
					softFired = false
				}
				if !phase.CountsAsIdle() {
					continue
				}
				if softTimeout > 0 && !softFired && idle >= softTimeout {
					softFired = true
					if onSoft != nil {
						onSoft(phase, idle)
					}
				}
				if hardTimeout > 0 && idle >= hardTimeout {
					if onHard != nil {
						onHard(phase, idle)
					}
					if cancelCause != nil {
						cancelCause(ErrHardIdleTimeout)
					}
					return
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
