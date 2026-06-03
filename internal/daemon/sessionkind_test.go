package daemon

import "testing"

func TestKindOf_ExclusionRule(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		// schedule — closed set, exact source match (case/space-insensitive)
		{"schedule", SessionKindSchedule},
		{"SCHEDULE", SessionKindSchedule},
		{"  schedule  ", SessionKindSchedule},

		// IM — closed set, all nine messaging platforms
		{"slack", SessionKindIM},
		{"feishu", SessionKindIM},
		{"lark", SessionKindIM},
		{"wecom", SessionKindIM},
		{"line", SessionKindIM},
		{"wechat", SessionKindIM},
		{"teams", SessionKindIM},
		{"discord", SessionKindIM},
		{"telegram", SessionKindIM},
		{"Slack", SessionKindIM}, // case-insensitive

		// interactive — the catch-all (open set). The first two are ~93% of
		// real sessions; a whitelist would misclassify them.
		{"", SessionKindInteractive},
		{"desktop", SessionKindInteractive},
		{"kocoro", SessionKindInteractive},
		{"tui", SessionKindInteractive},
		{"cli", SessionKindInteractive},
		{"one-shot", SessionKindInteractive},
		// bypass sources are not schedule/IM, so they classify interactive too
		{"web", SessionKindInteractive},
		{"webhook", SessionKindInteractive},
		{"cron", SessionKindInteractive},
		{"system", SessionKindInteractive},
		{"future-client", SessionKindInteractive},
	}
	for _, tt := range tests {
		if got := kindOf(tt.source); got != tt.want {
			t.Errorf("kindOf(%q) = %q, want %q", tt.source, got, tt.want)
		}
	}
}

func TestIsInteractiveSource(t *testing.T) {
	interactive := []string{"", "desktop", "kocoro", "tui", "cli", "web", "cron", "system"}
	for _, s := range interactive {
		if !isInteractiveSource(s) {
			t.Errorf("isInteractiveSource(%q) = false, want true", s)
		}
	}
	notInteractive := []string{"schedule", "slack", "feishu", "lark", "wecom", "line", "wechat", "teams", "discord", "telegram"}
	for _, s := range notInteractive {
		if isInteractiveSource(s) {
			t.Errorf("isInteractiveSource(%q) = true, want false", s)
		}
	}
}
