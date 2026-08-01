package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func persistedFastRunForDaemonTest() executionprofile.Run {
	return executionprofile.Run{
		LogicalTaskID: "burst:t01",
		RunID:         "burst:t01.r01",
		Profile:       fastProfileForDaemonTest(),
	}
}

func persistedKoeResumeRequestForValidationTest(
	t *testing.T,
	deps *ServerDeps,
	sessionID string,
	run executionprofile.Run,
) RunAgentRequest {
	t.Helper()
	storeDir := filepath.Join(deps.ShannonDir, "sessions")
	mgr := session.NewManager(storeDir)
	sess := mgr.NewSessionWithID(sessionID)
	checkpointAt := time.Now().Add(-time.Minute)
	sess.Source = "koe"
	sess.InProgress = true
	sess.InterruptedTurn = &session.InterruptedTurn{
		Source:       "koe",
		RouteKey:     "agent:default:koe:validation",
		ExecutionRun: cloneExecutionRun(run),
		UpdatedAt:    checkpointAt,
	}
	if run.RunID != "" {
		sess.ExecutionRuns = []executionprofile.Run{cloneExecutionRun(run)}
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("save invalid Koe checkpoint: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("close invalid Koe checkpoint store: %v", err)
	}

	candidates, err := discoverInterruptedTurns(deps.ShannonDir)
	if err != nil {
		t.Fatalf("discover invalid Koe checkpoint: %v", err)
	}
	for _, candidate := range candidates {
		if candidate.SessionID == sessionID {
			return buildInterruptedResumeRequest(candidate, 3, 4*time.Hour)
		}
	}
	t.Fatalf("invalid Koe checkpoint %q was not discovered", sessionID)
	return RunAgentRequest{}
}

func assertKoeCheckpointAbandonedForValidationTest(
	t *testing.T,
	deps *ServerDeps,
	sessionID string,
) {
	t.Helper()
	mgr := session.NewManager(filepath.Join(deps.ShannonDir, "sessions"))
	defer mgr.Close()
	sess, err := mgr.Load(sessionID)
	if err != nil {
		t.Fatalf("load abandoned Koe checkpoint: %v", err)
	}
	if sess.InProgress || sess.InterruptedTurn != nil {
		t.Fatalf("invalid Koe checkpoint was not abandoned: %+v", sess.InterruptedTurn)
	}
}

func TestRunAgentResumeRejectsValidKFP1PlusValidEP1WithoutCloudCall(t *testing.T) {
	var resolverCalls atomic.Int32
	var llmCalls atomic.Int32
	var otherCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions/resolve":
			resolverCalls.Add(1)
		case "/v1/completions":
			llmCalls.Add(1)
		default:
			otherCalls.Add(1)
		}
		http.Error(w, "unexpected cloud call", http.StatusInternalServerError)
	}))
	defer server.Close()

	deps := runAgentContractTestDeps(t, server.URL)
	defer deps.SessionCache.CloseAll()

	run := persistedFastRunForDaemonTest()
	run.ComputerActivation = &executionprofile.ComputerActivation{
		Profile:            anthropicComputerProfileForDaemonTest(),
		ToolName:           "computer",
		ToolsetFingerprint: strings.Repeat("a", 64),
	}
	const sessionID = "resume-invalid-profile-stack-001"
	req := persistedKoeResumeRequestForValidationTest(t, deps, sessionID, run)
	_, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
		!strings.Contains(err.Error(), "fast run cannot contain a computer activation") {
		t.Fatalf("RunAgent error = %v, want invalid stacked kfp1 + ep1", err)
	}
	if got := resolverCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0", got)
	}
	if got := llmCalls.Load(); got != 0 {
		t.Fatalf("LLM calls = %d, want 0", got)
	}
	if got := otherCalls.Load(); got != 0 {
		t.Fatalf("other Cloud calls = %d, want 0", got)
	}
	assertKoeCheckpointAbandonedForValidationTest(t, deps, sessionID)
}

// A checkpoint written by a pre-execution-run daemon decodes ExecutionRun as
// the zero value. That is "legacy, no ledger" — not corruption — and it must
// resume as an ordinary run under the normal configuration instead of being
// abandoned on the first post-upgrade daemon start. The resolve endpoint stays
// untouched (ResumeInterrupted never resolves a replacement profile); the run
// proceeds straight to the completion call.
func TestRunAgentResumeAcceptsLegacyKoeCheckpointWithoutExecutionRun(t *testing.T) {
	var resolverCalls atomic.Int32
	var llmCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/completions/resolve" {
			resolverCalls.Add(1)
		} else {
			llmCalls.Add(1)
		}
		http.Error(w, "cloud unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	deps := runAgentContractTestDeps(t, server.URL)
	defer deps.SessionCache.CloseAll()

	const sessionID = "resume-legacy-koe-checkpoint-001"
	req := persistedKoeResumeRequestForValidationTest(
		t,
		deps,
		sessionID,
		executionprofile.Run{},
	)
	_, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if errors.Is(err, executionprofile.ErrInvalidPersistedRun) {
		t.Fatalf("legacy koe checkpoint rejected as invalid: %v", err)
	}
	if got := resolverCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0 (resume never resolves a profile)", got)
	}
	if got := llmCalls.Load(); got == 0 {
		t.Fatal("legacy resume never reached the completion call — turn was dropped pre-flight")
	}
}

func TestValidatePersistedKoeRunAgainstLedgerChecksImmutableFields(t *testing.T) {
	run := persistedFastRunForDaemonTest()

	t.Run("matching immutable fields", func(t *testing.T) {
		ledgerRun := cloneExecutionRun(run)
		ledgerRun.Evidence.ToolOutcomes = []executionprofile.ToolOutcomeEvidence{{
			ToolCallID: "newer-evidence",
		}}
		if err := validatePersistedKoeRunAgainstLedger(
			run,
			[]executionprofile.Run{ledgerRun},
		); err != nil {
			t.Fatalf("validatePersistedKoeRunAgainstLedger() error = %v", err)
		}
	})

	t.Run("missing run", func(t *testing.T) {
		err := validatePersistedKoeRunAgainstLedger(run, nil)
		if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
			!strings.Contains(err.Error(), "missing run_id") {
			t.Fatalf("error = %v, want missing run_id", err)
		}
	})

	t.Run("duplicate run", func(t *testing.T) {
		err := validatePersistedKoeRunAgainstLedger(
			run,
			[]executionprofile.Run{run, run},
		)
		if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
			!strings.Contains(err.Error(), "duplicate run_id") {
			t.Fatalf("error = %v, want duplicate run_id", err)
		}
	})

	t.Run("profile changed", func(t *testing.T) {
		ledgerRun := cloneExecutionRun(run)
		ledgerRun.Profile.ProfileID = "kfp1_conflict"
		err := validatePersistedKoeRunAgainstLedger(
			run,
			[]executionprofile.Run{ledgerRun},
		)
		if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
			!strings.Contains(err.Error(), "immutable fields changed") {
			t.Fatalf("error = %v, want immutable field mismatch", err)
		}
	})
}

func TestValidatePersistedKoeResumeRequestLeavesNonKoeRecoveryCompatible(t *testing.T) {
	if err := validatePersistedKoeResumeRequest(RunAgentRequest{
		Source:            "desktop",
		ResumeInterrupted: true,
	}); err != nil {
		t.Fatalf("non-Koe recovery rejected: %v", err)
	}
}

func TestAuthorizeKoeExecutionLineageMintsChildForReusedRunID(t *testing.T) {
	parent := persistedFastRunForDaemonTest()
	parent.Evidence.ToolOutcomes = []executionprofile.ToolOutcomeEvidence{{
		ToolCallID: "write-1", ToolName: "file_write", Validated: true,
		Outcome: "succeeded", SideEffect: true,
		ArgumentsDigest: daemonTestArgumentsDigest,
	}}
	resolved := cloneExecutionRun(parent)
	resolved.Evidence = executionprofile.Evidence{}
	req := RunAgentRequest{
		Source:         "koe",
		LogicalTaskID:  parent.LogicalTaskID,
		ExecutionRunID: parent.RunID,
		ExecutionRun:   resolved,
	}

	if err := authorizeKoeExecutionLineage(&req, []executionprofile.Run{parent}); err != nil {
		t.Fatal(err)
	}
	if req.ExecutionRunID == parent.RunID ||
		req.ExecutionRun.RunID != req.ExecutionRunID ||
		req.ParentRunID != parent.RunID ||
		req.ExecutionRun.ParentRunID != parent.RunID ||
		req.ExecutionRun.LogicalTaskID != parent.LogicalTaskID {
		t.Fatalf("reused run did not mint an authoritative child: %+v", req.ExecutionRun)
	}
	if len(req.ExecutionRun.Evidence.ToolOutcomes) != 0 {
		t.Fatalf("resolver-owned run unexpectedly retained parent evidence before inheritance: %+v", req.ExecutionRun)
	}
	if err := inheritParentExecutionEvidence(
		&req.ExecutionRun,
		[]executionprofile.Run{parent},
	); err != nil {
		t.Fatal(err)
	}
	if len(req.ExecutionRun.Evidence.ToolOutcomes) != 1 ||
		req.ExecutionRun.Evidence.ToolOutcomes[0].ToolCallID != "write-1" {
		t.Fatalf("minted child lost validated parent evidence: %+v", req.ExecutionRun.Evidence)
	}
}

func TestAuthorizeKoeExecutionLineagePreservesValidatedFullFloor(t *testing.T) {
	parent := executionprofile.Run{
		LogicalTaskID: "burst:t01",
		RunID:         "ker1_full-parent",
		Profile: executionprofile.FullProfile(
			executionprofile.ModeFull,
			"requested_full",
		),
	}
	child := executionprofile.Run{
		LogicalTaskID: parent.LogicalTaskID,
		RunID:         "ker1_fast-child",
		ParentRunID:   parent.RunID,
		Profile:       fastProfileForDaemonTest(),
	}
	req := RunAgentRequest{
		Source:         "koe",
		LogicalTaskID:  child.LogicalTaskID,
		ExecutionRunID: child.RunID,
		ParentRunID:    child.ParentRunID,
		ExecutionRun:   child,
	}
	if err := authorizeKoeExecutionLineage(&req, []executionprofile.Run{parent}); err != nil {
		t.Fatal(err)
	}
	if req.InheritedMode != executionprofile.ModeFull ||
		req.ExecutionRun.Profile.RequestedMode != executionprofile.ModeFast ||
		req.ExecutionRun.Profile.EffectiveMode != executionprofile.ModeFull ||
		req.ExecutionRun.Profile.ResolutionReason != "lineage_full_preserved" {
		t.Fatalf("validated Full lineage was not preserved: inherited=%q profile=%+v",
			req.InheritedMode, req.ExecutionRun.Profile)
	}
	if err := inheritParentExecutionEvidence(
		&req.ExecutionRun,
		[]executionprofile.Run{parent},
	); err != nil {
		t.Fatal(err)
	}

	reused := RunAgentRequest{
		Source:         "koe",
		LogicalTaskID:  parent.LogicalTaskID,
		ExecutionRunID: parent.RunID,
		ExecutionRun: executionprofile.Run{
			LogicalTaskID: parent.LogicalTaskID,
			RunID:         parent.RunID,
			Profile:       fastProfileForDaemonTest(),
		},
	}
	if err := authorizeKoeExecutionLineage(
		&reused,
		[]executionprofile.Run{parent},
	); err != nil {
		t.Fatal(err)
	}
	if reused.ExecutionRun.RunID == parent.RunID ||
		reused.ExecutionRun.ParentRunID != parent.RunID ||
		reused.ExecutionRun.Profile.RequestedMode != executionprofile.ModeFast ||
		reused.ExecutionRun.Profile.EffectiveMode != executionprofile.ModeFull ||
		reused.ExecutionRun.Profile.ResolutionReason != "lineage_full_preserved" {
		t.Fatalf("reused Full generation downgraded or reused identity: %+v", reused.ExecutionRun)
	}
}

func TestAuthorizeKoeExecutionLineageRejectsForgedOrAmbiguousParent(t *testing.T) {
	child := executionprofile.Run{
		LogicalTaskID: "burst:t01",
		RunID:         "ker1_child",
		ParentRunID:   "ker1_parent",
		Profile:       fastProfileForDaemonTest(),
	}
	newRequest := func() RunAgentRequest {
		return RunAgentRequest{
			Source:         "koe",
			LogicalTaskID:  child.LogicalTaskID,
			ExecutionRunID: child.RunID,
			ParentRunID:    child.ParentRunID,
			ExecutionRun:   cloneExecutionRun(child),
		}
	}

	t.Run("missing parent", func(t *testing.T) {
		req := newRequest()
		err := authorizeKoeExecutionLineage(&req, nil)
		if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
			!strings.Contains(err.Error(), "missing parent run_id") {
			t.Fatalf("error = %v, want missing authoritative parent", err)
		}
	})

	t.Run("duplicate parent", func(t *testing.T) {
		parent := persistedFastRunForDaemonTest()
		parent.RunID = child.ParentRunID
		req := newRequest()
		err := authorizeKoeExecutionLineage(
			&req,
			[]executionprofile.Run{parent, cloneExecutionRun(parent)},
		)
		if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
			!strings.Contains(err.Error(), "duplicate run_id") {
			t.Fatalf("error = %v, want duplicate authoritative parent", err)
		}
	})

	t.Run("invalid full parent", func(t *testing.T) {
		parent := executionprofile.Run{
			LogicalTaskID: child.LogicalTaskID,
			RunID:         child.ParentRunID,
			Profile: executionprofile.FullProfile(
				executionprofile.ModeFull,
				"requested_full",
			),
		}
		parent.Profile.Model = "forged-model-override"
		req := newRequest()
		err := authorizeKoeExecutionLineage(&req, []executionprofile.Run{parent})
		if !errors.Is(err, executionprofile.ErrInvalidPersistedRun) ||
			!strings.Contains(err.Error(), "execution overrides") {
			t.Fatalf("error = %v, want invalid persisted Full parent", err)
		}
	})
}

func TestRunAgentReusedKoeRunIDPersistsDistinctFullLineageChild(t *testing.T) {
	gateway := &fakeGatewayBackend{reply: "done"}
	server := httptest.NewServer(gateway.handler())
	defer server.Close()

	deps := runAgentContractTestDeps(t, server.URL)
	defer deps.SessionCache.CloseAll()

	parent := executionprofile.Run{
		LogicalTaskID: "burst:t01",
		RunID:         "ker1_persisted-full-parent",
		Profile: executionprofile.FullProfile(
			executionprofile.ModeFull,
			"requested_full",
		),
		Evidence: executionprofile.Evidence{ToolOutcomes: []executionprofile.ToolOutcomeEvidence{{
			ToolCallID: "write-1", ToolName: "file_write", Validated: true,
			Outcome: "succeeded", SideEffect: true,
			ArgumentsDigest: daemonTestArgumentsDigest,
		}}},
	}
	const sessionID = "koe-reused-run-lineage-001"
	mgr := session.NewManager(deps.SessionCache.SessionsDir(""))
	sess := mgr.NewSessionWithID(sessionID)
	sess.Title = "Existing Koe task"
	sess.ExecutionRuns = []executionprofile.Run{cloneExecutionRun(parent)}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}

	rawFast := "fast"
	result, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:                   "continue the same task",
		Source:                 "koe",
		SessionID:              sessionID,
		ExecutionMode:          executionprofile.ModeFast,
		RequestedExecutionMode: &rawFast,
		FullReason:             executionprofile.FullReasonNone,
		LogicalTaskID:          parent.LogicalTaskID,
		ExecutionRunID:         parent.RunID,
	}, nullEventHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExecutionRun == nil ||
		result.ExecutionRun.RunID == parent.RunID ||
		result.ExecutionRun.ParentRunID != parent.RunID ||
		result.ExecutionRun.Profile.RequestedMode != executionprofile.ModeFast ||
		result.ExecutionRun.Profile.EffectiveMode != executionprofile.ModeFull ||
		result.ExecutionRun.Profile.ResolutionReason != "lineage_full_preserved" {
		t.Fatalf("RunAgent reused or downgraded persisted Full run: %+v", result.ExecutionRun)
	}
	if len(result.ExecutionRun.Evidence.ToolOutcomes) != 1 ||
		result.ExecutionRun.Evidence.ToolOutcomes[0].ToolCallID != "write-1" {
		t.Fatalf("RunAgent child lost parent side-effect evidence: %+v", result.ExecutionRun.Evidence)
	}

	verify := session.NewManager(deps.SessionCache.SessionsDir(""))
	defer verify.Close()
	saved, err := verify.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.ExecutionRuns) != 2 ||
		saved.ExecutionRuns[0].RunID != parent.RunID ||
		saved.ExecutionRuns[1].RunID != result.ExecutionRun.RunID ||
		saved.ExecutionRuns[1].ParentRunID != parent.RunID {
		t.Fatalf("persisted execution lineage = %+v", saved.ExecutionRuns)
	}
}
