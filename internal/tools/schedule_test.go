package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

// snapshotCtx builds a context with a fake conversation snapshot provider.
func snapshotCtx(msgs []client.Message) context.Context {
	return agent.WithConversationSnapshot(context.Background(), func() []client.Message {
		return msgs
	})
}

func TestExtractConversationContext_FiltersSystemAndEmpty(t *testing.T) {
	msgs := []client.Message{
		{Role: "system", Content: client.NewTextContent("you are helpful")},
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("")}, // empty — skip
		{Role: "assistant", Content: client.NewTextContent("hi there")},
	}
	got := extractConversationContext(snapshotCtx(msgs))
	if len(got) != 2 {
		t.Fatalf("got %d msgs, want 2: %+v", len(got), got)
	}
	if got[0].Role != "user" || got[0].Content != "hello" {
		t.Errorf("msg[0] = %+v", got[0])
	}
	if got[1].Role != "assistant" || got[1].Content != "hi there" {
		t.Errorf("msg[1] = %+v", got[1])
	}
}

func TestExtractConversationContext_NoSnapshotProvider(t *testing.T) {
	got := extractConversationContext(context.Background())
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestExtractConversationContext_EmptySnapshot(t *testing.T) {
	got := extractConversationContext(snapshotCtx(nil))
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestExtractConversationContext_Max20Messages(t *testing.T) {
	var msgs []client.Message
	for i := 0; i < 25; i++ {
		msgs = append(msgs, client.Message{
			Role:    "user",
			Content: client.NewTextContent(string(rune('a' + i%26))),
		})
	}
	got := extractConversationContext(snapshotCtx(msgs))
	if len(got) != 20 {
		t.Fatalf("got %d msgs, want 20", len(got))
	}
	// Must keep the most recent 20 (indices 5..24).
	if got[0].Content != string(rune('a'+5%26)) {
		t.Errorf("expected first kept msg to be index 5, got %q", got[0].Content)
	}
}

func TestExtractConversationContext_RuneCountedBudget(t *testing.T) {
	// Each Chinese char is 3 bytes, 1 rune. Budget is 8000 runes (not bytes).
	// Build two messages of 5000 runes each → 10000 runes total → must drop one.
	// Prior implementation counted bytes, so 5000 runes ≈ 15000 bytes would
	// overflow on the first message alone and (incorrectly) drop everything.
	const runesPerMsg = 5000
	cn := strings.Repeat("中", runesPerMsg)
	if utf8.RuneCountInString(cn) != runesPerMsg {
		t.Fatalf("setup: rune count = %d, want %d", utf8.RuneCountInString(cn), runesPerMsg)
	}
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent(cn)},
		{Role: "assistant", Content: client.NewTextContent(cn)},
	}
	got := extractConversationContext(snapshotCtx(msgs))
	if len(got) != 1 {
		t.Fatalf("got %d msgs, want 1 (8000-rune budget should drop exactly one)", len(got))
	}
	// The most recent one should survive — oldest is dropped first.
	if got[0].Role != "assistant" {
		t.Errorf("expected assistant msg to survive, got role=%q", got[0].Role)
	}
}

func TestExtractConversationContext_SkipsBlockMessagesWithoutText(t *testing.T) {
	// A message that is purely tool_use / tool_result blocks (no text block)
	// should be skipped, because we only want human-readable conversation.
	blockContent := client.NewBlockContent([]client.ContentBlock{
		{Type: "tool_use", ID: "tu1", Name: "some_tool"},
	})
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("real user message")},
		{Role: "assistant", Content: blockContent},
	}
	got := extractConversationContext(snapshotCtx(msgs))
	if len(got) != 1 {
		t.Fatalf("got %d msgs, want 1: %+v", len(got), got)
	}
	if got[0].Content != "real user message" {
		t.Errorf("msg[0] content = %q", got[0].Content)
	}
}

func TestExtractConversationContext_StripsToolResultPayloads(t *testing.T) {
	// Regression test: a user-role message whose content has BOTH a text
	// block (the human's reply) AND a tool_result block (e.g. a spill
	// preview containing "~/.shannon/tmp/tool_result_<id>.txt") must only
	// contribute the text block to the captured context. MessageContent.Text()
	// concatenates tool_result payloads (via ToolResultText on the
	// ToolContent field) too, so a naive Text() call would leak internal
	// spill paths into the persisted conversation context.
	blockContent := client.NewBlockContent([]client.ContentBlock{
		{Type: "text", Text: "here is my actual reply"},
		{
			Type:        "tool_result",
			ToolUseID:   "tu1",
			ToolContent: "INTERNAL SPILL PREVIEW: /Users/x/.shannon/tmp/tool_result_abc.txt",
		},
	})
	msgs := []client.Message{
		{Role: "user", Content: blockContent},
	}

	// Precondition: Content.Text() should actually include the tool_result
	// payload (that's the leak we're closing). If upstream semantics change
	// so Text() already excludes tool_result, this precondition fails and
	// the test becomes moot — update or delete it then.
	if !strings.Contains(blockContent.Text(), "SPILL") {
		t.Fatalf("precondition: MessageContent.Text() should include tool_result payload, got %q", blockContent.Text())
	}

	got := extractConversationContext(snapshotCtx(msgs))
	if len(got) != 1 {
		t.Fatalf("got %d msgs, want 1", len(got))
	}
	if got[0].Content != "here is my actual reply" {
		t.Errorf("msg content = %q — tool_result payload leaked into captured context", got[0].Content)
	}
	if strings.Contains(got[0].Content, "SPILL") || strings.Contains(got[0].Content, ".shannon/tmp") {
		t.Errorf("spill / internal path leaked into captured context: %q", got[0].Content)
	}
}

// setupShannonHomeWithAgent configures a fake ~/.shannon under HOME (via t.Setenv)
// and writes an agent named agentName with optional heartbeat.
func setupShannonHomeWithAgent(t *testing.T, agentName, heartbeatEvery string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	shan := filepath.Join(home, ".shannon")
	agentDir := filepath.Join(shan, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("# "+agentName+"\n"), 0o600); err != nil {
		t.Fatalf("write AGENT.md: %v", err)
	}
	if heartbeatEvery != "" {
		cfg := "heartbeat:\n  every: " + heartbeatEvery + "\n"
		if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatalf("write config.yaml: %v", err)
		}
	}
	return shan
}

func TestScheduleTool_CreateAppendsHeartbeatWarning(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "hb", "1h")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	res, err := tool.Run(context.Background(), `{"agent":"hb","cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError=true: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Schedule created:") {
		t.Errorf("missing success message: %q", res.Content)
	}
	if !strings.Contains(res.Content, "heartbeat") {
		t.Errorf("warning line missing, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "1h") {
		t.Errorf("warning missing interval, got: %q", res.Content)
	}
}

func TestScheduleTool_CreateNoWarningWithoutHeartbeat(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "plain", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	res, err := tool.Run(context.Background(), `{"agent":"plain","cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError=true: %s", res.Content)
	}
	if strings.Contains(res.Content, "heartbeat") {
		t.Errorf("unexpected heartbeat warning: %q", res.Content)
	}
}

func TestScheduleTool_UpdateAppendsHeartbeatWarning(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "hb", "30m")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))

	id, err := mgr.Create("hb", "*/5 * * * *", "initial", false)
	if err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	tool := &ScheduleTool{manager: mgr, action: "update"}

	res, err := tool.Run(context.Background(), `{"id":"`+id+`","prompt":"updated","description":"test"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError=true: %s", res.Content)
	}
	if !strings.Contains(res.Content, "updated.") {
		t.Errorf("missing success message: %q", res.Content)
	}
	if !strings.Contains(res.Content, "heartbeat") {
		t.Errorf("warning line missing, got: %q", res.Content)
	}
}

func TestExtractConversationContext_ConcatenatesMultipleTextBlocks(t *testing.T) {
	// If a message has multiple text blocks (unusual but valid), we keep
	// all of them joined — but still never include tool_result content.
	blockContent := client.NewBlockContent([]client.ContentBlock{
		{Type: "text", Text: "first part"},
		{Type: "tool_use", ID: "tu1", Name: "some_tool"},
		{Type: "text", Text: "second part"},
		{Type: "tool_result", ToolUseID: "tu1", ToolContent: "internal junk"},
	})
	msgs := []client.Message{
		{Role: "assistant", Content: blockContent},
	}
	got := extractConversationContext(snapshotCtx(msgs))
	if len(got) != 1 {
		t.Fatalf("got %d msgs, want 1", len(got))
	}
	if got[0].Content != "first part\nsecond part" {
		t.Errorf("msg content = %q, want %q", got[0].Content, "first part\nsecond part")
	}
}

// --- ctx agent-name fallback (stress tests) -------------------------------

// Case 1: LLM omits "agent" entirely → ctx-injected caller agent wins.
func TestScheduleTool_Create_InheritsAgentFromCtxWhenArgMissing(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "academic-writer", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	ctx := agent.WithAgentName(context.Background(), "academic-writer")
	res, err := tool.Run(ctx, `{"cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}

	list, _ := mgr.List()
	if len(list) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(list))
	}
	if list[0].Agent != "academic-writer" {
		t.Errorf("agent = %q, want %q (ctx fallback)", list[0].Agent, "academic-writer")
	}
}

func TestScheduleTool_Create_SnapshotsIMStatusContext(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "academic-writer", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	// Simulate an IM-originated run: the loop injected the inbound routing blob.
	blob := json.RawMessage(`{"platform":"slack","channel_id":"C1","message_ts":"123.45"}`)
	ctx := agent.WithIMStatusContext(agent.WithAgentName(context.Background(), "academic-writer"), blob)
	res, err := tool.Run(ctx, `{"cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}
	list, _ := mgr.List()
	if len(list) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(list))
	}
	// schedules.json is saved with MarshalIndent, which re-indents the opaque
	// blob; compare semantically (compacted) since Cloud parses it as JSON.
	var gotBuf, wantBuf bytes.Buffer
	if err := json.Compact(&gotBuf, list[0].IMStatusContext); err != nil {
		t.Fatalf("compact stored blob: %v", err)
	}
	if err := json.Compact(&wantBuf, blob); err != nil {
		t.Fatalf("compact want blob: %v", err)
	}
	if gotBuf.String() != wantBuf.String() {
		t.Errorf("IMStatusContext = %s, want %s (snapshot from ctx)", gotBuf.String(), wantBuf.String())
	}
}

func TestScheduleTool_Create_NoIMStatusContextWhenAbsent(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "academic-writer", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	ctx := agent.WithAgentName(context.Background(), "academic-writer") // non-IM run: no blob
	res, err := tool.Run(ctx, `{"cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}
	list, _ := mgr.List()
	if len(list) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(list))
	}
	if len(list[0].IMStatusContext) != 0 {
		t.Errorf("IMStatusContext = %q, want empty (no ctx blob → falls back to broadcast)", list[0].IMStatusContext)
	}
}

// Case 2: LLM explicit "agent": "" → respects intent, routes to default.
// This is the key "explicit empty vs missing" distinction.
func TestScheduleTool_Create_ExplicitEmptyAgentRoutesDefault(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "academic-writer", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	ctx := agent.WithAgentName(context.Background(), "academic-writer")
	res, err := tool.Run(ctx, `{"agent":"","cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}

	list, _ := mgr.List()
	if list[0].Agent != "" {
		t.Errorf("agent = %q, want empty (explicit empty = default)", list[0].Agent)
	}
}

// Case 3: LLM explicit different name → that name wins, ctx ignored.
func TestScheduleTool_Create_ExplicitAgentOverridesCtx(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "academic-writer", "")
	// also create the "explorer" agent dir so validation passes
	if err := os.MkdirAll(filepath.Join(shan, "agents", "explorer"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shan, "agents", "explorer", "AGENT.md"), []byte("explorer"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	ctx := agent.WithAgentName(context.Background(), "academic-writer")
	res, err := tool.Run(ctx, `{"agent":"explorer","cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}

	list, _ := mgr.List()
	if list[0].Agent != "explorer" {
		t.Errorf("agent = %q, want %q (explicit overrides ctx)", list[0].Agent, "explorer")
	}
}

// Case 4: Default-agent caller (ctx says ""). LLM omits arg → stays default.
// Don't accidentally promote default to anything else.
func TestScheduleTool_Create_DefaultCallerOmittedArgStaysDefault(t *testing.T) {
	shan := t.TempDir()
	t.Setenv("HOME", shan)
	mgr := schedule.NewManager(filepath.Join(shan, ".shannon", "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	// ctx says "default agent" (empty string, explicit injection)
	ctx := agent.WithAgentName(context.Background(), "")
	res, err := tool.Run(ctx, `{"cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}

	list, _ := mgr.List()
	if list[0].Agent != "" {
		t.Errorf("agent = %q, want empty (default caller stays default)", list[0].Agent)
	}
}

// Case 5: ctx has no injected agent name (e.g. tool invoked outside agent
// loop, like in tests or direct unit calls). Must not panic; falls back
// to default routing. Backward-compat with pre-fix call paths.
func TestScheduleTool_Create_NoCtxAgentSafelyDefaults(t *testing.T) {
	shan := t.TempDir()
	t.Setenv("HOME", shan)
	mgr := schedule.NewManager(filepath.Join(shan, ".shannon", "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	res, err := tool.Run(context.Background(), `{"cron":"*/5 * * * *","prompt":"check","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}

	list, _ := mgr.List()
	if list[0].Agent != "" {
		t.Errorf("agent = %q, want empty (no ctx → default)", list[0].Agent)
	}
}

// Case 6: stateful=true via tool args is honored (regression for the new
// schema arg we added).
func TestScheduleTool_Create_StatefulArgHonored(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "tracker", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	ctx := agent.WithAgentName(context.Background(), "tracker")
	res, err := tool.Run(ctx, `{"cron":"*/5 * * * *","prompt":"check","description":"test","stateful":true}`)
	if err != nil || res.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, res)
	}

	list, _ := mgr.List()
	if list[0].Stateful == nil || !*list[0].Stateful {
		t.Errorf("stateful=%v, want *true", list[0].Stateful)
	}
}

// --- Task 6: schedule_show tool --------------------------------------------

// Read-only contract: schedule_show is a query, never asks for approval,
// matches the schedule_list precedent. Concurrent-safety follows by
// inheritance from IsReadOnlyCall.
func TestScheduleTool_Show_NoApproval_ReadOnly(t *testing.T) {
	tool := &ScheduleTool{action: "show"}
	if tool.RequiresApproval() {
		t.Errorf("schedule_show must not require approval (read-only query)")
	}
	if !tool.IsReadOnlyCall("") {
		t.Errorf("schedule_show must be IsReadOnlyCall == true")
	}
}

func TestScheduleTool_Show_NeverRun(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "tracker", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	id, _ := mgr.Create("tracker", "0 9 * * *", "p", false)
	tool := &ScheduleTool{manager: mgr, action: "show", shannonDir: shan}

	res, err := tool.Run(context.Background(), `{"id":"`+id+`","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("show: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "has not run yet") {
		t.Errorf("never-run output should say 'has not run yet', got %q", res.Content)
	}
}

func TestScheduleTool_Show_RendersTurns(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "tracker", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	id, _ := mgr.Create("tracker", "0 9 * * *", "p", false)

	sessDir := filepath.Join(shan, "agents", "tracker", "sessions")
	os.MkdirAll(sessDir, 0700)
	os.WriteFile(filepath.Join(sessDir, "sess-1.json"), []byte(
		`{"id":"sess-1","schema_version":1,"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"hello from run"}]}`,
	), 0600)
	mgr.MarkLastRun(id, "sess-1", time.Now(), 0, 2)

	tool := &ScheduleTool{manager: mgr, action: "show", shannonDir: shan}
	res, err := tool.Run(context.Background(), `{"id":"`+id+`","description":"test"}`)
	if err != nil || res.IsError {
		t.Fatalf("show: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "hello from run") {
		t.Errorf("expected assistant text, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "sess-1") {
		t.Errorf("expected session id, got %q", res.Content)
	}
}

func TestScheduleTool_Show_UnknownID(t *testing.T) {
	shan := t.TempDir()
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "show", shannonDir: shan}

	res, _ := tool.Run(context.Background(), `{"id":"nope","description":"test"}`)
	if !res.IsError {
		t.Errorf("unknown id should set IsError, got %+v", res)
	}
}

func TestScheduleTool_Show_MissingSessionFile(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "tracker", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	id, _ := mgr.Create("tracker", "0 9 * * *", "p", false)
	mgr.MarkLastRun(id, "sess-vanished", time.Now(), 0, 4)

	tool := &ScheduleTool{manager: mgr, action: "show", shannonDir: shan}
	res, _ := tool.Run(context.Background(), `{"id":"`+id+`","description":"test"}`)
	if !res.IsError {
		t.Errorf("missing session should set IsError, got %+v", res)
	}
	if !strings.Contains(res.Content, "session") {
		t.Errorf("error should mention session, got %q", res.Content)
	}
}

func TestScheduleTool_Show_MaxTurnsArg(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "tracker", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	id, _ := mgr.Create("tracker", "0 9 * * *", "p", false)

	sessDir := filepath.Join(shan, "agents", "tracker", "sessions")
	os.MkdirAll(sessDir, 0700)
	msgs := `{"id":"sess","schema_version":1,"messages":[` +
		`{"role":"assistant","content":"turn 1"},` +
		`{"role":"assistant","content":"turn 2"},` +
		`{"role":"assistant","content":"turn 3"},` +
		`{"role":"assistant","content":"turn 4"},` +
		`{"role":"assistant","content":"turn 5"},` +
		`{"role":"assistant","content":"turn 6"}]}`
	os.WriteFile(filepath.Join(sessDir, "sess.json"), []byte(msgs), 0600)
	mgr.MarkLastRun(id, "sess", time.Now(), 0, 6)

	tool := &ScheduleTool{manager: mgr, action: "show", shannonDir: shan}
	res, _ := tool.Run(context.Background(), `{"id":"`+id+`","max_turns":2,"description":"test"}`)
	if strings.Contains(res.Content, "turn 4") {
		t.Errorf("max_turns=2 must NOT include turn 4: %q", res.Content)
	}
	if !strings.Contains(res.Content, "turn 5") || !strings.Contains(res.Content, "turn 6") {
		t.Errorf("max_turns=2 must include turns 5 and 6: %q", res.Content)
	}
}

// --- Task A6: broadcast enum + Source capture -----------------------------

// setupScheduleCreateTestEnv prepares a tool instance with a real Manager
// backed by a temp HOME, plus a ctx pre-populated with the originating
// source (mirroring how AgentLoop.Run wraps ctx for tools in production).
// Returns (ctx, tool, manager) so the test can inspect the saved schedule
// directly via Manager.List.
func setupScheduleCreateTestEnv(t *testing.T, source string) (context.Context, *ScheduleTool, *schedule.Manager) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	mgr := schedule.NewManager(filepath.Join(home, ".shannon", "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}
	ctx := agent.WithSource(context.Background(), source)
	return ctx, tool, mgr
}

func TestScheduleCreate_BroadcastParamMapping(t *testing.T) {
	cases := []struct {
		name          string
		broadcastArg  any
		wantBroadcast *bool
	}{
		{name: "absent_broadcast_yields_nil", broadcastArg: nil, wantBroadcast: nil},
		{name: "auto_yields_nil", broadcastArg: "auto", wantBroadcast: nil},
		{name: "on_yields_true", broadcastArg: "on", wantBroadcast: ptrBool(true)},
		{name: "off_yields_false", broadcastArg: "off", wantBroadcast: ptrBool(false)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tool, mgr := setupScheduleCreateTestEnv(t, "slack")
			args := map[string]any{
				"cron":        "* * * * *",
				"prompt":      "hi",
				"description": "test schedule",
			}
			if tc.broadcastArg != nil {
				args["broadcast"] = tc.broadcastArg
			}
			payload, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

			result, err := tool.Run(ctx, string(payload))
			if err != nil {
				t.Fatalf("Run returned go error: %v", err)
			}
			if result.IsError {
				t.Fatalf("Run returned tool error: %s", result.Content)
			}

			list, err := mgr.List()
			if err != nil {
				t.Fatalf("manager.List: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("want 1 schedule, got %d", len(list))
			}
			saved := list[0]
			if (saved.Broadcast == nil) != (tc.wantBroadcast == nil) {
				t.Errorf("Broadcast nil-ness: got %v want %v", saved.Broadcast, tc.wantBroadcast)
			}
			if saved.Broadcast != nil && tc.wantBroadcast != nil && *saved.Broadcast != *tc.wantBroadcast {
				t.Errorf("Broadcast value: got %v want %v", *saved.Broadcast, *tc.wantBroadcast)
			}
			if saved.CreatedFromSource != "slack" {
				t.Errorf("CreatedFromSource: got %q want %q", saved.CreatedFromSource, "slack")
			}
		})
	}
}

func TestScheduleCreate_InvalidBroadcastRejected(t *testing.T) {
	ctx, tool, mgr := setupScheduleCreateTestEnv(t, "slack")
	args := map[string]any{
		"cron":        "* * * * *",
		"prompt":      "hi",
		"description": "test",
		"broadcast":   "maybe", // invalid enum value
	}
	payload, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	result, err := tool.Run(ctx, string(payload))
	if err != nil {
		t.Fatalf("Run returned go error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error for invalid broadcast value, got success: %q", result.Content)
	}
	// The [validation error] prefix is load-bearing for loop detection — it
	// short-circuits same-tool+same-args 3-consecutive-error runs to
	// LoopForceStop. Returning a hand-rolled IsError result drops this signal.
	if !strings.Contains(result.Content, "[validation error]") {
		t.Errorf("expected [validation error] prefix in %q", result.Content)
	}
	// Defensive: no schedule should have been persisted.
	if list, _ := mgr.List(); len(list) != 0 {
		t.Errorf("expected no schedules saved after rejection, got %d", len(list))
	}
}

// Source capture is unconditional regardless of broadcast arg: even without
// a broadcast arg, an IM source captured at creation drives the smart-default
// downstream. This guards against a regression where the source-capture path
// is gated on broadcast being explicit.
func TestScheduleCreate_CapturesSourceWithoutBroadcastArg(t *testing.T) {
	ctx, tool, mgr := setupScheduleCreateTestEnv(t, "feishu")
	result, err := tool.Run(ctx, `{"cron":"* * * * *","prompt":"hi","description":"test"}`)
	if err != nil || result.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, result)
	}
	list, _ := mgr.List()
	if len(list) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(list))
	}
	if list[0].CreatedFromSource != "feishu" {
		t.Errorf("CreatedFromSource = %q, want %q", list[0].CreatedFromSource, "feishu")
	}
	if list[0].Broadcast != nil {
		t.Errorf("Broadcast = %v, want nil (smart default)", list[0].Broadcast)
	}
}

// Backward compat: a tool call from a path that never wraps ctx with
// WithSource (e.g. older callers, tests) must not crash — CreatedFromSource
// stays empty and the smart default downstream treats it as "unknown source".
func TestScheduleCreate_NoCtxSourceLeavesFieldEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mgr := schedule.NewManager(filepath.Join(home, ".shannon", "schedules.json"))
	tool := &ScheduleTool{manager: mgr, action: "create"}

	result, err := tool.Run(context.Background(), `{"cron":"* * * * *","prompt":"hi","description":"test"}`)
	if err != nil || result.IsError {
		t.Fatalf("run failed: err=%v res=%+v", err, result)
	}
	list, _ := mgr.List()
	if len(list) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(list))
	}
	if list[0].CreatedFromSource != "" {
		t.Errorf("CreatedFromSource = %q, want empty (no ctx source)", list[0].CreatedFromSource)
	}
}

func ptrBool(b bool) *bool { return &b }

// --- Task A7: schedule_update broadcast enum ------------------------------

// setupScheduleUpdateTestEnv builds a manager seeded with one existing
// schedule (so the update branch has something to mutate), plus a tool with
// action="update". Unlike create, update does NOT capture req.Source, so no
// agent.WithSource wrapping is needed — keep ctx plain to make that explicit.
func setupScheduleUpdateTestEnv(t *testing.T, seed schedule.Schedule) (context.Context, *ScheduleTool, *schedule.Manager) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	mgr := schedule.NewManager(filepath.Join(home, ".shannon", "schedules.json"))

	// Manager.Create allocates a fresh ID; we need to use the caller's seed.ID
	// so the test assertion can fetch it back deterministically. Seed via
	// CreateWithOpts (so Broadcast is preserved), then overwrite the ID in the
	// persisted JSON by re-creating the file directly.
	id, err := mgr.CreateWithOpts(seed.Agent, seed.Cron, seed.Prompt, false, schedule.CreateOpts{
		Broadcast:         seed.Broadcast,
		CreatedFromSource: seed.CreatedFromSource,
	})
	if err != nil {
		t.Fatalf("seed CreateWithOpts: %v", err)
	}
	// Rename the persisted schedule ID to match seed.ID (so the test can
	// reference it by a stable string). Simplest path: load → patch → save
	// via the public Get + a re-write to disk.
	loaded, err := mgr.Get(id)
	if err != nil || loaded == nil {
		t.Fatalf("seed Get: err=%v sched=%v", err, loaded)
	}
	loaded.ID = seed.ID
	// Rewrite the index file with the renamed schedule. We bypass Manager
	// here because Manager has no rename API, and rolling our own JSON write
	// is the minimum surface change for a test helper.
	data, err := json.MarshalIndent([]schedule.Schedule{*loaded}, "", "  ")
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".shannon", "schedules.json"), data, 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	tool := &ScheduleTool{manager: mgr, action: "update"}
	return context.Background(), tool, mgr
}

func TestScheduleUpdate_BroadcastEnumApplied(t *testing.T) {
	bTrue := true
	bFalse := false

	tests := []struct {
		name          string
		initial       *bool
		broadcastArg  string
		wantBroadcast *bool
	}{
		{name: "absent_does_not_change", initial: ptrBool(true), broadcastArg: "", wantBroadcast: ptrBool(true)},
		{name: "auto_clears_to_nil", initial: ptrBool(true), broadcastArg: "auto", wantBroadcast: nil},
		{name: "on_sets_true", initial: nil, broadcastArg: "on", wantBroadcast: &bTrue},
		{name: "off_sets_false", initial: &bTrue, broadcastArg: "off", wantBroadcast: &bFalse},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tool, mgr := setupScheduleUpdateTestEnv(t, schedule.Schedule{
				ID:        "s1",
				Cron:      "* * * * *",
				Prompt:    "hi",
				Broadcast: tc.initial,
			})

			args := map[string]any{
				"id":          "s1",
				"description": "update test",
			}
			if tc.broadcastArg != "" {
				args["broadcast"] = tc.broadcastArg
			}
			payload, _ := json.Marshal(args)

			result, err := tool.Run(ctx, string(payload))
			if err != nil {
				t.Fatalf("Run returned go error: %v", err)
			}
			if result.IsError {
				t.Fatalf("Run returned error: %s", result.Content)
			}

			updated, err := mgr.Get("s1")
			if err != nil || updated == nil {
				t.Fatalf("manager Get returned err=%v updated=%v", err, updated)
			}
			if (updated.Broadcast == nil) != (tc.wantBroadcast == nil) {
				t.Errorf("Broadcast nil-ness: got %v want %v", updated.Broadcast, tc.wantBroadcast)
			}
			if updated.Broadcast != nil && tc.wantBroadcast != nil && *updated.Broadcast != *tc.wantBroadcast {
				t.Errorf("Broadcast value: got %v want %v", *updated.Broadcast, *tc.wantBroadcast)
			}
		})
	}
}

func TestScheduleUpdate_InvalidBroadcastRejected(t *testing.T) {
	ctx, tool, _ := setupScheduleUpdateTestEnv(t, schedule.Schedule{ID: "s1", Cron: "* * * * *", Prompt: "hi"})

	args := map[string]any{
		"id":          "s1",
		"description": "update test",
		"broadcast":   "maybe",
	}
	payload, _ := json.Marshal(args)
	result, err := tool.Run(ctx, string(payload))
	if err != nil {
		t.Fatalf("Run returned go error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error for invalid broadcast value, got success")
	}
	if !strings.Contains(result.Content, "[validation error]") {
		t.Errorf("expected [validation error] prefix in %q", result.Content)
	}
	// Pin the error to the broadcast value (not the generic
	// "at least one of cron/prompt/..." gate, which also returns a
	// validation error and would mask a missing broadcast handler).
	if !strings.Contains(result.Content, "broadcast") {
		t.Errorf("expected error to mention 'broadcast', got %q", result.Content)
	}
}

// The behavior that motivated the entire spec: a named-agent session is
// shared between interactive chat AND scheduled runs. schedule_show MUST
// return only the run's slice, not the session's tail.
func TestScheduleTool_Show_RespectsMessageRange(t *testing.T) {
	shan := setupShannonHomeWithAgent(t, "tracker", "")
	mgr := schedule.NewManager(filepath.Join(shan, "schedules.json"))
	id, _ := mgr.Create("tracker", "0 9 * * *", "p", false)

	sessDir := filepath.Join(shan, "agents", "tracker", "sessions")
	os.MkdirAll(sessDir, 0700)
	msgs := `{"id":"sess-shared","schema_version":1,"messages":[` +
		`{"role":"user","content":"interactive q"},` +
		`{"role":"assistant","content":"INTERACTIVE_REPLY"},` +
		`{"role":"user","content":"scheduled prompt"},` +
		`{"role":"assistant","content":"SCHEDULED_REPLY"}]}`
	os.WriteFile(filepath.Join(sessDir, "sess-shared.json"), []byte(msgs), 0600)
	mgr.MarkLastRun(id, "sess-shared", time.Now(), 2, 4)

	tool := &ScheduleTool{manager: mgr, action: "show", shannonDir: shan}
	res, _ := tool.Run(context.Background(), `{"id":"`+id+`","description":"test"}`)
	if strings.Contains(res.Content, "INTERACTIVE_REPLY") {
		t.Errorf("interactive chat reply must NOT appear in show output: %q", res.Content)
	}
	if !strings.Contains(res.Content, "SCHEDULED_REPLY") {
		t.Errorf("scheduled reply must appear in show output: %q", res.Content)
	}
}

// TestParseStatefulArg locks that the stateful tool arg tolerates the LLM
// emitting the JSON boolean as a string ("true"/"false") instead of silently
// dropping it to false — the bug that made "stateful":"true" create a fresh
// (non-sticky) schedule despite an explicit continuity request.
func TestParseStatefulArg(t *testing.T) {
	cases := []struct {
		name             string
		args             map[string]any
		wantVal, wantSet bool
		wantErr          bool
	}{
		{"absent", map[string]any{}, false, false, false},
		{"nil", map[string]any{"stateful": nil}, false, false, false},
		{"bool true", map[string]any{"stateful": true}, true, true, false},
		{"bool false", map[string]any{"stateful": false}, false, true, false},
		{"string true", map[string]any{"stateful": "true"}, true, true, false},
		{"string false", map[string]any{"stateful": "false"}, false, true, false},
		{"string True", map[string]any{"stateful": "True"}, true, true, false},
		{"garbage string", map[string]any{"stateful": "yes"}, false, false, true},
		{"number", map[string]any{"stateful": float64(1)}, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, set, err := parseStatefulArg(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if val != tc.wantVal || set != tc.wantSet {
				t.Errorf("got (val=%v set=%v), want (val=%v set=%v)", val, set, tc.wantVal, tc.wantSet)
			}
		})
	}
}
