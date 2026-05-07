package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSpillToDisk_RejectsEmptyShannonDir is the regression for the bug
// where filepath.Join("", "tmp") = "tmp" — a relative path — caused spill
// files to land in the process cwd (e.g. internal/agent/tmp/ during unit
// tests). The guard now refuses to write when shannonDir is empty.
func TestSpillToDisk_RejectsEmptyShannonDir(t *testing.T) {
	_, err := spillToDisk("", "sess1", "call1", "content")
	if err == nil {
		t.Fatal("expected spillToDisk to reject empty shannonDir")
	}
	if !strings.Contains(err.Error(), "shannonDir") {
		t.Errorf("expected error to mention shannonDir, got: %v", err)
	}
}

func TestSpillToDisk_SmallResult(t *testing.T) {
	// Results under threshold should not be spilled (caller checks threshold).
	// This test verifies spillToDisk works even for small content.
	dir := t.TempDir()
	content := "small output"
	preview, err := spillToDisk(dir, "sess1", "call1", content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview, "Output saved to disk") {
		t.Fatal("expected spill preview header")
	}
	if !strings.Contains(preview, "small output") {
		t.Fatal("expected full content in preview for small output")
	}
}

func TestSpillToDisk_LargeResult(t *testing.T) {
	dir := t.TempDir()
	// Build a 60K rune string.
	content := strings.Repeat("x", 60000)
	preview, err := spillToDisk(dir, "sess1", "call2", content)
	if err != nil {
		t.Fatal(err)
	}

	// Preview should contain the file path and char count.
	if !strings.Contains(preview, "60000 chars") {
		t.Fatalf("expected char count in preview, got: %s", preview[:200])
	}

	// Preview should be truncated to ~2000 chars of content + header.
	previewContent := preview[strings.Index(preview, "Preview (first 2000 chars):\n")+len("Preview (first 2000 chars):\n"):]
	if len([]rune(previewContent)) != spillPreviewChars {
		t.Fatalf("expected preview content of %d runes, got %d", spillPreviewChars, len([]rune(previewContent)))
	}

	// Full content should be on disk.
	path := filepath.Join(dir, "tmp", "tool_result_sess1_call2.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file not readable: %v", err)
	}
	if len(data) != 60000 {
		t.Fatalf("expected 60000 bytes on disk, got %d", len(data))
	}
}

func TestSpillToDisk_RuneSafe(t *testing.T) {
	dir := t.TempDir()
	// Multi-byte chars: each is 3 bytes in UTF-8.
	content := strings.Repeat("あ", 60000)
	preview, err := spillToDisk(dir, "sess1", "call3", content)
	if err != nil {
		t.Fatal(err)
	}
	// Preview content should be exactly 2000 runes, not bytes.
	idx := strings.Index(preview, "Preview (first 2000 chars):\n")
	previewContent := preview[idx+len("Preview (first 2000 chars):\n"):]
	if len([]rune(previewContent)) != spillPreviewChars {
		t.Fatalf("expected %d runes in preview, got %d", spillPreviewChars, len([]rune(previewContent)))
	}
}

func TestCleanupSpills(t *testing.T) {
	dir := t.TempDir()
	// Create spill files for two sessions.
	spillToDisk(dir, "sess1", "a", "data-a")
	spillToDisk(dir, "sess1", "b", "data-b")
	spillToDisk(dir, "sess2", "c", "data-c")

	// Cleanup sess1 only.
	cleanupSpills(dir, "sess1")

	// sess1 files should be gone.
	matches, _ := filepath.Glob(filepath.Join(dir, "tmp", "tool_result_sess1_*.txt"))
	if len(matches) != 0 {
		t.Fatalf("expected 0 sess1 files after cleanup, got %d", len(matches))
	}

	// sess2 file should remain.
	matches, _ = filepath.Glob(filepath.Join(dir, "tmp", "tool_result_sess2_*.txt"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 sess2 file, got %d", len(matches))
	}
}

// TestSpillFiles_SurviveBetweenRuns verifies that spill files created in one
// run are still readable before cleanup (session close). This is the regression
// test for the bug where SpillCleanupFunc was deferred per-Run, deleting files
// that subsequent turns could still reference.
func TestSpillFiles_SurviveBetweenRuns(t *testing.T) {
	dir := t.TempDir()

	// Turn 1: spill a large result
	_, err := spillToDisk(dir, "sess1", "call1", strings.Repeat("x", 60000))
	if err != nil {
		t.Fatal(err)
	}

	// Between turns: file must still exist
	path1 := filepath.Join(dir, "tmp", "tool_result_sess1_call1.txt")
	if _, err := os.Stat(path1); err != nil {
		t.Fatalf("spill file should survive between turns: %v", err)
	}

	// Turn 2: spill another result
	_, err = spillToDisk(dir, "sess1", "call2", strings.Repeat("y", 60000))
	if err != nil {
		t.Fatal(err)
	}

	// Both files should exist (cleanup hasn't run yet)
	path2 := filepath.Join(dir, "tmp", "tool_result_sess1_call2.txt")
	for _, p := range []string{path1, path2} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("spill file should exist before session close: %v", err)
		}
	}

	// Session close: cleanup
	cleanupSpills(dir, "sess1")

	// Now both should be gone
	for _, p := range []string{path1, path2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("spill file should be gone after session close: %s", p)
		}
	}
}

func TestSpillThresholdIntegration(t *testing.T) {
	// Verify that content under spillThreshold would not trigger spill
	// (the check is in loop.go, but we verify the constant here).
	under := strings.Repeat("x", spillThreshold)
	if len([]rune(under)) > spillThreshold {
		t.Fatal("threshold constant mismatch")
	}
	over := strings.Repeat("x", spillThreshold+1)
	if len([]rune(over)) <= spillThreshold {
		t.Fatal("threshold constant mismatch")
	}
}

// TestApplyAggregateCap_NoOpUnderCap: when total content is under the cap,
// applyAggregateCap leaves execResults untouched.
func TestApplyAggregateCap_NoOpUnderCap(t *testing.T) {
	dir := t.TempDir()
	results := []toolExecResult{
		{result: ToolResult{Content: strings.Repeat("a", 30_000)}},
		{result: ToolResult{Content: strings.Repeat("b", 30_000)}},
		{result: ToolResult{Content: strings.Repeat("c", 30_000)}},
	}
	// total = 90K < 200K cap
	applyAggregateCap(results, dir, "test-sess")
	for i, er := range results {
		if len(er.result.Content) != 30_000 {
			t.Errorf("result[%d] should be untouched (30K), got %d bytes", i, len(er.result.Content))
		}
	}
}

// TestApplyAggregateCap_NoOpForSmallBatch: even if total > cap, single-element
// batches are no-ops (the per-result spillThreshold path handles them).
func TestApplyAggregateCap_NoOpForSmallBatch(t *testing.T) {
	dir := t.TempDir()
	results := []toolExecResult{
		{result: ToolResult{Content: strings.Repeat("a", 300_000)}},
	}
	applyAggregateCap(results, dir, "test-sess")
	if len(results[0].result.Content) != 300_000 {
		t.Errorf("single-element batch should be no-op, got %d bytes", len(results[0].result.Content))
	}
}

// TestApplyAggregateCap_TenBy30K: 10×30K = 300K, none individually past
// per-result spillThreshold. Aggregate cap must spill enough of the
// largest results to bring total below 200K.
func TestApplyAggregateCap_TenBy30K(t *testing.T) {
	dir := t.TempDir()
	results := make([]toolExecResult, 10)
	for i := range results {
		results[i] = toolExecResult{result: ToolResult{Content: strings.Repeat("x", 30_000)}}
	}
	applyAggregateCap(results, dir, "test-sess-tenby30k")
	total := 0
	spilled := 0
	for _, er := range results {
		total += len(er.result.Content)
		if strings.HasPrefix(er.result.Content, "[Output saved to disk:") {
			spilled++
		}
	}
	if total > aggregateCapThreshold {
		t.Errorf("total after cap (%d) still exceeds threshold (%d)", total, aggregateCapThreshold)
	}
	if spilled == 0 {
		t.Error("expected at least one spill, got none")
	}
	t.Logf("after cap: total=%d, spilled=%d / 10", total, spilled)
}

// TestApplyAggregateCap_MixedSizes: largest results get spilled first; small
// results below minAggregateSpillSize are protected from spill (the preview
// header would defeat the savings).
func TestApplyAggregateCap_MixedSizes(t *testing.T) {
	dir := t.TempDir()
	results := []toolExecResult{
		{result: ToolResult{Content: strings.Repeat("a", 80_000)}}, // largest
		{result: ToolResult{Content: strings.Repeat("b", 80_000)}},
		{result: ToolResult{Content: strings.Repeat("c", 80_000)}},
		{result: ToolResult{Content: strings.Repeat("d", 1_000)}}, // small, must not spill
	}
	// total = 241K > 200K
	applyAggregateCap(results, dir, "test-sess-mixed")
	// First three are eligible (>= minAggregateSpillSize), should be spilled in
	// some order until total < 200K. The 1K one must remain literal.
	if len(results[3].result.Content) != 1_000 {
		t.Errorf("small result should not be spilled, got %d bytes (content: %.50s)", len(results[3].result.Content), results[3].result.Content)
	}
	total := 0
	for _, er := range results {
		total += len(er.result.Content)
	}
	if total > aggregateCapThreshold {
		t.Errorf("total after cap (%d) still exceeds threshold (%d)", total, aggregateCapThreshold)
	}
}

// TestApplyAggregateCap_MultibyteUsesRunes: pick CJK content sized so the
// rune total is BELOW the 200K cap but the byte total is ABOVE it. With
// the byte-counting bug the cap fires and a result gets spilled; with the
// rune fix it must NOT fire. Asserts no spill happened.
func TestApplyAggregateCap_MultibyteUsesRunes(t *testing.T) {
	dir := t.TempDir()
	// "漢" = 3 bytes in UTF-8. 20K runes per result × 4 = 80K runes (< 200K cap),
	// but 60K bytes per result × 4 = 240K bytes (> 200K cap). The byte-counting
	// bug would spill at least one result; the rune fix leaves all four intact.
	cjk := strings.Repeat("漢", 20_000)
	results := []toolExecResult{
		{result: ToolResult{Content: cjk}},
		{result: ToolResult{Content: cjk}},
		{result: ToolResult{Content: cjk}},
		{result: ToolResult{Content: cjk}},
	}
	applyAggregateCap(results, dir, "test-sess-cjk-under-cap")
	for i, er := range results {
		if strings.HasPrefix(er.result.Content, "[Output saved to disk:") {
			t.Errorf("result %d was spilled despite rune total (%d) < cap (%d) — byte-counting bug regressed",
				i, utf8.RuneCountInString(cjk)*len(results), aggregateCapThreshold)
		}
		if utf8.RuneCountInString(er.result.Content) != 20_000 {
			t.Errorf("result %d content was modified; len(runes)=%d, want 20000",
				i, utf8.RuneCountInString(er.result.Content))
		}
	}
}

// TestApplyAggregateCap_CooperatesWithPerResultSpill: a result that already
// exceeds the per-result spillThreshold (50K) is naturally the "largest"
// candidate and gets spilled first. This documents the intended interaction
// — per-result spill (in loop.go) and aggregate cap don't conflict.
func TestApplyAggregateCap_CooperatesWithPerResultSpill(t *testing.T) {
	dir := t.TempDir()
	results := []toolExecResult{
		{result: ToolResult{Content: strings.Repeat("a", 100_000)}}, // > 50K per-result threshold
		{result: ToolResult{Content: strings.Repeat("b", 60_000)}},
		{result: ToolResult{Content: strings.Repeat("c", 60_000)}},
	}
	// total = 220K > 200K. Aggregate cap kicks in (per-result spill happens
	// elsewhere in loop.go; this test only exercises the aggregate path).
	applyAggregateCap(results, dir, "test-sess-coop")
	total := 0
	for _, er := range results {
		total += len(er.result.Content)
	}
	if total > aggregateCapThreshold {
		t.Errorf("total after cap (%d) still exceeds threshold (%d)", total, aggregateCapThreshold)
	}
	// The 100K one should be the first picked.
	if !strings.HasPrefix(results[0].result.Content, "[Output saved to disk:") {
		t.Errorf("largest (100K) result should be spilled first, but content[0] is: %.80s", results[0].result.Content)
	}
}

func TestApplyPerResultSpill_UsesPerToolPolicy(t *testing.T) {
	dir := t.TempDir()
	policy := map[string]int{
		"grep": 20_000,
	}

	content := strings.Repeat("g", 20_001)
	got := applyPerResultSpill(content, "grep", dir, "test-sess-grep", policy)
	if !strings.HasPrefix(got, "[Output saved to disk:") {
		t.Fatalf("grep result above 20K policy should spill, got prefix: %.80s", got)
	}
	if strings.Contains(got, strings.Repeat("g", 20_001)) {
		t.Fatal("spilled preview should not keep the entire original result in context")
	}
}

func TestContextResultMaxChars_UsesPerToolPolicy(t *testing.T) {
	policy := map[string]int{
		"grep":      20_000,
		"file_read": UnlimitedToolResultSizeChars,
	}

	if got := contextResultMaxChars("grep", false, 30_000, policy); got != 20_000 {
		t.Fatalf("grep should use 20K context limit, got %d", got)
	}
	if got := contextResultMaxChars("file_read", false, 30_000, policy); got != 30_000 {
		t.Fatalf("unlimited tool should preserve loop default limit, got %d", got)
	}
	if got := contextResultMaxChars("grep", true, 30_000, policy); got != 60_000 {
		t.Fatalf("cloud result should preserve cloud context limit, got %d", got)
	}
}
