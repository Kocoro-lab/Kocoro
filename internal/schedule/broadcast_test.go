package schedule

import "testing"

func TestParseThreadEnum(t *testing.T) {
	tests := []struct {
		in      string
		wantPtr *bool // nil means "expect (nil, ...)"
		wantOK  bool
	}{
		// Absent / explicit auto → (nil, true): follow session state downstream.
		{in: "", wantPtr: nil, wantOK: true},
		{in: "auto", wantPtr: nil, wantOK: true},
		// on → (&true, true): always thread-anchor.
		{in: "on", wantPtr: boolPtr(true), wantOK: true},
		// off → (&false, true): always top-level / independent.
		{in: "off", wantPtr: boolPtr(false), wantOK: true},
		// Anything else → (nil, false): caller surfaces a validation error.
		{in: "garbage", wantPtr: nil, wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseThreadEnum(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseThreadEnum(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if (got == nil) != (tc.wantPtr == nil) {
				t.Fatalf("ParseThreadEnum(%q) ptr nil-ness: got %v want %v", tc.in, got, tc.wantPtr)
			}
			if got != nil && tc.wantPtr != nil && *got != *tc.wantPtr {
				t.Errorf("ParseThreadEnum(%q) = %v, want %v", tc.in, *got, *tc.wantPtr)
			}
		})
	}
}

// boolPtr is defined in schedule_test.go (same package).
