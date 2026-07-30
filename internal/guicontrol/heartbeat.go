package guicontrol

import (
	"encoding/json"
	"fmt"
	"time"
)

const ComputerUseHeartbeatSchemaVersion = 1

type ComputerUseHeartbeatRequest struct {
	SchemaVersion int    `json:"schema_version"`
	LeaseID       string `json:"lease_id"`
}

func (request ComputerUseHeartbeatRequest) Validate() error {
	if request.SchemaVersion != ComputerUseHeartbeatSchemaVersion {
		return fmt.Errorf("unsupported computer-use heartbeat schema_version %d", request.SchemaVersion)
	}
	if request.LeaseID == "" {
		return fmt.Errorf("computer-use heartbeat lease_id is required")
	}
	return nil
}

func EncodeComputerUseHeartbeatRequest(request ComputerUseHeartbeatRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func DecodeComputerUseHeartbeatRequest(payload []byte) (ComputerUseHeartbeatRequest, error) {
	var request ComputerUseHeartbeatRequest
	if err := decodeStrictJSON(payload, &request, "computer-use heartbeat request"); err != nil {
		return ComputerUseHeartbeatRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return ComputerUseHeartbeatRequest{}, err
	}
	return request, nil
}

type ComputerUseHeartbeatResponse struct {
	SchemaVersion         int                   `json:"schema_version"`
	CoordinatorInstanceID string                `json:"coordinator_instance_id"`
	Revision              uint64                `json:"revision"`
	LeaseID               string                `json:"lease_id"`
	LeaseState            ComputerUseLeaseState `json:"lease_state"`
	HeartbeatAt           string                `json:"heartbeat_at"`
	ExpiresAt             string                `json:"expires_at"`
}

func (response ComputerUseHeartbeatResponse) Validate() error {
	if response.SchemaVersion != ComputerUseHeartbeatSchemaVersion {
		return fmt.Errorf("unsupported computer-use heartbeat schema_version %d", response.SchemaVersion)
	}
	if response.CoordinatorInstanceID == "" || response.LeaseID == "" {
		return fmt.Errorf("computer-use heartbeat coordinator_instance_id and lease_id are required")
	}
	if response.Revision == 0 {
		return fmt.Errorf("computer-use heartbeat revision is required")
	}
	if !ValidComputerUseLeaseState(response.LeaseState) {
		return fmt.Errorf("invalid computer-use heartbeat lease_state %q", response.LeaseState)
	}
	heartbeatAt, err := time.Parse(time.RFC3339Nano, response.HeartbeatAt)
	if err != nil {
		return fmt.Errorf("computer-use heartbeat heartbeat_at must be RFC3339: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, response.ExpiresAt)
	if err != nil {
		return fmt.Errorf("computer-use heartbeat expires_at must be RFC3339: %w", err)
	}
	if !expiresAt.After(heartbeatAt) {
		return fmt.Errorf("computer-use heartbeat expires_at must be after heartbeat_at")
	}
	return nil
}

func EncodeComputerUseHeartbeatResponse(response ComputerUseHeartbeatResponse) ([]byte, error) {
	if err := response.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

func DecodeComputerUseHeartbeatResponse(payload []byte) (ComputerUseHeartbeatResponse, error) {
	var response ComputerUseHeartbeatResponse
	if err := decodeStrictJSON(payload, &response, "computer-use heartbeat response"); err != nil {
		return ComputerUseHeartbeatResponse{}, err
	}
	if err := response.Validate(); err != nil {
		return ComputerUseHeartbeatResponse{}, err
	}
	return response, nil
}
