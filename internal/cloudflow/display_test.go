package cloudflow

import "testing"

func TestCloudStatusLine(t *testing.T) {
	cases := []struct {
		name    string
		agentID string
		status  string
		message string
		want    string
	}{
		{"named agent with cloud message", "Todoroki", "started", "Todoroki is on it", "[Todoroki] Todoroki is on it"},
		{"named agent empty message falls back", "Todoroki", "started", "", "[Todoroki] Agent working..."},
		{"empty message thinking fallback", "", "thinking", "", "Thinking..."},
		{"tool fallback", "", "tool", "", "Calling tool..."},
		{"orchestrator id is not prefixed", "orchestrator", "started", "planning", "planning"},
		{"streaming id is not prefixed", "streaming", "tool", "searching", "searching"},
		{"unknown status default fallback", "", "weird", "", "Working..."},
		{"processing status fallback", "", "processing", "", "Processing data..."},
		// DATA_PROCESSING rides the synthetic agent_id "preparing", which is NOT
		// in the deny-list, so it IS bracketed like a worker nickname. Pins
		// current behavior (whether to filter it out is a UX decision — see the
		// DATA_PROCESSING case in dispatch.go).
		{"preparing synthetic id is bracketed", "preparing", "processing", "Gathering context", "[preparing] Gathering context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CloudStatusLine(tc.agentID, tc.status, tc.message); got != tc.want {
				t.Fatalf("CloudStatusLine(%q,%q,%q) = %q, want %q", tc.agentID, tc.status, tc.message, got, tc.want)
			}
		})
	}
}
