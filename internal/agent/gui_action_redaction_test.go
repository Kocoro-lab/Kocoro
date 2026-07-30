package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/audit"
)

func TestRedactGUIActivityArgumentsKeepsOnlyStableActionMetadata(t *testing.T) {
	secret := "ordinary-user-text-that-must-not-reach-events"
	for _, toolName := range []string{"computer_use", "computer", "accessibility", "applescript", "ghostty"} {
		got := RedactGUIActivityArguments(toolName,
			`{"action":"type","text":"`+secret+`","keys":"command+secret","description":"`+secret+`"}`)
		if strings.Contains(got, secret) || strings.Contains(got, "command+secret") {
			t.Fatalf("%s activity arguments leaked GUI content: %q", toolName, got)
		}
		if !strings.Contains(got, `"redacted":true`) {
			t.Fatalf("%s activity arguments lack redaction marker: %q", toolName, got)
		}
		if toolName != "applescript" && !strings.Contains(got, `"action":"type"`) {
			t.Fatalf("%s lost stable action metadata: %q", toolName, got)
		}
	}

	ordinary := `{"path":"/tmp/example"}`
	if got := RedactGUIActivityArguments("file_read", ordinary); got != ordinary {
		t.Fatalf("non-GUI arguments changed: %q", got)
	}
}

func TestRedactGUIActivityArgumentsFailsClosedForMalformedOrUnboundedAction(t *testing.T) {
	for _, args := range []string{
		`not-json`,
		`{"action":"TYPE WITH USER CONTENT"}`,
		`{"action":"` + strings.Repeat("a", 81) + `"}`,
	} {
		got := RedactGUIActivityArguments("computer_use", args)
		if got != `{"redacted":true}` {
			t.Fatalf("malformed GUI activity arguments = %q", got)
		}
	}
}

func TestRedactGUIActivityResultSuppressesLegacyTypedContent(t *testing.T) {
	secret := "typed ordinary content"
	if got := RedactGUIActivityResult("computer", secret); strings.Contains(got, secret) || got != GUIActivityResultRedacted {
		t.Fatalf("GUI result was not redacted: %q", got)
	}
	if got := RedactGUIActivityResult("file_read", secret); got != secret {
		t.Fatalf("non-GUI result changed: %q", got)
	}
}

func TestAgentAuditBoundarySuppressesGUIArgumentsAndResult(t *testing.T) {
	logDir := t.TempDir()
	logger, err := audit.NewAuditLogger(logDir)
	if err != nil {
		t.Fatal(err)
	}
	loop := &AgentLoop{auditor: logger, sessionID: "sess-redaction"}
	secret := "ordinary typed content in audit"
	loop.logAudit("computer", `{"action":"type","text":"`+secret+`"}`,
		"Typed: "+secret, "allow", true, 12, nil)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(logDir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret) || strings.Contains(string(payload), "Typed:") {
		t.Fatalf("GUI audit row leaked typed content: %s", payload)
	}
	if !strings.Contains(string(payload), `\"action\":\"type\"`) ||
		!strings.Contains(string(payload), GUIActivityResultRedacted) {
		t.Fatalf("GUI audit row lost safe redacted metadata: %s", payload)
	}
}
