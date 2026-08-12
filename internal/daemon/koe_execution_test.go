package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const daemonTestArgumentsDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func activeExecutionRun(mode executionprofile.Mode, runID string) executionprofile.Run {
	return executionprofile.Run{
		LogicalTaskID: "burst:t01",
		RunID:         runID,
		Profile: executionprofile.Profile{
			RequestedMode: mode,
			EffectiveMode: mode,
		},
	}
}

func TestDecideKoeFollowUpBeforeGenericInjection(t *testing.T) {
	fast := activeExecutionRun(executionprofile.ModeFast, "ker1_fast")
	full := activeExecutionRun(executionprofile.ModeFull, "ker1_full")
	tests := []struct {
		name   string
		active executionprofile.Run
		req    RunAgentRequest
		want   koeFollowUpAction
	}{
		{
			name:   "fast to full creates child",
			active: fast,
			req: RunAgentRequest{
				Source: "koe", ExecutionMode: executionprofile.ModeFull,
				LogicalTaskID: "burst:t01", ExecutionRunID: "ker1_child", ParentRunID: "ker1_fast",
			},
			want: koeFollowUpChild,
		},
		{
			name:   "same fast run injects",
			active: fast,
			req: RunAgentRequest{
				Source: "koe", ExecutionMode: executionprofile.ModeFast,
				LogicalTaskID: "burst:t01", ExecutionRunID: "ker1_fast",
			},
			want: koeFollowUpInject,
		},
		{
			name:   "full to fast remains on full lineage",
			active: full,
			req: RunAgentRequest{
				Source: "koe", ExecutionMode: executionprofile.ModeFast,
				LogicalTaskID: "burst:t01", ExecutionRunID: "ker1_full",
			},
			want: koeFollowUpInject,
		},
		{
			name:   "pre-minted child survives parent resolver fallback",
			active: full,
			req: RunAgentRequest{
				Source: "koe", ExecutionMode: executionprofile.ModeFull,
				LogicalTaskID: "burst:t01", ExecutionRunID: "ker1_child", ParentRunID: "ker1_full",
			},
			want: koeFollowUpChild,
		},
		{
			name:   "non Koe never changes injection policy",
			active: fast,
			req:    RunAgentRequest{Source: "desktop", ExecutionMode: executionprofile.ModeFull},
			want:   koeFollowUpInject,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideKoeFollowUp(tc.active, tc.req); got != tc.want {
				t.Fatalf("decideKoeFollowUp() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFullAdmissionRunsBeforeFastToFullFork(t *testing.T) {
	active := activeExecutionRun(executionprofile.ModeFast, "ker1_fast")
	rawFast := "fast"
	rawFull := "full"

	contradictoryFast := RunAgentRequest{
		Source: "koe", ExecutionMode: executionprofile.ModeFull,
		RequestedExecutionMode: &rawFast,
		FullReason:             executionprofile.FullReasonProductionIncident,
		LogicalTaskID:          "burst:t01",
		ExecutionRunID:         "ker1_fast",
	}
	applyKoeModeAdmission(&contradictoryFast)
	if contradictoryFast.ExecutionMode != executionprofile.ModeFull ||
		contradictoryFast.ModeAdmission.AdmittedFullReason != executionprofile.FullReasonProductionIncident ||
		contradictoryFast.ModeAdmission.DecisionReason != executionprofile.AdmissionFastReasonConflict {
		t.Fatalf("contradictory Fast did not fail closed: %+v", contradictoryFast.ModeAdmission)
	}
	if got := decideKoeFollowUp(active, contradictoryFast); got != koeFollowUpChild {
		t.Fatalf("contradictory Fast fork = %q, want isolated Full child run", got)
	}

	selectedFull := RunAgentRequest{
		Source: "koe", ExecutionMode: executionprofile.ModeFast,
		RequestedExecutionMode: &rawFull,
		FullReason:             executionprofile.FullReasonNone,
		LogicalTaskID:          "burst:t01",
		ExecutionRunID:         "ker1_child",
		ParentRunID:            "ker1_fast",
	}
	applyKoeModeAdmission(&selectedFull)
	if selectedFull.ExecutionMode != executionprofile.ModeFull ||
		selectedFull.ModeAdmission.AdmittedFullReason != "" {
		t.Fatalf("selected Full admission = %+v mode=%q", selectedFull.ModeAdmission, selectedFull.ExecutionMode)
	}
	if got := decideKoeFollowUp(active, selectedFull); got != koeFollowUpChild {
		t.Fatalf("selected Full fork = %q, want child run", got)
	}
}

func TestExecutionRunUpsertKeepsProfileImmutableAndEvidenceReplaySafe(t *testing.T) {
	sess := &session.Session{}
	run := executionprofile.Run{
		LogicalTaskID: "burst:t01",
		RunID:         "ker1_parent",
		Profile: executionprofile.Profile{
			RequestedMode:    executionprofile.ModeFull,
			EffectiveMode:    executionprofile.ModeFull,
			ResolutionReason: "requested_full",
		},
		Evidence: executionprofile.Evidence{ToolOutcomes: []executionprofile.ToolOutcomeEvidence{{
			ToolCallID: "write-1", ToolName: "file_write", Validated: true,
			Outcome: "succeeded", SideEffect: true,
			ArgumentsDigest: daemonTestArgumentsDigest, ResultDigest: "abc",
		}}},
	}
	if err := upsertExecutionRun(sess, run); err != nil {
		t.Fatal(err)
	}
	// Re-checkpointing the same run updates mutable execution state in place,
	// never appends a second generation that recovery could interpret as work
	// to replay.
	run.Evidence.ToolOutcomes[0].ResultDigest = "def"
	run.ComputerActivation = &executionprofile.ComputerActivation{
		Profile:            anthropicComputerProfileForDaemonTest(),
		ToolName:           "computer",
		ToolsetFingerprint: "sha256:toolset-v1",
	}
	if err := upsertExecutionRun(sess, run); err != nil {
		t.Fatal(err)
	}
	if len(sess.ExecutionRuns) != 1 || len(sess.ExecutionRuns[0].Evidence.ToolOutcomes) != 1 ||
		!sess.ExecutionRuns[0].Evidence.ToolOutcomes[0].SideEffect ||
		sess.ExecutionRuns[0].ComputerActivation == nil ||
		sess.ExecutionRuns[0].ComputerActivation.Profile.ProfileID != "ep1_daemon-computer" ||
		sess.ExecutionRuns[0].ComputerActivation.ToolName != "computer" {
		t.Fatalf("execution ledger duplicated or lost side-effect evidence: %+v", sess.ExecutionRuns)
	}
	run.ComputerActivation.ToolName = "mutated-after-checkpoint"
	if sess.ExecutionRuns[0].ComputerActivation.ToolName != "computer" {
		t.Fatalf("checkpoint retained caller-owned computer activation: %+v", sess.ExecutionRuns[0])
	}
	conflict := run
	conflict.Profile.ResolutionReason = "mutated"
	if err := upsertExecutionRun(sess, conflict); err == nil {
		t.Fatal("same run accepted mutated immutable profile")
	}
}

func TestInheritParentExecutionEvidence(t *testing.T) {
	parent := activeExecutionRun(executionprofile.ModeFast, "ker1_parent")
	parent.Profile = fastProfileForDaemonTest()
	parent.Evidence = executionprofile.Evidence{
		ToolOutcomes: []executionprofile.ToolOutcomeEvidence{{
			ToolCallID: "write-1", ToolName: "file_write", Validated: true,
			Outcome: "succeeded", SideEffect: true,
			ArgumentsDigest: daemonTestArgumentsDigest, ResultDigest: "result-digest",
		}},
		Deliverables: []executionprofile.DeliverableEvidence{{
			ID: "artifact-1", Filename: "result.txt", ByteSize: 12,
		}},
	}

	child := activeExecutionRun(executionprofile.ModeFull, "ker1_child")
	child.ParentRunID = parent.RunID
	child.Profile = executionprofile.FullProfile(executionprofile.ModeFull, "requested_full")
	if err := inheritParentExecutionEvidence(&child, []executionprofile.Run{parent}); err != nil {
		t.Fatal(err)
	}
	if child.ComputerActivation != nil ||
		len(child.Evidence.ToolOutcomes) != 1 ||
		child.Evidence.ToolOutcomes[0].ArgumentsDigest != daemonTestArgumentsDigest ||
		len(child.Evidence.Deliverables) != 1 ||
		child.Evidence.Deliverables[0].ID != "artifact-1" {
		t.Fatalf("child inherited evidence = %+v", child)
	}

	// Evidence is copied in both directions: later checkpoint mutation in
	// either generation must not alter the other run's ledger row.
	parent.Evidence.ToolOutcomes[0].ToolName = "mutated-parent"
	parent.Evidence.Deliverables[0].Filename = "mutated-parent.txt"
	if child.Evidence.ToolOutcomes[0].ToolName != "file_write" ||
		child.Evidence.Deliverables[0].Filename != "result.txt" {
		t.Fatalf("child retained parent-owned evidence slices: %+v", child.Evidence)
	}
	child.Evidence.ToolOutcomes[0].ToolName = "mutated-child"
	if parent.Evidence.ToolOutcomes[0].ToolName != "mutated-parent" {
		t.Fatalf("parent retained child-owned evidence slices: %+v", parent.Evidence)
	}
}

func TestInheritParentExecutionEvidenceFailsClosed(t *testing.T) {
	parent := activeExecutionRun(executionprofile.ModeFast, "ker1_parent")
	parent.Profile = fastProfileForDaemonTest()
	parent.Evidence.ToolOutcomes = []executionprofile.ToolOutcomeEvidence{{
		ToolCallID: "write-1", ToolName: "file_write", Validated: true,
		Outcome: "succeeded", SideEffect: true,
		ArgumentsDigest: daemonTestArgumentsDigest,
	}}
	newChild := func() executionprofile.Run {
		child := activeExecutionRun(executionprofile.ModeFull, "ker1_child")
		child.ParentRunID = parent.RunID
		child.Profile = executionprofile.FullProfile(executionprofile.ModeFull, "requested_full")
		return child
	}

	tests := []struct {
		name   string
		mutate func(*executionprofile.Run, *[]executionprofile.Run)
	}{
		{
			name: "missing parent",
			mutate: func(_ *executionprofile.Run, ledger *[]executionprofile.Run) {
				*ledger = nil
			},
		},
		{
			name: "duplicate parent",
			mutate: func(_ *executionprofile.Run, ledger *[]executionprofile.Run) {
				*ledger = append(*ledger, cloneExecutionRun(parent))
			},
		},
		{
			name: "logical task mismatch",
			mutate: func(child *executionprofile.Run, _ *[]executionprofile.Run) {
				child.LogicalTaskID = "burst:other"
			},
		},
		{
			name: "invalid parent evidence",
			mutate: func(_ *executionprofile.Run, ledger *[]executionprofile.Run) {
				(*ledger)[0].Evidence.ToolOutcomes[0].ArgumentsDigest = ""
			},
		},
		{
			name: "prepopulated child evidence",
			mutate: func(child *executionprofile.Run, _ *[]executionprofile.Run) {
				child.Evidence.Deliverables = []executionprofile.DeliverableEvidence{{
					ID: "unexpected",
				}}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			child := newChild()
			ledger := []executionprofile.Run{cloneExecutionRun(parent)}
			tc.mutate(&child, &ledger)
			err := inheritParentExecutionEvidence(&child, ledger)
			if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) {
				t.Fatalf("error = %v, want ErrInvalidPersistedRun", err)
			}
		})
	}
}

func TestPrepareActiveFastToFullChildWaitsForSafeRouteBoundary(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:burst"
	sessionsDir := t.TempDir()
	sc.LockRouteWithManager(routeKey, sessionsDir)
	injectCh := make(chan agent.InjectedMessage, 1)
	parentDone := make(chan struct{})
	sc.SetRouteActiveRunState(
		routeKey,
		parentDone,
		injectCh,
		"",
		activeExecutionRun(executionprofile.ModeFast, "ker1_parent"),
	)
	var parentCanceled atomic.Bool
	sc.SetRouteCancel(routeKey, func() { parentCanceled.Store(true) })

	req, child := prepareActiveKoeChild(sc, routeKey, RunAgentRequest{
		Source: "koe", RouteKey: routeKey, ExecutionMode: executionprofile.ModeFull,
		LogicalTaskID: "burst:t01", ExecutionRunID: "ker1_child",
	})
	if !child || !req.WaitForRouteBoundary ||
		req.ParentRunID != "ker1_parent" || req.ExecutionRunID != "ker1_child" ||
		req.routeBoundaryGeneration == 0 || req.routeBoundaryDone != parentDone {
		t.Fatalf("prepared child = %t req=%+v", child, req)
	}
	select {
	case msg := <-injectCh:
		t.Fatalf("full child was injected into active fast run: %+v", msg)
	default:
	}

	type lockResult struct {
		entry *routeEntry
		err   error
	}
	acquired := make(chan lockResult, 1)
	go func() {
		entry, err := sc.LockRouteWithManagerAtSafeBoundary(
			context.Background(),
			routeKey,
			sessionsDir,
			req.routeBoundaryGeneration,
			req.routeBoundaryCancelGeneration,
			req.routeBoundaryDone,
		)
		acquired <- lockResult{entry: entry, err: err}
	}()
	select {
	case <-acquired:
		t.Fatal("child acquired route while parent run was still active")
	case <-time.After(30 * time.Millisecond):
	}
	if parentCanceled.Load() {
		t.Fatal("waiting child canceled its active parent")
	}
	sc.ClearRouteRunState(routeKey)
	close(parentDone)
	sc.UnlockRoute(routeKey)
	select {
	case got := <-acquired:
		if got.err != nil || got.entry == nil {
			t.Fatalf("safe-boundary acquire = (%v, %v)", got.entry, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("child did not acquire route after parent safe boundary")
	}
	sc.UnlockRoute(routeKey)
}

func TestKoeInjectionRaceMintsDistinctPersistedChild(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:inject-race"
	sc.LockRouteWithManager(routeKey, t.TempDir())

	parent := persistedFastRunForDaemonTest()
	parentDone := make(chan struct{})
	sc.SetRouteActiveRunState(
		routeKey,
		parentDone,
		make(chan agent.InjectedMessage, 1),
		"",
		parent,
	)
	req, child := prepareActiveKoeChild(sc, routeKey, RunAgentRequest{
		Source:         "koe",
		RouteKey:       routeKey,
		ExecutionMode:  executionprofile.ModeFast,
		LogicalTaskID:  parent.LogicalTaskID,
		ExecutionRunID: parent.RunID,
	})
	if child {
		t.Fatal("same-profile follow-up should first attempt active-run injection")
	}
	if req.AuthoritativeActiveParent.RunID != parent.RunID {
		t.Fatalf("active parent authority was not captured: %+v", req.AuthoritativeActiveParent)
	}

	// Deterministically close the injection window after the active snapshot
	// but before delivery. The HTTP path now falls through to a new AgentLoop.
	sc.ClearRouteRunState(routeKey)
	close(parentDone)
	sc.UnlockRoute(routeKey)
	if got := sc.InjectMessage(routeKey, agent.InjectedMessage{Text: "follow up"}); got != InjectNoActiveRun {
		t.Fatalf("post-teardown inject = %v, want InjectNoActiveRun", got)
	}

	req.ExecutionRun = cloneExecutionRun(parent)
	req.ExecutionRun.Evidence = executionprofile.Evidence{}
	if err := authorizeKoeExecutionLineage(&req, []executionprofile.Run{parent}); err != nil {
		t.Fatal(err)
	}
	if req.ExecutionRun.RunID == parent.RunID ||
		req.ExecutionRun.ParentRunID != parent.RunID ||
		req.ExecutionRunID != req.ExecutionRun.RunID {
		t.Fatalf("missed injection reused parent identity: %+v", req.ExecutionRun)
	}
}

func TestRouteExecutionSnapshotIsPublishedAtomically(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:atomic"
	sessionsDir := t.TempDir()
	sc.LockRouteWithManager(routeKey, sessionsDir)
	if _, ok := sc.ActiveRouteExecutionSnapshot(routeKey); ok {
		t.Fatal("route without published run state reported active")
	}

	done := make(chan struct{})
	run := activeExecutionRun(executionprofile.ModeFast, "ker1_atomic")
	generation := sc.SetRouteActiveRunState(routeKey, done, nil, "", run)
	snapshot, ok := sc.ActiveRouteExecutionSnapshot(routeKey)
	if !ok {
		t.Fatal("atomically published route was not visible")
	}
	if snapshot.Run.RunID != run.RunID ||
		snapshot.RunGeneration != generation ||
		snapshot.RunGeneration == 0 ||
		snapshot.Done != done {
		t.Fatalf("partial or mismatched route snapshot: %+v", snapshot)
	}

	sc.ClearRouteRunState(routeKey)
	close(done)
	sc.UnlockRoute(routeKey)
}

func TestRouteExecutionSnapshotUpdatesOnlyCapturedStartupGeneration(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:update"
	sc.LockRouteWithManager(routeKey, t.TempDir())
	done := make(chan struct{})
	parent := persistedFastRunForDaemonTest()
	generation := sc.SetRouteActiveRunState(routeKey, done, nil, "", parent)

	child := cloneExecutionRun(parent)
	child.RunID = "ker1_authoritative-child"
	child.ParentRunID = parent.RunID
	if !sc.UpdateRouteActiveExecutionRun(routeKey, generation, child) {
		t.Fatal("authoritative startup run update was rejected")
	}
	snapshot, ok := sc.ActiveRouteExecutionSnapshot(routeKey)
	if !ok || snapshot.Run.RunID != child.RunID ||
		snapshot.Run.ParentRunID != parent.RunID ||
		snapshot.RunGeneration != generation {
		t.Fatalf("updated route snapshot = %+v", snapshot)
	}
	if sc.UpdateRouteActiveExecutionRun(routeKey, generation+1, parent) {
		t.Fatal("stale generation overwrote authoritative route snapshot")
	}

	sc.SetRouteRunState(routeKey, done, make(chan agent.InjectedMessage, 1), "")
	if sc.UpdateRouteActiveExecutionRun(routeKey, generation, parent) {
		t.Fatal("open injection window accepted an execution identity rewrite")
	}
	sc.ClearRouteRunState(routeKey)
	close(done)
	sc.UnlockRoute(routeKey)
}

func TestSafeBoundaryChildRejectedWhenParentCanceled(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:cancel"
	sessionsDir := t.TempDir()
	sc.LockRouteWithManager(routeKey, sessionsDir)
	parentDone := make(chan struct{})
	sc.SetRouteActiveRunState(
		routeKey,
		parentDone,
		nil,
		"",
		activeExecutionRun(executionprofile.ModeFast, "ker1_parent"),
	)
	parentCanceled := make(chan struct{}, 1)
	sc.SetRouteCancel(routeKey, func() { parentCanceled <- struct{}{} })

	req, child := prepareActiveKoeChild(sc, routeKey, RunAgentRequest{
		Source: "koe", RouteKey: routeKey, ExecutionMode: executionprofile.ModeFull,
		LogicalTaskID: "burst:t01", ExecutionRunID: "ker1_child",
	})
	if !child {
		t.Fatal("expected a safe-boundary child")
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := sc.LockRouteWithManagerAtSafeBoundary(
			context.Background(),
			routeKey,
			sessionsDir,
			req.routeBoundaryGeneration,
			req.routeBoundaryCancelGeneration,
			req.routeBoundaryDone,
		)
		errCh <- err
	}()

	sc.CancelRoute(routeKey)
	select {
	case <-parentCanceled:
	case <-time.After(time.Second):
		t.Fatal("parent cancel handle was not invoked")
	}
	sc.ClearRouteRunState(routeKey)
	close(parentDone)
	sc.UnlockRoute(routeKey)

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRouteBoundaryCanceled) {
			t.Fatalf("safe child error = %v, want ErrRouteBoundaryCanceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("safe child did not stop after parent cancellation")
	}
	if _, ok := sc.ActiveRouteExecutionSnapshot(routeKey); ok {
		t.Fatal("canceled child published a new active route generation")
	}
}

func TestSafeBoundaryChildRejectsSupersededParentGeneration(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:superseded"
	sessionsDir := t.TempDir()
	sc.LockRouteWithManager(routeKey, sessionsDir)
	parentDone := make(chan struct{})
	sc.SetRouteActiveRunState(
		routeKey,
		parentDone,
		nil,
		"",
		activeExecutionRun(executionprofile.ModeFast, "ker1_parent"),
	)
	req, child := prepareActiveKoeChild(sc, routeKey, RunAgentRequest{
		Source: "koe", RouteKey: routeKey, ExecutionMode: executionprofile.ModeFull,
		ExecutionRunID: "ker1_child",
	})
	if !child {
		t.Fatal("expected a safe-boundary child")
	}
	sc.ClearRouteRunState(routeKey)
	close(parentDone)
	sc.UnlockRoute(routeKey)

	sc.LockRouteWithManager(routeKey, sessionsDir)
	replacementDone := make(chan struct{})
	sc.SetRouteActiveRunState(
		routeKey,
		replacementDone,
		nil,
		"",
		activeExecutionRun(executionprofile.ModeFull, "ker1_replacement"),
	)
	sc.ClearRouteRunState(routeKey)
	close(replacementDone)
	sc.UnlockRoute(routeKey)

	_, err := sc.LockRouteWithManagerAtSafeBoundary(
		context.Background(),
		routeKey,
		sessionsDir,
		req.routeBoundaryGeneration,
		req.routeBoundaryCancelGeneration,
		req.routeBoundaryDone,
	)
	if !errors.Is(err, ErrRouteBoundarySuperseded) {
		t.Fatalf("safe child error = %v, want ErrRouteBoundarySuperseded", err)
	}
}

func TestSafeBoundaryRecreatesManagerAfterDeferredEviction(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()
	const routeKey = "agent:default:koe:eviction"
	sessionsDir := t.TempDir()
	parent := sc.LockRouteWithManager(routeKey, sessionsDir)
	parentDone := make(chan struct{})
	sc.SetRouteActiveRunState(
		routeKey,
		parentDone,
		nil,
		"",
		activeExecutionRun(executionprofile.ModeFast, "ker1_parent"),
	)
	req, child := prepareActiveKoeChild(sc, routeKey, RunAgentRequest{
		Source: "koe", RouteKey: routeKey, ExecutionMode: executionprofile.ModeFull,
		ExecutionRunID: "ker1_child",
	})
	if !child {
		t.Fatal("expected a safe-boundary child")
	}

	// Deterministically model an eviction that was scheduled while the parent
	// held entry.mu. UnlockRoute will close and nil the old manager before the
	// waiter acquires the route.
	sc.mu.Lock()
	parent.evicting = true
	sc.mu.Unlock()

	type lockResult struct {
		entry *routeEntry
		err   error
	}
	resultCh := make(chan lockResult, 1)
	go func() {
		entry, err := sc.LockRouteWithManagerAtSafeBoundary(
			context.Background(),
			routeKey,
			sessionsDir,
			req.routeBoundaryGeneration,
			req.routeBoundaryCancelGeneration,
			req.routeBoundaryDone,
		)
		resultCh <- lockResult{entry: entry, err: err}
	}()
	sc.ClearRouteRunState(routeKey)
	close(parentDone)
	sc.UnlockRoute(routeKey)

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("safe-boundary acquire: %v", got.err)
		}
		if got.entry == nil || got.entry.manager == nil {
			t.Fatal("safe-boundary waiter returned a nil manager after eviction")
		}
		sc.UnlockRoute(routeKey)
	case <-time.After(time.Second):
		t.Fatal("safe-boundary waiter did not acquire after eviction")
	}
}

func TestInterruptedRecoveryPreservesExecutionProfileAndEvidence(t *testing.T) {
	run := activeExecutionRun(executionprofile.ModeFast, "ker1_resume")
	run.Profile = fastProfileForDaemonTest()
	run.LogicalTaskID = "burst:t01"
	run.ParentRunID = "ker1_parent"
	run.Evidence.ToolOutcomes = []executionprofile.ToolOutcomeEvidence{{
		ToolCallID: "write-1", ToolName: "file_write", Validated: true,
		Outcome: "succeeded", SideEffect: true,
		ArgumentsDigest: daemonTestArgumentsDigest, ResultDigest: "digest",
	}}
	snapshot := interruptedTurnSnapshot(RunAgentRequest{
		Source: "koe", ThreadID: "burst", RouteKey: "agent:default:koe:burst",
		ExecutionRun: run,
	}, "", t.TempDir())
	resume := buildInterruptedResumeRequest(interruptedTurnCandidate{
		SessionID: "session-1", State: *snapshot, UpdatedAt: snapshot.UpdatedAt,
	}, 3, time.Hour)
	if resume.ExecutionRun.RunID != run.RunID ||
		resume.ExecutionRun.Profile != run.Profile ||
		resume.ExecutionMode != executionprofile.ModeFast ||
		len(resume.ExecutionRun.Evidence.ToolOutcomes) != 1 ||
		!resume.ExecutionRun.Evidence.ToolOutcomes[0].SideEffect {
		t.Fatalf("resume execution run drifted: %+v", resume.ExecutionRun)
	}
}

func TestKoeFastCheckpointKeepsPreProfileAgentBaseline(t *testing.T) {
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "large", "", 13, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-5")
	loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096})
	loop.SetReasoningEffort("high")
	loop.SetEffortTier("xhigh")
	loop.SetServiceTier("default")
	loop.SetResponseLanguage("中文")
	loop.SetTemperature(0.27)
	loop.SetMaxTokens(7777)
	loop.SetContextWindowExplicit(200_000)

	req := RunAgentRequest{
		Source: "koe",
		ExecutionRun: executionprofile.Run{
			RunID:   "ker1_fast-baseline",
			Profile: fastProfileForDaemonTest(),
		},
	}
	lockAgentExecutionConfig(loop, &req)
	loop.SetKoeExecutionProfile(req.ExecutionRun.Profile)
	snapshot := interruptedTurnSnapshot(req, "", t.TempDir())
	if snapshot.ExecutionConfig == nil {
		t.Fatal("Fast checkpoint omitted Agent baseline")
	}
	if snapshot.ExecutionConfig.SpecificModel != "claude-sonnet-5" ||
		snapshot.ExecutionConfig.ModelTier != "large" ||
		snapshot.ExecutionConfig.ReasoningEffort != "high" ||
		snapshot.ExecutionConfig.EffortTier != "xhigh" ||
		snapshot.ExecutionConfig.ServiceTier != "default" ||
		snapshot.ExecutionConfig.ResponseLanguage != "中文" {
		t.Fatalf("Fast checkpoint baseline drifted: %+v", snapshot.ExecutionConfig)
	}
	raw, err := json.Marshal(snapshot.ExecutionConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "gpt-5.6-luna") ||
		strings.Contains(string(raw), `"reasoning_effort":"medium"`) ||
		strings.Contains(string(raw), `"service_tier":"fast"`) {
		t.Fatalf("Fast profile polluted Agent baseline: %s", raw)
	}
}

func TestInterruptedRecoveryPreservesComputerActivation(t *testing.T) {
	run := activeExecutionRun(executionprofile.ModeFull, "ker1_computer_resume")
	run.Profile = executionprofile.FullProfile(executionprofile.ModeFull, "requested_full")
	run.ComputerActivation = &executionprofile.ComputerActivation{
		Profile:            anthropicComputerProfileForDaemonTest(),
		ToolName:           "computer",
		ToolsetFingerprint: "sha256:toolset-v1",
	}
	snapshot := interruptedTurnSnapshot(RunAgentRequest{
		Source: "koe", ThreadID: "burst", RouteKey: "agent:default:koe:burst",
		ExecutionRun: run,
	}, "", t.TempDir())
	resume := buildInterruptedResumeRequest(interruptedTurnCandidate{
		SessionID: "session-1", State: *snapshot, UpdatedAt: snapshot.UpdatedAt,
	}, 3, time.Hour)
	got := resume.ExecutionRun.ComputerActivation
	if got == nil ||
		got.Profile != run.ComputerActivation.Profile ||
		got.ToolName != "computer" ||
		got.ToolsetFingerprint != "sha256:toolset-v1" {
		t.Fatalf("resume computer activation drifted: %+v", got)
	}
	run.ComputerActivation.ToolName = "mutated-after-snapshot"
	if resume.ExecutionRun.ComputerActivation.ToolName != "computer" {
		t.Fatalf("resume retained caller-owned computer activation: %+v", resume.ExecutionRun)
	}
}

func fastProfileForDaemonTest() executionprofile.Profile {
	return executionprofile.Profile{
		RequestedMode:       executionprofile.ModeFast,
		EffectiveMode:       executionprofile.ModeFast,
		SchemaVersion:       executionprofile.FastSchemaVersion,
		ProfileName:         executionprofile.FastProfileName,
		ProfileVersion:      executionprofile.FastProfileVersion,
		ProfileID:           "kfp1_daemon-test",
		Provider:            "openai",
		Model:               "gpt-5.6-luna",
		APISurface:          "openai_responses",
		ToolContract:        executionprofile.FastToolContract,
		ReasoningEffort:     "medium",
		ServiceTier:         "fast",
		ParallelToolCalls:   true,
		ResponseCachePolicy: executionprofile.ResponseCacheOff,
		ResolutionReason:    "cloud_profile_resolved",
	}
}

func anthropicComputerProfileForDaemonTest() executionprofile.Profile {
	return executionprofile.Profile{
		RequestedMode:      executionprofile.ModeFull,
		EffectiveMode:      executionprofile.ModeFull,
		SchemaVersion:      executionprofile.ComputerSchemaVersion,
		ContractRevision:   executionprofile.ComputerContractRevision,
		ProfileID:          "ep1_daemon-computer",
		Provider:           "anthropic",
		Model:              "claude-sonnet-5",
		APISurface:         executionprofile.AnthropicMessagesAPISurface,
		ExecutionMode:      executionprofile.ComputerExecutionModeNative,
		ToolContract:       executionprofile.AnthropicComputerToolContract,
		BetaContract:       executionprofile.AnthropicComputerBetaContract,
		SupportsImageInput: true,
		SupportsToolImages: true,
		SupportsFunctions:  true,
		ResolutionReason:   "cloud_computer_profile_resolved",
	}
}

func TestForgedHTTPInheritedModeCannotForceFullWithFakeCloud(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions/resolve" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(fastProfileForDaemonTest())
	}))
	defer server.Close()
	var req RunAgentRequest
	if err := json.Unmarshal([]byte(`{
		"source":"koe",
		"execution_mode":"fast",
		"requested_execution_mode":"fast",
		"full_reason":"none",
		"inherited_execution_mode":"full",
		"logical_task_id":"burst:t01",
		"execution_run_id":"ker1_fake-cloud"
	}`), &req); err != nil {
		t.Fatal(err)
	}
	applyKoeModeAdmission(&req)
	if req.InheritedMode != "" {
		t.Fatalf("forged HTTP inherited mode survived admission: %q", req.InheritedMode)
	}
	run := resolveKoeExecutionRun(context.Background(), &ServerDeps{
		GW: client.NewGatewayClient(server.URL, ""),
	}, req, config.KoeConfig{})
	if run.RunID != "ker1_fake-cloud" || run.Profile.ProfileID != "kfp1_daemon-test" ||
		run.Profile.EffectiveMode != executionprofile.ModeFast {
		t.Fatalf("resolved run = %+v", run)
	}
}
