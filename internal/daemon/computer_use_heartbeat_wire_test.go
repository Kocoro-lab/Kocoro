package daemon

import (
	"encoding/json"
	"testing"
)

func TestWireFixture_ComputerUseHeartbeatCodec(t *testing.T) {
	requestFixture := loadWireFixture(t, "computer_use.heartbeat.request.json")
	requestPayload, err := json.Marshal(requestFixture)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeComputerUseHeartbeatRequest(requestPayload)
	if err != nil {
		t.Fatalf("DecodeComputerUseHeartbeatRequest: %v", err)
	}
	if request.SchemaVersion != ComputerUseHeartbeatSchemaVersion || request.LeaseID != "cul_demo" {
		t.Fatalf("decoded heartbeat request = %+v", request)
	}
	reencodedRequest, err := EncodeComputerUseHeartbeatRequest(request)
	if err != nil {
		t.Fatalf("EncodeComputerUseHeartbeatRequest: %v", err)
	}
	assertSemanticEqual(t, requestFixture, parseJSONMap(t, reencodedRequest))

	responseFixture := loadWireFixture(t, "computer_use.heartbeat.response.json")
	response := ComputerUseHeartbeatResponse{
		SchemaVersion:         ComputerUseHeartbeatSchemaVersion,
		CoordinatorInstanceID: computerUseCoordinatorFixtureID,
		Revision:              41,
		LeaseID:               "cul_demo",
		LeaseState:            ComputerUseLeaseActive,
		HeartbeatAt:           computerUseFixtureTime,
		ExpiresAt:             "2026-07-22T12:00:30Z",
	}
	responsePayload, err := EncodeComputerUseHeartbeatResponse(response)
	if err != nil {
		t.Fatalf("EncodeComputerUseHeartbeatResponse: %v", err)
	}
	assertSemanticEqual(t, responseFixture, parseJSONMap(t, responsePayload))
	assertNoSensitiveComputerUseKeys(t, responsePayload)
	decodedResponse, err := DecodeComputerUseHeartbeatResponse(responsePayload)
	if err != nil {
		t.Fatalf("DecodeComputerUseHeartbeatResponse: %v", err)
	}
	if decodedResponse != response {
		t.Fatalf("decoded heartbeat response = %+v; want %+v", decodedResponse, response)
	}
}
