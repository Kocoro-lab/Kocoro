package guicontrol

import (
	"bytes"
	"testing"
)

func TestHeartbeatRequestWireIsStrict(t *testing.T) {
	want := ComputerUseHeartbeatRequest{
		SchemaVersion: ComputerUseHeartbeatSchemaVersion,
		LeaseID:       "cul_demo",
	}
	payload, err := EncodeComputerUseHeartbeatRequest(want)
	if err != nil {
		t.Fatalf("EncodeComputerUseHeartbeatRequest: %v", err)
	}
	if !bytes.Equal(payload, []byte(`{"schema_version":1,"lease_id":"cul_demo"}`)) {
		t.Fatalf("encoded heartbeat request = %s", payload)
	}
	got, err := DecodeComputerUseHeartbeatRequest(payload)
	if err != nil {
		t.Fatalf("DecodeComputerUseHeartbeatRequest: %v", err)
	}
	if got != want {
		t.Fatalf("decoded heartbeat request = %+v; want %+v", got, want)
	}

	for name, invalid := range map[string]string{
		"unknown":   `{"schema_version":1,"lease_id":"cul_demo","extra":true}`,
		"trailing":  `{"schema_version":1,"lease_id":"cul_demo"}{}`,
		"duplicate": `{"schema_version":1,"lease_id":"cul_demo","lease_id":"cul_other"}`,
		"schema":    `{"schema_version":2,"lease_id":"cul_demo"}`,
		"empty":     `{"schema_version":1,"lease_id":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			if request, err := DecodeComputerUseHeartbeatRequest([]byte(invalid)); err == nil {
				t.Fatalf("DecodeComputerUseHeartbeatRequest(%s) = %+v, nil", invalid, request)
			}
		})
	}
}

func TestHeartbeatResponseWire(t *testing.T) {
	want := ComputerUseHeartbeatResponse{
		SchemaVersion:         ComputerUseHeartbeatSchemaVersion,
		CoordinatorInstanceID: "cui_demo",
		Revision:              41,
		LeaseID:               "cul_demo",
		LeaseState:            ComputerUseLeaseActive,
		HeartbeatAt:           "2026-07-22T12:00:00Z",
		ExpiresAt:             "2026-07-22T12:00:30Z",
	}
	payload, err := EncodeComputerUseHeartbeatResponse(want)
	if err != nil {
		t.Fatalf("EncodeComputerUseHeartbeatResponse: %v", err)
	}
	got, err := DecodeComputerUseHeartbeatResponse(payload)
	if err != nil {
		t.Fatalf("DecodeComputerUseHeartbeatResponse: %v", err)
	}
	if got != want {
		t.Fatalf("decoded heartbeat response = %+v; want %+v", got, want)
	}

	for name, invalid := range map[string]string{
		"unknown":   `{"schema_version":1,"coordinator_instance_id":"cui_demo","revision":41,"lease_id":"cul_demo","lease_state":"active","heartbeat_at":"2026-07-22T12:00:00Z","expires_at":"2026-07-22T12:00:30Z","extra":true}`,
		"trailing":  `{"schema_version":1,"coordinator_instance_id":"cui_demo","revision":41,"lease_id":"cul_demo","lease_state":"active","heartbeat_at":"2026-07-22T12:00:00Z","expires_at":"2026-07-22T12:00:30Z"}[]`,
		"duplicate": `{"schema_version":1,"coordinator_instance_id":"cui_demo","revision":41,"revision":42,"lease_id":"cul_demo","lease_state":"active","heartbeat_at":"2026-07-22T12:00:00Z","expires_at":"2026-07-22T12:00:30Z"}`,
		"ordering":  `{"schema_version":1,"coordinator_instance_id":"cui_demo","revision":41,"lease_id":"cul_demo","lease_state":"active","heartbeat_at":"2026-07-22T12:00:30Z","expires_at":"2026-07-22T12:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if response, err := DecodeComputerUseHeartbeatResponse([]byte(invalid)); err == nil {
				t.Fatalf("DecodeComputerUseHeartbeatResponse(%s) = %+v, nil", invalid, response)
			}
		})
	}
}

func TestControlRequestRejectsDuplicateKeys(t *testing.T) {
	payload := []byte(`{"lease_id":"cul_demo","action":"pause","action":"stop","idempotency_key":"key"}`)
	if request, err := DecodeComputerUseControlRequest(payload); err == nil {
		t.Fatalf("DecodeComputerUseControlRequest = %+v, nil", request)
	}
}
