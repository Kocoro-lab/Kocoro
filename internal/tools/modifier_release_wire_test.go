package tools

import "testing"

func TestModifierReleaseFailureTuplesRemainStrictAndNonRetryable(t *testing.T) {
	code := "modifier_release_unconfirmed"

	mouse, err := DecodeCoordinateMouseEventResultV1(loadCoordinateFixture(
		t, "coordinate_mouse_event.response.move.completed.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	mouse.Status = "completed_unverified"
	mouse.FailureCode = &code
	if mouse.RetrySafe || mouse.ValidateTaggedUnion() != nil {
		t.Fatalf("mouse modifier release tuple rejected: %+v", mouse)
	}

	drag, err := DecodeCoordinateDragResultV1(loadCoordinateFixture(
		t, "coordinate_drag.response.completed_unverified.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	drag.FailureCode = &code
	if drag.RetrySafe || drag.ValidateTaggedUnion() != nil {
		t.Fatalf("drag modifier release tuple rejected: %+v", drag)
	}

	scroll, err := DecodeCoordinatePixelScrollResultV1(loadCoordinateFixture(
		t, "coordinate_pixel_scroll.response.committed_unverified.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	scroll.FailureCode = &code
	if scroll.RetrySafe || scroll.ValidateTaggedUnion() != nil {
		t.Fatalf("scroll modifier release tuple rejected: %+v", scroll)
	}

	keypress := TargetBoundInputResultV1{
		SchemaVersion: 1, Status: "completed_unverified", Action: "keypress",
		InputCommitted: true, Phase: "action", FailureCode: &code,
	}
	if keypress.RetrySafe || keypress.ValidateTaggedUnion() != nil {
		t.Fatalf("keypress modifier release tuple rejected: %+v", keypress)
	}
}
