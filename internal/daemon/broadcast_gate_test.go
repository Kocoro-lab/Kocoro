package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

func TestShouldBroadcast(t *testing.T) {
	bTrue := true
	bFalse := false

	tests := []struct {
		name     string
		schedule schedule.Schedule
		want     bool
	}{
		// Smart default, IM source → broadcast
		{name: "smart_default_slack", schedule: schedule.Schedule{CreatedFromSource: "slack"}, want: true},
		{name: "smart_default_lark", schedule: schedule.Schedule{CreatedFromSource: "lark"}, want: true},
		{name: "smart_default_feishu", schedule: schedule.Schedule{CreatedFromSource: "feishu"}, want: true},
		{name: "smart_default_telegram", schedule: schedule.Schedule{CreatedFromSource: "telegram"}, want: true},
		{name: "smart_default_wecom", schedule: schedule.Schedule{CreatedFromSource: "wecom"}, want: true},
		{name: "smart_default_line", schedule: schedule.Schedule{CreatedFromSource: "line"}, want: true},
		{name: "smart_default_webhook", schedule: schedule.Schedule{CreatedFromSource: "webhook"}, want: true},

		// Smart default, non-IM source → silent
		{name: "smart_default_webview", schedule: schedule.Schedule{CreatedFromSource: "webview"}, want: false},
		{name: "smart_default_tui", schedule: schedule.Schedule{CreatedFromSource: "tui"}, want: false},
		{name: "smart_default_cli", schedule: schedule.Schedule{CreatedFromSource: "cli"}, want: false},
		{name: "smart_default_one_shot", schedule: schedule.Schedule{CreatedFromSource: "one-shot"}, want: false},
		{name: "smart_default_research", schedule: schedule.Schedule{CreatedFromSource: "research"}, want: false},

		// Pre-feature schedule (no CreatedFromSource) → silent (safe)
		{name: "smart_default_empty_source", schedule: schedule.Schedule{}, want: false},

		// Explicit override true → broadcast regardless of source
		{name: "explicit_true_overrides_webview", schedule: schedule.Schedule{Broadcast: &bTrue, CreatedFromSource: "webview"}, want: true},
		{name: "explicit_true_overrides_empty_source", schedule: schedule.Schedule{Broadcast: &bTrue}, want: true},

		// Explicit override false → silent regardless of source
		{name: "explicit_false_overrides_slack", schedule: schedule.Schedule{Broadcast: &bFalse, CreatedFromSource: "slack"}, want: false},
		{name: "explicit_false_overrides_empty_source", schedule: schedule.Schedule{Broadcast: &bFalse}, want: false},

		// Agent identity does not affect the gate (uniform rule for default + named)
		{name: "named_agent_smart_default_slack", schedule: schedule.Schedule{Agent: "analyst", CreatedFromSource: "slack"}, want: true},
		{name: "named_agent_smart_default_webview", schedule: schedule.Schedule{Agent: "analyst", CreatedFromSource: "webview"}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldBroadcast(&tc.schedule); got != tc.want {
				t.Errorf("shouldBroadcast() = %v, want %v", got, tc.want)
			}
		})
	}
}

// shouldBroadcast delegates to isCloudSource (session_cwd.go) for the
// smart-default check; isCloudSource has its own positive/negative coverage
// elsewhere. The matrix above asserts the integration end-to-end so this
// file deliberately doesn't re-test the source enum.

func TestResolveThread(t *testing.T) {
	bTrue := true
	bFalse := false

	tests := []struct {
		name     string
		thread   *bool
		isSticky bool
		hasBlob  bool
		want     bool // dereferenced value; resolveThread always returns non-nil
	}{
		// auto (thread==nil): follow session state (isSticky && hasBlob).
		{name: "auto_sticky_blob_anchors", thread: nil, isSticky: true, hasBlob: true, want: true},
		{name: "auto_stateless_blob_top_level", thread: nil, isSticky: false, hasBlob: true, want: false},
		{name: "auto_sticky_noblob_top_level", thread: nil, isSticky: true, hasBlob: false, want: false},
		{name: "auto_stateless_noblob_top_level", thread: nil, isSticky: false, hasBlob: false, want: false},

		// on (thread==&true): always anchor, ignoring isSticky / hasBlob.
		{name: "on_ignores_stateless_noblob", thread: &bTrue, isSticky: false, hasBlob: false, want: true},
		{name: "on_ignores_sticky_blob", thread: &bTrue, isSticky: true, hasBlob: true, want: true},

		// off (thread==&false): always top-level, ignoring isSticky / hasBlob.
		{name: "off_ignores_sticky_blob", thread: &bFalse, isSticky: true, hasBlob: true, want: false},
		{name: "off_ignores_stateless_noblob", thread: &bFalse, isSticky: false, hasBlob: false, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveThread(tc.thread, tc.isSticky, tc.hasBlob)
			if got == nil {
				t.Fatalf("resolveThread returned nil; want non-nil *bool (%v)", tc.want)
			}
			if *got != tc.want {
				t.Errorf("resolveThread() = %v, want %v", *got, tc.want)
			}
		})
	}
}

func TestIsValidScheduleSource(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		// Empty is accepted (legacy schedules + CLI leave it unset).
		{"", true},
		// Cloud sources flow through isCloudSource.
		{"slack", true},
		{"line", true},
		{"feishu", true},
		{"lark", true},
		{"wecom", true},
		{"telegram", true},
		{"webhook", true},
		// Local origins the daemon recognizes.
		{"kocoro", true},
		{"webview", true},
		{"tui", true},
		{"cli", true},
		{"one-shot", true},
		// Case / whitespace normalization mirrors isCloudSource.
		{"  Slack ", true},
		{"WEBVIEW", true},
		// Free-form garbage a buggy client could POST → rejected.
		{"totally-made-up", false},
		{"slackk", false},
		{"../etc/passwd", false},
		{"discord", false},
	}
	for _, tc := range tests {
		t.Run(tc.source, func(t *testing.T) {
			if got := isValidScheduleSource(tc.source); got != tc.want {
				t.Errorf("isValidScheduleSource(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}
