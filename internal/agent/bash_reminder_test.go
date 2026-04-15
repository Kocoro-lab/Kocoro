package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// Fix E: the bash reminder is for "use the dedicated tool instead of bash"
// hints, which only applies to a narrow set of read-only commands. Noisy
// false positives on mkdir/pip/python/curl burn context for no signal.

func TestSystemReminder_BashDedicatedReplacements(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		{"cat simple", "cat foo.txt", true},
		{"head", "head -n 5 foo.txt", true},
		{"tail", "tail -f log", true},
		{"find", "find . -name '*.go'", true},
		{"grep", "grep TODO foo.go", true},
		{"rg", "rg TODO", true},
		{"ls", "ls -lh /tmp", true},
		// Shell composition means the intent isn't a plain read — reminding the
		// model to "use glob not find" on these would be wrong.
		{"cat piped to wc", "cat foo | wc -l", false},
		{"cat chained", "cat foo && echo done", false},
		{"cat with redirect", "cat foo > bar", false},
		{"cat with backtick", "cat `which foo`", false},
		{"cat with subshell", "cat $(find . -name x)", false},
		// Not-a-read commands: no replacement.
		{"mkdir", "mkdir -p /tmp/x", false},
		{"pip show", "pip3 show python-docx", false},
		{"python script", "python3 /tmp/build.py", false},
		{"curl", "curl -s https://example.com", false},
		{"git status", "git status", false},
		// Degenerate inputs.
		{"empty command", "", false},
		{"whitespace", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": tc.command})
			got := bashCommandHasDedicatedToolReplacement(json.RawMessage(args))
			if got != tc.want {
				t.Errorf("bashCommandHasDedicatedToolReplacement(%q) = %v, want %v", tc.command, got, tc.want)
			}

			reminder := systemReminder("bash", json.RawMessage(args))
			hasReminder := reminder != ""
			if hasReminder != tc.want {
				t.Errorf("systemReminder(bash, %q) -> %q, wantReminder=%v", tc.command, reminder, tc.want)
			}
			if hasReminder && !strings.Contains(reminder, "dedicated tools") {
				t.Errorf("reminder wording drifted: %q", reminder)
			}
		})
	}
}

func TestSystemReminder_BashEmptyArgsNoReminder(t *testing.T) {
	if got := systemReminder("bash", nil); got != "" {
		t.Errorf("nil args should not produce reminder, got %q", got)
	}
	if got := systemReminder("bash", json.RawMessage(`{}`)); got != "" {
		t.Errorf("empty args object should not produce reminder, got %q", got)
	}
	if got := systemReminder("bash", json.RawMessage(`not-json`)); got != "" {
		t.Errorf("bad json should not produce reminder, got %q", got)
	}
}

func TestSystemReminder_OtherToolsUnchanged(t *testing.T) {
	if got := systemReminder("file_read", nil); !strings.Contains(got, "Read before modifying") {
		t.Errorf("file_read reminder wording drifted: %q", got)
	}
	if got := systemReminder("file_write", nil); !strings.Contains(got, "Verify changes") {
		t.Errorf("file_write reminder wording drifted: %q", got)
	}
	if got := systemReminder("file_edit", nil); !strings.Contains(got, "Verify changes") {
		t.Errorf("file_edit reminder wording drifted: %q", got)
	}
	if got := systemReminder("http", nil); got != "" {
		t.Errorf("unrelated tool should have no reminder, got %q", got)
	}
}
