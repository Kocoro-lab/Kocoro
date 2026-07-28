package daemon

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

// The daemon names remain aliases during migration so existing callers and
// cross-repository wire fixtures compile against one canonical contract.
const ComputerUseActivitySchemaVersion = guicontrol.ComputerUseActivitySchemaVersion

type ComputerUseLeaseState = guicontrol.ComputerUseLeaseState

const (
	ComputerUseLeaseRequestingApproval = guicontrol.ComputerUseLeaseRequestingApproval
	ComputerUseLeaseActive             = guicontrol.ComputerUseLeaseActive
	ComputerUseLeasePaused             = guicontrol.ComputerUseLeasePaused
	ComputerUseLeaseStopping           = guicontrol.ComputerUseLeaseStopping
	ComputerUseLeaseTerminal           = guicontrol.ComputerUseLeaseTerminal
)

type ComputerUseActionPhase = guicontrol.ComputerUseActionPhase

const (
	ComputerUsePhaseIdle           = guicontrol.ComputerUsePhaseIdle
	ComputerUsePhaseObserving      = guicontrol.ComputerUsePhaseObserving
	ComputerUsePhaseMoving         = guicontrol.ComputerUsePhaseMoving
	ComputerUsePhaseActing         = guicontrol.ComputerUsePhaseActing
	ComputerUsePhaseInputCommitted = guicontrol.ComputerUsePhaseInputCommitted
	ComputerUsePhaseVerifying      = guicontrol.ComputerUsePhaseVerifying
	ComputerUsePhaseWaitingForUser = guicontrol.ComputerUsePhaseWaitingForUser
)

type ComputerUseActionResult = guicontrol.ComputerUseActionResult

const (
	ComputerUseResultVerified            = guicontrol.ComputerUseResultVerified
	ComputerUseResultCompletedUnverified = guicontrol.ComputerUseResultCompletedUnverified
	ComputerUseResultFailed              = guicontrol.ComputerUseResultFailed
	ComputerUseResultCancelled           = guicontrol.ComputerUseResultCancelled
	ComputerUseResultUserInterference    = guicontrol.ComputerUseResultUserInterference
)

type ComputerUseExecutionPath = guicontrol.ComputerUseExecutionPath

const (
	ComputerUseExecutionAccessibility       = guicontrol.ComputerUseExecutionAccessibility
	ComputerUseExecutionSyntheticCoordinate = guicontrol.ComputerUseExecutionSyntheticCoordinate
)

type ComputerUseExecutionLane = guicontrol.ComputerUseExecutionLane

const (
	ComputerUseExecutionForeground         = guicontrol.ComputerUseExecutionForeground
	ComputerUseExecutionBackgroundSemantic = guicontrol.ComputerUseExecutionBackgroundSemantic
	ComputerUseExecutionBackgroundKeyboard = guicontrol.ComputerUseExecutionBackgroundKeyboard
)

type ComputerUseCoordinateSpace = guicontrol.ComputerUseCoordinateSpace

const ComputerUseCoordinateQuartzGlobalPoints = guicontrol.ComputerUseCoordinateQuartzGlobalPoints

type ComputerUsePointer = guicontrol.ComputerUsePointer
type ComputerUseActivityState = guicontrol.ComputerUseActivityState
type ConsequentialRiskMarkerV1 = guicontrol.ConsequentialRiskMarkerV1
type ComputerUseActivityEvent = guicontrol.ComputerUseActivityEvent
type ComputerUseActivitySnapshot = guicontrol.ComputerUseActivitySnapshot

func EncodeComputerUseActivitySnapshot(snapshot ComputerUseActivitySnapshot) ([]byte, error) {
	return guicontrol.EncodeComputerUseActivitySnapshot(snapshot)
}

type ComputerUseControlAction = guicontrol.ComputerUseControlAction

const (
	ComputerUseControlPause    = guicontrol.ComputerUseControlPause
	ComputerUseControlResume   = guicontrol.ComputerUseControlResume
	ComputerUseControlTakeOver = guicontrol.ComputerUseControlTakeOver
	ComputerUseControlStop     = guicontrol.ComputerUseControlStop
)

type ComputerUseControlRequest = guicontrol.ComputerUseControlRequest
type ComputerUseControlResponse = guicontrol.ComputerUseControlResponse

func DecodeComputerUseControlRequest(payload []byte) (ComputerUseControlRequest, error) {
	return guicontrol.DecodeComputerUseControlRequest(payload)
}

func EncodeComputerUseControlResponse(response ComputerUseControlResponse) ([]byte, error) {
	return guicontrol.EncodeComputerUseControlResponse(response)
}

const ComputerUseHeartbeatSchemaVersion = guicontrol.ComputerUseHeartbeatSchemaVersion

type ComputerUseHeartbeatRequest = guicontrol.ComputerUseHeartbeatRequest
type ComputerUseHeartbeatResponse = guicontrol.ComputerUseHeartbeatResponse

func EncodeComputerUseHeartbeatRequest(request ComputerUseHeartbeatRequest) ([]byte, error) {
	return guicontrol.EncodeComputerUseHeartbeatRequest(request)
}

func DecodeComputerUseHeartbeatRequest(payload []byte) (ComputerUseHeartbeatRequest, error) {
	return guicontrol.DecodeComputerUseHeartbeatRequest(payload)
}

func EncodeComputerUseHeartbeatResponse(response ComputerUseHeartbeatResponse) ([]byte, error) {
	return guicontrol.EncodeComputerUseHeartbeatResponse(response)
}

func DecodeComputerUseHeartbeatResponse(payload []byte) (ComputerUseHeartbeatResponse, error) {
	return guicontrol.DecodeComputerUseHeartbeatResponse(payload)
}

// ComputerUseActivityEmitter is retained as a compatibility seam for the
// established daemon wire-fixture tests. New runtime wiring emits from the
// process-wide guicontrol.Coordinator instead.
type ComputerUseActivityEmitter struct {
	mu                    sync.Mutex
	bus                   *EventBus
	revision              uint64
	coordinatorInstanceID string
	now                   func() time.Time
}

func NewComputerUseActivityEmitter(bus *EventBus) *ComputerUseActivityEmitter {
	return newComputerUseActivityEmitter(bus, 0, newComputerUseCoordinatorInstanceID(), time.Now)
}

func newComputerUseActivityEmitter(bus *EventBus, revision uint64, coordinatorInstanceID string, now func() time.Time) *ComputerUseActivityEmitter {
	if now == nil {
		now = time.Now
	}
	return &ComputerUseActivityEmitter{
		bus:                   bus,
		revision:              revision,
		coordinatorInstanceID: coordinatorInstanceID,
		now:                   now,
	}
}

func newComputerUseCoordinatorInstanceID() string {
	return guicontrol.NewCoordinatorInstanceID()
}

func (e *ComputerUseActivityEmitter) Emit(state ComputerUseActivityState) (ComputerUseActivityEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.coordinatorInstanceID == "" {
		return ComputerUseActivityEvent{}, fmt.Errorf("computer-use activity coordinator_instance_id is required")
	}
	state.TS = e.now().UTC().Format(time.RFC3339Nano)
	if err := state.Validate(); err != nil {
		return ComputerUseActivityEvent{}, err
	}
	nextRevision := e.revision + 1
	event := ComputerUseActivityEvent{
		SchemaVersion:            ComputerUseActivitySchemaVersion,
		CoordinatorInstanceID:    e.coordinatorInstanceID,
		Revision:                 nextRevision,
		ComputerUseActivityState: state,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return ComputerUseActivityEvent{}, fmt.Errorf("encode computer-use activity: %w", err)
	}
	e.revision = nextRevision
	if e.bus != nil {
		e.bus.Emit(Event{Type: EventComputerUseActivity, Payload: payload})
	}
	return event, nil
}
