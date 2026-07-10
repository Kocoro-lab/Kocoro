package context

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type fakeCompleter struct {
	out string
	got client.CompletionRequest
}

func (f *fakeCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	f.got = req
	return &client.CompletionResponse{OutputText: f.out}, nil
}

func TestGenerateTitle(t *testing.T) {
	fc := &fakeCompleter{out: "  \"创建定时任务\"  "}
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("帮我设置一个每分钟跑的任务")}}
	got, err := GenerateTitle(context.Background(), fc, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "创建定时任务" {
		t.Errorf("title=%q, want 创建定时任务", got)
	}
	if fc.got.ModelTier != "small" || fc.got.CacheSource != "helper" {
		t.Errorf("tier=%q source=%q, want small/helper", fc.got.ModelTier, fc.got.CacheSource)
	}
}

func TestGenerateTitleErrors(t *testing.T) {
	// (a) empty transcript → error, title "". A lone system-role message is
	// skipped by buildTranscript, so the transcript is empty.
	emptyFC := &fakeCompleter{out: "ignored"}
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("you are a bot")}}
	got, err := GenerateTitle(context.Background(), emptyFC, msgs)
	if err == nil {
		t.Errorf("empty transcript: err=nil, want error")
	}
	if got != "" {
		t.Errorf("empty transcript: title=%q, want \"\"", got)
	}

	// (b) sanitize rejects model output → error, title "". "truncated" is a
	// rejected marker, so sanitizeTitle returns "".
	rejectFC := &fakeCompleter{out: "truncated"}
	got, err = GenerateTitle(context.Background(), rejectFC, []client.Message{
		{Role: "user", Content: client.NewTextContent("帮我设置一个每分钟跑的任务")},
	})
	if err == nil {
		t.Errorf("rejected output: err=nil, want error")
	}
	if got != "" {
		t.Errorf("rejected output: title=%q, want \"\"", got)
	}
}

func TestSanitizeTitle(t *testing.T) {
	cases := map[string]string{
		"  Fix login bug  ":          "Fix login bug",
		"\"Quoted\"":                 "Quoted",
		"[Incomplete response: ...]": "",
		strings.Repeat("x", 80):      strings.Repeat("x", 57) + "...",
	}
	for in, want := range cases {
		if got := sanitizeTitle(in); got != want {
			t.Errorf("sanitizeTitle(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestSanitizeTitleCJKTruncation(t *testing.T) {
	// 67 CJK runes ≈ 201 bytes — over the byte-based garbage gate (200) but a
	// legitimate over-long title, not garbage. It must be truncated to
	// maxTitleRunes with a trailing "...", NOT dropped to "".
	in := strings.Repeat("测", 67)
	if len(in) <= 200 || utf8.RuneCountInString(in) > 200 {
		t.Fatalf("test setup: input should be >200 bytes and <=200 runes (runes=%d bytes=%d)",
			utf8.RuneCountInString(in), len(in))
	}
	got := sanitizeTitle(in)
	if got == "" {
		t.Fatalf("over-long CJK title dropped to \"\"; want truncation")
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("got %q, want trailing \"...\"", got)
	}
	if n := utf8.RuneCountInString(got); n != maxTitleRunes {
		t.Errorf("rune length = %d, want %d", n, maxTitleRunes)
	}
}

func TestBuildTitleTranscriptTailCap(t *testing.T) {
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent(strings.Repeat("a", 2000))}}
	if got := buildTitleTranscript(msgs); len([]rune(got)) > maxTitleInputRunes {
		t.Errorf("len=%d, want <= %d", len([]rune(got)), maxTitleInputRunes)
	}
}

func TestBuildTitleTranscriptExcludesToolNoise(t *testing.T) {
	// A tool-heavy first turn: a long tool_use input + a long tool_result would
	// blow past the tail cap and evict the opening question if serialized. The
	// title transcript must keep ONLY the user's question + the final reply.
	question := "How do I set up a cron job?"
	bigToolInput := json.RawMessage(`{"command":"` + strings.Repeat("noise ", 400) + `"}`)
	bigToolResult := strings.Repeat("tool output line\n", 400)

	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent(question)},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "t1", Name: "bash", Input: bigToolInput},
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_result", ToolUseID: "t1", ToolContent: bigToolResult},
		})},
		{Role: "assistant", Content: client.NewTextContent("Use the schedule_create tool.")},
	}

	got := buildTitleTranscript(msgs)
	if !strings.Contains(got, question) {
		t.Errorf("title transcript dropped the user's question:\n%q", got)
	}
	if !strings.Contains(got, "schedule_create") {
		t.Errorf("title transcript dropped the final assistant reply:\n%q", got)
	}
	if strings.Contains(got, "noise") || strings.Contains(got, "tool output line") {
		t.Errorf("title transcript leaked tool noise:\n%q", got)
	}
}

// TestBuildTitleTranscriptKeepsImageCaption covers multimodal opening turns:
// an image attachment + a text caption arrives as a block-content user message
// (NOT a tool_result). The caption is the most title-worthy content and must
// survive into the title transcript — the old HasBlocks() skip dropped it.
func TestBuildTitleTranscriptKeepsImageCaption(t *testing.T) {
	caption := "why is this dashboard chart broken?"
	msgs := []client.Message{
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: caption},
			{Type: "image"},
		})},
		{Role: "assistant", Content: client.NewTextContent("The Y-axis scale is misconfigured.")},
	}
	got := buildTitleTranscript(msgs)
	if !strings.Contains(got, caption) {
		t.Errorf("title transcript dropped the image caption:\n%q", got)
	}
}

func TestSourceLabel(t *testing.T) {
	cases := map[string]string{
		"slack":    "Slack",
		"line":     "LINE",     // brand override (all-caps)
		"wecom":    "WeCom",    // brand override (camel-cased)
		"wechat":   "WeChat",   // brand override (camel-cased)
		"feishu":   "Feishu",   // upper-first is already correct
		"lark":     "Lark",     // upper-first is already correct
		"telegram": "Telegram", // upper-first is already correct
		"webhook":  "Webhook",  // upper-first is already correct
		"LINE":     "LINE",     // case-insensitive input
		// Interactive (non-IM) sources → "".
		"":         "",
		"desktop":  "",
		"shanclaw": "",
		"kocoro":   "",
		// Autonomous local sources that piggyback on the user's interactive
		// session must NOT prefix its title (e.g. no "Watcher · ...").
		"watcher":   "",
		"heartbeat": "",
		"mcp":       "",
	}
	for in, want := range cases {
		if got := SourceLabel(in); got != want {
			t.Errorf("SourceLabel(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestSourceLabelKoeHidden(t *testing.T) {
	for _, src := range []string{"koe", "koe-reachy", " KOE "} {
		if got := SourceLabel(src); got != "" {
			t.Errorf("SourceLabel(%q) = %q, want \"\" (internal name must not surface in titles)", src, got)
		}
	}
	// Regression guard: real channels keep their brand label.
	if got := SourceLabel("slack"); got == "" {
		t.Errorf("SourceLabel(\"slack\") = \"\", want a non-empty brand label")
	}
}

func TestDecorateTitle(t *testing.T) {
	cases := []struct{ src, sender, channel, smart, want string }{
		{"slack", "", "", "创建定时任务", "Slack · 创建定时任务"},
		{"line", "", "", "Daily standup", "LINE · Daily standup"},
		{"wecom", "", "", "Daily standup", "WeCom · Daily standup"},
		// Sender preserved through the upgrade (shared-channel distinction).
		{"slack", "Wayland", "", "My smart title", "Slack · Wayland · My smart title"},
		// No sender → channel fallback mirrors routeTitle ("Slack · #general").
		{"slack", "", "#general", "My smart title", "Slack · #general · My smart title"},
		// Sender wins over channel when both are present.
		{"slack", "Wayland", "#general", "My smart title", "Slack · Wayland · My smart title"},
		// Channel equal to the source is dropped (avoid "Slack · slack").
		{"slack", "", "slack", "T", "Slack · T"},
		{"slack", "", "", "T", "Slack · T"},
		// Interactive sources drop label, sender, and channel.
		{"desktop", "Wayland", "#general", "Daily standup", "Daily standup"},
		{"", "Wayland", "", "Daily standup", "Daily standup"},
		{"kocoro", "", "", "Daily standup", "Daily standup"},
	}
	for _, c := range cases {
		if got := DecorateTitle(c.src, c.sender, c.channel, c.smart); got != c.want {
			t.Errorf("DecorateTitle(%q,%q,%q,%q)=%q, want %q", c.src, c.sender, c.channel, c.smart, got, c.want)
		}
	}
}

type fakePatcher struct {
	wroteTitle string
	atTurns    int
	skip       bool
}

func (p *fakePatcher) PatchAutoTitle(id, title string, atTurns int) (bool, error) {
	p.wroteTitle = title
	p.atTurns = atTurns
	return !p.skip, nil
}

func TestUpgradeTitle(t *testing.T) {
	fc := &fakeCompleter{out: "创建定时任务"}
	fp := &fakePatcher{}
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("帮我设置任务")}}
	got := UpgradeTitle(context.Background(), fc, fp, "s1", "slack", "Wayland", "", msgs, 3)
	if got != "Slack · Wayland · 创建定时任务" {
		t.Errorf("returned %q, want Slack · Wayland · 创建定时任务", got)
	}
	if fp.wroteTitle != "Slack · Wayland · 创建定时任务" || fp.atTurns != 3 {
		t.Errorf("persisted title=%q turns=%d", fp.wroteTitle, fp.atTurns)
	}
}

func TestCountCompletedTurns(t *testing.T) {
	toolUse := client.NewBlockContent([]client.ContentBlock{{Type: "tool_use", ID: "t1", Name: "bash"}})
	toolResult := client.NewBlockContent([]client.ContentBlock{{Type: "tool_result", ToolUseID: "t1"}})

	// A single 1-tool turn: user, assistant(tool_use), user(tool_result), assistant(final text).
	oneTool := []client.Message{
		{Role: "user", Content: client.NewTextContent("do X")},
		{Role: "assistant", Content: toolUse},
		{Role: "user", Content: toolResult},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}
	if got := CountCompletedTurns(oneTool); got != 1 {
		t.Errorf("1-tool turn: got %d, want 1", got)
	}

	// No-tool turn.
	noTool := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewTextContent("hello")},
	}
	if got := CountCompletedTurns(noTool); got != 1 {
		t.Errorf("no-tool turn: got %d, want 1", got)
	}

	// Two turns, second uses two tools — still 2 completed turns.
	twoTurns := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewTextContent("hello")},
		{Role: "user", Content: client.NewTextContent("more")},
		{Role: "assistant", Content: toolUse},
		{Role: "user", Content: toolResult},
		{Role: "assistant", Content: toolUse},
		{Role: "user", Content: toolResult},
		{Role: "assistant", Content: client.NewTextContent("final")},
	}
	if got := CountCompletedTurns(twoTurns); got != 2 {
		t.Errorf("two turns (2nd multi-tool): got %d, want 2", got)
	}

	// Production shape: the final assistant reply arrives as a block-content
	// message (NewBlockContent), NOT NewTextContent. NewTextContent's Blocks()
	// is nil, so the tool_use scan short-circuits trivially; the block shape is
	// what actually walks hasToolUseBlock. Verify a block-content final reply
	// (no tool_use) still counts as one completed turn.
	blockReply := client.NewBlockContent([]client.ContentBlock{{Type: "text", Text: "final answer"}})
	blockFinal := []client.Message{
		{Role: "user", Content: client.NewTextContent("do X")},
		{Role: "assistant", Content: toolUse},
		{Role: "user", Content: toolResult},
		{Role: "assistant", Content: blockReply},
	}
	if got := CountCompletedTurns(blockFinal); got != 1 {
		t.Errorf("block-content final reply: got %d, want 1", got)
	}

	// A thinking+text final reply (interleaved thinking) has blocks but no
	// tool_use — still a completed turn.
	thinkingReply := client.NewBlockContent([]client.ContentBlock{
		{Type: "thinking", Text: "let me reason"},
		{Type: "text", Text: "the answer"},
	})
	thinkingFinal := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: thinkingReply},
	}
	if got := CountCompletedTurns(thinkingFinal); got != 1 {
		t.Errorf("thinking+text final reply: got %d, want 1", got)
	}
}

func TestUpgradeTitleSkipped(t *testing.T) {
	fc := &fakeCompleter{out: "创建定时任务"}
	fp := &fakePatcher{skip: true}
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("帮我设置任务")}}
	if got := UpgradeTitle(context.Background(), fc, fp, "s1", "slack", "", "", msgs, 3); got != "" {
		t.Errorf("returned %q, want \"\" when patcher skips (locked/straggler)", got)
	}
}
