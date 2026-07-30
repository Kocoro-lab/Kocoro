// Package guicontrol owns the process-wide GUI workflow lease and its
// provider-neutral, redacted activity contract. It deliberately has no
// dependency on agent, tools, or daemon so all three layers can share it
// without import cycles.
package guicontrol

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const ComputerUseActivitySchemaVersion = 1

type ComputerUseLeaseState string

const (
	ComputerUseLeaseRequestingApproval ComputerUseLeaseState = "requesting_approval"
	ComputerUseLeaseActive             ComputerUseLeaseState = "active"
	ComputerUseLeasePaused             ComputerUseLeaseState = "paused"
	ComputerUseLeaseStopping           ComputerUseLeaseState = "stopping"
	ComputerUseLeaseTerminal           ComputerUseLeaseState = "terminal"
)

type ComputerUseActionPhase string

const (
	ComputerUsePhaseIdle           ComputerUseActionPhase = "idle"
	ComputerUsePhaseObserving      ComputerUseActionPhase = "observing"
	ComputerUsePhaseMoving         ComputerUseActionPhase = "moving"
	ComputerUsePhaseActing         ComputerUseActionPhase = "acting"
	ComputerUsePhaseInputCommitted ComputerUseActionPhase = "input_committed"
	ComputerUsePhaseVerifying      ComputerUseActionPhase = "verifying"
	ComputerUsePhaseWaitingForUser ComputerUseActionPhase = "waiting_for_user"
)

type ComputerUseActionResult string

const (
	ComputerUseResultVerified            ComputerUseActionResult = "verified"
	ComputerUseResultCompletedUnverified ComputerUseActionResult = "completed_unverified"
	ComputerUseResultFailed              ComputerUseActionResult = "failed"
	ComputerUseResultCancelled           ComputerUseActionResult = "cancelled"
	ComputerUseResultUserInterference    ComputerUseActionResult = "user_interference"
)

type ComputerUseExecutionPath string

const (
	ComputerUseExecutionAccessibility       ComputerUseExecutionPath = "accessibility"
	ComputerUseExecutionSyntheticCoordinate ComputerUseExecutionPath = "synthetic_coordinate"
)

type ComputerUseExecutionLane string

const (
	ComputerUseExecutionForeground         ComputerUseExecutionLane = "foreground"
	ComputerUseExecutionBackgroundSemantic ComputerUseExecutionLane = "background_semantic"
	ComputerUseExecutionBackgroundKeyboard ComputerUseExecutionLane = "background_keyboard"
)

type ComputerUseCoordinateSpace string

const ComputerUseCoordinateQuartzGlobalPoints ComputerUseCoordinateSpace = "quartz_global_points"

// ComputerUsePointer contains only the geometry required by trusted Desktop
// presentation. Captured pixels and target content never belong here.
type ComputerUsePointer struct {
	DisplayID          uint32                     `json:"display_id"`
	TopologyID         string                     `json:"topology_id"`
	TopologyGeneration uint64                     `json:"topology_generation"`
	X                  float64                    `json:"x"`
	Y                  float64                    `json:"y"`
	CoordinateSpace    ComputerUseCoordinateSpace `json:"coordinate_space"`
}

// ConsequentialRiskMarkerV1 is the only content-free confirmation state that
// may cross the activity/snapshot wire. Target digests and labels cannot be
// represented by this type.
type ConsequentialRiskMarkerV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Required      bool   `json:"required"`
	Kind          string `json:"kind"`
	IntentID      string `json:"intent_id"`
	ExpiresAt     string `json:"expires_at"`
}

func (m ConsequentialRiskMarkerV1) Validate() error {
	if m.SchemaVersion != 1 || !m.Required {
		return fmt.Errorf("invalid consequential-risk marker version/required state")
	}
	if m.Kind != "send" && m.Kind != "delete" && m.Kind != "purchase" {
		return fmt.Errorf("invalid consequential-risk marker kind")
	}
	if !strings.HasPrefix(m.IntentID, "cri_") {
		return fmt.Errorf("invalid consequential-risk marker intent_id")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(m.IntentID, "cri_"))
	if err != nil || len(raw) != 16 {
		return fmt.Errorf("invalid consequential-risk marker intent_id")
	}
	if _, err := time.Parse(time.RFC3339, m.ExpiresAt); err != nil {
		return fmt.Errorf("invalid consequential-risk marker expires_at")
	}
	return nil
}

func (p ComputerUsePointer) Validate() error {
	if p.TopologyID == "" || p.TopologyGeneration == 0 {
		return fmt.Errorf("computer-use pointer topology authority is required")
	}
	if p.CoordinateSpace != ComputerUseCoordinateQuartzGlobalPoints {
		return fmt.Errorf("invalid computer-use pointer coordinate_space %q", p.CoordinateSpace)
	}
	if math.IsNaN(p.X) || math.IsInf(p.X, 0) || math.IsNaN(p.Y) || math.IsInf(p.Y, 0) {
		return fmt.Errorf("computer-use pointer coordinates must be finite")
	}
	return nil
}

// ComputerUseActivityState is the shared state body used by activity events
// and reconnect snapshots. Never add typed text, raw tool arguments, prompts,
// clipboard contents, screenshots, or Accessibility values to this contract.
type ComputerUseActivityState struct {
	LeaseID            string                     `json:"lease_id"`
	SessionID          string                     `json:"session_id"`
	ActionID           string                     `json:"action_id,omitempty"`
	ToolUseID          string                     `json:"tool_use_id,omitempty"`
	SourceKind         string                     `json:"source_kind,omitempty"`
	SourceLabel        string                     `json:"source_label,omitempty"`
	TargetBundleID     string                     `json:"target_bundle_id,omitempty"`
	TargetAppName      string                     `json:"target_app_name,omitempty"`
	ActionKind         string                     `json:"action_kind,omitempty"`
	LeaseState         ComputerUseLeaseState      `json:"lease_state"`
	ActionPhase        ComputerUseActionPhase     `json:"action_phase"`
	ActionResult       *ComputerUseActionResult   `json:"action_result"`
	ExecutionPath      *ComputerUseExecutionPath  `json:"execution_path"`
	ExecutionLane      *ComputerUseExecutionLane  `json:"execution_lane"`
	ForegroundFallback bool                       `json:"foreground_fallback"`
	Pointer            *ComputerUsePointer        `json:"pointer"`
	FailureCode        *string                    `json:"failure_code"`
	ConsequentialRisk  *ConsequentialRiskMarkerV1 `json:"consequential_risk"`
	TS                 string                     `json:"ts"`
}

func (s ComputerUseActivityState) Validate() error {
	if s.LeaseID == "" {
		return fmt.Errorf("computer-use activity lease_id is required")
	}
	if s.SessionID == "" {
		return fmt.Errorf("computer-use activity session_id is required")
	}
	if !ValidComputerUseLeaseState(s.LeaseState) {
		return fmt.Errorf("invalid computer-use lease_state %q", s.LeaseState)
	}
	if !ValidComputerUseActionPhase(s.ActionPhase) {
		return fmt.Errorf("invalid computer-use action_phase %q", s.ActionPhase)
	}
	if s.ActionResult != nil && !ValidComputerUseActionResult(*s.ActionResult) {
		return fmt.Errorf("invalid computer-use action_result %q", *s.ActionResult)
	}
	if s.ExecutionPath != nil && !ValidComputerUseExecutionPath(*s.ExecutionPath) {
		return fmt.Errorf("invalid computer-use execution_path %q", *s.ExecutionPath)
	}
	if s.ExecutionLane != nil && !ValidComputerUseExecutionLane(*s.ExecutionLane) {
		return fmt.Errorf("invalid computer-use execution_lane %q", *s.ExecutionLane)
	}
	if s.ForegroundFallback &&
		(s.ExecutionLane == nil ||
			*s.ExecutionLane != ComputerUseExecutionForeground) {
		return fmt.Errorf("foreground fallback requires foreground execution_lane")
	}
	if s.ExecutionLane != nil &&
		(*s.ExecutionLane == ComputerUseExecutionBackgroundSemantic ||
			*s.ExecutionLane == ComputerUseExecutionBackgroundKeyboard) &&
		s.Pointer != nil {
		return fmt.Errorf("background activity cannot publish a physical pointer")
	}
	if s.Pointer != nil {
		if err := s.Pointer.Validate(); err != nil {
			return err
		}
	}
	if s.ConsequentialRisk != nil {
		if err := s.ConsequentialRisk.Validate(); err != nil {
			return err
		}
		if s.ActionPhase != ComputerUsePhaseWaitingForUser {
			return fmt.Errorf("consequential-risk marker requires waiting_for_user phase")
		}
	}
	if s.TS == "" {
		return fmt.Errorf("computer-use activity ts is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, s.TS); err != nil {
		return fmt.Errorf("computer-use activity ts must be RFC3339: %w", err)
	}
	return nil
}

func ValidComputerUseLeaseState(value ComputerUseLeaseState) bool {
	switch value {
	case ComputerUseLeaseRequestingApproval, ComputerUseLeaseActive, ComputerUseLeasePaused,
		ComputerUseLeaseStopping, ComputerUseLeaseTerminal:
		return true
	default:
		return false
	}
}

func ValidComputerUseActionPhase(value ComputerUseActionPhase) bool {
	switch value {
	case ComputerUsePhaseIdle, ComputerUsePhaseObserving, ComputerUsePhaseMoving,
		ComputerUsePhaseActing, ComputerUsePhaseInputCommitted, ComputerUsePhaseVerifying,
		ComputerUsePhaseWaitingForUser:
		return true
	default:
		return false
	}
}

func ValidComputerUseActionResult(value ComputerUseActionResult) bool {
	switch value {
	case ComputerUseResultVerified, ComputerUseResultCompletedUnverified, ComputerUseResultFailed,
		ComputerUseResultCancelled, ComputerUseResultUserInterference:
		return true
	default:
		return false
	}
}

func ValidComputerUseExecutionPath(value ComputerUseExecutionPath) bool {
	switch value {
	case ComputerUseExecutionAccessibility, ComputerUseExecutionSyntheticCoordinate:
		return true
	default:
		return false
	}
}

func ValidComputerUseExecutionLane(value ComputerUseExecutionLane) bool {
	switch value {
	case ComputerUseExecutionForeground, ComputerUseExecutionBackgroundSemantic,
		ComputerUseExecutionBackgroundKeyboard:
		return true
	default:
		return false
	}
}

type ComputerUseActivityEvent struct {
	SchemaVersion         int    `json:"schema_version"`
	CoordinatorInstanceID string `json:"coordinator_instance_id"`
	Revision              uint64 `json:"revision"`
	ComputerUseActivityState
}

type ComputerUseActivitySnapshot struct {
	SchemaVersion         int                       `json:"schema_version"`
	CoordinatorInstanceID string                    `json:"coordinator_instance_id"`
	Revision              uint64                    `json:"revision"`
	Active                *ComputerUseActivityState `json:"active"`
}

func EncodeComputerUseActivitySnapshot(snapshot ComputerUseActivitySnapshot) ([]byte, error) {
	if snapshot.SchemaVersion != ComputerUseActivitySchemaVersion {
		return nil, fmt.Errorf("unsupported computer-use activity schema_version %d", snapshot.SchemaVersion)
	}
	if snapshot.CoordinatorInstanceID == "" {
		return nil, fmt.Errorf("computer-use activity coordinator_instance_id is required")
	}
	if snapshot.Active != nil {
		if err := snapshot.Active.Validate(); err != nil {
			return nil, err
		}
	}
	return json.Marshal(snapshot)
}

type ComputerUseControlAction string

const (
	ComputerUseControlPause    ComputerUseControlAction = "pause"
	ComputerUseControlResume   ComputerUseControlAction = "resume"
	ComputerUseControlTakeOver ComputerUseControlAction = "take_over"
	ComputerUseControlStop     ComputerUseControlAction = "stop"
)

type ComputerUseControlRequest struct {
	LeaseID        string                   `json:"lease_id"`
	Action         ComputerUseControlAction `json:"action"`
	IdempotencyKey string                   `json:"idempotency_key"`
}

func (request ComputerUseControlRequest) Validate() error {
	if request.LeaseID == "" || request.IdempotencyKey == "" {
		return fmt.Errorf("computer-use control lease_id and idempotency_key are required")
	}
	switch request.Action {
	case ComputerUseControlPause, ComputerUseControlResume, ComputerUseControlTakeOver, ComputerUseControlStop:
		return nil
	default:
		return fmt.Errorf("invalid computer-use control action %q", request.Action)
	}
}

func DecodeComputerUseControlRequest(payload []byte) (ComputerUseControlRequest, error) {
	var request ComputerUseControlRequest
	if err := decodeStrictJSON(payload, &request, "computer-use control request"); err != nil {
		return ComputerUseControlRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return ComputerUseControlRequest{}, err
	}
	return request, nil
}

type ComputerUseControlResponse struct {
	Accepted   bool                  `json:"accepted"`
	Quiesced   bool                  `json:"quiesced"`
	LeaseID    string                `json:"lease_id"`
	Revision   uint64                `json:"revision"`
	LeaseState ComputerUseLeaseState `json:"lease_state"`
}

func EncodeComputerUseControlResponse(response ComputerUseControlResponse) ([]byte, error) {
	if response.LeaseID == "" {
		return nil, fmt.Errorf("computer-use control response lease_id is required")
	}
	if !ValidComputerUseLeaseState(response.LeaseState) {
		return nil, fmt.Errorf("invalid computer-use control response lease_state %q", response.LeaseState)
	}
	return json.Marshal(response)
}

func NewCoordinatorInstanceID() string {
	return newID("cui")
}

func newID(prefix string) string {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err == nil {
		return prefix + "_" + hex.EncodeToString(entropy[:])
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func cloneActivityState(state ComputerUseActivityState) ComputerUseActivityState {
	clone := state
	if state.ActionResult != nil {
		value := *state.ActionResult
		clone.ActionResult = &value
	}
	if state.ExecutionPath != nil {
		value := *state.ExecutionPath
		clone.ExecutionPath = &value
	}
	if state.Pointer != nil {
		value := *state.Pointer
		clone.Pointer = &value
	}
	if state.FailureCode != nil {
		value := *state.FailureCode
		clone.FailureCode = &value
	}
	if state.ConsequentialRisk != nil {
		value := *state.ConsequentialRisk
		clone.ConsequentialRisk = &value
	}
	return clone
}
