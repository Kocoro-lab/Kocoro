package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

func TestForkSessionCopiesConversationWithoutRuntimeState(t *testing.T) {
	mgr := NewManager(t.TempDir())
	source := mgr.NewSessionWithID("source-session-1")
	source.Title = "Travel plan"
	source.TitleAuto = true
	source.CWD = "/tmp/project"
	source.ProjectID = "project-1"
	source.Source = "desktop"
	source.Messages = []client.Message{
		mkMsg("user", "first"),
		mkMsg("assistant", "answer one"),
		mkMsg("user", "second"),
		mkMsg("assistant", "answer two"),
	}
	now := time.Now()
	source.MessageMeta = []MessageMeta{
		{MessageID: "u1", Timestamp: &now},
		{MessageID: "a1", Timestamp: &now},
		{MessageID: "u2", Timestamp: &now},
		{MessageID: "a2", Timestamp: &now},
	}
	source.Usage = &UsageSummary{LLMCalls: 2}
	source.WorkPlan = &WorkPlanSnapshot{Lifecycle: WorkPlanActive}
	source.InProgress = true
	source.RouteKey = "session:source-session-1"
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	fork, err := mgr.ForkSession(source.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if fork.ID == source.ID {
		t.Fatal("fork reused source id")
	}
	if got := len(fork.Messages); got != 2 {
		t.Fatalf("fork message count = %d, want 2", got)
	}
	if fork.Messages[1].Content.Text() != "answer one" {
		t.Fatalf("fork reply = %q", fork.Messages[1].Content.Text())
	}
	if fork.Title != source.Title || fork.CWD != source.CWD || fork.ProjectID != source.ProjectID {
		t.Fatalf("fork lost conversation identity: %#v", fork)
	}
	if fork.Usage != nil || fork.WorkPlan != nil || fork.InProgress || fork.RouteKey != "" {
		t.Fatalf("fork inherited runtime state: %#v", fork)
	}
	loaded, err := mgr.Load(fork.ID)
	if err != nil {
		t.Fatalf("fork was not persisted: %v", err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("persisted fork message count = %d", len(loaded.Messages))
	}
}

func TestForkSessionIntoPersistsInTargetAgentDirectory(t *testing.T) {
	root := t.TempDir()
	sourceManager := NewManager(filepath.Join(root, "source"))
	targetManager := NewManager(filepath.Join(root, "target"))
	source := sourceManager.NewSessionWithID("source-session-cross-agent")
	source.Title = "Cross-agent context"
	source.Messages = []client.Message{
		mkMsg("user", "question"),
		mkMsg("assistant", "answer"),
	}
	if err := sourceManager.Save(); err != nil {
		t.Fatal(err)
	}

	fork, err := sourceManager.ForkSessionInto(source.ID, 2, targetManager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetManager.Load(fork.ID); err != nil {
		t.Fatalf("target manager could not load fork: %v", err)
	}
	if _, err := sourceManager.Load(fork.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fork leaked into source manager: %v", err)
	}
}

func TestCompleteTurnBoundaryRejectsToolTrajectory(t *testing.T) {
	toolUse := client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{{
		Type: "tool_use", ID: "call-1", Name: "read_file",
	}})}
	toolResult := client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{{
		Type: "tool_result", ToolUseID: "call-1", ToolContent: "ok",
	}})}
	messages := []client.Message{
		mkMsg("user", "inspect"),
		toolUse,
		toolResult,
		mkMsg("assistant", "done"),
	}
	for _, index := range []int{1, 2, 3} {
		if err := validateCompleteTurnBoundary(messages, index); !errors.Is(err, ErrIncompleteTurnBoundary) {
			t.Fatalf("boundary %d error = %v, want incomplete turn", index, err)
		}
	}
	if err := validateCompleteTurnBoundary(messages, 4); err != nil {
		t.Fatalf("final boundary rejected: %v", err)
	}
}

func TestCopyHistoryThroughDropsInjectedMessagesAndDoesNotAlias(t *testing.T) {
	mgr := NewManager(t.TempDir())
	source := mgr.NewSessionWithID("source-session-2")
	source.Messages = []client.Message{
		mkMsg("user", "hello"),
		mkMsg("assistant", "internal guard"),
		mkMsg("assistant", "answer"),
	}
	source.MessageMeta = []MessageMeta{
		{},
		{SystemInjected: true},
		{},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	history, err := mgr.CopyHistoryThrough(source.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Content.Text() != "answer" {
		t.Fatalf("copied history = %#v", history)
	}
	history[0] = mkMsg("user", "changed")
	reloaded, err := mgr.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Messages[0].Content.Text() != "hello" {
		t.Fatal("copied history aliases the persisted source")
	}
}

// compactedSourceSession builds a session whose first two turns are covered by
// a valid compaction checkpoint (marker included) and whose tail continues past
// it, mirroring what daemon auto-compaction persists.
func compactedSourceSession(mgr *Manager, id string) *Session {
	source := mgr.NewSessionWithID(id)
	source.Messages = []client.Message{
		mkMsg("user", "first question"),
		mkMsg("assistant", "first answer"),
		mkMsg("user", "second question"),
		mkMsg("assistant", "second answer"),
		mkMsg("user", "tail question"),
		mkMsg("assistant", "tail answer"),
	}
	source.MessageMeta = make([]MessageMeta, len(source.Messages))
	source.CompactionCheckpoint = &CompactionCheckpoint{
		SchemaVersion:       CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: 4,
		Messages: []client.Message{
			mkMsg("user", "first question"),
			mkMsg("user", ctxwin.CompactionSummaryPrefix+"compacted summary of the first turns"),
		},
	}
	return source
}

func TestForkSessionCarriesCoveringCompactionCheckpoint(t *testing.T) {
	mgr := NewManager(t.TempDir())
	source := compactedSourceSession(mgr, "compacted-fork-source")
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	fork, err := mgr.ForkSession(source.ID, 6)
	if err != nil {
		t.Fatal(err)
	}
	if fork.CompactionCheckpoint == nil {
		t.Fatal("fork dropped the covering compaction checkpoint")
	}
	history := fork.HistoryForLoop()
	if len(history) != 4 {
		t.Fatalf("fork history = %d messages, want checkpoint(2)+tail(2)", len(history))
	}
	if history[1].Content.Text() != ctxwin.CompactionSummaryPrefix+"compacted summary of the first turns" ||
		history[2].Content.Text() != "tail question" {
		t.Fatalf("fork history is not checkpoint+tail: %#v", history)
	}
	// Deep copy: mutating the fork's checkpoint must not reach the source.
	fork.CompactionCheckpoint.Messages[1] = mkMsg("user", "mutated")
	reloaded, err := mgr.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CompactionCheckpoint.Messages[1].Content.Text() == "mutated" {
		t.Fatal("fork checkpoint aliases the source checkpoint")
	}

	// A cut BEFORE the checkpoint's coverage cannot use it — the checkpoint
	// summarizes messages the fork does not contain.
	early, err := mgr.ForkSession(source.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if early.CompactionCheckpoint != nil {
		t.Fatal("fork kept a checkpoint covering messages beyond its cut")
	}
}

func TestCopyHistoryThroughUsesCoveringCompactionCheckpoint(t *testing.T) {
	mgr := NewManager(t.TempDir())
	source := compactedSourceSession(mgr, "compacted-copy-source")
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	history, err := mgr.CopyHistoryThrough(source.ID, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[1].Content.Text() != ctxwin.CompactionSummaryPrefix+"compacted summary of the first turns" {
		t.Fatalf("copy is not checkpoint+tail: %#v", history)
	}

	// A boundary inside the covered range falls back to the raw prefix.
	rawPrefix, err := mgr.CopyHistoryThrough(source.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawPrefix) != 2 || rawPrefix[1].Content.Text() != "first answer" {
		t.Fatalf("pre-coverage copy = %#v", rawPrefix)
	}
}

func TestForkSessionIntoConcurrentForksDoNotInterleave(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	source := mgr.NewSessionWithID("concurrent-fork-source")
	source.Messages = []client.Message{
		mkMsg("user", "question"),
		mkMsg("assistant", "answer"),
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	target := NewManager(filepath.Join(dir, "other-agent"))

	type outcome struct {
		fork *Session
		err  error
	}
	results := make(chan outcome, 2)
	go func() {
		fork, err := mgr.ForkSessionInto(source.ID, 2, mgr)
		results <- outcome{fork, err}
	}()
	go func() {
		fork, err := mgr.ForkSessionInto(source.ID, 2, target)
		results <- outcome{fork, err}
	}()
	seen := map[string]bool{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if seen[r.fork.ID] {
			t.Fatalf("concurrent forks shared id %q", r.fork.ID)
		}
		seen[r.fork.ID] = true
		if len(r.fork.Messages) != 2 || r.fork.Messages[1].Content.Text() != "answer" {
			t.Fatalf("concurrent fork corrupted messages: %#v", r.fork.Messages)
		}
	}
}
