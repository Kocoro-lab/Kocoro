package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// mockFnCompleter implements ctxwin.Completer via a callback function.
type mockFnCompleter struct {
	fn func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error)
}

func (m *mockFnCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	return m.fn(ctx, req)
}

func TestParseDiscoveryOutput(t *testing.T) {
	loaded := []*skills.Skill{
		{Name: "kocoro", Description: "platform management"},
		{Name: "pdf-reader", Description: "analyze PDFs"},
		{Name: "mcp-builder", Description: "build MCP servers"},
	}

	tests := []struct {
		name     string
		output   string
		expected []string
	}{
		{"single match", "kocoro", []string{"kocoro"}},
		{"multiple matches", "kocoro\npdf-reader", []string{"kocoro", "pdf-reader"}},
		{"none", "none", nil},
		{"unknown ignored", "kocoro\nunknown-skill\npdf-reader", []string{"kocoro", "pdf-reader"}},
		{"blank lines ignored", "\nkocoro\n\n", []string{"kocoro"}},
		{"all unknown", "foo\nbar", nil},
		{"empty output", "", nil},
		{"with whitespace", "  kocoro  \n  pdf-reader  ", []string{"kocoro", "pdf-reader"}},
		{"duplicates deduped", "kocoro\nkocoro", []string{"kocoro"}},
		{"NONE case insensitive", "None", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDiscoveryOutput(tt.output, loaded)
			if len(got) != len(tt.expected) {
				t.Errorf("got %d results, want %d", len(got), len(tt.expected))
				return
			}
			for i := range got {
				if got[i].Name != tt.expected[i] {
					t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, tt.expected[i])
				}
			}
		})
	}
}

func TestFormatDiscoveryHint(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if hint := formatDiscoveryHint(nil); hint != "" {
			t.Error("nil skills should produce empty hint")
		}
	})

	t.Run("single skill", func(t *testing.T) {
		matched := []*skills.Skill{
			{Name: "kocoro", Description: "Set up agents, skills, MCP servers"},
		}
		hint := formatDiscoveryHint(matched)
		if !strings.Contains(hint, "<system-reminder>") {
			t.Error("hint should contain system-reminder tags")
		}
		if !strings.Contains(hint, "kocoro") {
			t.Error("hint should contain matched skill name")
		}
		if !strings.Contains(hint, "use_skill") {
			t.Error("hint should mention use_skill")
		}
	})

	t.Run("long description truncated", func(t *testing.T) {
		long := strings.Repeat("a", 200)
		matched := []*skills.Skill{
			{Name: "test", Description: long},
		}
		hint := formatDiscoveryHint(matched)
		if strings.Contains(hint, long) {
			t.Error("long description should be truncated")
		}
		if !strings.Contains(hint, "...") {
			t.Error("truncated description should end with ...")
		}
	})
}

func TestDiscoverRelevantSkills_EmptyInputs(t *testing.T) {
	mock := &mockFnCompleter{
		fn: func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
			t.Fatal("should not call LLM with empty inputs")
			return nil, nil
		},
	}

	loaded := []*skills.Skill{{Name: "kocoro", Description: "test"}}

	// Empty user text
	result, _ := discoverRelevantSkills(context.Background(), mock, "", loaded)
	if len(result) != 0 {
		t.Error("empty user text should return nil")
	}

	// Empty skills
	result, _ = discoverRelevantSkills(context.Background(), mock, "hello", nil)
	if len(result) != 0 {
		t.Error("empty skills should return nil")
	}
}

func TestDiscoverRelevantSkills_Timeout(t *testing.T) {
	mock := &mockFnCompleter{
		fn: func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
			select {
			case <-time.After(10 * time.Second):
				return &client.CompletionResponse{OutputText: "kocoro"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	// Description is intentionally non-policy ("test") so the pre-filter
	// does NOT short-circuit; this keeps the test focused on timeout.
	loaded := []*skills.Skill{{Name: "kocoro", Description: "test"}}
	start := time.Now()
	result, _ := discoverRelevantSkills(context.Background(), mock, "create agent", loaded)
	elapsed := time.Since(start)

	if len(result) != 0 {
		t.Error("expected no results on timeout")
	}
	if elapsed > 7*time.Second {
		t.Errorf("should timeout in ~5s, took %v", elapsed)
	}
}

func TestIsPolicySkill(t *testing.T) {
	tests := []struct {
		name  string
		skill *skills.Skill
		want  bool
	}{
		{
			name:  "platform management",
			skill: &skills.Skill{Name: "kocoro", Description: "Configure the Kocoro platform: agents, skills, schedules, permissions, daemon"},
			want:  true,
		},
		{
			name:  "just agents",
			skill: &skills.Skill{Name: "agents", Description: "Create and manage agent definitions"},
			want:  true,
		},
		{
			name:  "unrelated skill",
			skill: &skills.Skill{Name: "pdf", Description: "Read and annotate PDF documents"},
			want:  false,
		},
		{
			name:  "metadata only match",
			skill: &skills.Skill{Name: "meta", Description: "generic helper", Metadata: map[string]any{"kind": "daemon-tooling"}},
			want:  true,
		},
		{
			name:  "empty skill",
			skill: &skills.Skill{Name: "empty"},
			want:  false,
		},
		{
			name:  "nil",
			skill: nil,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPolicySkill(tt.skill); got != tt.want {
				t.Errorf("isPolicySkill(%v) = %v, want %v", tt.skill, got, tt.want)
			}
		})
	}
}

func TestMatchPlatformIntent(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		// Positive: core platform intents
		{"EN create agent", "please create an agent that summarises my inbox", true},
		{"EN set up schedule", "help me set up a schedule for daily reports", true},
		{"EN schedule a", "schedule a task every morning", true},
		{"EN configure daemon", "I want to configure daemon settings", true},
		{"EN mixed case", "CREATE AN AGENT for testing", true},
		{"ZH 设置一个agent", "设置一个agent帮我每天总结邮件", true},
		{"ZH 创建agent", "我想创建一个agent", true},
		{"ZH 定时", "每天定时运行一下", true},
		{"ZH 配置", "帮我配置一下daemon", true},
		{"ZH 权限", "加个权限让它能跑bash", true},
		// Negative: non-platform intents
		{"unrelated EN", "write a Python script to sort a list", false},
		{"unrelated ZH", "帮我写一段Python排序代码", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := matchPlatformIntent(tt.text)
			if got != tt.want {
				t.Errorf("matchPlatformIntent(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestScorePlatformIntent_CalendarFalsePositives verifies the veto system
// rejects calendar/social intents that a pure-phrase matcher would have
// wrongly flagged as platform.
func TestScorePlatformIntent_CalendarFalsePositives(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool // true = fires (platform intent); false = vetoed
	}{
		// Calendar intents — should NOT fire
		{"schedule a meeting", "can you schedule a meeting tomorrow at 3pm", false},
		{"schedule doctor appt", "help me schedule a doctor's appointment for Friday", false},
		{"gym schedule (no verb)", "I need a better gym schedule for my workouts", false},
		{"team standup calendar", "what's on the calendar for our team standup?", false},
		{"dentist reminder", "add a reminder for my dentist appointment", false},
		// Ambiguous but strong-pair: platform signal wins (agent+create beats veto)
		{"create agent for standup", "create an agent for my daily standup updates", true},
		{"create agent for meeting notes", "create an agent that summarises my meeting notes", true},
		// Anchor-immune cases: even with veto terms, anchor fires
		{"kocoro + meeting veto", "set up a kocoro agent for my meeting scheduler", true},
		{"daemon + calendar", "the daemon crashed during my calendar sync", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fired := matchPlatformIntent(tt.text)
			if fired != tt.want {
				score, bd := scorePlatformIntent(tt.text)
				t.Errorf("matchPlatformIntent(%q) fired=%v, want=%v (score=%d anchors=%v nouns=%v verbs=%v vetoes=%v)",
					tt.text, fired, tt.want, score, bd.Anchors, bd.Nouns, bd.Verbs, bd.Vetoes)
			}
		})
	}
}

// TestScorePlatformIntent_WordBoundary verifies EN word-boundary matching so
// "agenda" doesn't wrongly trigger on "agent", etc.
func TestScorePlatformIntent_WordBoundary(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"agenda not agent", "what's on today's agenda", false},
		{"agented not agent", "i agented the process", false},
		{"agent proper word", "please create an agent", true},
		{"scheduling (substring)", "our scheduling system is broken", false},
		{"schedule proper", "please create a schedule", true},
		{"skilled not skill", "she's very skilled at python", false},
		{"skill proper", "add a skill for pdf handling", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fired := matchPlatformIntent(tt.text)
			if fired != tt.want {
				score, bd := scorePlatformIntent(tt.text)
				t.Errorf("fired=%v, want=%v (score=%d %+v)", fired, tt.want, score, bd)
			}
		})
	}
}

// TestScorePlatformIntent_Japanese verifies JA coverage for nouns, verbs,
// and calendar veto.
func TestScorePlatformIntent_Japanese(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		// JA platform intents
		{"JA create agent", "エージェントを作成したい", true},
		{"JA configure schedule", "スケジュールを設定してください", true},
		{"JA add permission", "パーミッションを追加", true},
		{"JA manage skill", "スキルを管理する", true},
		{"JA periodic scheduled exec", "定期実行を設定", true},
		{"JA katakana daemon", "デーモンを再起動", true},
		// JA calendar/social — should NOT fire
		{"JA meeting", "明日ミーティングの予約をしてください", false},
		{"JA discussion", "チームで打ち合わせをする", false},
		{"JA appointment", "アポを入れて", false},
		{"JA calendar check", "カレンダーを確認", false},
		{"JA gym date", "ジムに行ってからデート", false},
		// JA anchor-wins-over-veto
		{"JA kocoro + meeting", "kocoroでミーティング用のエージェントを作成", true},
		// Mixed JA/EN (common)
		{"mixed JA/EN create agent", "agentを作成してください", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fired := matchPlatformIntent(tt.text)
			if fired != tt.want {
				score, bd := scorePlatformIntent(tt.text)
				t.Errorf("matchPlatformIntent(%q) fired=%v, want=%v (score=%d anchors=%v nouns=%v verbs=%v vetoes=%v)",
					tt.text, fired, tt.want, score, bd.Anchors, bd.Nouns, bd.Verbs, bd.Vetoes)
			}
		})
	}
}

// TestScorePlatformIntent_Breakdown verifies the score breakdown is correctly
// recorded (needed for SHANNON_SKILL_DEBUG=1 debug output tuning).
func TestScorePlatformIntent_Breakdown(t *testing.T) {
	score, bd := scorePlatformIntent("please create an agent and configure a schedule")
	if score < scoreThreshold {
		t.Errorf("expected score >= threshold, got %d", score)
	}
	if !stringSliceContains(bd.Nouns, "agent") {
		t.Errorf("expected 'agent' in nouns, got %v", bd.Nouns)
	}
	if !stringSliceContains(bd.Nouns, "schedule") {
		t.Errorf("expected 'schedule' in nouns, got %v", bd.Nouns)
	}
	if !stringSliceContains(bd.Verbs, "create") {
		t.Errorf("expected 'create' in verbs, got %v", bd.Verbs)
	}
	if !stringSliceContains(bd.Verbs, "configure") {
		t.Errorf("expected 'configure' in verbs, got %v", bd.Verbs)
	}
	if len(bd.Vetoes) != 0 {
		t.Errorf("expected no vetoes, got %v", bd.Vetoes)
	}
}

// TestPolicySkillPreFilter_ObserveOnly verifies SHANNON_SKILL_DISCOVERY_OBSERVE
// suppresses actual pre-filter firing (safety rollback).
// Uses t.Setenv to avoid package-level mutation, keeping the test safe under
// -race and future parallel runs.
func TestPolicySkillPreFilter_ObserveOnly(t *testing.T) {
	t.Setenv("SHANNON_SKILL_DISCOVERY_OBSERVE", "1")

	policy := &skills.Skill{
		Name:        "kocoro",
		Description: "Manage agents, skills, schedules, permissions and daemon configuration",
	}
	loaded := []*skills.Skill{policy}
	matched, fired := policySkillPreFilter("create an agent for summarizing my inbox", loaded)
	if fired || len(matched) != 0 {
		t.Errorf("observe-only mode should suppress pre-filter fire, got fired=%v matched=%v",
			fired, skillNames(matched))
	}
}

// TestSkillDiscoveryPreFilter exercises the full pre-filter path end-to-end:
// when the small-model returns nothing, the pre-filter alone must still
// surface the policy skill so the hint is injected.
func TestSkillDiscoveryPreFilter(t *testing.T) {
	policy := &skills.Skill{
		Name:        "kocoro",
		Description: "Manage agents, skills, schedules, permissions and daemon configuration",
	}
	unrelated := &skills.Skill{
		Name:        "pdf",
		Description: "Read PDF documents",
	}

	// Small-model returns "none" so any match MUST come from the pre-filter.
	noneCompleter := &mockFnCompleter{
		fn: func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
			return &client.CompletionResponse{OutputText: "none"}, nil
		},
	}

	t.Run("zh creates agent → kocoro surfaced", func(t *testing.T) {
		loaded := []*skills.Skill{policy, unrelated}
		matched, _ := discoverRelevantSkills(context.Background(), noneCompleter, "我想创建一个agent", loaded)
		if len(matched) != 1 || matched[0].Name != "kocoro" {
			t.Fatalf("expected [kocoro], got %v", skillNames(matched))
		}
	})

	t.Run("en set up daily schedule → kocoro surfaced", func(t *testing.T) {
		loaded := []*skills.Skill{policy, unrelated}
		matched, _ := discoverRelevantSkills(context.Background(), noneCompleter, "please set up a daily schedule", loaded)
		if len(matched) != 1 || matched[0].Name != "kocoro" {
			t.Fatalf("expected [kocoro], got %v", skillNames(matched))
		}
	})

	t.Run("non-platform intent → no hint", func(t *testing.T) {
		loaded := []*skills.Skill{policy, unrelated}
		matched, _ := discoverRelevantSkills(context.Background(), noneCompleter, "write a Python script to sort a list", loaded)
		if len(matched) != 0 {
			t.Fatalf("expected no match, got %v", skillNames(matched))
		}
	})

	t.Run("no policy skill loaded → graceful no-op", func(t *testing.T) {
		loaded := []*skills.Skill{unrelated}
		matched, _ := discoverRelevantSkills(context.Background(), noneCompleter, "我想创建一个agent", loaded)
		if len(matched) != 0 {
			t.Fatalf("expected no match when no policy skill loaded, got %v", skillNames(matched))
		}
	})

	t.Run("pre-filter survives small-model timeout", func(t *testing.T) {
		slow := &mockFnCompleter{
			fn: func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
				select {
				case <-time.After(10 * time.Second):
					return &client.CompletionResponse{OutputText: "none"}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}
		loaded := []*skills.Skill{policy, unrelated}
		start := time.Now()
		matched, _ := discoverRelevantSkills(context.Background(), slow, "create an agent for daily standup", loaded)
		elapsed := time.Since(start)
		if len(matched) != 1 || matched[0].Name != "kocoro" {
			t.Fatalf("expected [kocoro] from pre-filter even on timeout, got %v", skillNames(matched))
		}
		if elapsed > 7*time.Second {
			t.Errorf("pre-filter should not block on slow model beyond the 5s timeout, took %v", elapsed)
		}
	})

	t.Run("pre-filter merges with small-model result", func(t *testing.T) {
		bothCompleter := &mockFnCompleter{
			fn: func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
				return &client.CompletionResponse{OutputText: "pdf"}, nil
			},
		}
		loaded := []*skills.Skill{policy, unrelated}
		matched, _ := discoverRelevantSkills(context.Background(), bothCompleter, "我想创建一个agent", loaded)
		if len(matched) != 2 {
			t.Fatalf("expected 2 skills, got %v", skillNames(matched))
		}
		// Pre-filter result should come first so it appears prominently in the hint.
		if matched[0].Name != "kocoro" {
			t.Errorf("expected kocoro first, got %q", matched[0].Name)
		}
	})
}

func skillNames(ss []*skills.Skill) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

// Helper-tier callers must tag CacheSource="helper". See cache-action-plan §1.1.
func TestDiscoverRelevantSkills_TagsHelperCacheSource(t *testing.T) {
	loaded := []*skills.Skill{
		{Name: "alpha", Description: "alpha skill"},
		{Name: "beta", Description: "beta skill"},
	}
	var captured client.CompletionRequest
	mock := &mockFnCompleter{
		fn: func(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
			captured = req
			return &client.CompletionResponse{OutputText: "none"}, nil
		},
	}
	_, _ = discoverRelevantSkills(context.Background(), mock, "do something with alpha", loaded)
	if captured.CacheSource != "helper" {
		t.Errorf("discoverRelevantSkills CacheSource = %q, want %q", captured.CacheSource, "helper")
	}
}
