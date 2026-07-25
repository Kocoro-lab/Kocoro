package tools

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// What actually protects the action path is a STRUCTURAL restriction, not
// arithmetic: an actionable frame must carry exactly one transform region that
// covers final_image completely, with a matching display_id, no shear
// (b≈c≈0) and positive scale (a>0, d>0). Multi-display stitching, inter-display
// gaps, overlapping regions, rotation and mirroring therefore cannot reach a
// click at all — the desktop-overview frame that does span displays is
// non-actionable. These tests pin that, plus the real secondary-display case,
// which is a SINGLE-region frame whose Quartz origin is simply non-zero.
//
// The affine is a pure scale+translate derived from the region's own Quartz
// rect. Both the screenshot and CGEvent use a top-left origin with y growing
// downward (CGDisplayBounds, not NSScreen.frame — the topology carries the two
// spaces as separate fields and only the Quartz one reaches this path), so
// there is no Y-flip to apply and none is applied.

func decodeCoordinateFrameFromMap(t *testing.T, obj map[string]any) (CoordinateFrameV1, error) {
	t.Helper()
	encoded, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return DecodeCoordinateFrameV1(encoded)
}

func TestCoordinateFrameV1ActionableRejectsMultiDisplayStitching(t *testing.T) {
	base := loadCoordinateFixture(t, "coordinate_frame.display.v1.json")
	var obj map[string]any
	if err := json.Unmarshal(base, &obj); err != nil {
		t.Fatal(err)
	}
	region := obj["transform_regions"].([]any)[0].(map[string]any)
	second := map[string]any{
		"display_id":  2.0,
		"pixel_rect":  map[string]any{"x": 0.0, "y": 0.0, "width": 1280.0, "height": 800.0},
		"quartz_rect": map[string]any{"x": 1280.0, "y": 0.0, "width": 1280.0, "height": 800.0},
		"affine":      map[string]any{"a": 1.0, "b": 0.0, "c": 0.0, "d": 1.0, "tx": 1280.0, "ty": 0.0},
	}
	obj["transform_regions"] = []any{region, second}

	if _, err := decodeCoordinateFrameFromMap(t, obj); err == nil {
		t.Fatal("an actionable frame spanning two displays was accepted; " +
			"single-region coverage is what makes gap/overlap/rotation unreachable")
	}
}

// The real secondary-display case: capturing display 2 yields a single-region
// frame whose Quartz origin is non-zero. Every existing mapping assertion uses a
// display frame at origin (0,0), so a dropped translation would not show there.
func TestCoordinateMapV1SecondaryDisplayNonZeroOriginAndDownscale(t *testing.T) {
	base := loadCoordinateFixture(t, "coordinate_frame.display.v1.json")
	var obj map[string]any
	if err := json.Unmarshal(base, &obj); err != nil {
		t.Fatal(err)
	}
	// Display 2 sits right of and below the main display, captured at 2x
	// downscale (2560x1600 logical points rendered into a 1280x800 image).
	const originX, originY = 1280.0, 300.0
	const scale = 2.0
	obj["display_id"] = 2.0
	obj["captured_quartz_rect"] = map[string]any{
		"x": originX, "y": originY, "width": 2560.0, "height": 1600.0,
	}
	obj["transform_regions"] = []any{map[string]any{
		"display_id":  2.0,
		"pixel_rect":  map[string]any{"x": 0.0, "y": 0.0, "width": 1280.0, "height": 800.0},
		"quartz_rect": map[string]any{"x": originX, "y": originY, "width": 2560.0, "height": 1600.0},
		"affine": map[string]any{
			"a": scale, "b": 0.0, "c": 0.0, "d": scale, "tx": originX, "ty": originY,
		},
	}}

	frame, err := decodeCoordinateFrameFromMap(t, obj)
	if err != nil {
		t.Fatalf("secondary-display frame rejected: %v", err)
	}
	now := time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC)

	seen := map[[2]float64]bool{}
	for px := 0; px < 1280; px += 29 {
		for py := 0; py < 800; py += 31 {
			point, err := MapCoordinatePixelCenterV1(
				frame, frame.TopologyRef, frame.StateID, frame.FrameID,
				now, float64(px), float64(py))
			if err != nil {
				t.Fatalf("pixel (%d,%d) failed to map: %v", px, py, err)
			}
			if point.DisplayID != 2 {
				t.Fatalf("pixel (%d,%d) mapped to display %d, want 2", px, py, point.DisplayID)
			}
			// Exact expected value: pixel CENTER, scaled, then translated.
			wantX := (float64(px)+0.5)*scale + originX
			wantY := (float64(py)+0.5)*scale + originY
			if math.Abs(point.X-wantX) > 1e-9 || math.Abs(point.Y-wantY) > 1e-9 {
				t.Fatalf("pixel (%d,%d) -> (%.4f,%.4f), want (%.4f,%.4f)",
					px, py, point.X, point.Y, wantX, wantY)
			}
			// Inside the display's own Quartz rect, and never on the main display.
			if point.X < originX || point.X >= originX+2560 ||
				point.Y < originY || point.Y >= originY+1600 {
				t.Fatalf("pixel (%d,%d) escaped display 2: (%.2f,%.2f)", px, py, point.X, point.Y)
			}
			key := [2]float64{point.X, point.Y}
			if seen[key] {
				t.Fatalf("pixel (%d,%d) collided on %v", px, py, key)
			}
			seen[key] = true
		}
	}
}
