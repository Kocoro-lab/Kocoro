package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeComputerUseBundleID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "canonical", input: "com.example.Editor", want: "com.example.editor", ok: true},
		{name: "hyphen", input: "dev.warp.Warp-Stable", want: "dev.warp.warp-stable", ok: true},
		{name: "whitespace", input: " com.example.editor ", ok: false},
		{name: "one component", input: "Terminal", ok: false},
		{name: "empty component", input: "com..example", ok: false},
		{name: "underscore", input: "com.example_bad.app", ok: false},
		{name: "leading hyphen", input: "com.-example.app", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeComputerUseBundleID(test.input)
			if test.ok && err != nil {
				t.Fatalf("normalizeComputerUseBundleID(%q): %v", test.input, err)
			}
			if !test.ok && err == nil {
				t.Fatalf("normalizeComputerUseBundleID(%q) unexpectedly succeeded: %q", test.input, got)
			}
			if got != test.want {
				t.Fatalf("normalizeComputerUseBundleID(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestComputerUseAppPolicyBuiltInsAndUserEntries(t *testing.T) {
	store := NewComputerUseAppPolicyStore(t.TempDir())

	for _, bundleID := range []string{
		"run.shannon.shanclaw",
		"run.shannon.shanclaw.dev.ax-server",
		"com.apple.systempreferences",
		"com.apple.SecurityAgent",
		"com.apple.Passwords",
		"com.apple.KeychainAccess",
		"com.apple.Terminal",
		"com.mitchellh.ghostty",
		"dev.warp.Warp-Stable",
	} {
		decision := store.DecisionFor(bundleID)
		if decision.Decision != ComputerUseAppPolicyBlocked || decision.Source != ComputerUseAppPolicySourceBuiltIn {
			t.Errorf("DecisionFor(%q) = %#v, want built-in blocked", bundleID, decision)
		}
	}

	ordinary := store.DecisionFor("com.tinyspeck.slackmacgap")
	if ordinary.Decision != ComputerUseAppPolicyAsk || ordinary.Source != ComputerUseAppPolicySourceDefault {
		t.Fatalf("ordinary app decision = %#v, want default ask", ordinary)
	}

	if _, err := store.Update("com.example.editor", ComputerUseAppPolicyBlocked); err != nil {
		t.Fatalf("Update blocked: %v", err)
	}
	blocked := store.DecisionFor("COM.EXAMPLE.EDITOR")
	if blocked.Decision != ComputerUseAppPolicyBlocked || blocked.Source != ComputerUseAppPolicySourceUser {
		t.Fatalf("user decision = %#v, want user blocked", blocked)
	}
	if _, err := store.Update("com.example.editor", ComputerUseAppPolicyAsk); err != nil {
		t.Fatalf("Update ask: %v", err)
	}
	asked := store.DecisionFor("com.example.editor")
	if asked.Decision != ComputerUseAppPolicyAsk || asked.Source != ComputerUseAppPolicySourceUser {
		t.Fatalf("explicit ask decision = %#v, want user ask", asked)
	}

	if _, err := store.Update("com.apple.Terminal", ComputerUseAppPolicyAsk); !errors.Is(err, ErrComputerUseAppPolicyBuiltIn) {
		t.Fatalf("built-in override error = %v, want ErrComputerUseAppPolicyBuiltIn", err)
	}
}

func TestComputerUseAppPolicyPersistsAndRevokes(t *testing.T) {
	dir := t.TempDir()
	store := NewComputerUseAppPolicyStore(dir)
	if _, err := store.Update("com.example.editor", ComputerUseAppPolicyBlocked); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded := NewComputerUseAppPolicyStore(dir)
	if got := reloaded.DecisionFor("com.example.editor"); got.Decision != ComputerUseAppPolicyBlocked {
		t.Fatalf("reloaded decision = %#v, want blocked", got)
	}
	if _, err := reloaded.Revoke("com.example.editor"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := reloaded.DecisionFor("com.example.editor"); got.Source != ComputerUseAppPolicySourceDefault {
		t.Fatalf("revoked decision = %#v, want default ask", got)
	}

	info, err := os.Stat(filepath.Join(dir, computerUseAppPolicyFilename))
	if err != nil {
		t.Fatalf("stat policy file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("policy mode = %o, want 600", got)
	}
}

func TestComputerUseAppPolicyCorruptionFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "forbidden decision", payload: `{"schema_version":1,"revision":1,"entries":[{"bundle_id":"com.example.editor","decision":"always_allow","source":"user"}]}`},
		{name: "trailing object", payload: `{"schema_version":1,"revision":1,"entries":[]} {"schema_version":1,"revision":2,"entries":[]}`},
		{name: "duplicate revision", payload: `{"schema_version":1,"revision":1,"revision":2,"entries":[]}`},
		{name: "missing revision", payload: `{"schema_version":1,"entries":[]}`},
		{name: "missing entries", payload: `{"schema_version":1,"revision":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, computerUseAppPolicyFilename), []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewComputerUseAppPolicyStore(dir)
			if store.LoadError() == nil {
				t.Fatal("corrupt policy unexpectedly loaded")
			}
			if got := store.DecisionFor("com.example.editor"); got.Decision != ComputerUseAppPolicyBlocked || got.Source != ComputerUseAppPolicySourceInvalidStore {
				t.Fatalf("corrupt-store decision = %#v, want fail-closed blocked", got)
			}
		})
	}
}

func TestComputerUseAppPolicyEmptyDirectoryFailsClosed(t *testing.T) {
	store := NewComputerUseAppPolicyStore("")
	if store.LoadError() == nil {
		t.Fatal("empty ShannonDir unexpectedly available")
	}
	if got := store.DecisionFor("com.example.editor"); got.Decision != ComputerUseAppPolicyBlocked {
		t.Fatalf("empty-dir decision = %#v, want blocked", got)
	}
}
