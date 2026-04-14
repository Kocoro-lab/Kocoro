package tools

import (
	"strings"
	"testing"
)

func TestNormalizeMCPResult_ModalMissingHintPrefixesError(t *testing.T) {
	raw := `### Error
Error: The tool "browser_file_upload" can only be used when there is related modal state present.`
	got := normalizeMCPResult("playwright", "browser_file_upload", raw, true)
	if !strings.HasPrefix(got, "[hint]") {
		t.Errorf("hint must come FIRST so the model reads it before the raw error; got: %q", got)
	}
	if !strings.Contains(got, raw) {
		t.Error("original error text must still be present after the hint")
	}
	if !strings.Contains(got, "STOP calling browser_file_upload") {
		t.Error("hint should imperatively tell the model to stop retrying")
	}
	if strings.Contains(got, "previous upload already succeeded") {
		t.Error("hint should not over-assert that a previous upload succeeded")
	}
	if !strings.Contains(got, "browser_snapshot") {
		t.Error("hint should point to browser_snapshot for verification")
	}
	if !strings.Contains(got, "reopen the chooser") {
		t.Error("hint should tell the model how to recover if the upload is not present")
	}
}

func TestNormalizeMCPResult_AllowedRootsHintPrefixesError(t *testing.T) {
	raw := `### Error
Error: File access denied: /tmp/foo.png is outside allowed roots. Allowed roots: /Users/x/Desktop`
	got := normalizeMCPResult("playwright", "browser_file_upload", raw, true)
	if !strings.HasPrefix(got, "[hint]") {
		t.Errorf("hint must come FIRST; got: %q", got)
	}
	if !strings.Contains(got, "~/.shannon/tmp/attachments") {
		t.Error("hint should reference the attachments staging directory")
	}
	if !strings.Contains(got, raw) {
		t.Error("original error must still be present")
	}
}

func TestNormalizeMCPResult_SuccessAppendsCompletionHint(t *testing.T) {
	raw := `### Ran Playwright code
` + "```js\nawait fileChooser.setFiles([\"/path/to/file.png\"])\n```\n"
	got := normalizeMCPResult("playwright", "browser_file_upload", raw, false)
	if !strings.Contains(got, raw) {
		t.Error("original success output must be preserved")
	}
	if !strings.Contains(got, "attached to the composer") {
		t.Error("success hint should state the file is attached")
	}
	if !strings.Contains(got, "DO NOT call browser_file_upload again") {
		t.Error("success hint should discourage redundant retry")
	}
	if !strings.Contains(got, "browser_type") {
		t.Error("success hint should direct to the next step (typing)")
	}
}

func TestNormalizeMCPResult_SuccessNoSetFilesPassesThrough(t *testing.T) {
	raw := "some unrelated success output"
	got := normalizeMCPResult("playwright", "browser_file_upload", raw, false)
	if got != raw {
		t.Errorf("no completion hint if setFiles marker absent; got %q", got)
	}
}

func TestNormalizeMCPResult_UnknownErrorPassesThrough(t *testing.T) {
	raw := "some unexpected error text"
	got := normalizeMCPResult("playwright", "browser_file_upload", raw, true)
	if got != raw {
		t.Errorf("unrecognized error must pass through unchanged; got %q", got)
	}
}

func TestNormalizeMCPResult_OtherToolsUnaffected(t *testing.T) {
	raw := `Error: related modal state present`
	got := normalizeMCPResult("playwright", "browser_click", raw, true)
	if got != raw {
		t.Errorf("hint should not fire for non-upload tools; got %q", got)
	}
	successRaw := "await fileChooser.setFiles([])"
	got = normalizeMCPResult("playwright", "browser_click", successRaw, false)
	if got != successRaw {
		t.Errorf("success hint should not fire for non-upload tools; got %q", got)
	}
}

func TestNormalizeMCPResult_EmptyContentPassesThrough(t *testing.T) {
	if got := normalizeMCPResult("playwright", "browser_file_upload", "", true); got != "" {
		t.Errorf("empty content must pass through; got %q", got)
	}
	if got := normalizeMCPResult("playwright", "browser_file_upload", "", false); got != "" {
		t.Errorf("empty content must pass through on success; got %q", got)
	}
}
