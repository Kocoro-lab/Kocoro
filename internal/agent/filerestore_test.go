package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func recordRead(rt *ReadTracker, path string, offset, limit int) {
	ctx := context.WithValue(context.Background(), readTrackerKey{}, rt)
	RecordFileRead(ctx, path, offset, limit, time.Now(), 1)
}

func TestReadTracker_RecentReads(t *testing.T) {
	// The tracker stores symlink-resolved paths, so expectations must resolve
	// too: on macOS /tmp is a symlink to /private/tmp.
	dir, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatalf("resolve /tmp: %v", err)
	}
	wantA, wantB := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")

	rt := NewReadTracker()
	recordRead(rt, "/tmp/a.txt", 0, 0)
	time.Sleep(2 * time.Millisecond)
	recordRead(rt, "/tmp/b.txt", 0, 100)
	time.Sleep(2 * time.Millisecond)
	recordRead(rt, "/tmp/a.txt", 100, 100) // newer range of the same path wins

	reads := rt.RecentReads(10)
	if len(reads) != 2 {
		t.Fatalf("per-path dedup expected 2 entries, got %d: %+v", len(reads), reads)
	}
	if reads[0].Path != wantA || reads[0].Offset != 100 {
		t.Errorf("most recent read (a.txt offset=100) must come first, got %+v", reads[0])
	}
	if reads[1].Path != wantB {
		t.Errorf("second entry should be b.txt, got %+v", reads[1])
	}

	if got := rt.RecentReads(1); len(got) != 1 {
		t.Errorf("max must cap the result, got %d", len(got))
	}
}

func newRestoreTestLoop(rt *ReadTracker) *AgentLoop {
	gw := client.NewGatewayClient("http://127.0.0.1:0", "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(200_000)
	loop.SetReadTracker(rt)
	return loop
}

func smallShaped() []client.Message {
	return []client.Message{
		{Role: "system", Content: client.NewTextContent("sys")},
		{Role: "user", Content: client.NewTextContent("do the work")},
		{Role: "user", Content: client.NewTextContent("Previous context summary: earlier steps")},
	}
}

func TestBuildPostCompactionFileRestore_RestoresRecentFiles(t *testing.T) {
	dir := t.TempDir()
	decoy := filepath.Join(dir, "decoy.md")
	os.WriteFile(decoy, []byte("alpha\nRESTORE_MARKER_9f3c2a71d4b85e06\nomega\n"), 0o644)
	memory := filepath.Join(dir, "MEMORY.md")
	os.WriteFile(memory, []byte("memory body"), 0o644)
	gone := filepath.Join(dir, "deleted.txt")

	rt := NewReadTracker()
	recordRead(rt, decoy, 0, 0)
	recordRead(rt, memory, 0, 0)
	recordRead(rt, gone, 0, 0)

	loop := newRestoreTestLoop(rt)
	msg, ok := loop.buildPostCompactionFileRestore(smallShaped(), 0)
	if !ok {
		t.Fatal("restore message expected")
	}
	text := msg.Content.Text()
	if msg.Role != "user" || !strings.Contains(text, "RESTORE_MARKER_9f3c2a71d4b85e06") {
		t.Fatalf("restored content missing: %.300q", text)
	}
	if !strings.Contains(text, decoy) {
		t.Errorf("restored file path must be named: %.300q", text)
	}
	if strings.Contains(text, "memory body") {
		t.Errorf("MEMORY.md must be excluded (re-injected via system prompt): %.300q", text)
	}
	if strings.Contains(text, "deleted.txt") {
		t.Errorf("missing files must be skipped silently: %.300q", text)
	}
}

func TestBuildPostCompactionFileRestore_SkipsReadsKeptInTail(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.go")
	os.WriteFile(kept, []byte("package kept\n"), 0o644)

	rt := NewReadTracker()
	recordRead(rt, kept, 0, 0)

	shaped := smallShaped()
	input, _ := json.Marshal(map[string]any{"path": kept})
	shaped = append(shaped, client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
		{Type: "tool_use", ID: "toolu_k", Name: "file_read", Input: input},
	})})

	loop := newRestoreTestLoop(rt)
	if _, ok := loop.buildPostCompactionFileRestore(shaped, 0); ok {
		t.Fatal("a file whose file_read survives in the kept tail must not be re-injected")
	}
}

func TestBuildPostCompactionFileRestore_Budgets(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	os.WriteFile(big, []byte(strings.Repeat("line of filler text for the big file\n", 20000)), 0o644)

	rt := NewReadTracker()
	recordRead(rt, big, 0, 0)

	loop := newRestoreTestLoop(rt)
	msg, ok := loop.buildPostCompactionFileRestore(smallShaped(), 0)
	if !ok {
		t.Fatal("restore expected")
	}
	if got := len(msg.Content.Text()); got > restoreFileTokenCap*4+2000 {
		t.Errorf("per-file clip must bound the payload, got %d chars", got)
	}

	// No headroom under the trigger line → restoration must decline
	// entirely rather than re-arm the compaction it just paid for.
	tight := newRestoreTestLoop(rt)
	tight.SetContextWindowExplicit(20_000)
	// trigger 18_000 − overhead 17_500 ≈ 500 < floor
	if _, ok := tight.buildPostCompactionFileRestore(smallShaped(), 17_500); ok {
		t.Fatal("restore must be skipped when the trigger budget has no headroom")
	}
}

func TestBuildPostCompactionFileRestore_SkipsBinaryAndInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nfakebinary"), 0o644)
	pdf := filepath.Join(dir, "doc.pdf")
	os.WriteFile(pdf, []byte("%PDF-1.7 fake"), 0o644)
	garbled := filepath.Join(dir, "raw.dat")
	os.WriteFile(garbled, []byte{0xff, 0xfe, 0x01, 0x02, 0x80, 0x81}, 0o644)

	rt := NewReadTracker()
	recordRead(rt, png, 0, 0)
	recordRead(rt, pdf, 0, 0)
	recordRead(rt, garbled, 0, 0)

	loop := newRestoreTestLoop(rt)
	if msg, ok := loop.buildPostCompactionFileRestore(smallShaped(), 0); ok {
		t.Fatalf("binary / non-UTF-8 reads must not be re-injected as text: %.300q", msg.Content.Text())
	}
}

func TestBuildPostCompactionFileRestore_SkipsFilesInPriorRestoreBlock(t *testing.T) {
	dir := t.TempDir()
	already := filepath.Join(dir, "already.md")
	os.WriteFile(already, []byte("previously restored content\n"), 0o644)

	rt := NewReadTracker()
	recordRead(rt, already, 0, 0)

	loop := newRestoreTestLoop(rt)
	first, ok := loop.buildPostCompactionFileRestore(smallShaped(), 0)
	if !ok {
		t.Fatal("first restore expected")
	}
	// A second compaction in the same Run keeps the first restore block in
	// the tail — the same file must not be injected twice.
	shaped := append(smallShaped(), first)
	if msg, ok := loop.buildPostCompactionFileRestore(shaped, 0); ok {
		t.Fatalf("file already present in a prior restore block must be skipped: %.300q", msg.Content.Text())
	}
}

// A markdown heading INSIDE restored file content must not be misread as a
// restored-path header: a doc whose text contains a path-shaped heading
// ("## internal/agent/loop.go") would otherwise silently suppress that
// file's restoration at the next compaction in the same Run.
func TestBuildPostCompactionFileRestore_ContentHeadingsAreNotRestorePaths(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.md")
	os.WriteFile(other, []byte("real content of the other file\n"), 0o644)
	doc := filepath.Join(dir, "design.md")
	os.WriteFile(doc, []byte("# Design\n\n## "+other+"\nsection about that file\n"), 0o644)

	rt := NewReadTracker()
	recordRead(rt, doc, 0, 0)

	loop := newRestoreTestLoop(rt)
	first, ok := loop.buildPostCompactionFileRestore(smallShaped(), 0)
	if !ok {
		t.Fatal("first restore expected")
	}

	// Second compaction: the prior block (containing the doc's heading that
	// NAMES other.md) survives in the tail; other.md itself was read since
	// and must still be restorable.
	recordRead(rt, other, 0, 0)
	shaped := append(smallShaped(), first)
	msg, ok := loop.buildPostCompactionFileRestore(shaped, 0)
	if !ok || !strings.Contains(msg.Content.Text(), "real content of the other file") {
		t.Fatalf("a heading inside restored content must not suppress that file's restoration (ok=%v)", ok)
	}
}

func TestBuildPostCompactionFileRestore_MaxFilesCap(t *testing.T) {
	dir := t.TempDir()
	rt := NewReadTracker()
	for i := 0; i < restoreMaxFiles+2; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		os.WriteFile(p, []byte(fmt.Sprintf("content of file %d\n", i)), 0o644)
		recordRead(rt, p, 0, 0)
		time.Sleep(time.Millisecond)
	}

	loop := newRestoreTestLoop(rt)
	msg, ok := loop.buildPostCompactionFileRestore(smallShaped(), 0)
	if !ok {
		t.Fatal("restore expected")
	}
	if got := strings.Count(msg.Content.Text(), "## "); got != restoreMaxFiles {
		t.Fatalf("restore must cap at %d files, got %d sections", restoreMaxFiles, got)
	}
}

// TestAgentLoop_ReactiveRestoreRespectsEvidenceFloor pins the reactive-path
// policy: NO file restoration after a context-length 400. The provider just
// rejected this history, the evidence floor only proves a LOWER bound on the
// true overhead (so any budget computed against it can overshoot), and
// reactiveCompacted makes a second overflow terminal — the retry must keep
// every token shaping recovered.
func TestAgentLoop_ReactiveRestoreRespectsEvidenceFloor(t *testing.T) {
	dir := t.TempDir()
	decoy := filepath.Join(dir, "decoy.md")
	os.WriteFile(decoy, []byte("alpha\nRESTORE_MARKER_9f3c2a71d4b85e06\nomega\n"), 0o644)

	var mu sync.Mutex
	firstErrorMsgCount := 0
	restoredInRetry := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()
		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		if req.ModelTier == "small" {
			json.NewEncoder(w).Encode(nativeResponse(
				"## Current task & next steps\nsummary of prior steps", "end_turn", nil, 50, 30))
			return
		}

		mu.Lock()
		msgCount := len(req.Messages)
		if msgCount < 12 && firstErrorMsgCount == 0 {
			mu.Unlock()
			json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall("think", fmt.Sprintf(`{"thought":"step %d"}`, msgCount)),
				50, 20))
			return
		}
		if firstErrorMsgCount == 0 || msgCount >= firstErrorMsgCount {
			if firstErrorMsgCount == 0 {
				firstErrorMsgCount = msgCount
			}
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
			return
		}
		if strings.Contains(string(raw), "RESTORE_MARKER_9f3c2a71d4b85e06") {
			restoredInRetry = true
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(nativeResponse("done after shaped retry", "end_turn", nil, 100, 20))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	rt := NewReadTracker()
	recordRead(rt, decoy, 0, 0)

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(200_000)
	loop.SetMemoryDir(t.TempDir())
	loop.SetHandler(&mockHandler{approveResult: true})
	loop.SetReadTracker(rt)

	_, _, err := loop.Run(context.Background(), "run the steps", nil, nil)
	if err != nil {
		t.Errorf("Run should recover through the shaped retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if restoredInRetry {
		t.Error("post-400 retry must not carry restored file content — restoration is skipped on the reactive path")
	}
}

// TestAgentLoop_PostCompactionRestoresRecentReads is the loop-level wiring
// test: after a proactive compaction actually rewrites history, the next main
// request must carry the re-read content of recently-read files (the
// 2026-08-04 e2e lost decoy identifiers precisely because nothing restored
// the dropped file content).
func TestAgentLoop_PostCompactionRestoresRecentReads(t *testing.T) {
	dir := t.TempDir()
	decoy := filepath.Join(dir, "decoy.md")
	os.WriteFile(decoy, []byte("alpha\nRESTORE_MARKER_9f3c2a71d4b85e06\nomega\n"), 0o644)

	// Fat history: estimate ~12K tokens so the shaped tail frees real
	// headroom under the landing budget once the middle is dropped.
	history := make([]client.Message, 0, 60)
	for i := 0; i < 30; i++ {
		history = append(history,
			client.Message{Role: "user", Content: client.NewTextContent(fmt.Sprintf("step request %d %s", i, strings.Repeat("filler words to fatten the turn ", 20)))},
			client.Message{Role: "assistant", Content: client.NewTextContent(fmt.Sprintf("step reply %d %s", i, strings.Repeat("acknowledged and completed nicely ", 20)))},
		)
	}

	var mu sync.Mutex
	restoredInMain := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()
		var req struct {
			ModelTier string `json:"model_tier"`
		}
		json.Unmarshal(raw, &req)
		if req.ModelTier == "small" {
			json.NewEncoder(w).Encode(nativeResponse(
				"<summary>## Current task & next steps\ncontinue the steps</summary>", "end_turn", nil, 50, 30))
			return
		}
		mu.Lock()
		if strings.Contains(string(raw), "RESTORE_MARKER_9f3c2a71d4b85e06") {
			restoredInMain = true
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 100, 10))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	rt := NewReadTracker()
	recordRead(rt, decoy, 0, 0)

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(60_000)
	loop.SetMemoryDir(t.TempDir())
	loop.SetHandler(&mockHandler{approveResult: true})
	loop.SetReadTracker(rt)
	loop.SetSessionID("resumed-restore")
	// Restored calibration arms the i==0 trigger: estimate (~12K) + 43K
	// crosses the 54K trigger, while the shaped floor (~2K) + 43K stays
	// under the 48K landing with headroom for the restoration payload.
	fp := loop.ToolsFingerprint()
	loop.SetEstOverheadState(43_000, "test-model", fp)

	if _, _, err := loop.Run(context.Background(), "continue the work", nil, history); err != nil {
		t.Logf("Run error (tolerated): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !restoredInMain {
		t.Error("post-compaction main request must carry the restored file content")
	}
}
