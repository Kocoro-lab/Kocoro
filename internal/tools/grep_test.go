package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// TestGrep_FindsMatches verifies the default output_mode (files_with_matches):
// result contains the file path of any file containing the pattern, NOT the
// matching line text. This is the cost-saving default — call sites that need
// content must opt in via output_mode=content (covered by
// TestGrep_FindsMatches_ContentMode).
func TestGrep_FindsMatches(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"hello","path":%q,"description":"test grep"}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "a.txt") {
		t.Errorf("expected file path 'a.txt' in result, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "hello world") {
		t.Errorf("default mode must NOT include match content; got: %s", result.Content)
	}
}

// TestGrep_FindsMatches_ContentMode is the explicit opt-in for the old
// behavior — file:line:text with match content. Required to keep grep
// useful for "what does the matching line say" use cases.
func TestGrep_FindsMatches_ContentMode(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(),
		fmt.Sprintf(`{"pattern":"hello","path":%q,"output_mode":"content","description":"test content grep"}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "hello world") {
		t.Errorf("content mode must include matching line text; got: %s", result.Content)
	}
}

// TestGrep_CountMode returns per-file match counts in path:N form.
func TestGrep_CountMode(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello\nhello\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(),
		fmt.Sprintf(`{"pattern":"hello","path":%q,"output_mode":"count","description":"test count grep"}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "a.txt:3") {
		t.Errorf("count mode must include 'a.txt:3' (3 matches in 1 file); got: %s", result.Content)
	}
}

func TestGrepTool_TypeHeadLimitAndOffset(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "alpha\nmatch one\nmatch two\nmatch three\n")
	write("b.txt", "match txt\n")

	tool := &GrepTool{}
	result, err := tool.Run(cwdctx.WithSessionCWD(context.Background(), dir), `{
		"pattern":"match",
		"type":"go",
		"output_mode":"content",
		"head_limit":1,
		"offset":1,
		"description":"test type and offset"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("grep error: %s", result.Content)
	}
	if strings.Contains(result.Content, "b.txt") {
		t.Fatalf("type filter leaked txt file: %s", result.Content)
	}
	if !strings.Contains(result.Content, "match two") || strings.Contains(result.Content, "match one") {
		t.Fatalf("offset/head_limit not honored: %s", result.Content)
	}
}

func TestGrepTool_ContextAndIgnoreCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("before\nTarget\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(cwdctx.WithSessionCWD(context.Background(), dir), `{
		"pattern":"target",
		"output_mode":"content",
		"ignore_case":true,
		"context":1,
		"description":"test context grep"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("grep error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "before") || !strings.Contains(result.Content, "after") {
		t.Fatalf("context lines missing: %s", result.Content)
	}
}

// TestGrep_InvalidMode rejects unknown output_mode values.
func TestGrep_InvalidMode(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(),
		fmt.Sprintf(`{"pattern":"hello","path":%q,"output_mode":"json","description":"test invalid mode"}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error for invalid output_mode, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "output_mode") {
		t.Errorf("error must reference output_mode, got: %s", result.Content)
	}
}

func TestGrep_NoMatchesReturnsSuccess(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"goodbye","path":%q,"description":"test no matches"}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success for no matches, got error: %s", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "no matches") {
		t.Errorf("expected 'no matches' message, got: %s", result.Content)
	}
}

// TestGrep_GlobalLineCap: when a single search produces more than max_results
// total lines across many files, the result must be globally capped (not
// per-file, which is what rg's --max-count does). Creates a directory of 50
// files each containing 10 matches (500 total) and asks for max_results=20.
func TestGrep_GlobalLineCap(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 50; i++ {
		var sb strings.Builder
		for j := 0; j < 10; j++ {
			sb.WriteString(fmt.Sprintf("needle line %d in file %d\n", j, i))
		}
		path := filepath.Join(tmp, fmt.Sprintf("file%02d.txt", i))
		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &GrepTool{}
	// Explicit content mode — the cap-on-match-lines semantics this test
	// targets only applies in content mode. Default files_with_matches
	// would return ≤50 paths and never exercise per-line cap.
	result, err := tool.Run(context.Background(),
		fmt.Sprintf(`{"pattern":"needle","path":%q,"max_results":20,"output_mode":"content","description":"test global cap"}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}

	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	matchLines := 0
	hasTruncation := false
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "truncated") {
			hasTruncation = true
			continue
		}
		if strings.Contains(l, "needle") {
			matchLines++
		}
	}
	if matchLines > 20 {
		t.Errorf("expected <= 20 match lines, got %d (cap is not global)", matchLines)
	}
	if !hasTruncation {
		t.Errorf("expected truncation notice, got: %s", result.Content)
	}
}

func TestGrep_RelativePathRefusedWithoutSessionCWD(t *testing.T) {
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(), `{"pattern":"anything","description":"test relative guard"}`)
	if err != nil {
		t.Fatalf("Run should not return a transport error, got %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when session CWD unset, got: %s", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "session working directory") &&
		!strings.Contains(strings.ToLower(result.Content), "absolute path") {
		t.Errorf("expected guard message, got: %s", result.Content)
	}
}

func TestGrep_RelativePathWorksWithSessionCWD(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("findme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := cwdctx.WithSessionCWD(context.Background(), tmp)
	tool := &GrepTool{}
	result, err := tool.Run(ctx, `{"pattern":"findme","description":"test relative grep"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	// Default output_mode=files_with_matches returns the file path, not the
	// matching line. Assert the path appears.
	if !strings.Contains(result.Content, "a.txt") {
		t.Errorf("expected file path 'a.txt' in result, got: %s", result.Content)
	}
}

func TestGrep_ContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := &GrepTool{}
	_, _ = tool.Run(ctx, fmt.Sprintf(`{"pattern":"x","path":%q,"description":"test cancellation"}`, tmp))
	// Must not hang.
}

func TestGrepTool_RelativizesOutputPaths(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "a.txt"), []byte("needle\nneedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := cwdctx.WithSessionCWD(context.Background(), tmp)
	tool := &GrepTool{}
	expectedPath := filepath.ToSlash(filepath.Join("nested", "a.txt"))
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{name: "files", args: `{"pattern":"needle","description":"test files output"}`, want: expectedPath},
		{name: "content", args: `{"pattern":"needle","output_mode":"content","description":"test content output"}`, want: expectedPath + ":1:needle"},
		{name: "count", args: `{"pattern":"needle","output_mode":"count","description":"test count output"}`, want: expectedPath + ":2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Run(ctx, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("grep error: %s", result.Content)
			}
			got := filepath.ToSlash(result.Content)
			if strings.Contains(got, filepath.ToSlash(tmp)) {
				t.Fatalf("result should use relative paths, got: %s", result.Content)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("result missing %q, got: %s", tc.want, result.Content)
			}
		})
	}
}

func TestGrepTool_RipgrepFindsHiddenFilesButExcludesVCS(t *testing.T) {
	requireRipgrep(t)

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, ".env"), []byte("SECRET_TOKEN=needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(tmp, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("needle in git metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &GrepTool{}
	result, err := tool.Run(cwdctx.WithSessionCWD(context.Background(), tmp), `{"pattern":"needle","description":"test hidden files"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("grep error: %s", result.Content)
	}
	if !strings.Contains(result.Content, ".env") {
		t.Fatalf("hidden file should be searched, got: %s", result.Content)
	}
	if strings.Contains(result.Content, ".git") {
		t.Fatalf("VCS metadata should be excluded, got: %s", result.Content)
	}
}

func TestGrepTool_RipgrepTreatsDashLeadingPatternAsPattern(t *testing.T) {
	requireRipgrep(t)

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "dash.txt"), []byte("-needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &GrepTool{}
	result, err := tool.Run(cwdctx.WithSessionCWD(context.Background(), tmp), `{
		"pattern":"-needle",
		"output_mode":"content",
		"description":"test dash pattern"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("dash-leading pattern should not be parsed as an rg flag: %s", result.Content)
	}
	if !strings.Contains(result.Content, "-needle") {
		t.Fatalf("expected dash-leading match, got: %s", result.Content)
	}
}

func TestGrepTool_RipgrepSplitsGlobListAndSortsFilesByMTime(t *testing.T) {
	requireRipgrep(t)

	tmp := t.TempDir()
	files := map[string]string{
		"old.go":     "needle\n",
		"new.txt":    "needle\n",
		"skipped.md": "needle\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(filepath.Join(tmp, "old.go"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(tmp, "new.txt"), newTime, newTime); err != nil {
		t.Fatal(err)
	}

	tool := &GrepTool{}
	result, err := tool.Run(cwdctx.WithSessionCWD(context.Background(), tmp), `{
		"pattern":"needle",
		"glob":"*.go,*.txt",
		"description":"test glob list"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("grep error: %s", result.Content)
	}
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected two matching files, got: %s", result.Content)
	}
	if lines[0] != "new.txt" || lines[1] != "old.go" {
		t.Fatalf("files_with_matches should be mtime sorted after glob split, got: %v", lines)
	}
	if strings.Contains(result.Content, "skipped.md") {
		t.Fatalf("glob list should exclude skipped.md, got: %s", result.Content)
	}
}

func TestGrepTool_RipgrepOmitsVeryLongContentLines(t *testing.T) {
	requireRipgrep(t)

	tmp := t.TempDir()
	longLine := "needle" + strings.Repeat("x", 1000) + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "long.txt"), []byte(longLine), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &GrepTool{}
	result, err := tool.Run(cwdctx.WithSessionCWD(context.Background(), tmp), `{
		"pattern":"needle",
		"output_mode":"content",
		"description":"test long line"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("grep error: %s", result.Content)
	}
	if strings.Contains(result.Content, strings.Repeat("x", 600)) {
		t.Fatalf("long matching line should be omitted, got %d chars", len(result.Content))
	}
	if !strings.Contains(result.Content, "Omitted long matching line") {
		t.Fatalf("expected rg max-columns omission marker, got: %s", result.Content)
	}
}

func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
}
