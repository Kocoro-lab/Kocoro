package guicontrol

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	defaultLeaseTTL          = 15 * time.Second
	defaultControlLedgerSize = 256
	defaultTombstoneLimit    = 4096
)

type ActivitySink func(ComputerUseActivityEvent)

type CoordinatorOptions struct {
	InstanceID                 string
	Now                        func() time.Time
	NewID                      func(prefix string) string
	LeaseTTL                   time.Duration
	ControlLedgerSize          int
	TombstoneLimit             int
	RequireControllerHeartbeat bool
	Sink                       ActivitySink
}

type WorkflowRequest struct {
	SessionID            string
	TurnID               string
	SourceKind           string
	SourceLabel          string
	RequestedAppBundleID string
	RequestedAppName     string
	AllowedAppBundleIDs  []string
	PolicySnapshotID     string
}

type WorkflowLease struct {
	LeaseID              string
	SessionID            string
	TurnID               string
	SourceKind           string
	SourceLabel          string
	RequestedAppBundleID string
	RequestedAppName     string
	AllowedAppBundleIDs  []string
	PolicySnapshotID     string
	State                ComputerUseLeaseState
	CreatedAt            time.Time
	HeartbeatAt          time.Time
	ExpiresAt            time.Time
}

type ActionRequest struct {
	LeaseID        string
	TurnID         string
	ToolName       string
	ToolUseID      string
	ActionKind     string
	ActionPhase    ComputerUseActionPhase
	TargetBundleID string
	TargetAppName  string
	ExecutionPath  *ComputerUseExecutionPath
	Pointer        *ComputerUsePointer
	Effect         ComputerUseActionEffect
	// OrderedBatchAction keeps one already-observed provider screenshot valid
	// across sequential actions. Pause, Take Over, and user interference still
	// establish the ordinary re-observation barrier.
	OrderedBatchAction bool
	// RiskIntentID and RiskTargetDigest are process-local execution authority.
	// They are never projected into ComputerUseActivityState or routine logs.
	RiskIntentID     string
	RiskTargetDigest string
}

type ComputerUseActionEffect string

const (
	ComputerUseActionObservation ComputerUseActionEffect = "observation"
	ComputerUseActionMutation    ComputerUseActionEffect = "mutation"
)

type ActionHandle struct {
	LeaseID  string
	ActionID string
	Context  context.Context

	executionAuthority *executionAuthority
}

type ConsequentialRiskHandle struct {
	LeaseID  string
	IntentID string
	Context  context.Context
}

type ActionFinish struct {
	LeaseID       string
	ActionID      string
	Phase         ComputerUseActionPhase
	Result        *ComputerUseActionResult
	ExecutionPath *ComputerUseExecutionPath
	Pointer       *ComputerUsePointer
	FailureCode   *string
}

type actionRecord struct {
	id                      string
	effect                  ComputerUseActionEffect
	toolName                string
	actionKind              string
	targetBundleID          string
	targetAppName           string
	orderedBatchAction      bool
	cancel                  context.CancelFunc
	executionAuthority      *executionAuthority
	cancellationResult      *ComputerUseActionResult
	cancellationFailureCode *string
}

type consequentialRiskRecord struct {
	request ActionRequest
	marker  ConsequentialRiskMarkerV1
	cancel  context.CancelFunc
}

type leaseRecord struct {
	lease                  WorkflowLease
	activity               ComputerUseActivityState
	controllerAcknowledged bool
	currentAction          *actionRecord
	pendingRisk            *consequentialRiskRecord
	requiresObservation    bool
	stateChanged           chan struct{}
}

type tombstoneReason uint8

const (
	tombstoneStopped tombstoneReason = iota + 1
	tombstoneExpired
)

type controlLedgerEntry struct {
	request  ComputerUseControlRequest
	response ComputerUseControlResponse
}

// Coordinator is the only process-wide authority for admitting GUI-mutating
// workflows. All state, revision, action cancellation, tombstones, and control
// idempotency live under mu. Activity sinks are always invoked after unlocking.
type Coordinator struct {
	mu sync.Mutex

	instanceID     string
	revision       uint64
	active         *leaseRecord
	tombstones     map[string]tombstoneReason
	tombstoneOrder []string
	tombstoneLimit int

	controlLedger map[string]controlLedgerEntry
	controlOrder  []string
	ledgerLimit   int

	now      func() time.Time
	newID    func(string) string
	leaseTTL time.Duration
	sink     ActivitySink

	requireControllerHeartbeat bool

	emitMu           sync.Mutex
	emitCond         *sync.Cond
	nextEmitRevision uint64
}

var (
	processCoordinatorOnce sync.Once
	processCoordinator     *Coordinator
)

func ProcessCoordinator() *Coordinator {
	processCoordinatorOnce.Do(func() {
		processCoordinator = NewCoordinator(CoordinatorOptions{RequireControllerHeartbeat: true})
	})
	return processCoordinator
}

func NewCoordinator(options CoordinatorOptions) *Coordinator {
	if options.InstanceID == "" {
		options.InstanceID = NewCoordinatorInstanceID()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewID == nil {
		options.NewID = newID
	}
	if options.LeaseTTL <= 0 {
		options.LeaseTTL = defaultLeaseTTL
	}
	if options.ControlLedgerSize <= 0 {
		options.ControlLedgerSize = defaultControlLedgerSize
	}
	if options.TombstoneLimit <= 0 {
		options.TombstoneLimit = defaultTombstoneLimit
	}
	coordinator := &Coordinator{
		instanceID:     options.InstanceID,
		tombstones:     make(map[string]tombstoneReason),
		tombstoneLimit: options.TombstoneLimit,
		controlLedger:  make(map[string]controlLedgerEntry),
		ledgerLimit:    options.ControlLedgerSize,
		now:            options.Now,
		newID:          options.NewID,
		leaseTTL:       options.LeaseTTL,
		sink:           options.Sink,

		requireControllerHeartbeat: options.RequireControllerHeartbeat,
		nextEmitRevision:           1,
	}
	coordinator.emitCond = sync.NewCond(&coordinator.emitMu)
	return coordinator
}

// SetSink installs the process integration after its event transport exists.
// It does not replay current state; consumers obtain authority via Snapshot.
func (c *Coordinator) SetSink(sink ActivitySink) {
	c.mu.Lock()
	c.sink = sink
	c.mu.Unlock()
}

func (c *Coordinator) BeginWorkflow(request WorkflowRequest) (WorkflowLease, error) {
	if request.SessionID == "" || request.TurnID == "" {
		return WorkflowLease{}, fmt.Errorf("computer-use workflow session_id and turn_id are required")
	}

	c.mu.Lock()
	if reason, found := c.tombstones[request.TurnID]; found {
		c.mu.Unlock()
		if reason == tombstoneExpired {
			return WorkflowLease{}, &LeaseExpiredError{TurnID: request.TurnID}
		}
		return WorkflowLease{}, &StoppedTurnError{TurnID: request.TurnID}
	}
	if c.active != nil {
		// A cancelled executor remains the only GUI owner until it acknowledges
		// quiescence through FinishAction. This applies to explicit Stop and to
		// heartbeat expiry; admitting a replacement while the old action can still
		// commit would violate the single-controller invariant.
		if c.active.lease.State == ComputerUseLeaseStopping && c.active.currentAction != nil {
			busy := c.busyErrorLocked()
			c.mu.Unlock()
			return WorkflowLease{}, busy
		}
		if !c.now().Before(c.active.lease.ExpiresAt) {
			cancel, event, sink := c.expireLocked()
			retainedForQuiescence := c.active != nil
			var busy *BusyError
			if retainedForQuiescence {
				busy = c.busyErrorLocked()
			}
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			c.emitOrdered(sink, event)
			if retainedForQuiescence {
				return WorkflowLease{}, busy
			}
			return c.BeginWorkflow(request)
		}
		if c.active.lease.TurnID == request.TurnID && c.active.lease.SessionID == request.SessionID {
			lease := cloneWorkflowLease(c.active.lease)
			c.mu.Unlock()
			return lease, nil
		}
		busy := c.busyErrorLocked()
		c.mu.Unlock()
		return WorkflowLease{}, busy
	}

	// Keep the monotonic component for all internal deadline comparisons. UTC
	// conversion belongs only at the wire-format boundary.
	now := c.now()
	lease := WorkflowLease{
		LeaseID:              c.newID("cul"),
		SessionID:            request.SessionID,
		TurnID:               request.TurnID,
		SourceKind:           request.SourceKind,
		SourceLabel:          request.SourceLabel,
		RequestedAppBundleID: request.RequestedAppBundleID,
		RequestedAppName:     request.RequestedAppName,
		AllowedAppBundleIDs:  append([]string(nil), request.AllowedAppBundleIDs...),
		PolicySnapshotID:     request.PolicySnapshotID,
		State:                ComputerUseLeaseActive,
		CreatedAt:            now,
		ExpiresAt:            now.Add(c.leaseTTL),
	}
	c.active = &leaseRecord{
		lease:        lease,
		stateChanged: make(chan struct{}),
		activity: ComputerUseActivityState{
			LeaseID:        lease.LeaseID,
			SessionID:      lease.SessionID,
			SourceKind:     lease.SourceKind,
			SourceLabel:    lease.SourceLabel,
			TargetBundleID: lease.RequestedAppBundleID,
			TargetAppName:  lease.RequestedAppName,
			LeaseState:     ComputerUseLeaseActive,
			ActionPhase:    ComputerUsePhaseIdle,
		},
	}
	event, sink, err := c.eventLocked()
	c.mu.Unlock()
	if err != nil {
		return WorkflowLease{}, err
	}
	c.emitOrdered(sink, event)
	return cloneWorkflowLease(lease), nil
}

// AwaitController blocks one newly admitted workflow until the trusted local
// controller proves liveness with Heartbeat. State transitions broadcast on a
// per-lease channel; callers never need to poll and BeginWorkflow itself never
// counts as an acknowledgement.
func (c *Coordinator) AwaitController(ctx context.Context, leaseID, turnID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if leaseID == "" || turnID == "" {
		return fmt.Errorf("computer-use controller wait lease_id and turn_id are required")
	}
	for {
		c.mu.Lock()
		if reason, found := c.tombstones[turnID]; found {
			c.mu.Unlock()
			if reason == tombstoneExpired {
				return &LeaseExpiredError{LeaseID: leaseID, TurnID: turnID}
			}
			return &StoppedTurnError{TurnID: turnID}
		}
		if c.active == nil || c.active.lease.LeaseID != leaseID || c.active.lease.TurnID != turnID {
			c.mu.Unlock()
			return &StaleLeaseError{LeaseID: leaseID}
		}
		now := c.now()
		if !now.Before(c.active.lease.ExpiresAt) {
			cancel, event, sink := c.expireLocked()
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			c.emitOrdered(sink, event)
			return &LeaseExpiredError{LeaseID: leaseID, TurnID: turnID}
		}
		if c.active.lease.State != ComputerUseLeaseActive {
			state := c.active.lease.State
			c.mu.Unlock()
			return &InvalidTransitionError{State: state}
		}
		if c.active.controllerAcknowledged {
			c.mu.Unlock()
			return nil
		}
		changed := c.active.stateChanged
		remaining := c.active.lease.ExpiresAt.Sub(now)
		c.mu.Unlock()

		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			stopAndDrainTimer(timer)
			return ctx.Err()
		case <-changed:
			stopAndDrainTimer(timer)
		case <-timer.C:
			c.ExpireStale()
		}
	}
}

// AwaitActionQuiescence blocks until the active executor has acknowledged
// cleanup through FinishAction. Take Over uses this boundary before Desktop
// is allowed to activate the target app and return the pointer to the user.
func (c *Coordinator) AwaitActionQuiescence(ctx context.Context, leaseID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if leaseID == "" {
		return fmt.Errorf("computer-use quiescence wait lease_id is required")
	}
	for {
		c.mu.Lock()
		if c.active == nil || c.active.lease.LeaseID != leaseID {
			// A lease that is no longer active is trivially quiesced: it owns no
			// action that could still commit input, which is the only property
			// this barrier asserts. Reporting a stale-lease error here instead
			// would make an idempotent take-over retry — the exact case the
			// idempotency key exists for, after the first response was lost —
			// answer 409 for a take-over that already succeeded, and a correct
			// client reacts to 409 by NOT handing control back to the user.
			c.mu.Unlock()
			return nil
		}
		if c.active.currentAction == nil {
			c.mu.Unlock()
			return nil
		}
		changed := c.active.stateChanged
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (c *Coordinator) BeginAction(parent context.Context, request ActionRequest) (ActionHandle, error) {
	if parent == nil {
		parent = context.Background()
	}
	if request.LeaseID == "" || request.TurnID == "" || request.ToolUseID == "" || request.ActionKind == "" {
		return ActionHandle{}, fmt.Errorf("computer-use action lease_id, turn_id, tool_use_id, and action_kind are required")
	}
	if request.ActionPhase != "" && !ValidComputerUseActionPhase(request.ActionPhase) {
		return ActionHandle{}, fmt.Errorf("invalid computer-use action_phase %q", request.ActionPhase)
	}
	if request.ExecutionPath != nil && !ValidComputerUseExecutionPath(*request.ExecutionPath) {
		return ActionHandle{}, fmt.Errorf("invalid computer-use execution_path %q", *request.ExecutionPath)
	}
	if request.Pointer != nil {
		if err := request.Pointer.Validate(); err != nil {
			return ActionHandle{}, err
		}
	}
	if request.Effect != ComputerUseActionObservation && request.Effect != ComputerUseActionMutation {
		return ActionHandle{}, &PolicyDeniedError{
			LeaseID: request.LeaseID,
			Reason:  "an explicit observation or mutation classification is required",
		}
	}
	if !validConsequentialRiskExecutionScope(
		request.ToolName, request.ActionKind, string(request.Effect),
		request.TargetBundleID, executionPathString(request.ExecutionPath),
		request.RiskIntentID, request.RiskTargetDigest,
	) {
		return ActionHandle{}, &PolicyDeniedError{
			LeaseID: request.LeaseID,
			Reason:  "consequential-risk execution authority is invalid",
		}
	}

	c.mu.Lock()
	if reason, found := c.tombstones[request.TurnID]; found {
		c.mu.Unlock()
		if reason == tombstoneExpired {
			return ActionHandle{}, &LeaseExpiredError{LeaseID: request.LeaseID, TurnID: request.TurnID}
		}
		return ActionHandle{}, &StoppedTurnError{TurnID: request.TurnID}
	}
	if c.active == nil || c.active.lease.LeaseID != request.LeaseID || c.active.lease.TurnID != request.TurnID {
		c.mu.Unlock()
		return ActionHandle{}, &StaleLeaseError{LeaseID: request.LeaseID}
	}
	if !c.now().Before(c.active.lease.ExpiresAt) {
		cancel, event, sink := c.expireLocked()
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.emitOrdered(sink, event)
		return ActionHandle{}, &LeaseExpiredError{LeaseID: request.LeaseID, TurnID: request.TurnID}
	}
	if c.active.lease.State != ComputerUseLeaseActive {
		state := c.active.lease.State
		c.mu.Unlock()
		return ActionHandle{}, &InvalidTransitionError{State: state}
	}
	if c.requireControllerHeartbeat && !c.active.controllerAcknowledged {
		c.mu.Unlock()
		return ActionHandle{}, &ControllerUnavailableError{LeaseID: request.LeaseID}
	}
	if c.active.currentAction != nil {
		existing := c.active.currentAction.id
		c.mu.Unlock()
		return ActionHandle{}, &ActionInProgressError{LeaseID: request.LeaseID, ActionID: existing}
	}
	var stagedCancel context.CancelFunc
	if c.active.pendingRisk != nil {
		if !exactConsequentialRiskActionRequest(c.active.pendingRisk.request, request) {
			c.mu.Unlock()
			return ActionHandle{}, &PolicyDeniedError{LeaseID: request.LeaseID, Reason: "action does not match staged consequential-risk confirmation"}
		}
		stagedCancel = c.active.pendingRisk.cancel
		c.active.pendingRisk = nil
		c.active.activity.ConsequentialRisk = nil
	} else if request.RiskIntentID != "" || request.RiskTargetDigest != "" {
		c.mu.Unlock()
		return ActionHandle{}, &PolicyDeniedError{LeaseID: request.LeaseID, Reason: "consequential-risk confirmation was not staged"}
	}
	if request.Effect == ComputerUseActionMutation && c.active.requiresObservation {
		c.mu.Unlock()
		return ActionHandle{}, &ReobservationRequiredError{LeaseID: request.LeaseID}
	}
	if request.Effect == ComputerUseActionMutation {
		if err := c.enforceAllowedTargetLocked(request); err != nil {
			c.mu.Unlock()
			return ActionHandle{}, err
		}
	}

	actionCtx, cancel := context.WithCancel(parent)
	actionID := c.newID("cua")
	executionAuthority := newExecutionAuthority(request, actionID)
	c.active.currentAction = &actionRecord{
		id: actionID, effect: request.Effect, toolName: request.ToolName,
		actionKind:     request.ActionKind,
		targetBundleID: request.TargetBundleID, targetAppName: request.TargetAppName,
		orderedBatchAction: request.OrderedBatchAction,
		cancel:             cancel, executionAuthority: executionAuthority,
	}
	state := &c.active.activity
	state.ActionID = actionID
	state.ToolUseID = request.ToolUseID
	state.ActionKind = request.ActionKind
	state.ActionResult = nil
	state.FailureCode = nil
	state.TargetBundleID = nonempty(request.TargetBundleID, c.active.lease.RequestedAppBundleID)
	state.TargetAppName = nonempty(request.TargetAppName, c.active.lease.RequestedAppName)
	state.ActionPhase = request.ActionPhase
	if state.ActionPhase == "" {
		state.ActionPhase = ComputerUsePhaseActing
	}
	state.ExecutionPath = cloneExecutionPath(request.ExecutionPath)
	state.Pointer = clonePointer(request.Pointer)
	event, sink, err := c.eventLocked()
	c.mu.Unlock()
	if stagedCancel != nil {
		stagedCancel()
	}
	if err != nil {
		cancel()
		return ActionHandle{}, err
	}
	c.emitOrdered(sink, event)
	return ActionHandle{
		LeaseID: request.LeaseID, ActionID: actionID, Context: actionCtx,
		executionAuthority: executionAuthority,
	}, nil
}

// StageConsequentialRisk publishes only the content-free marker while keeping
// the exact action/digest binding private inside the coordinator.
func (c *Coordinator) StageConsequentialRisk(parent context.Context, request ActionRequest, marker ConsequentialRiskMarkerV1) (ConsequentialRiskHandle, error) {
	if parent == nil {
		parent = context.Background()
	}
	if err := marker.Validate(); err != nil {
		return ConsequentialRiskHandle{}, err
	}
	if marker.IntentID != request.RiskIntentID || !validConsequentialRiskExecutionScope(
		request.ToolName, request.ActionKind, string(request.Effect), request.TargetBundleID,
		executionPathString(request.ExecutionPath), request.RiskIntentID, request.RiskTargetDigest,
	) {
		return ConsequentialRiskHandle{}, fmt.Errorf("invalid consequential-risk staged binding")
	}
	c.mu.Lock()
	if c.active == nil || c.active.lease.LeaseID != request.LeaseID || c.active.lease.TurnID != request.TurnID {
		c.mu.Unlock()
		return ConsequentialRiskHandle{}, &StaleLeaseError{LeaseID: request.LeaseID}
	}
	if !c.now().Before(c.active.lease.ExpiresAt) {
		cancel, event, sink := c.expireLocked()
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.emitOrdered(sink, event)
		return ConsequentialRiskHandle{}, &LeaseExpiredError{LeaseID: request.LeaseID, TurnID: request.TurnID}
	}
	if c.active.lease.State != ComputerUseLeaseActive || c.active.currentAction != nil || c.active.pendingRisk != nil {
		state := c.active.lease.State
		c.mu.Unlock()
		return ConsequentialRiskHandle{}, &InvalidTransitionError{State: state}
	}
	if c.requireControllerHeartbeat && !c.active.controllerAcknowledged {
		c.mu.Unlock()
		return ConsequentialRiskHandle{}, &ControllerUnavailableError{LeaseID: request.LeaseID}
	}
	if c.active.requiresObservation {
		c.mu.Unlock()
		return ConsequentialRiskHandle{}, &ReobservationRequiredError{LeaseID: request.LeaseID}
	}
	if err := c.enforceAllowedTargetLocked(request); err != nil {
		c.mu.Unlock()
		return ConsequentialRiskHandle{}, err
	}
	waitCtx, cancel := context.WithCancel(parent)
	c.active.pendingRisk = &consequentialRiskRecord{request: request, marker: marker, cancel: cancel}
	state := &c.active.activity
	state.ToolUseID, state.ActionKind = request.ToolUseID, request.ActionKind
	state.TargetBundleID = nonempty(request.TargetBundleID, c.active.lease.RequestedAppBundleID)
	state.TargetAppName = nonempty(request.TargetAppName, c.active.lease.RequestedAppName)
	state.ExecutionPath = cloneExecutionPath(request.ExecutionPath)
	state.ActionPhase = ComputerUsePhaseWaitingForUser
	state.ActionResult, state.FailureCode, state.Pointer = nil, nil, nil
	state.ConsequentialRisk = &marker
	event, sink, err := c.eventLocked()
	c.mu.Unlock()
	if err != nil {
		cancel()
		return ConsequentialRiskHandle{}, err
	}
	c.emitOrdered(sink, event)
	return ConsequentialRiskHandle{LeaseID: request.LeaseID, IntentID: marker.IntentID, Context: waitCtx}, nil
}

func (c *Coordinator) CancelConsequentialRisk(leaseID, intentID string) error {
	c.mu.Lock()
	if c.active == nil || c.active.lease.LeaseID != leaseID {
		c.mu.Unlock()
		return &StaleLeaseError{LeaseID: leaseID}
	}
	if c.active.pendingRisk == nil || c.active.pendingRisk.marker.IntentID != intentID {
		c.mu.Unlock()
		return nil
	}
	cancel := c.active.pendingRisk.cancel
	c.active.pendingRisk = nil
	c.active.activity.ConsequentialRisk = nil
	c.active.activity.ActionPhase = ComputerUsePhaseIdle
	c.clearActionPresentationLocked()
	event, sink, err := c.eventLocked()
	c.mu.Unlock()
	cancel()
	if err == nil {
		c.emitOrdered(sink, event)
	}
	return err
}

func exactConsequentialRiskActionRequest(a, b ActionRequest) bool {
	return a.LeaseID == b.LeaseID && a.TurnID == b.TurnID && a.ToolName == b.ToolName &&
		a.ToolUseID == b.ToolUseID && a.ActionKind == b.ActionKind && a.ActionPhase == b.ActionPhase &&
		a.TargetBundleID == b.TargetBundleID &&
		a.TargetAppName == b.TargetAppName && executionPathString(a.ExecutionPath) == executionPathString(b.ExecutionPath) &&
		a.Effect == b.Effect && a.OrderedBatchAction == b.OrderedBatchAction &&
		a.RiskIntentID == b.RiskIntentID && a.RiskTargetDigest == b.RiskTargetDigest
}

func (c *Coordinator) enforceAllowedTargetLocked(request ActionRequest) error {
	deny := func(reason string) error {
		return &PolicyDeniedError{
			LeaseID:          request.LeaseID,
			TargetBundleID:   request.TargetBundleID,
			PolicySnapshotID: c.active.lease.PolicySnapshotID,
			Reason:           reason,
		}
	}
	if request.TargetBundleID == "" {
		return deny("a mutating action requires an explicit target bundle")
	}
	if len(c.active.lease.AllowedAppBundleIDs) == 0 {
		return deny("no app target is bound to this workflow; start the app and observe the running app before attempting a mutation")
	}
	for _, allowed := range c.active.lease.AllowedAppBundleIDs {
		if request.TargetBundleID == allowed {
			return nil
		}
	}
	return deny("this app has not been observed in the current workflow; observe the running app before attempting a mutation")
}

func (c *Coordinator) FinishAction(finish ActionFinish) error {
	if finish.LeaseID == "" || finish.ActionID == "" {
		return fmt.Errorf("computer-use finish lease_id and action_id are required")
	}
	if finish.Phase != "" && !ValidComputerUseActionPhase(finish.Phase) {
		return fmt.Errorf("invalid computer-use action_phase %q", finish.Phase)
	}
	if finish.Result != nil && !ValidComputerUseActionResult(*finish.Result) {
		return fmt.Errorf("invalid computer-use action_result %q", *finish.Result)
	}
	if finish.ExecutionPath != nil && !ValidComputerUseExecutionPath(*finish.ExecutionPath) {
		return fmt.Errorf("invalid computer-use execution_path %q", *finish.ExecutionPath)
	}
	if finish.Pointer != nil {
		if err := finish.Pointer.Validate(); err != nil {
			return err
		}
	}

	c.mu.Lock()
	if c.active == nil || c.active.lease.LeaseID != finish.LeaseID {
		c.mu.Unlock()
		return &StaleLeaseError{LeaseID: finish.LeaseID}
	}
	if c.active.currentAction == nil || c.active.currentAction.id != finish.ActionID {
		c.mu.Unlock()
		return &StaleActionError{LeaseID: finish.LeaseID, ActionID: finish.ActionID}
	}
	action := c.active.currentAction
	if action.executionAuthority != nil {
		action.executionAuthority.active.Store(false)
	}
	cancel := action.cancel
	c.active.currentAction = nil
	state := &c.active.activity
	state.ActionResult = cloneActionResult(finish.Result)
	state.ExecutionPath = cloneExecutionPath(finish.ExecutionPath)
	state.Pointer = clonePointer(finish.Pointer)
	state.FailureCode = cloneString(finish.FailureCode)
	state.ActionPhase = finish.Phase
	if state.ActionPhase == "" {
		state.ActionPhase = ComputerUsePhaseIdle
	}
	terminal := c.active.lease.State == ComputerUseLeaseStopping
	mutationRequiresObservation := action.effect == ComputerUseActionMutation &&
		!action.orderedBatchAction && finish.Result != nil &&
		(*finish.Result == ComputerUseResultVerified ||
			*finish.Result == ComputerUseResultCompletedUnverified ||
			(*finish.Result == ComputerUseResultCancelled && finish.FailureCode != nil &&
				*finish.FailureCode == "commit_status_unknown_after_cancel"))
	if mutationRequiresObservation {
		// A verified mutation consumed the old observation, while an unverified
		// or commit-unknown result may still have changed it. Preserve the
		// stale-state barrier across later Pause/Resume; only an exact verified
		// computer_use get_app_state may clear it below.
		c.active.requiresObservation = true
	}
	if action.effect == ComputerUseActionObservation &&
		finish.Result != nil && *finish.Result == ComputerUseResultVerified &&
		action.targetBundleID != "" {
		if c.active.lease.RequestedAppBundleID == "" {
			c.active.lease.RequestedAppBundleID = action.targetBundleID
			c.active.lease.RequestedAppName = action.targetAppName
		}
		alreadyAllowed := false
		for _, allowed := range c.active.lease.AllowedAppBundleIDs {
			if action.targetBundleID == allowed {
				alreadyAllowed = true
				break
			}
		}
		if !alreadyAllowed {
			c.active.lease.AllowedAppBundleIDs = append(
				c.active.lease.AllowedAppBundleIDs, action.targetBundleID)
		}
	}
	if terminal {
		state.LeaseState = ComputerUseLeaseTerminal
		if action.cancellationResult != nil {
			state.ActionResult = cloneActionResult(action.cancellationResult)
		} else {
			result := ComputerUseResultCancelled
			state.ActionResult = &result
		}
		if action.cancellationFailureCode != nil {
			state.FailureCode = cloneString(action.cancellationFailureCode)
		}
	} else if c.active.lease.State == ComputerUseLeasePaused && action.cancellationResult != nil {
		state.LeaseState = ComputerUseLeasePaused
		state.ActionPhase = ComputerUsePhaseWaitingForUser
		state.ActionResult = cloneActionResult(action.cancellationResult)
		state.FailureCode = nil
	} else if finish.Result != nil && *finish.Result == ComputerUseResultUserInterference {
		c.active.requiresObservation = true
		c.active.lease.State = ComputerUseLeasePaused
		state.LeaseState = ComputerUseLeasePaused
		state.ActionPhase = ComputerUsePhaseWaitingForUser
	} else if action.effect == ComputerUseActionObservation && action.toolName == "computer_use" &&
		action.actionKind == "get_app_state" && finish.Result != nil &&
		*finish.Result == ComputerUseResultVerified {
		c.active.requiresObservation = false
	}
	c.notifyStateChangedLocked()
	event, sink, err := c.eventLocked()
	if terminal {
		c.active = nil
	}
	c.mu.Unlock()
	cancel()
	if err != nil {
		return err
	}
	c.emitOrdered(sink, event)
	return nil
}

func (c *Coordinator) EndTurn(turnID string, result ComputerUseActionResult) error {
	if turnID == "" {
		return fmt.Errorf("computer-use end turn_id is required")
	}
	if !ValidComputerUseActionResult(result) {
		return fmt.Errorf("invalid computer-use terminal result %q", result)
	}
	c.mu.Lock()
	if c.active == nil {
		c.mu.Unlock()
		return nil
	}
	if c.active.lease.TurnID != turnID {
		state := c.active.lease.State
		c.mu.Unlock()
		return &InvalidTransitionError{State: state}
	}
	if c.active.currentAction != nil {
		actionID := c.active.currentAction.id
		leaseID := c.active.lease.LeaseID
		c.mu.Unlock()
		return &ActionInProgressError{LeaseID: leaseID, ActionID: actionID}
	}
	var pendingCancel context.CancelFunc
	if c.active.pendingRisk != nil {
		pendingCancel = c.active.pendingRisk.cancel
		c.active.pendingRisk = nil
		c.active.activity.ConsequentialRisk = nil
	}
	if c.active.lease.State == ComputerUseLeaseStopping {
		result = ComputerUseResultCancelled
	}
	c.active.lease.State = ComputerUseLeaseTerminal
	c.active.activity.LeaseState = ComputerUseLeaseTerminal
	c.active.activity.ActionPhase = ComputerUsePhaseIdle
	c.active.activity.ActionResult = &result
	c.notifyStateChangedLocked()
	event, sink, err := c.eventLocked()
	c.active = nil
	c.mu.Unlock()
	if pendingCancel != nil {
		pendingCancel()
	}
	if err != nil {
		return err
	}
	c.emitOrdered(sink, event)
	return nil
}

func (c *Coordinator) Snapshot() ComputerUseActivitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := ComputerUseActivitySnapshot{
		SchemaVersion:         ComputerUseActivitySchemaVersion,
		CoordinatorInstanceID: c.instanceID,
		Revision:              c.revision,
	}
	if c.active != nil && c.active.lease.State != ComputerUseLeaseTerminal {
		state := cloneActivityState(c.active.activity)
		snapshot.Active = &state
	}
	return snapshot
}

func (c *Coordinator) Control(request ComputerUseControlRequest) (ComputerUseControlResponse, error) {
	if err := request.Validate(); err != nil {
		return ComputerUseControlResponse{}, err
	}
	c.mu.Lock()
	if prior, found := c.controlLedger[request.IdempotencyKey]; found {
		if prior.request != request {
			c.mu.Unlock()
			return ComputerUseControlResponse{}, &IdempotencyConflictError{Key: request.IdempotencyKey}
		}
		response := prior.response
		c.mu.Unlock()
		return response, nil
	}
	if c.active == nil || c.active.lease.LeaseID != request.LeaseID {
		c.mu.Unlock()
		return ComputerUseControlResponse{}, &StaleLeaseError{LeaseID: request.LeaseID}
	}
	if c.active.lease.State == ComputerUseLeaseStopping &&
		c.tombstones[c.active.lease.TurnID] == tombstoneExpired {
		turnID := c.active.lease.TurnID
		c.mu.Unlock()
		return ComputerUseControlResponse{}, &LeaseExpiredError{LeaseID: request.LeaseID, TurnID: turnID}
	}
	if !c.now().Before(c.active.lease.ExpiresAt) {
		turnID := c.active.lease.TurnID
		cancel, event, sink := c.expireLocked()
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.emitOrdered(sink, event)
		return ComputerUseControlResponse{}, &LeaseExpiredError{LeaseID: request.LeaseID, TurnID: turnID}
	}

	var cancel context.CancelFunc
	terminalImmediately := false
	state := c.active.lease.State
	switch request.Action {
	case ComputerUseControlPause:
		if state != ComputerUseLeaseActive {
			c.mu.Unlock()
			return ComputerUseControlResponse{}, &InvalidTransitionError{Action: request.Action, State: state}
		}
		cancel = c.requestInFlightCancellationLocked(ComputerUseResultCancelled)
		c.active.requiresObservation = true
		c.active.lease.State = ComputerUseLeasePaused
		c.active.activity.LeaseState = ComputerUseLeasePaused
		c.active.activity.ActionPhase = ComputerUsePhaseWaitingForUser

	case ComputerUseControlResume:
		if state != ComputerUseLeasePaused {
			c.mu.Unlock()
			return ComputerUseControlResponse{}, &InvalidTransitionError{Action: request.Action, State: state}
		}
		if c.active.currentAction != nil {
			actionID := c.active.currentAction.id
			c.mu.Unlock()
			return ComputerUseControlResponse{}, &ActionInProgressError{LeaseID: request.LeaseID, ActionID: actionID}
		}
		c.active.lease.State = ComputerUseLeaseActive
		c.active.activity.LeaseState = ComputerUseLeaseActive
		c.active.activity.ActionPhase = ComputerUsePhaseObserving
		c.clearActionPresentationLocked()

	case ComputerUseControlTakeOver:
		if state != ComputerUseLeaseActive && state != ComputerUseLeasePaused {
			c.mu.Unlock()
			return ComputerUseControlResponse{}, &InvalidTransitionError{Action: request.Action, State: state}
		}
		cancel = c.requestInFlightCancellationLocked(ComputerUseResultUserInterference)
		c.active.requiresObservation = true
		c.active.lease.State = ComputerUseLeasePaused
		c.active.activity.LeaseState = ComputerUseLeasePaused
		c.active.activity.ActionPhase = ComputerUsePhaseWaitingForUser

	case ComputerUseControlStop:
		if state != ComputerUseLeaseActive && state != ComputerUseLeasePaused && state != ComputerUseLeaseRequestingApproval {
			c.mu.Unlock()
			return ComputerUseControlResponse{}, &InvalidTransitionError{Action: request.Action, State: state}
		}
		cancel = c.requestInFlightCancellationLocked(ComputerUseResultCancelled)
		c.rememberTombstoneLocked(c.active.lease.TurnID, tombstoneStopped)
		terminalImmediately = c.active.currentAction == nil
		if terminalImmediately {
			c.active.lease.State = ComputerUseLeaseTerminal
			c.active.activity.LeaseState = ComputerUseLeaseTerminal
		} else {
			c.active.lease.State = ComputerUseLeaseStopping
			c.active.activity.LeaseState = ComputerUseLeaseStopping
		}
		c.active.activity.ActionPhase = ComputerUsePhaseIdle
		result := ComputerUseResultCancelled
		c.active.activity.ActionResult = &result
	}
	c.notifyStateChangedLocked()

	event, sink, err := c.eventLocked()
	if err != nil {
		c.mu.Unlock()
		return ComputerUseControlResponse{}, err
	}
	response := ComputerUseControlResponse{
		Accepted:   true,
		Quiesced:   c.active == nil || c.active.currentAction == nil,
		LeaseID:    request.LeaseID,
		Revision:   event.Revision,
		LeaseState: event.LeaseState,
	}
	c.rememberControlLocked(request, response)
	if terminalImmediately {
		c.active = nil
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.emitOrdered(sink, event)
	return response, nil
}

func (c *Coordinator) Heartbeat(leaseID string) (ComputerUseHeartbeatResponse, error) {
	if leaseID == "" {
		return ComputerUseHeartbeatResponse{}, fmt.Errorf("computer-use heartbeat lease_id is required")
	}
	c.mu.Lock()
	if c.active == nil || c.active.lease.LeaseID != leaseID {
		c.mu.Unlock()
		return ComputerUseHeartbeatResponse{}, &StaleLeaseError{LeaseID: leaseID}
	}
	if c.active.lease.State == ComputerUseLeaseStopping &&
		c.tombstones[c.active.lease.TurnID] == tombstoneExpired {
		turnID := c.active.lease.TurnID
		c.mu.Unlock()
		return ComputerUseHeartbeatResponse{}, &LeaseExpiredError{LeaseID: leaseID, TurnID: turnID}
	}
	if c.active.lease.State == ComputerUseLeaseStopping || c.active.lease.State == ComputerUseLeaseTerminal {
		state := c.active.lease.State
		c.mu.Unlock()
		return ComputerUseHeartbeatResponse{}, &InvalidTransitionError{State: state}
	}
	now := c.now()
	if !now.Before(c.active.lease.ExpiresAt) {
		turnID := c.active.lease.TurnID
		cancel, event, sink := c.expireLocked()
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.emitOrdered(sink, event)
		return ComputerUseHeartbeatResponse{}, &LeaseExpiredError{LeaseID: leaseID, TurnID: turnID}
	}
	c.active.lease.HeartbeatAt = now
	c.active.lease.ExpiresAt = now.Add(c.leaseTTL)
	c.active.controllerAcknowledged = true
	c.notifyStateChangedLocked()
	response := ComputerUseHeartbeatResponse{
		SchemaVersion:         ComputerUseHeartbeatSchemaVersion,
		CoordinatorInstanceID: c.instanceID,
		Revision:              c.revision,
		LeaseID:               c.active.lease.LeaseID,
		LeaseState:            c.active.lease.State,
		HeartbeatAt:           c.active.lease.HeartbeatAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:             c.active.lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	c.mu.Unlock()
	return response, nil
}

// ExpireStale is the deterministic sweep seam used by the daemon heartbeat
// ticker and the authoritative activity GET. It returns whether a lease expired.
func (c *Coordinator) ExpireStale() bool {
	c.mu.Lock()
	if c.active == nil || c.active.lease.State == ComputerUseLeaseStopping ||
		c.now().Before(c.active.lease.ExpiresAt) {
		c.mu.Unlock()
		return false
	}
	cancel, event, sink := c.expireLocked()
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.emitOrdered(sink, event)
	return true
}

func (c *Coordinator) expireLocked() (context.CancelFunc, ComputerUseActivityEvent, ActivitySink) {
	var cancel context.CancelFunc
	if c.active.currentAction != nil {
		c.revokeExecutionAuthorityLocked()
		cancel = c.active.currentAction.cancel
	} else if c.active.pendingRisk != nil {
		cancel = c.active.pendingRisk.cancel
		c.active.pendingRisk = nil
		c.active.activity.ConsequentialRisk = nil
	}
	turnID := c.active.lease.TurnID
	c.rememberTombstoneLocked(turnID, tombstoneExpired)
	terminalImmediately := c.active.currentAction == nil
	if terminalImmediately {
		c.active.lease.State = ComputerUseLeaseTerminal
		c.active.activity.LeaseState = ComputerUseLeaseTerminal
	} else {
		c.active.lease.State = ComputerUseLeaseStopping
		c.active.activity.LeaseState = ComputerUseLeaseStopping
	}
	c.active.activity.ActionPhase = ComputerUsePhaseIdle
	result := ComputerUseResultCancelled
	c.active.activity.ActionResult = &result
	failure := "lease_heartbeat_expired"
	c.active.activity.FailureCode = &failure
	if c.active.currentAction != nil {
		c.active.currentAction.cancellationResult = cloneActionResult(&result)
		c.active.currentAction.cancellationFailureCode = cloneString(&failure)
	}
	c.notifyStateChangedLocked()
	event, sink, _ := c.eventLocked()
	if terminalImmediately {
		c.active = nil
	}
	return cancel, event, sink
}

// revokeExecutionAuthorityLocked kills the capability minted for the in-flight
// action. Cancelling its context alone is not sufficient: ExecutionAuthorized is
// a pure capability check, so a Stop / Take Over / lease expiry that lands while
// the tool is still inside DescribeGUIAction or a consequential-risk preflight
// would otherwise still find a live authority at the final execution gate.
func (c *Coordinator) revokeExecutionAuthorityLocked() {
	if c.active == nil || c.active.currentAction == nil {
		return
	}
	if authority := c.active.currentAction.executionAuthority; authority != nil {
		authority.active.Store(false)
	}
}

func (c *Coordinator) requestActionCancellationLocked(result ComputerUseActionResult) context.CancelFunc {
	if c.active.currentAction == nil {
		return nil
	}
	c.revokeExecutionAuthorityLocked()
	cancel := c.active.currentAction.cancel
	c.active.currentAction.cancellationResult = cloneActionResult(&result)
	c.active.activity.ActionResult = &result
	return cancel
}

func (c *Coordinator) requestInFlightCancellationLocked(result ComputerUseActionResult) context.CancelFunc {
	if c.active.pendingRisk != nil {
		cancel := c.active.pendingRisk.cancel
		c.active.pendingRisk = nil
		c.active.activity.ConsequentialRisk = nil
		c.active.activity.ActionResult = &result
		return cancel
	}
	return c.requestActionCancellationLocked(result)
}

func (c *Coordinator) clearActionPresentationLocked() {
	c.active.activity.ActionID = ""
	c.active.activity.ToolUseID = ""
	c.active.activity.ActionKind = ""
	c.active.activity.ActionResult = nil
	c.active.activity.ExecutionPath = nil
	c.active.activity.Pointer = nil
	c.active.activity.FailureCode = nil
	c.active.activity.ConsequentialRisk = nil
}

func (c *Coordinator) notifyStateChangedLocked() {
	close(c.active.stateChanged)
	c.active.stateChanged = make(chan struct{})
}

func stopAndDrainTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (c *Coordinator) eventLocked() (ComputerUseActivityEvent, ActivitySink, error) {
	c.active.activity.TS = c.now().UTC().Format(time.RFC3339Nano)
	if err := c.active.activity.Validate(); err != nil {
		return ComputerUseActivityEvent{}, nil, err
	}
	c.revision++
	event := ComputerUseActivityEvent{
		SchemaVersion:            ComputerUseActivitySchemaVersion,
		CoordinatorInstanceID:    c.instanceID,
		Revision:                 c.revision,
		ComputerUseActivityState: cloneActivityState(c.active.activity),
	}
	return event, c.sink, nil
}

func (c *Coordinator) busyErrorLocked() *BusyError {
	retryAfter := time.Until(c.active.lease.ExpiresAt)
	if c.now != nil {
		retryAfter = c.active.lease.ExpiresAt.Sub(c.now())
	}
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &BusyError{
		ActiveLeaseID:  c.active.lease.LeaseID,
		SourceKind:     c.active.lease.SourceKind,
		SourceLabel:    c.active.lease.SourceLabel,
		TargetBundleID: c.active.activity.TargetBundleID,
		TargetAppName:  c.active.activity.TargetAppName,
		RetryAfter:     retryAfter,
	}
}

func (c *Coordinator) rememberControlLocked(request ComputerUseControlRequest, response ComputerUseControlResponse) {
	if len(c.controlOrder) >= c.ledgerLimit {
		oldest := c.controlOrder[0]
		c.controlOrder = c.controlOrder[1:]
		delete(c.controlLedger, oldest)
	}
	c.controlOrder = append(c.controlOrder, request.IdempotencyKey)
	c.controlLedger[request.IdempotencyKey] = controlLedgerEntry{request: request, response: response}
}

func (c *Coordinator) rememberTombstoneLocked(turnID string, reason tombstoneReason) {
	if _, exists := c.tombstones[turnID]; exists {
		c.tombstones[turnID] = reason
		return
	}
	if len(c.tombstoneOrder) >= c.tombstoneLimit {
		oldest := c.tombstoneOrder[0]
		c.tombstoneOrder = c.tombstoneOrder[1:]
		delete(c.tombstones, oldest)
	}
	c.tombstoneOrder = append(c.tombstoneOrder, turnID)
	c.tombstones[turnID] = reason
}

// emitOrdered keeps the transport-visible order equal to coordinator revision
// order without invoking the sink under the state mutex. A later mutation may
// finish first, but it cannot overtake an earlier event at the sink boundary.
func (c *Coordinator) emitOrdered(sink ActivitySink, event ComputerUseActivityEvent) {
	c.emitMu.Lock()
	for event.Revision != c.nextEmitRevision {
		c.emitCond.Wait()
	}
	defer func() {
		c.nextEmitRevision++
		c.emitCond.Broadcast()
		c.emitMu.Unlock()
	}()
	if sink != nil {
		sink(event)
	}
}

func cloneWorkflowLease(lease WorkflowLease) WorkflowLease {
	clone := lease
	clone.AllowedAppBundleIDs = append([]string(nil), lease.AllowedAppBundleIDs...)
	return clone
}

func cloneActionResult(value *ComputerUseActionResult) *ComputerUseActionResult {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneExecutionPath(value *ComputerUseExecutionPath) *ComputerUseExecutionPath {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func clonePointer(value *ComputerUsePointer) *ComputerUsePointer {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
