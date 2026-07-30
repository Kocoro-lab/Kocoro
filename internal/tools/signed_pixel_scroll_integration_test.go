//go:build darwin && kocoro_signed_integration

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSignedPixelScrollThroughImmutableFrame(t *testing.T) {
	if os.Getenv("KOCORO_RUN_SIGNED_PIXEL_SCROLL") != "1" {
		t.Skip("set KOCORO_RUN_SIGNED_PIXEL_SCROLL=1 for real signed input")
	}
	receiptPath := requireSignedPixelScrollAbsolutePath(
		t, "KOCORO_SIGNED_PIXEL_SCROLL_RECEIPT_PATH")
	nonce := os.Getenv("KOCORO_SIGNED_PIXEL_SCROLL_NONCE")
	if nonce == "" {
		t.Fatal("KOCORO_SIGNED_PIXEL_SCROLL_NONCE is required")
	}
	probePID, err := strconv.Atoi(
		os.Getenv("KOCORO_SIGNED_PIXEL_SCROLL_PROBE_PID"))
	if err != nil || probePID <= 0 {
		t.Fatal("KOCORO_SIGNED_PIXEL_SCROLL_PROBE_PID must be positive")
	}
	if data, readErr := os.ReadFile(receiptPath); readErr != nil ||
		len(data) != 0 {
		t.Fatalf("receipt must exist and be empty before input: bytes=%d err=%v",
			len(data), readErr)
	}

	client := &AXClient{}
	t.Cleanup(client.Close)
	tool := &ComputerUseTool{
		client:                        client,
		coordinateExecutor:            client.CoordinateMouseEventV1,
		coordinateDragExecutor:        client.CoordinateDragV1,
		coordinatePixelScrollExecutor: client.CoordinatePixelScrollV1,
		semanticTextSelectionExecutor: client.SemanticTextSelectionV2,
		semanticPressExecutor:         client.SemanticPressV2,
		semanticScrollExecutor:        client.SemanticScrollV1,
		targetBoundInputExecutor:      client.TargetBoundInputV1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	observed, err := tool.Run(ctx, `{
		"action":"get_app_state",
		"app":"Kocoro Pixel Scroll Probe",
		"filter":"interactive",
		"semantic_budget":25,
		"include_screenshot":true,
		"description":"Bind the signed scroll probe window"
	}`)
	if err != nil || observed.IsError || tool.snapshot == nil ||
		tool.coordinateArtifact == nil || len(observed.Images) != 1 {
		t.Fatalf("signed observation failed: result=%+v err=%v", observed, err)
	}
	frame := tool.coordinateArtifact.Frame()
	stateID := tool.snapshot.id
	x := frame.FinalImage.WidthPX / 2
	y := frame.FinalImage.HeightPX / 2
	const providerDeltaX, providerDeltaY = 0, 120
	region := frame.TransformRegions[0]
	wantAxis1, wantAxis2, ok := coordinatePixelScrollCGDeltasV1(
		providerDeltaX, providerDeltaY, region.Affine.A, region.Affine.D)
	if !ok {
		t.Fatalf("signed frame has unusable delta transform: %+v", region)
	}

	startedAt := time.Now().UTC()
	scrolled, err := tool.Run(
		ContextWithOpenAINativeComputerActionV1(ctx),
		fmt.Sprintf(`{
			"action":"pixel_scroll",
			"state_id":%q,
			"x":%d,
			"y":%d,
			"scroll_x":%d,
			"scroll_y":%d,
			"description":"One isolated signed probe scroll"
		}`, stateID, x, y, providerDeltaX, providerDeltaY),
	)
	if err != nil || scrolled.IsError || scrolled.GUIOutcome == nil ||
		scrolled.GUIOutcome.Result != "completed_unverified" ||
		scrolled.GUIOutcome.FailureCode != "scroll_postcondition_not_declared" {
		if scrolled.GUIOutcome != nil {
			t.Fatalf(
				"signed pixel scroll failed: content=%q outcome=%+v err=%v",
				scrolled.Content, *scrolled.GUIOutcome, err)
		}
		t.Fatalf("signed pixel scroll failed: result=%+v err=%v", scrolled, err)
	}
	helperPID := client.bundlePID
	if helperPID <= 0 {
		t.Fatal("signed AX helper PID was not captured")
	}

	receipt := waitForSignedPixelScrollReceipt(t, receiptPath)
	if receipt.SchemaVersion != "kocoro.pixel_scroll_probe.receipt.v2" ||
		receipt.Nonce != nonce || receipt.Mode != "apply" ||
		receipt.EventIndex != 1 || receipt.ProbeIdentity.ProcessIdentifier != int32(probePID) ||
		receipt.EventSource.UnixProcessIdentifier != int64(helperPID) ||
		receipt.CGPointDeltaAxis1 != wantAxis1 ||
		receipt.CGPointDeltaAxis2 != wantAxis2 ||
		receipt.ClipOriginBefore == receipt.ClipOriginAfter {
		t.Fatalf("signed receipt did not bind exact execution: %+v helper_pid=%d axes=(%d,%d)",
			receipt, helperPID, wantAxis1, wantAxis2)
	}
	recordedAt := time.UnixMilli(int64(receipt.RecordedAtUnixMilliseconds))
	if recordedAt.Before(startedAt.Add(-time.Second)) ||
		recordedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("signed receipt is outside this run: %s", recordedAt)
	}
}

func requireSignedPixelScrollAbsolutePath(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) == "/" {
		t.Fatalf("%s must be a non-root absolute path", name)
	}
	return filepath.Clean(value)
}

func waitForSignedPixelScrollReceipt(
	t *testing.T,
	path string,
) signedPixelScrollReceiptV2 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			if !bytes.HasSuffix(data, []byte{'\n'}) ||
				bytes.Count(data, []byte{'\n'}) != 1 {
				t.Fatalf("signed receipt must contain exactly one JSONL record")
			}
			var receipt signedPixelScrollReceiptV2
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&receipt); err != nil {
				t.Fatalf("decode signed receipt: %v", err)
			}
			return receipt
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("signed scroll receipt was not written")
	return signedPixelScrollReceiptV2{}
}

type signedPixelScrollPointV2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type signedPixelScrollProbeIdentityV2 struct {
	ProcessIdentifier int32  `json:"process_identifier"`
	ExecutablePath    string `json:"executable_path"`
	BundleIdentifier  string `json:"bundle_identifier"`
}

type signedPixelScrollEventSourceV2 struct {
	UnixProcessIdentifier int64 `json:"unix_process_identifier"`
	UserIdentifier        int64 `json:"user_identifier"`
	GroupIdentifier       int64 `json:"group_identifier"`
	StateIdentifier       int64 `json:"state_identifier"`
}

type signedPixelScrollPhaseV2 struct {
	RawValue uint64   `json:"raw_value"`
	Flags    []string `json:"flags"`
}

type signedPixelScrollReceiptV2 struct {
	SchemaVersion              string                           `json:"schema_version"`
	Nonce                      string                           `json:"nonce"`
	Mode                       string                           `json:"mode"`
	EventIndex                 uint64                           `json:"event_index"`
	RecordedAtUnixMilliseconds uint64                           `json:"recorded_at_unix_milliseconds"`
	ProbeIdentity              signedPixelScrollProbeIdentityV2 `json:"probe_identity"`
	CGPointDeltaAxis1          int64                            `json:"cg_point_delta_axis_1"`
	CGPointDeltaAxis2          int64                            `json:"cg_point_delta_axis_2"`
	NSScrollingDeltaX          float64                          `json:"ns_scrolling_delta_x"`
	NSScrollingDeltaY          float64                          `json:"ns_scrolling_delta_y"`
	HasPreciseScrollingDeltas  bool                             `json:"has_precise_scrolling_deltas"`
	DirectionInverted          bool                             `json:"is_direction_inverted_from_device"`
	Phase                      signedPixelScrollPhaseV2         `json:"phase"`
	MomentumPhase              signedPixelScrollPhaseV2         `json:"momentum_phase"`
	CGLocation                 signedPixelScrollPointV2         `json:"cg_location"`
	NSLocationInWindow         signedPixelScrollPointV2         `json:"ns_location_in_window"`
	EventTimestampNanoseconds  uint64                           `json:"event_timestamp_nanoseconds"`
	EventSource                signedPixelScrollEventSourceV2   `json:"event_source"`
	ClipOriginBefore           signedPixelScrollPointV2         `json:"clip_origin_before"`
	ClipOriginAfter            signedPixelScrollPointV2         `json:"clip_origin_after"`
}
