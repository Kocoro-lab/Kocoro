package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const computerUseFixtureTime = "2026-07-22T12:00:00Z"
const computerUseCoordinatorFixtureID = "cui_demo"

func stringPointer[T ~string](value T) *T { return &value }

func fixtureComputerUseState() ComputerUseActivityState {
	return ComputerUseActivityState{
		LeaseID:        "cul_demo",
		SessionID:      "session_demo",
		ActionID:       "cua_demo",
		ToolUseID:      "toolu_demo",
		SourceKind:     "desktop",
		SourceLabel:    "Kocoro Desktop",
		TargetBundleID: "com.apple.Notes",
		TargetAppName:  "Notes",
		ActionKind:     "press",
		LeaseState:     ComputerUseLeaseActive,
		ActionPhase:    ComputerUsePhaseVerifying,
		ActionResult:   nil,
		ExecutionPath:  stringPointer(ComputerUseExecutionAccessibility),
		Pointer: &ComputerUsePointer{
			DisplayID:          1,
			TopologyID:         "topo_mixed_001",
			TopologyGeneration: 7,
			X:                  640.5,
			Y:                  412.25,
			CoordinateSpace:    ComputerUseCoordinateQuartzGlobalPoints,
		},
		FailureCode: nil,
		TS:          computerUseFixtureTime,
	}
}

func fixtureComputerUseRiskState() ComputerUseActivityState {
	state := fixtureComputerUseState()
	state.ActionID = ""
	state.ActionPhase = ComputerUsePhaseWaitingForUser
	state.Pointer = nil
	state.TargetBundleID = "com.tinyspeck.slackmacgap"
	state.TargetAppName = "Slack"
	state.ConsequentialRisk = &ConsequentialRiskMarkerV1{
		SchemaVersion: 1, Required: true, Kind: "send",
		IntentID: "cri_AAECAwQFBgcICQoLDA0ODw", ExpiresAt: "2026-07-22T12:00:20Z",
	}
	return state
}

func fixtureComputerUseScrollState() ComputerUseActivityState {
	state := fixtureComputerUseState()
	state.ActionID = "cua_scroll_demo"
	state.ToolUseID = "toolu_scroll_demo"
	state.ActionKind = "scroll"
	state.ActionPhase = ComputerUsePhaseVerifying
	state.ActionResult = stringPointer(ComputerUseResultVerified)
	state.Pointer = nil
	return state
}

func assertNoSensitiveComputerUseKeys(t *testing.T, payload []byte) {
	t.Helper()
	for _, forbidden := range []string{
		`"text"`, `"typed_text"`, `"raw_args"`, `"args"`, `"prompt"`,
		`"clipboard"`, `"screenshot"`, `"image_base64"`, `"ax_value"`, `"value"`,
	} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("computer-use wire payload contains forbidden field %s: %s", forbidden, payload)
		}
	}
}

func TestWireFixture_ComputerUseActivityEvent(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.computer_use.activity.json")
	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	emitter := newComputerUseActivityEmitter(bus, 40, computerUseCoordinatorFixtureID, func() time.Time {
		return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	})
	emitted, err := emitter.Emit(fixtureComputerUseState())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	evt := waitBusEvent(t, sub, EventComputerUseActivity)
	if evt.ID == emitted.Revision {
		t.Fatalf("coordinator revision %d must not be sourced from SSE event id %d", emitted.Revision, evt.ID)
	}
	produced := parseJSONMap(t, evt.Payload)
	assertSemanticEqual(t, fixture, produced)
	assertNoSensitiveComputerUseKeys(t, evt.Payload)

	// Consumer-shaped decode catches producer renames independently of the
	// fixture equality assertion. This mirrors the fields Desktop consumes.
	var consumer struct {
		SchemaVersion         int                 `json:"schema_version"`
		CoordinatorInstanceID string              `json:"coordinator_instance_id"`
		Revision              uint64              `json:"revision"`
		LeaseID               string              `json:"lease_id"`
		SessionID             string              `json:"session_id"`
		ActionID              string              `json:"action_id"`
		ToolUseID             string              `json:"tool_use_id"`
		SourceKind            string              `json:"source_kind"`
		SourceLabel           string              `json:"source_label"`
		TargetBundleID        string              `json:"target_bundle_id"`
		TargetAppName         string              `json:"target_app_name"`
		ActionKind            string              `json:"action_kind"`
		LeaseState            string              `json:"lease_state"`
		ActionPhase           string              `json:"action_phase"`
		ActionResult          *string             `json:"action_result"`
		ExecutionPath         *string             `json:"execution_path"`
		Pointer               *ComputerUsePointer `json:"pointer"`
		FailureCode           *string             `json:"failure_code"`
		TS                    string              `json:"ts"`
	}
	if err := json.Unmarshal(evt.Payload, &consumer); err != nil {
		t.Fatalf("consumer decode: %v", err)
	}
	if consumer.SchemaVersion != 1 || consumer.CoordinatorInstanceID != computerUseCoordinatorFixtureID ||
		consumer.Revision != 41 || consumer.LeaseID == "" ||
		consumer.ExecutionPath == nil || *consumer.ExecutionPath != "accessibility" || consumer.Pointer == nil {
		t.Fatalf("consumer decode lost required fields: %+v", consumer)
	}
}

func TestWireFixture_ComputerUseScrollActivityEvent(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.computer_use.activity.scroll.json")
	emitter := newComputerUseActivityEmitter(nil, 43, computerUseCoordinatorFixtureID, func() time.Time {
		return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	})
	emitted, err := emitter.Emit(fixtureComputerUseScrollState())
	if err != nil {
		t.Fatalf("Emit scroll activity: %v", err)
	}
	payload, err := json.Marshal(emitted)
	if err != nil {
		t.Fatalf("marshal scroll activity: %v", err)
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, payload))
	assertNoSensitiveComputerUseKeys(t, payload)
	if emitted.Revision != 44 || emitted.ActionKind != "scroll" ||
		emitted.ActionResult == nil || *emitted.ActionResult != ComputerUseResultVerified ||
		emitted.ExecutionPath == nil || *emitted.ExecutionPath != ComputerUseExecutionAccessibility ||
		emitted.Pointer != nil {
		t.Fatalf("scroll activity lost semantic/non-pointer authority: %+v", emitted)
	}
}

func TestComputerUseActivityEmitterRevisionIsMonotonic(t *testing.T) {
	emitter := newComputerUseActivityEmitter(nil, 7, computerUseCoordinatorFixtureID, func() time.Time { return time.Unix(0, 0).UTC() })
	first, err := emitter.Emit(fixtureComputerUseState())
	if err != nil {
		t.Fatal(err)
	}
	second, err := emitter.Emit(fixtureComputerUseState())
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 8 || second.Revision != 9 {
		t.Fatalf("revisions = %d, %d; want 8, 9", first.Revision, second.Revision)
	}
	if first.CoordinatorInstanceID == "" || first.CoordinatorInstanceID != second.CoordinatorInstanceID {
		t.Fatalf("coordinator instance IDs = %q, %q; want same nonempty value", first.CoordinatorInstanceID, second.CoordinatorInstanceID)
	}
	if generated := NewComputerUseActivityEmitter(nil).coordinatorInstanceID; generated == "" {
		t.Fatal("public emitter constructor generated empty coordinator instance ID")
	}
}

func TestWireFixture_ComputerUseActivitySnapshots(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		revision   uint64
		active     *ComputerUseActivityState
		wantActive bool
	}{
		{name: "active", fixture: "computer_use.activity_snapshot.active.json", revision: 41, active: func() *ComputerUseActivityState { v := fixtureComputerUseState(); return &v }(), wantActive: true},
		{name: "idle", fixture: "computer_use.activity_snapshot.idle.json", revision: 42, active: nil, wantActive: false},
		{name: "waiting confirmation", fixture: "computer_use.activity_snapshot.waiting_confirmation.json", revision: 43, active: func() *ComputerUseActivityState { v := fixtureComputerUseRiskState(); return &v }(), wantActive: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := loadWireFixture(t, tc.fixture)
			payload, err := EncodeComputerUseActivitySnapshot(ComputerUseActivitySnapshot{
				SchemaVersion:         ComputerUseActivitySchemaVersion,
				CoordinatorInstanceID: computerUseCoordinatorFixtureID,
				Revision:              tc.revision,
				Active:                tc.active,
			})
			if err != nil {
				t.Fatalf("EncodeComputerUseActivitySnapshot: %v", err)
			}
			assertSemanticEqual(t, fixture, parseJSONMap(t, payload))
			assertNoSensitiveComputerUseKeys(t, payload)

			var decoded ComputerUseActivitySnapshot
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatalf("decode snapshot: %v", err)
			}
			if decoded.SchemaVersion != 1 || decoded.CoordinatorInstanceID != computerUseCoordinatorFixtureID ||
				decoded.Revision != tc.revision || (decoded.Active != nil) != tc.wantActive {
				t.Fatalf("decoded snapshot = %+v", decoded)
			}
		})
	}
}

func TestWireFixture_ComputerUseStopCodec(t *testing.T) {
	requestFixture := loadWireFixture(t, "computer_use.control.stop.request.json")
	responseFixture := loadWireFixture(t, "computer_use.control.stop.response.json")

	requestBytes, err := json.Marshal(requestFixture)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeComputerUseControlRequest(requestBytes)
	if err != nil {
		t.Fatalf("DecodeComputerUseControlRequest: %v", err)
	}
	if request.LeaseID != "cul_demo" || request.Action != ComputerUseControlStop || request.IdempotencyKey != "stop_demo" {
		t.Fatalf("decoded request = %+v", request)
	}
	reencodedRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	assertSemanticEqual(t, requestFixture, parseJSONMap(t, reencodedRequest))

	responseBytes, err := EncodeComputerUseControlResponse(ComputerUseControlResponse{
		Accepted:   true,
		LeaseID:    "cul_demo",
		Revision:   42,
		LeaseState: ComputerUseLeaseStopping,
	})
	if err != nil {
		t.Fatalf("EncodeComputerUseControlResponse: %v", err)
	}
	assertSemanticEqual(t, responseFixture, parseJSONMap(t, responseBytes))
	assertNoSensitiveComputerUseKeys(t, responseBytes)
}

func TestComputerUseControlRequestDecoderFailsClosed(t *testing.T) {
	for _, payload := range []string{
		`{"lease_id":"cul_demo","action":"stop","idempotency_key":"stop_demo","text":"secret"}`,
		`{"lease_id":"cul_demo","action":"stop","idempotency_key":"stop_demo"}{"action":"stop"}`,
		`{"lease_id":"cul_demo","action":"shutdown","idempotency_key":"stop_demo"}`,
		`{"lease_id":"","action":"stop","idempotency_key":"stop_demo"}`,
	} {
		if request, err := DecodeComputerUseControlRequest([]byte(payload)); err == nil {
			t.Fatalf("decoder accepted unsafe/invalid payload %+v from %s", request, payload)
		}
	}
}

func TestComputerUseWireEnumsRejectUnknownValues(t *testing.T) {
	valid := fixtureComputerUseState()
	tests := []struct {
		name   string
		mutate func(*ComputerUseActivityState)
	}{
		{"lease state", func(v *ComputerUseActivityState) { v.LeaseState = "queued" }},
		{"action phase", func(v *ComputerUseActivityState) { v.ActionPhase = "clicking" }},
		{"action result", func(v *ComputerUseActivityState) { v.ActionResult = stringPointer(ComputerUseActionResult("ok")) }},
		{"execution path", func(v *ComputerUseActivityState) { v.ExecutionPath = stringPointer(ComputerUseExecutionPath("visual")) }},
		{"coordinate space", func(v *ComputerUseActivityState) { v.Pointer.CoordinateSpace = "pixels" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			pointerCopy := *valid.Pointer
			candidate.Pointer = &pointerCopy
			tc.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("Validate accepted unknown enum value")
			}
		})
	}
}

func TestComputerUsePointerRequiresTopologyAuthority(t *testing.T) {
	valid := fixtureComputerUseState()
	for _, mutate := range []func(*ComputerUsePointer){
		func(pointer *ComputerUsePointer) { pointer.TopologyID = "" },
		func(pointer *ComputerUsePointer) { pointer.TopologyGeneration = 0 },
	} {
		candidate := valid
		pointer := *valid.Pointer
		candidate.Pointer = &pointer
		mutate(candidate.Pointer)
		if err := candidate.Validate(); err == nil {
			t.Fatal("pointer without topology authority passed validation")
		}
	}

	withoutPointer := valid
	withoutPointer.Pointer = nil
	if err := withoutPointer.Validate(); err != nil {
		t.Fatalf("activity without pointer should remain valid: %v", err)
	}
}

func TestComputerUseActivityJSONShapeHasNoUnexpectedFields(t *testing.T) {
	event := ComputerUseActivityEvent{
		SchemaVersion:            ComputerUseActivitySchemaVersion,
		CoordinatorInstanceID:    computerUseCoordinatorFixtureID,
		Revision:                 41,
		ComputerUseActivityState: fixtureComputerUseState(),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "coordinator_instance_id", "revision", "lease_id", "session_id", "action_id", "tool_use_id",
		"source_kind", "source_label", "target_bundle_id", "target_app_name", "action_kind",
		"lease_state", "action_phase", "action_result", "execution_path", "pointer", "failure_code", "consequential_risk", "ts",
	}
	got := make([]string, 0, len(fields))
	for key := range fields {
		got = append(got, key)
	}
	if len(got) != len(want) {
		t.Fatalf("wire fields = %v; want exactly %v", got, want)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("wire payload missing %q: %s", key, payload)
		}
	}

	// Avoid a false sense of safety from only checking a few forbidden names:
	// the committed fixture must remain exactly equal to the production shape.
	fixture := loadWireFixture(t, "bus_event.computer_use.activity.json")
	if !reflect.DeepEqual(fixture, fields) {
		t.Fatalf("production shape differs from canonical fixture")
	}
}
