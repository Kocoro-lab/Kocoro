package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDaemonStartModeRequiresContainedIsolation(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	tests := []struct {
		name       string
		isolated   bool
		detach     bool
		force      bool
		port       int
		stateDir   string
		rpcSocket  string
		rpcPIDFile string
		wantErr    string
	}{
		{name: "normal default", port: defaultDaemonPort},
		{name: "normal alternate port", port: 17533, wantErr: "requires --isolated"},
		{name: "normal state directory", port: defaultDaemonPort, stateDir: stateDir, wantErr: "--state-dir requires --isolated"},
		{name: "isolated", isolated: true, port: 17533, stateDir: stateDir},
		{name: "default port", isolated: true, port: defaultDaemonPort, stateDir: stateDir, wantErr: "non-default --port"},
		{name: "ephemeral port", isolated: true, port: 0, stateDir: stateDir, wantErr: "--port"},
		{name: "missing state", isolated: true, port: 17533, wantErr: "--state-dir"},
		{name: "relative state", isolated: true, port: 17533, stateDir: "relative", wantErr: "--state-dir"},
		{name: "detach", isolated: true, detach: true, port: 17533, stateDir: stateDir, wantErr: "--detach"},
		{name: "force", isolated: true, force: true, port: 17533, stateDir: stateDir, wantErr: "--force"},
		{name: "rpc", isolated: true, port: 17533, stateDir: stateDir, rpcSocket: "/tmp/rpc.sock", rpcPIDFile: "/tmp/rpc.pid", wantErr: "Desktop RPC"},
		{name: "port too high", port: 65536, wantErr: "--port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDaemonStartMode(tc.isolated, tc.detach, tc.force, tc.port, tc.stateDir, tc.rpcSocket, tc.rpcPIDFile)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateDaemonStartMode() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateDaemonStartMode() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestDaemonLiveE2EFlagsAreHidden(t *testing.T) {
	for _, name := range []string{"isolated", "port", "state-dir", "isolated-mcp", "isolated-api-key-stdin"} {
		flag := daemonStartCmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("daemon start flag --%s is not registered", name)
		}
		if !flag.Hidden {
			t.Errorf("daemon start flag --%s is public, want hidden test-harness control", name)
		}
	}
}

func TestReadIsolatedAPIKey(t *testing.T) {
	t.Run("reads without logging or rewriting", func(t *testing.T) {
		got, err := readIsolatedAPIKey(strings.NewReader("  test-credential\n"))
		if err != nil {
			t.Fatalf("readIsolatedAPIKey: %v", err)
		}
		if got != "test-credential" {
			t.Fatalf("credential = %q, want exact trimmed value", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := readIsolatedAPIKey(strings.NewReader(" \n")); err == nil || !strings.Contains(err.Error(), "empty credential") {
			t.Fatalf("error = %v, want empty credential", err)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		if _, err := readIsolatedAPIKey(strings.NewReader(strings.Repeat("x", 16*1024+1))); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v, want size bound", err)
		}
	})
}

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
