// Package privacy_test.go is the CI guard for the §7 privacy invariants from
// docs/superpowers/specs/2026-05-14-claude-migrate-design.md. Each test maps
// to one invariant. Any regression here should block the build.
//
// These tests intentionally duplicate coverage that other test files have
// against narrower paths; the goal is to keep the privacy contract verifiable
// in one place even if individual scanners or converters change.
package claudecode

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestPrivacy_NoOutboundHTTPImports is a static check: production source
// files (non-_test.go) in the migrate package must not import net/http for
// outbound use. The package is local-only; any inbound HTTP handling lives
// in internal/daemon, not here.
func TestPrivacy_NoOutboundHTTPImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(data), `"net/http"`) {
			t.Errorf("FILE %s imports net/http — migrate package must stay local-only", f)
		}
	}
}

// TestPrivacy_NoOutboundCallsAtRuntime installs a panic-transport as
// http.DefaultTransport, runs a full Scan → BuildPlan → Apply cycle, and
// confirms that no code path in the migrate package attempted any outbound
// HTTP. Belt-and-suspenders with the static check above.
func TestPrivacy_NoOutboundCallsAtRuntime(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = &panicTransport{t: t}
	defer func() { http.DefaultTransport = original }()

	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, err := Scan(src)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	target := t.TempDir()
	plan, err := BuildPlan(scan, src, target, "/Users/wayland", time.Now())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := NewApplier(target).Apply(plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

type panicTransport struct{ t *testing.T }

func (p *panicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.t.Errorf("migrate package made an outbound HTTP call to %s — privacy invariant violated", req.URL)
	return nil, nil
}

// TestPrivacy_EnvValuesNeverInResponse plants distinctive sentinel values in
// the env of an MCP source server, runs Scan → BuildPlan → Apply, and asserts
// that none of the sentinel values appear in the JSON serialization of any
// data structure that leaves this package over the wire (scan, plan, apply
// result, written config.yaml).
func TestPrivacy_EnvValuesNeverInResponse(t *testing.T) {
	const sentinel1 = "sk-LEAKY-PRIVATE-KEY-AAAAA"
	const sentinel2 = "Bearer-LEAKY-TOKEN-BBBBB"

	home := t.TempDir()
	claudeJSON := filepath.Join(home, ".claude.json")
	cfg := `{
		"mcpServers": {
			"leaky": {
				"command": "node",
				"env": {
					"PRIVATE_KEY": "` + sentinel1 + `",
					"AUTH_TOKEN":  "` + sentinel2 + `"
				}
			}
		}
	}`
	if err := os.WriteFile(claudeJSON, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := SourcePaths{
		ClaudeHome:       filepath.Join(home, ".claude"),
		ClaudeUserConfig: claudeJSON,
	}
	scan, err := Scan(src)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	target := t.TempDir()
	plan, _ := BuildPlan(scan, src, target, home, time.Now())
	res, _ := NewApplier(target).Apply(plan)

	// Serialize every public surface the migrate package produces.
	surfaces := map[string]any{
		"scan":   scan,
		"plan":   plan,
		"result": res,
	}
	for name, v := range surfaces {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Errorf("marshal %s: %v", name, err)
			continue
		}
		s := string(blob)
		if strings.Contains(s, sentinel1) {
			t.Errorf("LEAK: %s contains PRIVATE_KEY value", name)
		}
		if strings.Contains(s, sentinel2) {
			t.Errorf("LEAK: %s contains AUTH_TOKEN value", name)
		}
	}

	// Also assert the written config.yaml contains the env KEY NAMES but not
	// the values. This is the on-disk persistence path.
	yaml, err := os.ReadFile(filepath.Join(target, "config.yaml"))
	if err != nil {
		t.Fatalf("read written config.yaml: %v", err)
	}
	ys := string(yaml)
	if !strings.Contains(ys, "PRIVATE_KEY") || !strings.Contains(ys, "AUTH_TOKEN") {
		t.Errorf("env key names should appear in config.yaml: %s", ys)
	}
	if strings.Contains(ys, sentinel1) || strings.Contains(ys, sentinel2) {
		t.Errorf("LEAK: config.yaml contains env value: %s", ys)
	}
	// Tighten further: any value matching a sk-* / Bearer-* sentinel shape is
	// suspicious in the on-disk artifact.
	leakRe := regexp.MustCompile(`(sk|Bearer)-[A-Z]{5,}`)
	if leakRe.MatchString(ys) {
		t.Errorf("LEAK: config.yaml matches secret-shaped regex: %s", ys)
	}
}

// TestPrivacy_SymlinkEscape_AlwaysRejected proves that no source root or
// per-category entry can sneak past as a symlink. Table-driven across every
// place a symlink could plausibly be planted in ~/.claude.
func TestPrivacy_SymlinkEscape_AlwaysRejected(t *testing.T) {
	outsideRoot := t.TempDir()
	outsideFile := filepath.Join(outsideRoot, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		setup func(t *testing.T) (SourcePaths, string) // returns (src, sentinelSymlinkPath)
	}{
		{
			name: "ClaudeHome root",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				link := filepath.Join(home, ".claude")
				if err := os.Symlink(outsideRoot, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: link, ClaudeUserConfig: filepath.Join(home, ".claude.json")}, link
			},
		},
		{
			name: "skill flat .md symlink",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(home, ".claude", "skills", "evil.md")
				if err := os.Symlink(outsideFile, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: filepath.Join(home, ".claude"), ClaudeUserConfig: filepath.Join(home, ".claude.json")}, link
			},
		},
		{
			name: "agent .md symlink",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				if err := os.MkdirAll(filepath.Join(home, ".claude", "agents"), 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(home, ".claude", "agents", "evil.md")
				if err := os.Symlink(outsideFile, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: filepath.Join(home, ".claude"), ClaudeUserConfig: filepath.Join(home, ".claude.json")}, link
			},
		},
		{
			name: "command .md symlink",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				if err := os.MkdirAll(filepath.Join(home, ".claude", "commands"), 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(home, ".claude", "commands", "evil.md")
				if err := os.Symlink(outsideFile, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: filepath.Join(home, ".claude"), ClaudeUserConfig: filepath.Join(home, ".claude.json")}, link
			},
		},
		{
			name: "CLAUDE.md symlink",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(home, ".claude", "CLAUDE.md")
				if err := os.Symlink(outsideFile, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: filepath.Join(home, ".claude"), ClaudeUserConfig: filepath.Join(home, ".claude.json")}, link
			},
		},
		{
			name: "claude.json symlink",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				outside := filepath.Join(t.TempDir(), "real.json")
				if err := os.WriteFile(outside, []byte(`{"mcpServers":{"x":{"command":"node"}}}`), 0o644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(home, ".claude.json")
				if err := os.Symlink(outside, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: filepath.Join(home, ".claude"), ClaudeUserConfig: link}, link
			},
		},
		{
			name: "skill dir contains internal symlink",
			setup: func(t *testing.T) (SourcePaths, string) {
				home := t.TempDir()
				skill := filepath.Join(home, ".claude", "skills", "evil-dir")
				if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
					[]byte("---\nname: evil-dir\ndescription: x\n---\nbody\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(skill, "scripts", "leak")
				if err := os.Symlink(outsideFile, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return SourcePaths{ClaudeHome: filepath.Join(home, ".claude"), ClaudeUserConfig: filepath.Join(home, ".claude.json")}, link
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := tc.setup(t)
			scan, err := Scan(src)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			gotEscape := false
			for _, w := range scan.Warnings {
				if w.Kind == "symlink_escape" {
					gotEscape = true
				}
			}
			if !gotEscape {
				t.Errorf("expected symlink_escape warning, got warnings=%+v", scan.Warnings)
			}
			// Apply the plan; nothing should land for the symlinked entry.
			target := t.TempDir()
			plan, _ := BuildPlan(scan, src, target, "/tmp", time.Now())
			if _, err := NewApplier(target).Apply(plan); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			// Target shouldn't contain anything created from the symlink path.
			if err := filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
				if err != nil || info == nil {
					return nil
				}
				if info.Mode().IsRegular() {
					b, _ := os.ReadFile(p)
					if strings.Contains(string(b), "CONFIDENTIAL") {
						t.Errorf("LEAK: target file %s contains symlink-target content", p)
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
		})
	}
}

// TestPrivacy_SizeLimits_Enforced confirms the 5 MB per-file cap applies to
// every category that takes a single .md file (skills flat, agents,
// commands, rules). Oversize files are skipped with size_limit warning, not
// truncated or partially read.
func TestPrivacy_SizeLimits_Enforced(t *testing.T) {
	big := strings.Repeat("x", int(MaxFileBytes)+1)

	cases := []struct {
		name string
		path string
	}{
		{"flat skill", "skills/huge.md"},
		{"agent", "agents/huge.md"},
		{"command", "commands/huge.md"},
		{"rules", "CLAUDE.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			full := filepath.Join(home, tc.path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(big), 0o644); err != nil {
				t.Fatal(err)
			}
			scan, _ := Scan(SourcePaths{ClaudeHome: home, ClaudeUserConfig: filepath.Join(t.TempDir(), "absent.json")})

			gotWarning := false
			for _, w := range scan.Warnings {
				if w.Kind == "size_limit" {
					gotWarning = true
				}
			}
			if !gotWarning {
				t.Errorf("%s: expected size_limit warning, got %+v", tc.name, scan.Warnings)
			}
			// Importable count should be zero for this category.
			if scan.TotalImportable() != 0 {
				t.Errorf("%s: oversize should not be importable, got TotalImportable=%d", tc.name, scan.TotalImportable())
			}
		})
	}
}
