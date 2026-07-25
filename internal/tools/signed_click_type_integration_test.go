//go:build darwin && kocoro_signed_integration

package tools

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestSignedClickAndTypeThroughImmutableFrame(t *testing.T) {
	if os.Getenv("KOCORO_RUN_SIGNED_CLICK_TYPE") != "1" {
		t.Skip("set KOCORO_RUN_SIGNED_CLICK_TYPE=1 for real signed input")
	}
	probePID, err := strconv.Atoi(os.Getenv("KOCORO_SIGNED_CLICK_TYPE_PROBE_PID"))
	if err != nil || probePID <= 0 {
		t.Fatal("KOCORO_SIGNED_CLICK_TYPE_PROBE_PID must be positive")
	}
	nonce := os.Getenv("KOCORO_SIGNED_CLICK_TYPE_NONCE")
	if nonce == "" {
		t.Fatal("KOCORO_SIGNED_CLICK_TYPE_NONCE is required")
	}
	inputText := "Kocoro Gate 2 " + nonce

	client := &AXClient{}
	t.Cleanup(client.Close)
	tool := &ComputerUseTool{
		client:                        client,
		coordinateExecutor: func(
			ctx context.Context,
			request CoordinateMouseEventRequestV1,
		) (CoordinateMouseEventResultV1, error) {
			result, err := client.CoordinateMouseEventV1(ctx, request)
			if err != nil {
				t.Logf("coordinate helper error: %v", err)
			}
			return result, err
		},
		coordinateDragExecutor:        client.CoordinateDragV1,
		coordinatePixelScrollExecutor: client.CoordinatePixelScrollV1,
		semanticTextSelectionExecutor: client.SemanticTextSelectionV2,
		semanticPressExecutor:         client.SemanticPressV2,
		semanticScrollExecutor:        client.SemanticScrollV1,
		targetBoundInputExecutor:      client.TargetBoundInputV1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	warmed, err := tool.Run(ctx, `{
		"action":"get_app_state",
		"app":"Kocoro Pixel Scroll Probe",
		"filter":"all",
		"semantic_budget":100,
		"description":"Stabilize the signed click/type AX snapshot"
	}`)
	if err != nil || warmed.IsError || tool.snapshot == nil ||
		tool.snapshot.pid != probePID {
		t.Fatalf("signed click/type warm observation failed: result=%+v err=%v", warmed, err)
	}

	var observed agent.ToolResult
	for attempt := 0; attempt < 3; attempt++ {
		observed, err = tool.Run(ctx, `{
			"action":"get_app_state",
			"app":"Kocoro Pixel Scroll Probe",
			"filter":"all",
			"semantic_budget":100,
			"include_screenshot":true,
			"description":"Bind the signed click/type probe window"
		}`)
		if err != nil || observed.IsError || tool.coordinateArtifact != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || observed.IsError || tool.snapshot == nil ||
		tool.snapshot.pid != probePID || tool.coordinateArtifact == nil ||
		len(observed.Images) != 1 {
		t.Fatalf("signed click observation failed: result=%+v err=%v", observed, err)
	}
	button, ok := signedFindElementByIdentifierV1(
		tool.snapshot.elements, "kocoro.gate2.click-button")
	if !ok || button.Frame == nil {
		t.Fatalf("signed click button is not observable: %+v", button)
	}
	initialCounter, ok := signedFindElementByIdentifierV1(
		tool.snapshot.elements, "kocoro.gate2.click-count")
	if !ok || initialCounter.Value == nil ||
		*initialCounter.Value != "click_count: 0" {
		initialInput, _ := signedFindElementByIdentifierV1(
			tool.snapshot.elements, "kocoro.gate2.input-field")
		t.Fatalf(
			"signed click probe is not pristine: counter=%+v input=%+v",
			initialCounter,
			initialInput,
		)
	}
	frame := tool.coordinateArtifact.Frame()
	x, y := signedProviderPointForAXFrameV1(t, frame, *button.Frame)

	clicked, err := tool.Run(
		ContextWithOpenAINativeComputerActionV1(ctx),
		fmt.Sprintf(`{
			"action":"click",
			"state_id":%q,
			"x":%d,
			"y":%d,
			"button":"left",
			"clicks":1,
			"description":"One isolated signed probe click"
		}`, frame.StateID, x, y),
	)
	if err != nil || clicked.IsError || clicked.GUIOutcome == nil ||
		clicked.GUIOutcome.Result != "completed_unverified" ||
		clicked.GUIOutcome.FailureCode != "click_postcondition_not_declared" {
		t.Fatalf("signed coordinate click failed: result=%+v err=%v", clicked, err)
	}
	if client.bundlePID <= 0 {
		t.Fatal("signed click did not bind an AX helper PID")
	}

	afterClick, err := tool.Run(ctx, `{
		"action":"get_app_state",
		"app":"Kocoro Pixel Scroll Probe",
		"filter":"all",
		"semantic_budget":100,
		"include_screenshot":true,
		"description":"Verify click and bind focused input"
	}`)
	if err != nil || afterClick.IsError || tool.snapshot == nil ||
		tool.coordinateArtifact == nil {
		t.Fatalf("signed post-click observation failed: result=%+v err=%v", afterClick, err)
	}
	counter, ok := signedFindElementByIdentifierV1(
		tool.snapshot.elements, "kocoro.gate2.click-count")
	if !ok || counter.Value == nil || *counter.Value != "click_count: 1" {
		t.Fatalf("signed click postcondition was not observed: %+v", counter)
	}
	input, ok := signedFindElementByIdentifierV1(
		tool.snapshot.elements, "kocoro.gate2.input-field")
	if !ok || input.Ref == "" || !input.Focused || tool.snapshot.id == "" {
		t.Fatalf("signed target-bound input is not focused: %+v", input)
	}
	stateID := tool.snapshot.id

	typed, err := tool.Run(
		ContextWithOpenAINativeComputerActionV1(ctx),
		fmt.Sprintf(`{
			"action":"type",
			"state_id":%q,
			"ref":%q,
			"text":%q,
			"description":"One isolated non-sensitive signed probe input"
		}`, stateID, input.Ref, inputText),
	)
	if err != nil || typed.IsError || typed.GUIOutcome == nil ||
		typed.GUIOutcome.Result != "verified" ||
		strings.Contains(typed.Content, inputText) {
		if typed.GUIOutcome != nil {
			t.Fatalf(
				"signed target-bound type failed or leaked content: outcome=%+v content=%q err=%v",
				*typed.GUIOutcome,
				typed.Content,
				err,
			)
		}
		t.Fatalf("signed target-bound type failed or leaked content: result=%+v err=%v", typed, err)
	}

	afterType, err := tool.Run(ctx, `{
		"action":"get_app_state",
		"app":"Kocoro Pixel Scroll Probe",
		"filter":"all",
		"semantic_budget":100,
		"description":"Verify exact typed value"
	}`)
	if err != nil || afterType.IsError || tool.snapshot == nil {
		t.Fatalf("signed post-type observation failed: result=%+v err=%v", afterType, err)
	}
	typedInput, ok := signedFindElementByIdentifierV1(
		tool.snapshot.elements, "kocoro.gate2.input-field")
	if !ok || typedInput.Value == nil || *typedInput.Value != inputText {
		t.Fatalf("signed type postcondition was not observed: %+v", typedInput)
	}

	t.Logf(
		"signed click/type verified probe_pid=%d helper_pid=%d click=(%d,%d)",
		probePID, client.bundlePID, x, y,
	)
}

func signedFindElementByIdentifierV1(
	elements []computerUseElement,
	identifier string,
) (computerUseElement, bool) {
	for _, element := range elements {
		if element.Identifier != nil && *element.Identifier == identifier {
			return element, true
		}
		if found, ok := signedFindElementByIdentifierV1(
			element.Children, identifier); ok {
			return found, true
		}
	}
	return computerUseElement{}, false
}

func signedProviderPointForAXFrameV1(
	t *testing.T,
	frame CoordinateFrameV1,
	element computerUseFrame,
) (int, int) {
	t.Helper()
	if len(frame.TransformRegions) != 1 {
		t.Fatalf("signed window frame must have one transform region: %+v", frame)
	}
	affine := frame.TransformRegions[0].Affine
	if affine.A <= 0 || affine.D <= 0 || affine.B != 0 || affine.C != 0 {
		t.Fatalf("signed window frame has unsupported affine: %+v", affine)
	}
	centerX := element.X + element.Width/2
	centerY := element.Y + element.Height/2
	x := int(math.Floor((centerX - affine.TX) / affine.A))
	y := int(math.Floor((centerY - affine.TY) / affine.D))
	mapped, err := MapCoordinatePixelCenterV1(
		frame,
		frame.TopologyRef,
		frame.StateID,
		frame.FrameID,
		time.Now().UTC(),
		float64(x),
		float64(y),
	)
	if err != nil {
		t.Fatalf("map signed element center: %v", err)
	}
	if mapped.X < element.X || mapped.X > element.X+element.Width ||
		mapped.Y < element.Y || mapped.Y > element.Y+element.Height {
		t.Fatalf("mapped signed point escaped AX element: mapped=%+v element=%+v", mapped, element)
	}
	return x, y
}
