package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestPrintMemoryStatus_Shapes smoke-tests the three wire shapes the
// daemonStatusCmd needs to render without panicking:
//  1. No memory field (older daemons, or daemon that hasn't started memory yet)
//  2. Memory enabled (provider only, no reason)
//  3. Memory disabled with reason=tlm_binary_too_old and a repair_needed
//     detail block (the schema-mismatch lockout case from production 2026-05-22)
func TestPrintMemoryStatus_Shapes(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantLines []string
		notWant   []string
	}{
		{
			name:      "no_memory_field",
			input:     `{"is_connected":true,"active_agent":"","uptime":0,"version":"v0"}`,
			wantLines: nil,
			notWant:   []string{"Memory:", "Repair:"},
		},
		{
			name:  "enabled_no_reason",
			input: `{"is_connected":true,"memory":{"provider":"enabled"}}`,
			wantLines: []string{
				"Memory:    enabled",
			},
			notWant: []string{"Repair:"},
		},
		{
			name: "degraded_with_repair_needed",
			input: `{"is_connected":true,"memory":{
				"provider":"disabled",
				"reason":"tlm_binary_too_old",
				"detail":{
					"restart_attempts":5,
					"repair_needed":{
						"compatibility":"incompatible",
						"sub_code":"no_manifest",
						"bundle_version":""
					}
				}
			}}`,
			wantLines: []string{
				"Memory:    disabled (tlm_binary_too_old)",
				"restart_attempts=5",
				"Repair:    bundle_version= compatibility=incompatible sub_code=no_manifest",
			},
		},
		{
			name: "degraded_generic_startup_timeout_no_repair_block",
			input: `{"memory":{
				"provider":"disabled",
				"reason":"startup_timeout",
				"detail":{"restart_attempts":3}
			}}`,
			wantLines: []string{
				"Memory:    disabled (startup_timeout)",
				"restart_attempts=3",
			},
			notWant: []string{"Repair:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp daemonStatusResponse
			if err := json.Unmarshal([]byte(tc.input), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			var buf bytes.Buffer
			printMemoryStatus(&buf, resp.Memory)
			out := buf.String()
			for _, want := range tc.wantLines {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in output:\n%s", want, out)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Fatalf("unexpected %q in output:\n%s", notWant, out)
				}
			}
		})
	}
}
