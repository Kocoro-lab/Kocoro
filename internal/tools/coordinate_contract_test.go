package tools

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func loadCoordinateFixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read coordinate fixture %s: %v", name, err)
	}
	return payload
}

func assertCoordinateJSONRoundTrip(t *testing.T, fixture []byte, produced []byte) {
	t.Helper()
	var want, got any
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(produced, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip differs\nwant: %s\n got: %s", fixture, produced)
	}
}

func TestDisplayTopologyV1CanonicalFixturesDecode(t *testing.T) {
	for _, name := range []string{
		"display_topology.mixed_horizontal.v1.json",
		"display_topology.vertical.v1.json",
		"display_topology.mirrored_rotated.v1.json",
	} {
		t.Run(name, func(t *testing.T) {
			topology, err := DecodeDisplayTopologyV1(loadCoordinateFixture(t, name))
			if err != nil {
				t.Fatal(err)
			}
			if topology.SchemaVersion != 1 || topology.TopologyID == "" || topology.Generation == 0 {
				t.Fatalf("lost topology authority: %+v", topology)
			}
			if name == "display_topology.mixed_horizontal.v1.json" {
				if topology.Displays[1].QuartzBounds.X != -1600 || topology.Displays[1].QuartzBounds.Y != 100 ||
					topology.Displays[1].AppKitFrame.Y != -200 {
					t.Fatalf("mixed coordinate spaces drifted: %+v", topology.Displays[1])
				}
			}
		})
	}
}

func TestDisplayTopologyV1RejectsInvalidTopology(t *testing.T) {
	base, err := DecodeDisplayTopologyV1(loadCoordinateFixture(t, "display_topology.mixed_horizontal.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*DisplayTopologyV1)
	}{
		{name: "duplicate display", mutate: func(v *DisplayTopologyV1) { v.Displays[1].DisplayID = v.Displays[0].DisplayID }},
		{name: "zero main display", mutate: func(v *DisplayTopologyV1) { v.MainDisplayID = 0 }},
		{name: "zero display", mutate: func(v *DisplayTopologyV1) { v.Displays[1].DisplayID = 0 }},
		{name: "zero matching main and display", mutate: func(v *DisplayTopologyV1) {
			v.MainDisplayID = 0
			v.Displays[0].DisplayID = 0
		}},
		{name: "negative size", mutate: func(v *DisplayTopologyV1) { v.Displays[0].QuartzBounds.Width = -1 }},
		{name: "zero size", mutate: func(v *DisplayTopologyV1) { v.Displays[0].AppKitFrame.Width = 0 }},
		{name: "zero pixels", mutate: func(v *DisplayTopologyV1) { v.Displays[0].PixelWidth = 0 }},
		{name: "non finite", mutate: func(v *DisplayTopologyV1) { v.Displays[0].BackingScaleFactor = math.Inf(1) }},
		{name: "logical size mismatch", mutate: func(v *DisplayTopologyV1) { v.Displays[0].AppKitFrame.Width = 1279 }},
		{name: "main not unique", mutate: func(v *DisplayTopologyV1) { v.Displays[1].IsMain = true }},
		{name: "mirror self", mutate: func(v *DisplayTopologyV1) { id := v.Displays[0].DisplayID; v.Displays[0].MirrorMasterDisplayID = &id }},
		{name: "zero mirror master", mutate: func(v *DisplayTopologyV1) { id := uint32(0); v.Displays[1].MirrorMasterDisplayID = &id }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Displays = append([]DisplayTopologyDisplayV1(nil), base.Displays...)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid topology passed validation")
			}
		})
	}
}

func TestDisplayTopologyV1StrictWireRequiresEveryNestedField(t *testing.T) {
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "display_topology.mixed_horizontal.v1.json"))
	topologyFields := []string{
		"schema_version", "topology_id", "helper_boot_id", "generation", "captured_at", "main_display_id", "displays",
	}
	for _, field := range topologyFields {
		t.Run("topology missing "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			delete(candidate, field)
			if _, err := DecodeDisplayTopologyV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("missing topology field passed strict decoder")
			}
		})
		t.Run("topology null "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			candidate[field] = nil
			if _, err := DecodeDisplayTopologyV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("null topology field passed strict decoder")
			}
		})
	}

	displayFields := []string{
		"display_id", "is_main", "is_builtin", "is_active", "is_online", "is_asleep",
		"quartz_bounds", "appkit_frame", "appkit_visible_frame", "backing_scale_factor",
		"pixel_width", "pixel_height", "rotation_degrees", "mirror_master_display_id",
	}
	for _, field := range displayFields {
		t.Run("display missing "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			display := candidate["displays"].([]any)[0].(map[string]any)
			delete(display, field)
			if _, err := DecodeDisplayTopologyV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("missing display field passed strict decoder")
			}
		})
		if field != "mirror_master_display_id" {
			t.Run("display null "+field, func(t *testing.T) {
				candidate := cloneCaptureWindowJSONMap(t, base)
				display := candidate["displays"].([]any)[0].(map[string]any)
				display[field] = nil
				if _, err := DecodeDisplayTopologyV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
					t.Fatal("null display field passed strict decoder")
				}
			})
		}
	}

	for _, boundsField := range []string{"quartz_bounds", "appkit_frame", "appkit_visible_frame"} {
		for _, rectField := range []string{"x", "y", "width", "height"} {
			t.Run(boundsField+" missing "+rectField, func(t *testing.T) {
				candidate := cloneCaptureWindowJSONMap(t, base)
				display := candidate["displays"].([]any)[0].(map[string]any)
				delete(display[boundsField].(map[string]any), rectField)
				if _, err := DecodeDisplayTopologyV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
					t.Fatal("missing rectangle field passed strict decoder")
				}
			})
			t.Run(boundsField+" null "+rectField, func(t *testing.T) {
				candidate := cloneCaptureWindowJSONMap(t, base)
				display := candidate["displays"].([]any)[0].(map[string]any)
				display[boundsField].(map[string]any)[rectField] = nil
				if _, err := DecodeDisplayTopologyV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
					t.Fatal("null rectangle field passed strict decoder")
				}
			})
		}
	}
}

func TestCoordinateImageProfileV1CanonicalRoundTrip(t *testing.T) {
	fixture := loadCoordinateFixture(t, "coordinate_image_profile.default.v1.json")
	profile, err := DecodeCoordinateImageProfileV1(fixture)
	if err != nil {
		t.Fatal(err)
	}
	produced, err := EncodeCoordinateImageProfileV1(profile)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, fixture, produced)
}

func TestCoordinateImageProfileV1RawByteCapFitsInlineBase64(t *testing.T) {
	profile, err := DecodeCoordinateImageProfileV1(
		loadCoordinateFixture(t, "coordinate_image_profile.default.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if profile.MaxEncodedBytes != CoordinateMaxRawImageBytesV1 ||
		CoordinateMaxRawImageBytesV1 != TargetRawImageBytes {
		t.Fatalf(
			"coordinate raw cap = %d, profile = %d, generic target = %d",
			CoordinateMaxRawImageBytesV1, profile.MaxEncodedBytes, TargetRawImageBytes)
	}
	if expanded := base64.StdEncoding.EncodedLen(profile.MaxEncodedBytes); expanded > client.MaxInlineImageBase64Bytes {
		t.Fatalf(
			"coordinate profile raw cap expands to %d base64 bytes, over inline cap %d",
			expanded, client.MaxInlineImageBase64Bytes)
	}

	tooLarge := profile
	tooLarge.MaxEncodedBytes = CoordinateMaxRawImageBytesV1 + 1
	if err := tooLarge.Validate(); err == nil {
		t.Fatal("profile accepted a max_encoded_bytes value above the v1 raw-byte safety cap")
	}

	for _, name := range []string{
		"coordinate_frame.window.v1.json",
		"coordinate_frame.display.v1.json",
		"coordinate_frame.mixed_desktop.v1.json",
		"coordinate_frame.gap.v1.json",
		"coordinate_frame.overlap.v1.json",
	} {
		var frame CoordinateFrameV1
		if err := json.Unmarshal(loadCoordinateFixture(t, name), &frame); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if frame.FinalImage.ByteLength > profile.MaxEncodedBytes {
			t.Fatalf("%s byte_length %d exceeds profile cap %d", name, frame.FinalImage.ByteLength, profile.MaxEncodedBytes)
		}
	}
}

func TestCoordinateImageProfileV1StrictWireRequiresEveryField(t *testing.T) {
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "coordinate_image_profile.default.v1.json"))
	for field := range base {
		t.Run("missing "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			delete(candidate, field)
			if _, err := DecodeCoordinateImageProfileV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("profile accepted missing field")
			}
		})
		t.Run("null "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			candidate[field] = nil
			if _, err := DecodeCoordinateImageProfileV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("profile accepted null field")
			}
		})
	}
}

func TestCoordinateFrameV1StrictWireRequiresEveryField(t *testing.T) {
	base := captureWindowJSONMap(t, loadCoordinateFixture(t, "coordinate_frame.window.v1.json"))
	for field := range base {
		t.Run("top missing "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, base)
			delete(candidate, field)
			if _, err := DecodeCoordinateFrameV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("frame accepted missing top-level field")
			}
		})
		if field != "display_id" && field != "target_pid" && field != "target_bundle_id" && field != "target_window_id" {
			t.Run("top null "+field, func(t *testing.T) {
				candidate := cloneCaptureWindowJSONMap(t, base)
				candidate[field] = nil
				if _, err := DecodeCoordinateFrameV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
					t.Fatal("frame accepted null required top-level field")
				}
			})
		}
	}

	desktop := captureWindowJSONMap(t, loadCoordinateFixture(t, "coordinate_frame.mixed_desktop.v1.json"))
	for _, field := range []string{"display_id", "target_pid", "target_bundle_id", "target_window_id"} {
		t.Run("nullable key still required "+field, func(t *testing.T) {
			candidate := cloneCaptureWindowJSONMap(t, desktop)
			delete(candidate, field)
			if _, err := DecodeCoordinateFrameV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
				t.Fatal("frame accepted omitted explicit nullable field")
			}
		})
	}

	nestedObjects := []struct {
		name   string
		object func(map[string]any) map[string]any
	}{
		{name: "topology_ref", object: func(v map[string]any) map[string]any { return v["topology_ref"].(map[string]any) }},
		{name: "captured_quartz_rect", object: func(v map[string]any) map[string]any { return v["captured_quartz_rect"].(map[string]any) }},
		{name: "raw_image", object: func(v map[string]any) map[string]any { return v["raw_image"].(map[string]any) }},
		{name: "final_image", object: func(v map[string]any) map[string]any { return v["final_image"].(map[string]any) }},
		{name: "provider_profile", object: func(v map[string]any) map[string]any { return v["provider_profile"].(map[string]any) }},
		{name: "transform_region", object: func(v map[string]any) map[string]any {
			return v["transform_regions"].([]any)[0].(map[string]any)
		}},
		{name: "pixel_rect", object: func(v map[string]any) map[string]any {
			return v["transform_regions"].([]any)[0].(map[string]any)["pixel_rect"].(map[string]any)
		}},
		{name: "quartz_rect", object: func(v map[string]any) map[string]any {
			return v["transform_regions"].([]any)[0].(map[string]any)["quartz_rect"].(map[string]any)
		}},
		{name: "affine", object: func(v map[string]any) map[string]any {
			return v["transform_regions"].([]any)[0].(map[string]any)["affine"].(map[string]any)
		}},
	}
	for _, nested := range nestedObjects {
		for field := range nested.object(base) {
			t.Run(nested.name+" missing "+field, func(t *testing.T) {
				candidate := cloneCaptureWindowJSONMap(t, base)
				delete(nested.object(candidate), field)
				if _, err := DecodeCoordinateFrameV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
					t.Fatal("frame accepted missing nested field")
				}
			})
			t.Run(nested.name+" null "+field, func(t *testing.T) {
				candidate := cloneCaptureWindowJSONMap(t, base)
				nested.object(candidate)[field] = nil
				if _, err := DecodeCoordinateFrameV1(marshalCaptureWindowJSON(t, candidate)); err == nil {
					t.Fatal("frame accepted null nested field")
				}
			})
		}
	}
}

func TestCoordinateFrameV1ValidatesAgainstImageProfile(t *testing.T) {
	profile, err := DecodeCoordinateImageProfileV1(loadCoordinateFixture(t, "coordinate_image_profile.default.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := DecodeCoordinateFrameV1(loadCoordinateFixture(t, "coordinate_frame.mixed_desktop.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.ValidateAgainst(profile); err != nil {
		t.Fatalf("canonical frame/profile pair rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CoordinateFrameV1, *CoordinateImageProfileV1)
	}{
		{name: "profile identity", mutate: func(f *CoordinateFrameV1, _ *CoordinateImageProfileV1) { f.ProviderProfile.Version++ }},
		{name: "media type", mutate: func(f *CoordinateFrameV1, _ *CoordinateImageProfileV1) { f.FinalImage.MediaType = "image/webp" }},
		{name: "long edge", mutate: func(f *CoordinateFrameV1, _ *CoordinateImageProfileV1) { f.FinalImage.WidthPX = 1569 }},
		{name: "total pixels", mutate: func(f *CoordinateFrameV1, p *CoordinateImageProfileV1) { p.MaxTotalPixels = 100 }},
		{name: "encoded bytes", mutate: func(f *CoordinateFrameV1, p *CoordinateImageProfileV1) {
			p.MaxEncodedBytes = f.FinalImage.ByteLength - 1
		}},
		{name: "undeclared padding", mutate: func(_ *CoordinateFrameV1, p *CoordinateImageProfileV1) { p.PaddingMode = "letterbox" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateFrame := frame
			candidateProfile := profile
			test.mutate(&candidateFrame, &candidateProfile)
			if err := candidateFrame.ValidateAgainst(candidateProfile); err == nil {
				t.Fatal("invalid frame/profile pair passed validation")
			}
		})
	}
}

func TestCoordinateFrameV1ProductionAdmissionRequiresProfileValidation(t *testing.T) {
	payload := loadCoordinateFixture(t, "coordinate_frame.display.v1.json")
	profile, err := DecodeCoordinateImageProfileV1(loadCoordinateFixture(t, "coordinate_image_profile.default.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AdmitCoordinateFrameV1(payload, profile); err != nil {
		t.Fatalf("canonical frame admission failed: %v", err)
	}
	profile.MaxEncodedBytes = 1
	if _, err := AdmitCoordinateFrameV1(payload, profile); err == nil {
		t.Fatal("production admission bypassed provider profile limits")
	}
}

func TestCoordinateFrameV1CanonicalRoundTripAndPixelCenterMapping(t *testing.T) {
	tests := []struct {
		fixture string
		x, y    float64
		display uint32
		wantX   float64
		wantY   float64
	}{
		{fixture: "coordinate_frame.window.v1.json", x: 0, y: 0, display: 1, wantX: 100 + 1.0/3.0, wantY: 100 + 1.0/3.0},
		{fixture: "coordinate_frame.display.v1.json", x: 0, y: 0, display: 1, wantX: 0.5, wantY: 0.5},
		{fixture: "coordinate_frame.display.v1.json", x: 1279, y: 799, display: 1, wantX: 1279.5, wantY: 799.5},
	}
	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			fixture := loadCoordinateFixture(t, test.fixture)
			frame, err := DecodeCoordinateFrameV1(fixture)
			if err != nil {
				t.Fatal(err)
			}
			produced, err := EncodeCoordinateFrameV1(frame)
			if err != nil {
				t.Fatal(err)
			}
			assertCoordinateJSONRoundTrip(t, fixture, produced)
			now := time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC)
			point, err := MapCoordinatePixelCenterV1(
				frame, frame.TopologyRef, frame.StateID, frame.FrameID, now, test.x, test.y)
			if err != nil {
				t.Fatal(err)
			}
			if point.DisplayID != test.display || math.Abs(point.X-test.wantX) > 1e-9 || math.Abs(point.Y-test.wantY) > 1e-9 {
				t.Fatalf("mapped point = %+v", point)
			}
		})
	}
}

func TestCoordinateFrameV1DesktopOverviewIsNonActionable(t *testing.T) {
	fixture := loadCoordinateFixture(t, "coordinate_frame.mixed_desktop.v1.json")
	frame, err := DecodeCoordinateFrameV1(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Scope != "desktop" || frame.Actionable {
		t.Fatalf("mixed desktop fixture must remain a non-actionable overview: %+v", frame)
	}
	produced, err := EncodeCoordinateFrameV1(frame)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, fixture, produced)

	frame.Actionable = true
	if err := frame.Validate(); err == nil {
		t.Fatal("desktop overview became actionable")
	}
}

func TestCoordinateMapV1CanonicalErrorVectors(t *testing.T) {
	var fixture struct {
		SchemaVersion int `json:"schema_version"`
		Vectors       []struct {
			Name               string                  `json:"name"`
			FrameFixture       string                  `json:"frame_fixture"`
			CurrentTopologyRef CoordinateTopologyRefV1 `json:"current_topology_ref"`
			CurrentStateID     string                  `json:"current_state_id"`
			FrameID            string                  `json:"frame_id"`
			Now                string                  `json:"now"`
			X                  float64                 `json:"x"`
			Y                  float64                 `json:"y"`
			ExpectedError      string                  `json:"expected_error"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(loadCoordinateFixture(t, "coordinate_map.error_vectors.v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			payload := loadCoordinateFixture(t, vector.FrameFixture)
			var frame CoordinateFrameV1
			var err error
			if vector.ExpectedError == "invalid_frame" {
				if err := json.Unmarshal(payload, &frame); err != nil {
					t.Fatal(err)
				}
			} else {
				frame, err = DecodeCoordinateFrameV1(payload)
				if err != nil {
					t.Fatal(err)
				}
			}
			now, parseErr := time.Parse(time.RFC3339Nano, vector.Now)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			_, err = MapCoordinatePixelCenterV1(
				frame, vector.CurrentTopologyRef, vector.CurrentStateID, vector.FrameID, now, vector.X, vector.Y)
			if CoordinateMapErrorCodeV1(err) != vector.ExpectedError {
				t.Fatalf("map error = %v (%s), want %s", err, CoordinateMapErrorCodeV1(err), vector.ExpectedError)
			}
		})
	}
}

func TestCoordinateMapV1RejectsNonActionableAndEscapingAffine(t *testing.T) {
	frame, err := DecodeCoordinateFrameV1(loadCoordinateFixture(t, "coordinate_frame.window.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	frame.Actionable = false
	now := time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC)
	if _, err := MapCoordinatePixelCenterV1(frame, frame.TopologyRef, frame.StateID, frame.FrameID, now, 0, 0); CoordinateMapErrorCodeV1(err) != "frame_not_actionable" {
		t.Fatalf("non-actionable error = %v", err)
	}
	frame.Actionable = true
	frame.TransformRegions[0].Affine.TX = -10000
	if _, err := MapCoordinatePixelCenterV1(frame, frame.TopologyRef, frame.StateID, frame.FrameID, now, 0, 0); CoordinateMapErrorCodeV1(err) != "invalid_frame" {
		t.Fatalf("escaping affine error = %v", err)
	}
}

func TestCoordinateFrameV1RejectsPlausibleButWrongActionableAffine(t *testing.T) {
	frame, err := DecodeCoordinateFrameV1(loadCoordinateFixture(t, "coordinate_frame.window.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	wrongScale := frame
	wrongScale.TransformRegions = append([]CoordinateTransformRegionV1(nil), frame.TransformRegions...)
	wrongScale.TransformRegions[0].Affine.A = 0.1
	if err := wrongScale.Validate(); err == nil {
		t.Fatal("plausible but wrong actionable scale passed validation")
	}

	wrongTranslation := frame
	wrongTranslation.TransformRegions = append([]CoordinateTransformRegionV1(nil), frame.TransformRegions...)
	wrongTranslation.TransformRegions[0].Affine.TX += 0.1
	if err := wrongTranslation.Validate(); err == nil {
		t.Fatal("plausible but wrong actionable translation passed validation")
	}

	zeroDisplay := frame
	zeroDisplayID := uint32(0)
	zeroDisplay.DisplayID = &zeroDisplayID
	zeroDisplay.TransformRegions = append([]CoordinateTransformRegionV1(nil), frame.TransformRegions...)
	zeroDisplay.TransformRegions[0].DisplayID = 0
	if err := zeroDisplay.Validate(); err == nil {
		t.Fatal("coordinate frame accepted kCGNullDirectDisplay")
	}
}

func TestCoordinateMapV1RequiresIntegerPixelIndicesBeforeCenterOffset(t *testing.T) {
	frame, err := DecodeCoordinateFrameV1(loadCoordinateFixture(t, "coordinate_frame.display.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC)
	tests := []struct {
		name     string
		x        float64
		wantCode string
	}{
		{name: "last pixel", x: 1279, wantCode: ""},
		{name: "width outside", x: 1280, wantCode: "outside_final_image"},
		{name: "fractional near edge", x: 1279.75, wantCode: "invalid_coordinate"},
		{name: "negative", x: -1, wantCode: "outside_final_image"},
		{name: "nan", x: math.NaN(), wantCode: "invalid_coordinate"},
		{name: "infinity", x: math.Inf(1), wantCode: "invalid_coordinate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			point, err := MapCoordinatePixelCenterV1(
				frame, frame.TopologyRef, frame.StateID, frame.FrameID, now, test.x, 0)
			if got := CoordinateMapErrorCodeV1(err); got != test.wantCode {
				t.Fatalf("error code = %q (%v), want %q", got, err, test.wantCode)
			}
			if test.wantCode == "" && point.X != 1279.5 {
				t.Fatalf("last pixel mapped to %+v", point)
			}
		})
	}
}
