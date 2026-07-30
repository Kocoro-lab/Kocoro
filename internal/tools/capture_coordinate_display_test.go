package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func canonicalCaptureDisplayRequest(t *testing.T) CaptureCoordinateDisplayRequestV1 {
	t.Helper()
	envelope, err := DecodeCaptureCoordinateDisplayRPCRequestV1(
		loadCoordinateFixture(t, "capture_coordinate_display.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Params
}

func canonicalCaptureDisplayTopology() DisplayTopologyV1 {
	mainBounds := DisplayTopologyRectV1{X: 0, Y: 0, Width: 1, Height: 1}
	displayBounds := DisplayTopologyRectV1{X: -100, Y: 200, Width: 1, Height: 1}
	return DisplayTopologyV1{
		SchemaVersion: 1, TopologyID: "topo_display_001",
		HelperBootID: "helper_display_001", Generation: 9,
		CapturedAt: "2026-07-23T01:02:02Z", MainDisplayID: 1,
		Displays: []DisplayTopologyDisplayV1{
			{
				DisplayID: 1, IsMain: true, IsBuiltin: true,
				IsActive: true, IsOnline: true,
				QuartzBounds: mainBounds, AppKitFrame: mainBounds, AppKitVisibleFrame: mainBounds,
				BackingScaleFactor: 1, PixelWidth: 1, PixelHeight: 1,
			},
			{
				DisplayID: 2, IsActive: true, IsOnline: true,
				QuartzBounds: displayBounds, AppKitFrame: displayBounds, AppKitVisibleFrame: displayBounds,
				BackingScaleFactor: 1, PixelWidth: 1, PixelHeight: 1,
			},
		},
	}
}

func canonicalCaptureDisplayLimits() CaptureCoordinateDisplayLimitsV1 {
	return CaptureCoordinateDisplayLimitsV1{
		MaxRawBytes: 1024, MaxNDJSONBytes: 4096, MaxPixels: 1024,
	}
}

func TestCaptureCoordinateDisplayV1CanonicalFixturesAndTypedCall(t *testing.T) {
	requestPayload := loadCoordinateFixture(t, "capture_coordinate_display.request.v1.json")
	envelope, err := DecodeCaptureCoordinateDisplayRPCRequestV1(requestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.ID != 802 || envelope.Method != "capture_coordinate_display" ||
		envelope.Params.DisplayID != 2 || envelope.Params.TopologyRef.Generation != 9 {
		t.Fatalf("request lost display authority: %+v", envelope)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, requestPayload, encoded)

	for _, name := range []string{
		"capture_coordinate_display.response.success.v1.json",
		"capture_coordinate_display.response.failure.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := AdmitCaptureCoordinateDisplayV1(
			payload, envelope.Params, canonicalCaptureDisplayTopology(), canonicalCaptureDisplayLimits())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		produced, err := EncodeCaptureCoordinateDisplayResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, produced)
	}

	caller := &displayTopologyAXCaller{
		result: loadCoordinateFixture(t, "capture_coordinate_display.response.success.v1.json"),
	}
	result, err := ReadCaptureCoordinateDisplayV1(
		context.Background(), caller, envelope.Params,
		canonicalCaptureDisplayTopology(), canonicalCaptureDisplayLimits())
	if err != nil {
		t.Fatal(err)
	}
	if caller.method != "capture_coordinate_display" || result.Status != "captured" {
		t.Fatalf("typed call = method %q result %+v", caller.method, result)
	}
}

func TestCaptureCoordinateDisplayV1StrictWireAndFailurePolicy(t *testing.T) {
	request := string(loadCoordinateFixture(t, "capture_coordinate_display.request.v1.json"))
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(request, `"display_id": 2`, `"display_id": 2, "extra": 1`, 1)),
		[]byte(strings.Replace(request, `"display_id": 2`, `"display_id": 2, "display_id": 2`, 1)),
		append(loadCoordinateFixture(t, "capture_coordinate_display.request.v1.json"), []byte(`{}`)...),
	} {
		if _, err := DecodeCaptureCoordinateDisplayRPCRequestV1(invalid); err == nil {
			t.Fatal("strict request accepted malformed JSON")
		}
	}

	base := captureWindowJSONMap(t,
		loadCoordinateFixture(t, "capture_coordinate_display.response.failure.v1.json"))
	policy := map[string]bool{
		"topology_unavailable": true, "stale_topology": true,
		"display_not_found": true, "display_not_actionable": true,
		"capture_timeout": true, "capture_failed": true, "topology_changed": true,
		"invalid_request": false, "image_too_large": false, "invalid_png": false,
		"image_dimensions_mismatch": false, "response_too_large": false,
	}
	for code, retrySafe := range policy {
		candidate := cloneCaptureWindowJSONMap(t, base)
		candidate["failure_code"] = code
		candidate["retry_safe"] = retrySafe
		if _, err := DecodeCaptureCoordinateDisplayResultV1(
			marshalCaptureWindowJSON(t, candidate)); err != nil {
			t.Fatalf("known policy %s rejected: %v", code, err)
		}
		candidate["retry_safe"] = !retrySafe
		if _, err := DecodeCaptureCoordinateDisplayResultV1(
			marshalCaptureWindowJSON(t, candidate)); err == nil {
			t.Fatalf("inverse retry policy accepted for %s", code)
		}
	}

	success := captureWindowJSONMap(t,
		loadCoordinateFixture(t, "capture_coordinate_display.response.success.v1.json"))
	success["target_pid"] = 42
	if _, err := DecodeCaptureCoordinateDisplayResultV1(
		marshalCaptureWindowJSON(t, success)); err == nil {
		t.Fatal("strict result accepted an off-contract target field")
	}
	resultText := string(loadCoordinateFixture(t,
		"capture_coordinate_display.response.success.v1.json"))
	duplicate := strings.Replace(
		resultText, `"display_id": 2`, `"display_id": 2, "display_id": 2`, 1)
	if _, err := DecodeCaptureCoordinateDisplayResultV1([]byte(duplicate)); err == nil {
		t.Fatal("strict result accepted duplicate display_id")
	}
	trailing := append(loadCoordinateFixture(t,
		"capture_coordinate_display.response.success.v1.json"), []byte(`{}`)...)
	if _, err := DecodeCaptureCoordinateDisplayResultV1(trailing); err == nil {
		t.Fatal("strict result accepted trailing JSON")
	}
}

func TestCaptureCoordinateDisplayV1AdmissionFullyDecodesPNG(t *testing.T) {
	base := captureWindowJSONMap(t,
		loadCoordinateFixture(t, "capture_coordinate_display.response.success.v1.json"))
	raw, err := base64.StdEncoding.DecodeString(base["image_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	raw[50] ^= 0xff
	base["image_base64"] = base64.StdEncoding.EncodeToString(raw)
	base["byte_length"] = len(raw)
	base["sha256"] = captureWindowSHA256(raw)
	if _, err := AdmitCaptureCoordinateDisplayV1(
		marshalCaptureWindowJSON(t, base), canonicalCaptureDisplayRequest(t),
		canonicalCaptureDisplayTopology(), canonicalCaptureDisplayLimits()); err == nil {
		t.Fatal("full decoder accepted CRC-corrupt display PNG with matching metadata digest")
	}
}

func TestCaptureCoordinateDisplayV1AdmissionRejectsAuthorityAndContentDrift(t *testing.T) {
	request := canonicalCaptureDisplayRequest(t)
	topology := canonicalCaptureDisplayTopology()
	base := captureWindowJSONMap(t,
		loadCoordinateFixture(t, "capture_coordinate_display.response.success.v1.json"))
	tests := []struct {
		name   string
		mutate func(map[string]any, *DisplayTopologyV1, *CaptureCoordinateDisplayLimitsV1)
	}{
		{"display id", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) { v["display_id"] = 1 }},
		{"bounds", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v["display_quartz_bounds"].(map[string]any)["x"] = -99.0
		}},
		{"scale", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v["backing_scale_factor"] = 2
		}},
		{"dimensions", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) { v["width_px"] = 2 }},
		{"digest", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v["sha256"] = strings.Repeat("0", 64)
		}},
		{"bytes", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v["byte_length"] = 67
		}},
		{"timestamp", func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v["captured_at"] = "2026-07-23T01:02:02Z"
		}},
		{"helper", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v.HelperBootID = "restarted"
		}},
		{"topology", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) { v.Generation = 10 }},
		{"inactive", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v.Displays[1].IsActive = false
		}},
		{"offline", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v.Displays[1].IsOnline = false
		}},
		{"asleep", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v.Displays[1].IsAsleep = true
		}},
		{"rotated", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			v.Displays[1].RotationDegrees = 90
		}},
		{"mirrored", func(_ map[string]any, v *DisplayTopologyV1, _ *CaptureCoordinateDisplayLimitsV1) {
			master := uint32(1)
			v.Displays[1].MirrorMasterDisplayID = &master
		}},
		{"raw cap", func(_ map[string]any, _ *DisplayTopologyV1, v *CaptureCoordinateDisplayLimitsV1) { v.MaxRawBytes = 67 }},
		{"wire cap", func(_ map[string]any, _ *DisplayTopologyV1, v *CaptureCoordinateDisplayLimitsV1) {
			v.MaxNDJSONBytes = 100
		}},
		{"pixel cap", func(_ map[string]any, _ *DisplayTopologyV1, v *CaptureCoordinateDisplayLimitsV1) { v.MaxPixels = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			candidateTopology := topology
			candidateTopology.Displays = append([]DisplayTopologyDisplayV1(nil), topology.Displays...)
			limits := canonicalCaptureDisplayLimits()
			test.mutate(candidate, &candidateTopology, &limits)
			if _, err := AdmitCaptureCoordinateDisplayV1(
				marshalCaptureWindowJSON(t, candidate), request, candidateTopology, limits); err == nil {
				t.Fatal("drifted capture passed strict admission")
			}
		})
	}
}
