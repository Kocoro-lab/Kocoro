package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// TestFileWrite_RejectsMissingContent guards against the "wrote 0 bytes"
// pseudo-success that triggered the 2026-05-13 stuck-loop incident: the
// model emitted file_write calls without a `content` field, Go's json
// decoder zero-valued args.Content to "", os.WriteFile happily wrote
// 0 bytes, and the tool returned IsError=false. The model's loop
// detector and the user both interpreted the success as a real write.
func TestFileWrite_RejectsMissingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "should_not_exist.txt")

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	// Raw JSON without the "content" key — mirrors the actual wire shape
	// observed in the incident session (LLM omitted the field entirely).
	rawArgs := fmt.Sprintf(`{"path":%q}`, path)

	result, err := tool.Run(ctx, rawArgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true for missing content, got success: %s", result.Content)
	}
	if !contains(result.Content, "content") {
		t.Errorf("error message should mention `content`, got: %s", result.Content)
	}
	// The pseudo-success bug created a 0-byte file; verify nothing was written.
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("file should NOT have been created when content was missing")
	}
}

// TestFileWrite_RejectsExplicitEmptyContent covers the second wire shape:
// the model sends `"content":""` explicitly. json.Unmarshal yields the same
// args.Content == "" as the missing-key case, so the same guard fires.
func TestFileWrite_RejectsExplicitEmptyContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "should_not_exist.txt")

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	rawArgs := fmt.Sprintf(`{"path":%q,"content":""}`, path)

	result, err := tool.Run(ctx, rawArgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true for empty content, got success: %s", result.Content)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("file should NOT have been created when content was empty")
	}
}

// TestFileWrite_RejectsNullContent covers `"content":null`. Go's
// json.Unmarshal treats JSON null on a string field as zero-value, so this
// also lands in args.Content == "" and must be rejected.
func TestFileWrite_RejectsNullContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "should_not_exist.txt")

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	rawArgs := fmt.Sprintf(`{"path":%q,"content":null}`, path)

	result, err := tool.Run(ctx, rawArgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true for null content, got success: %s", result.Content)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("file should NOT have been created when content was null")
	}
}

// TestFileWrite_MissingContent_DoesNotTruncateExistingFile is the disaster
// scenario the bug enabled in the incident session: the user had an existing
// index.html, the model read it (passing the read-before-write guard), then
// fired file_write with no content. Pre-fix this would silently truncate the
// file to 0 bytes — destroying the user's data. Post-fix the existing file
// must remain byte-for-byte unchanged.
func TestFileWrite_MissingContent_DoesNotTruncateExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "important.html")
	original := []byte("<html><body>important user data</body></html>")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	originalInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("setup stat: %v", err)
	}

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path) // simulate model already file_read'd this path
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	rawArgs := fmt.Sprintf(`{"path":%q}`, path)

	result, err := tool.Run(ctx, rawArgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true, got success: %s", result.Content)
	}

	// Bytes unchanged.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("file was modified! before=%q after=%q", original, after)
	}
	// Size unchanged (defensive: catches partial-truncate).
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("readback stat: %v", err)
	}
	if afterInfo.Size() != originalInfo.Size() {
		t.Errorf("file size changed: %d → %d", originalInfo.Size(), afterInfo.Size())
	}
}

// TestFileWrite_IncidentReplay simulates the exact 2026-05-13 stuck-loop
// pattern: model fires file_write({path, 无 content}) repeatedly against an
// already-read file. Pre-fix this returned 4× IsError=False ("wrote 0
// bytes"), silently truncated the file, and only tripped ConsecutiveDup at
// the 4th identical call. Post-fix every call must return IsError=true and
// the file must remain byte-for-byte unchanged across the entire run. The
// LoopDetector is also driven so we observe what action it takes when every
// call is a deterministic ValidationError on the same args (all-errors 2x
// path → ConsecutiveDup nudge at 6, force-stop at 7).
func TestFileWrite_IncidentReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.html")
	original := []byte("<html><body>vacation lifestyle page draft</body></html>")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path) // model file_read'd it (incident msg#26)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	rawArgs := fmt.Sprintf(`{"path":%q}`, path)
	ld := agent.NewLoopDetector()

	const iterations = 8
	var actions []agent.LoopAction
	sawForceStop := false
	for i := range iterations {
		result, err := tool.Run(ctx, rawArgs)
		if err != nil {
			t.Fatalf("call %d: unexpected runner error: %v", i+1, err)
		}
		if !result.IsError {
			t.Errorf("call %d: expected IsError=true (post-fix), got success: %s", i+1, result.Content)
		}
		ld.Record("file_write", rawArgs, result.IsError, result.Content, "", false)
		action, msg := ld.Check("file_write")
		actions = append(actions, action)
		t.Logf("call %d  IsError=%v  loop_action=%v  msg=%s",
			i+1, result.IsError, action, msg)
		if action == agent.LoopForceStop {
			sawForceStop = true
		}
	}

	// File survived: every byte intact, no truncation, no rewrite.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("file was modified across %d calls!\n  before=%q\n  after =%q",
			iterations, original, after)
	}

	if !sawForceStop {
		t.Errorf("expected LoopForceStop within %d identical-error calls; got actions=%v",
			iterations, actions)
	}
}

// TestFileWrite_ConcurrentMissingContent_PreservesFile is the racy variant of
// the disaster scenario: 50 goroutines × 100 calls each (5000 total) all
// fire file_write({path, 无 content}) at the same already-read file. Every
// single call must reject (IsError=true), no count must succeed, and the
// file must remain byte-for-byte unchanged. Run with -race to also catch
// any TOCTOU between the new content guard, read-tracker, and stat path.
func TestFileWrite_ConcurrentMissingContent_PreservesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "important.html")
	original := []byte("<html><body>important user data</body></html>")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	rawArgs := fmt.Sprintf(`{"path":%q}`, path)

	const goroutines = 50
	const callsPerGoroutine = 100
	var wg sync.WaitGroup
	var errCount, sucCount, runnerErr atomic.Int64

	for range goroutines {
		wg.Go(func() {
			for range callsPerGoroutine {
				result, err := tool.Run(ctx, rawArgs)
				if err != nil {
					runnerErr.Add(1)
					return
				}
				if result.IsError {
					errCount.Add(1)
				} else {
					sucCount.Add(1)
				}
			}
		})
	}
	wg.Wait()

	total := int64(goroutines * callsPerGoroutine)
	if runnerErr.Load() != 0 {
		t.Errorf("Run returned non-nil error %d times (must always be nil)", runnerErr.Load())
	}
	if sucCount.Load() != 0 {
		t.Errorf("expected 0 successes across %d concurrent calls, got %d", total, sucCount.Load())
	}
	if errCount.Load() != total {
		t.Errorf("expected %d IsError=true, got %d", total, errCount.Load())
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("file changed under concurrent attack:\n  before=%q\n  after =%q", original, after)
	}
}

// TestFileWrite_MixedValidAndInvalid interleaves invalid (no content) and
// valid (with content) calls against the same already-read file. Verifies
// that invalid calls never side-effect the file's state and valid calls
// still succeed — i.e. the new guard only rejects what it should and does
// not break the happy path even under interleaved error pressure.
func TestFileWrite_MixedValidAndInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(path, []byte("v0"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	bad := fmt.Sprintf(`{"path":%q}`, path)
	good := func(version string) string {
		return fmt.Sprintf(`{"path":%q,"content":%q}`, path, version)
	}

	// Sequence: bad, good v1, bad, bad, good v2, bad, good v3, bad
	// Every read-tracker entry stays valid because file_write itself
	// re-marks the path on success (see CheckReadBeforeWrite path).
	steps := []struct {
		args   string
		isErr  bool
		expect string // file content expected after this step
	}{
		{bad, true, "v0"},
		{good("v1"), false, "v1"},
		{bad, true, "v1"},
		{bad, true, "v1"},
		{good("v2"), false, "v2"},
		{bad, true, "v2"},
		{good("v3"), false, "v3"},
		{bad, true, "v3"},
	}

	for i, step := range steps {
		result, err := tool.Run(ctx, step.args)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if result.IsError != step.isErr {
			t.Errorf("step %d: expected IsError=%v, got %v (%s)", i, step.isErr, result.IsError, result.Content)
		}
		got, _ := os.ReadFile(path)
		if string(got) != step.expect {
			t.Errorf("step %d: expected file=%q, got %q", i, step.expect, got)
		}
	}
}

// FuzzFileWrite_RawJSON throws random JSON at file_write under a sandboxed
// session CWD. The contract under test, regardless of input shape:
//
//   - Run never returns a Go-level error (always returns a graceful
//     ToolResult).
//   - Run never panics.
//   - When IsError=false, the file actually got written and its size
//     matches the post-validation content length (no 0-byte pseudo-success
//     and no out-of-band side effects).
//
// `go test` runs this against the seed corpus by default; `go test -fuzz`
// extends with mutation-based exploration.
func FuzzFileWrite_RawJSON(f *testing.F) {
	seeds := []string{
		`{"path":"a.txt"}`,                            // missing content (incident shape)
		`{"path":"a.txt","content":""}`,               // explicit empty
		`{"path":"a.txt","content":null}`,             // null
		`{"path":"a.txt","content":" "}`,              // single space (allowed)
		`{"path":"a.txt","content":"hello"}`,          // happy path
		`{"path":"","content":"x"}`,                   // empty path
		``,                                            // empty input
		`{}`,                                          // empty object
		`[]`,                                          // wrong root type
		`{"path":"a.txt","content":123}`,              // wrong type for content
		`{"path":"a.txt","content":["a","b"]}`,        // array content
		`{"path":"a.txt","content":{"nested":"yes"}}`, // object content
		`{"path":null,"content":"x"}`,                 // null path
		`not json at all`,                             // total garbage
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		dir := t.TempDir()
		tracker := agent.NewReadTracker()
		ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)
		ctx = cwdctx.WithSessionCWD(ctx, dir)

		tool := &FileWriteTool{}
		// Recover any panic and surface as fail (testing framework would
		// already mark this as fatal, but explicit defer makes the failure
		// message specifically attribute the offending input).
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Run panicked on input %q: %v", raw, r)
			}
		}()

		result, err := tool.Run(ctx, raw)
		if err != nil {
			t.Fatalf("Run returned non-nil error on %q: %v", raw, err)
		}

		if !result.IsError {
			// On reported success, parse to learn what content was, then
			// verify the on-disk file matches.
			var parsed fileWriteArgs
			if jerr := json.Unmarshal([]byte(raw), &parsed); jerr != nil {
				t.Fatalf("declared success but input did not parse: %v (raw=%q result=%q)", jerr, raw, result.Content)
			}
			if parsed.Content == "" {
				t.Fatalf("success with empty content — guard should have rejected (raw=%q)", raw)
			}
			// file path resolution may have happened under cwdctx; recover
			// path from the success message: "wrote N bytes to <path>".
			// Skip strict path parsing — minimum invariant is that *some*
			// file in the sandbox dir has size == len(parsed.Content).
			matched := false
			_ = filepath.Walk(dir, func(p string, info os.FileInfo, _ error) error {
				if info != nil && !info.IsDir() && info.Size() == int64(len(parsed.Content)) {
					b, _ := os.ReadFile(p)
					if string(b) == parsed.Content {
						matched = true
					}
				}
				return nil
			})
			if !matched {
				t.Fatalf("success reported but no file in sandbox matches expected content (raw=%q result=%q)", raw, result.Content)
			}
		}
	})
}

// TestFileWrite_AllowsSingleSpaceContent verifies the documented "truncate"
// workaround in the error message: the model can pass content=" " (single
// space) to deliberately produce a near-empty file. Distinguishes "I forgot
// content" (rejected) from "I want a near-empty file" (allowed).
func TestFileWrite_AllowsSingleSpaceContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "near_empty.txt")

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	rawArgs := fmt.Sprintf(`{"path":%q,"content":" "}`, path)

	result, err := tool.Run(ctx, rawArgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success for single-space content, got: %s", result.Content)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(data) != " " {
		t.Errorf("expected single space, got: %q", data)
	}
}

func TestFileWrite_RejectsUnreadExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	os.WriteFile(path, []byte("original"), 0644)

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	args, _ := json.Marshal(fileWriteArgs{
		Path:    path,
		Content: "overwritten",
	})

	result, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result when overwriting unread file")
	}
	if !contains(result.Content, "file_read") {
		t.Errorf("error message should mention file_read, got: %s", result.Content)
	}

	// Verify the file was NOT modified
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Error("file should not have been modified")
	}
}

func TestFileWrite_AllowsNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	args, _ := json.Marshal(fileWriteArgs{
		Path:    path,
		Content: "new content",
	})

	result, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success for new file, got error: %s", result.Content)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "new content" {
		t.Errorf("expected 'new content', got: %s", string(data))
	}
}

func TestFileWrite_AllowsReadExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	os.WriteFile(path, []byte("original"), 0644)

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileWriteTool{}
	args, _ := json.Marshal(fileWriteArgs{
		Path:    path,
		Content: "overwritten",
	})

	result, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "overwritten" {
		t.Errorf("expected 'overwritten', got: %s", string(data))
	}
}
