package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func canonicalCaptureWindowRequest(t *testing.T) CaptureCoordinateWindowRequestV1 {
	t.Helper()
	envelope, err := DecodeCaptureCoordinateWindowRPCRequestV1(
		loadCoordinateFixture(t, "capture_coordinate_window.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Params
}

func canonicalCaptureWindowTopology(t *testing.T) DisplayTopologyV1 {
	t.Helper()
	topology, err := DecodeDisplayTopologyV1(
		loadCoordinateFixture(t, "display_topology.mixed_horizontal.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return topology
}

func canonicalCaptureWindowLimits() CaptureCoordinateWindowLimitsV1 {
	return CaptureCoordinateWindowLimitsV1{
		MaxRawBytes:    1024,
		MaxNDJSONBytes: 4096,
		MaxPixels:      1024,
	}
}

func TestCaptureCoordinateWindowV1CanonicalRequestAndResults(t *testing.T) {
	requestFixture := loadCoordinateFixture(t, "capture_coordinate_window.request.v1.json")
	envelope, err := DecodeCaptureCoordinateWindowRPCRequestV1(requestFixture)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.ID != 801 || envelope.Method != "capture_coordinate_window" ||
		envelope.Params.WindowID != 7001 || envelope.Params.TopologyRef.Generation != 7 {
		t.Fatalf("request lost identity: %+v", envelope)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, requestFixture, encoded)

	for _, name := range []string{
		"capture_coordinate_window.response.success.v1.json",
		"capture_coordinate_window.response.failure.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := AdmitCaptureCoordinateWindowV1(
			payload,
			envelope.Params,
			canonicalCaptureWindowTopology(t),
			canonicalCaptureWindowLimits())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		produced, err := EncodeCaptureCoordinateWindowResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, produced)
	}
}

func TestCaptureCoordinateWindowV1StrictRequestRequiresNestedFields(t *testing.T) {
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.request.v1.json"))
	for _, field := range []string{"x", "y", "width", "height"} {
		t.Run("missing expected bounds "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			params := candidate["params"].(map[string]any)
			delete(params["expected_quartz_bounds"].(map[string]any), field)
			if _, err := DecodeCaptureCoordinateWindowRPCRequestV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("request accepted missing expected bounds field")
			}
		})
		t.Run("null expected bounds "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			params := candidate["params"].(map[string]any)
			params["expected_quartz_bounds"].(map[string]any)[field] = nil
			if _, err := DecodeCaptureCoordinateWindowRPCRequestV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("request accepted null expected bounds field")
			}
		})
	}
}

func TestCaptureCoordinateWindowV1StrictWireRejectsMalformedUnion(t *testing.T) {
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.response.success.v1.json"))
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown", mutate: func(v map[string]any) { v["unexpected"] = true }},
		{name: "missing nullable key", mutate: func(v map[string]any) { delete(v, "failure_code") }},
		{name: "success failure code", mutate: func(v map[string]any) { v["failure_code"] = "capture_failed" }},
		{name: "success retry safe", mutate: func(v map[string]any) { v["retry_safe"] = true }},
		{name: "null retry safe", mutate: func(v map[string]any) { v["retry_safe"] = nil }},
		{name: "missing success value", mutate: func(v map[string]any) { v["display_id"] = nil }},
		{name: "missing window bounds x", mutate: func(v map[string]any) {
			delete(v["window_quartz_bounds"].(map[string]any), "x")
		}},
		{name: "null window bounds x", mutate: func(v map[string]any) {
			v["window_quartz_bounds"].(map[string]any)["x"] = nil
		}},
		{name: "bad status", mutate: func(v map[string]any) { v["status"] = "ok" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			test.mutate(candidate)
			if _, err := DecodeCaptureCoordinateWindowResultV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("malformed tagged union passed strict decoder")
			}
		})
	}

	failure := captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.response.failure.v1.json"))
	failure["pid"] = 4242
	if _, err := DecodeCaptureCoordinateWindowResultV1(marshalCaptureWindowJSON(t, failure)); err == nil {
		t.Fatal("failure union accepted success payload")
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown failure code", mutate: func(v map[string]any) { v["failure_code"] = "try_again" }},
		{name: "retryable code marked unsafe", mutate: func(v map[string]any) { v["retry_safe"] = false }},
		{name: "nonretryable code marked safe", mutate: func(v map[string]any) {
			v["failure_code"] = "invalid_request"
			v["retry_safe"] = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t,
				captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.response.failure.v1.json")))
			test.mutate(candidate)
			if _, err := DecodeCaptureCoordinateWindowResultV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("failure result violated fixed failure_code/retry_safe policy")
			}
		})
	}
	trailing := append(loadCoordinateFixture(t, "capture_coordinate_window.response.success.v1.json"), []byte(`{}`)...)
	if _, err := DecodeCaptureCoordinateWindowResultV1(trailing); err == nil {
		t.Fatal("decoder accepted trailing JSON")
	}
}

func TestCaptureCoordinateWindowV1FailurePolicyMatrix(t *testing.T) {
	policy := map[string]bool{
		"topology_unavailable":      true,
		"stale_topology":            true,
		"window_not_found":          true,
		"window_not_actionable":     true,
		"window_bounds_mismatch":    true,
		"display_not_actionable":    true,
		"capture_timeout":           true,
		"capture_failed":            true,
		"topology_changed":          true,
		"window_changed":            true,
		"invalid_request":           false,
		"process_identity_mismatch": false,
		"window_identity_mismatch":  false,
		"image_too_large":           false,
		"invalid_png":               false,
		"image_dimensions_mismatch": true,
		"response_too_large":        false,
	}
	fixture := captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.response.failure.v1.json"))
	for code, retrySafe := range policy {
		t.Run(code, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, fixture)
			candidate["failure_code"] = code
			candidate["retry_safe"] = retrySafe
			if code == "image_dimensions_mismatch" {
				candidate["failure_diagnostics"] = canonicalCaptureWindowFailureDiagnosticsJSON()
			}
			if _, err := DecodeCaptureCoordinateWindowResultV1(marshalCaptureWindowJSON(t, candidate)); err != nil {
				t.Fatalf("known policy rejected: %v", err)
			}
			candidate["retry_safe"] = !retrySafe
			if _, err := DecodeCaptureCoordinateWindowResultV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("inverse retry_safe policy passed")
			}
		})
	}
}

func canonicalCaptureWindowFailureDiagnosticsJSON() map[string]any {
	return map[string]any{
		"stage":     "decoded_dimensions",
		"pid":       float64(4242),
		"bundle_id": "com.example.fixture",
		"window_id": float64(7001),
		"pre_window_quartz_bounds": map[string]any{
			"x": -100.0, "y": 200.0, "width": 1.0, "height": 1.0,
		},
		"post_window_quartz_bounds": map[string]any{
			"x": -100.0, "y": 200.0, "width": 1.0, "height": 1.0,
		},
		"display_id":           float64(2),
		"backing_scale_factor": 1.0,
		"expected_width_px":    1.0,
		"expected_height_px":   1.0,
		"metadata_width_px":    float64(2),
		"metadata_height_px":   float64(1),
		"decoded_width_px":     float64(2),
		"decoded_height_px":    float64(1),
	}
}

func TestCaptureCoordinateWindowV1AdmissionRejectsContentAndAuthorityMismatch(t *testing.T) {
	request := canonicalCaptureWindowRequest(t)
	topology := canonicalCaptureWindowTopology(t)
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.response.success.v1.json"))
	tests := []struct {
		name   string
		mutate func(map[string]any, *DisplayTopologyV1, *CaptureCoordinateWindowLimitsV1)
	}{
		{name: "invalid base64", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["image_base64"] = "%%%"
		}},
		{name: "byte length", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["byte_length"] = 67
		}},
		{name: "digest", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["sha256"] = strings.Repeat("0", 64)
		}},
		{name: "metadata dimensions", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) { v["width_px"] = 2 }},
		{name: "pid", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) { v["pid"] = 4243 }},
		{name: "bundle", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["bundle_id"] = "com.example.other"
		}},
		{name: "request window bounds", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["window_quartz_bounds"].(map[string]any)["x"] = -96.0
		}},
		{name: "media type", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["media_type"] = "image/jpeg"
		}},
		{name: "capture timestamp malformed", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["captured_at"] = "not-a-time"
		}},
		{name: "capture timestamp equal topology", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["captured_at"] = "2026-07-22T12:00:00Z"
		}},
		{name: "capture timestamp older than topology", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["captured_at"] = "2026-07-22T11:59:59Z"
		}},
		{name: "topology generation", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Generation = 8
		}},
		{name: "topology id", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.TopologyID = "topo_other"
		}},
		{name: "helper boot", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.HelperBootID = "helper_restarted"
		}},
		{name: "window", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["window_id"] = 7002
		}},
		{name: "display", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) { v["display_id"] = 1 }},
		{name: "scale", mutate: func(v map[string]any, _ *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			v["backing_scale_factor"] = 2
		}},
		{name: "display no longer contains window", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Displays[1].QuartzBounds.X = -99.5
			topology.Displays[1].AppKitFrame.X = -99.5
		}},
		{name: "display inactive", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Displays[1].IsActive = false
		}},
		{name: "display offline", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Displays[1].IsOnline = false
		}},
		{name: "display asleep", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Displays[1].IsAsleep = true
		}},
		{name: "display rotated", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Displays[1].RotationDegrees = 90
		}},
		{name: "display mirror follower", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			master := uint32(1)
			topology.Displays[1].MirrorMasterDisplayID = &master
		}},
		{name: "ambiguous actionable display overlap", mutate: func(_ map[string]any, topology *DisplayTopologyV1, _ *CaptureCoordinateWindowLimitsV1) {
			topology.Displays[0].QuartzBounds = DisplayTopologyRectV1{X: -200, Y: 0, Width: 1480, Height: 800}
			topology.Displays[0].AppKitFrame = DisplayTopologyRectV1{X: -200, Y: 0, Width: 1480, Height: 800}
		}},
		{name: "raw cap", mutate: func(_ map[string]any, _ *DisplayTopologyV1, limits *CaptureCoordinateWindowLimitsV1) {
			limits.MaxRawBytes = 67
		}},
		{name: "ndjson cap", mutate: func(_ map[string]any, _ *DisplayTopologyV1, limits *CaptureCoordinateWindowLimitsV1) {
			limits.MaxNDJSONBytes = 100
		}},
		{name: "pixel cap", mutate: func(_ map[string]any, _ *DisplayTopologyV1, limits *CaptureCoordinateWindowLimitsV1) {
			limits.MaxPixels = 0
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			candidateTopology := topology
			candidateTopology.Displays = append([]DisplayTopologyDisplayV1(nil), topology.Displays...)
			limits := canonicalCaptureWindowLimits()
			test.mutate(candidate, &candidateTopology, &limits)
			if _, err := AdmitCaptureCoordinateWindowV1(
				marshalCaptureWindowJSON(t, candidate), request, candidateTopology, limits); err == nil {
				t.Fatal("mismatched capture passed admission")
			}
		})
	}
}

func TestCaptureCoordinateWindowV1AdmissionIgnoresOverlappingMirrorFollower(t *testing.T) {
	topology := canonicalCaptureWindowTopology(t)
	topology.Displays = append([]DisplayTopologyDisplayV1(nil), topology.Displays...)
	topology.Displays[0].QuartzBounds = DisplayTopologyRectV1{X: -200, Y: 0, Width: 1480, Height: 800}
	topology.Displays[0].AppKitFrame = DisplayTopologyRectV1{X: -200, Y: 0, Width: 1480, Height: 800}
	master := uint32(2)
	topology.Displays[0].MirrorMasterDisplayID = &master

	if _, err := AdmitCaptureCoordinateWindowV1(
		loadCoordinateFixture(t, "capture_coordinate_window.response.success.v1.json"),
		canonicalCaptureWindowRequest(t), topology, canonicalCaptureWindowLimits()); err != nil {
		t.Fatalf("non-actionable mirror follower created false display ambiguity: %v", err)
	}
}

func TestCaptureCoordinateWindowV1AdmissionFullyDecodesPNG(t *testing.T) {
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "capture_coordinate_window.response.success.v1.json"))
	raw, err := base64.StdEncoding.DecodeString(base["image_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	raw[50] ^= 0xff
	base["image_base64"] = base64.StdEncoding.EncodeToString(raw)
	base["byte_length"] = len(raw)
	base["sha256"] = captureWindowSHA256(raw)
	if _, err := AdmitCaptureCoordinateWindowV1(
		marshalCaptureWindowJSON(t, base),
		canonicalCaptureWindowRequest(t),
		canonicalCaptureWindowTopology(t),
		canonicalCaptureWindowLimits()); err == nil {
		t.Fatal("full decoder accepted CRC-corrupt PNG with matching metadata digest")
	}
}

func TestCaptureCoordinateWindowV1TypedCall(t *testing.T) {
	caller := &displayTopologyAXCaller{
		result: loadCoordinateFixture(t, "capture_coordinate_window.response.success.v1.json"),
	}
	request := canonicalCaptureWindowRequest(t)
	result, err := ReadCaptureCoordinateWindowV1(
		context.Background(), caller, request,
		canonicalCaptureWindowTopology(t), canonicalCaptureWindowLimits())
	if err != nil {
		t.Fatal(err)
	}
	if caller.method != "capture_coordinate_window" || result.Status != "captured" {
		t.Fatalf("typed capture call = method %q result %+v", caller.method, result)
	}
	encodedParams, err := json.Marshal(caller.params)
	if err != nil {
		t.Fatal(err)
	}
	var requestFixture struct {
		Params any `json:"params"`
	}
	if err := json.Unmarshal(loadCoordinateFixture(t, "capture_coordinate_window.request.v1.json"), &requestFixture); err != nil {
		t.Fatal(err)
	}
	wantParams, err := json.Marshal(requestFixture.Params)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, wantParams, encodedParams)

	caller.result = loadCoordinateFixture(t, "capture_coordinate_window.response.failure.v1.json")
	failure, err := ReadCaptureCoordinateWindowV1(
		context.Background(), caller, request,
		canonicalCaptureWindowTopology(t), canonicalCaptureWindowLimits())
	if err != nil {
		t.Fatalf("typed helper failure must remain a typed result: %v", err)
	}
	if failure.Status != "failed" || failure.FailureCode == nil || *failure.FailureCode != "window_bounds_mismatch" {
		t.Fatalf("typed failure result = %+v", failure)
	}
}

func captureWindowJSONMap(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func cloneCaptureWindowJSONMap(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	return captureWindowJSONMap(t, marshalCaptureWindowJSON(t, source))
}

func marshalCaptureWindowJSON(t *testing.T, object any) []byte {
	t.Helper()
	payload, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
