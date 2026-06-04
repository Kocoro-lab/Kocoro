package context

import (
	"context"
	"strings"
	"testing"

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

func TestBuildTitleTranscriptTailCap(t *testing.T) {
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent(strings.Repeat("a", 2000))}}
	if got := buildTitleTranscript(msgs); len([]rune(got)) > maxTitleInputRunes {
		t.Errorf("len=%d, want <= %d", len([]rune(got)), maxTitleInputRunes)
	}
}

func TestDecorateTitle(t *testing.T) {
	cases := []struct{ src, smart, want string }{
		{"slack", "创建定时任务", "Slack · 创建定时任务"},
		{"line", "Daily standup", "Line · Daily standup"},
		{"desktop", "Daily standup", "Daily standup"},
		{"", "Daily standup", "Daily standup"},
		{"kocoro", "Daily standup", "Daily standup"},
	}
	for _, c := range cases {
		if got := DecorateTitle(c.src, c.smart); got != c.want {
			t.Errorf("DecorateTitle(%q,%q)=%q, want %q", c.src, c.smart, got, c.want)
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
	got := UpgradeTitle(context.Background(), fc, fp, "s1", "slack", msgs, 3)
	if got != "Slack · 创建定时任务" {
		t.Errorf("returned %q, want Slack · 创建定时任务", got)
	}
	if fp.wroteTitle != "Slack · 创建定时任务" || fp.atTurns != 3 {
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
}

func TestUpgradeTitleSkipped(t *testing.T) {
	fc := &fakeCompleter{out: "创建定时任务"}
	fp := &fakePatcher{skip: true}
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("帮我设置任务")}}
	if got := UpgradeTitle(context.Background(), fc, fp, "s1", "slack", msgs, 3); got != "" {
		t.Errorf("returned %q, want \"\" when patcher skips (locked/straggler)", got)
	}
}
