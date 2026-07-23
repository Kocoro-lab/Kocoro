package tools

import (
	"context"
	"testing"
)

func TestNotify_Info(t *testing.T) {
	tool := &NotifyTool{}
	info := tool.Info()
	if info.Name != "notify" {
		t.Errorf("expected name 'notify', got %q", info.Name)
	}
	if !containsString(info.Required, "title") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'title' and 'description', got %v", info.Required)
	}
}

func TestNotify_InvalidArgs(t *testing.T) {
	tool := &NotifyTool{}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
}

func TestNotify_BuildScript(t *testing.T) {
	tests := []struct {
		title string
		body  string
		sound bool
		want  string
	}{
		{
			title: "Test",
			body:  "Hello",
			sound: false,
			want:  `display notification "Hello" with title "Test"`,
		},
		{
			title: "Test",
			body:  "Hello",
			sound: true,
			want:  `display notification "Hello" with title "Test" sound name "default"`,
		},
		{
			title: "Test",
			body:  "",
			sound: false,
			want:  `display notification "" with title "Test"`,
		},
		{
			title: `Say "hi"`,
			body:  `It's "great"`,
			sound: false,
			want:  `display notification "It's \"great\"" with title "Say \"hi\""`,
		},
	}
	for _, tt := range tests {
		got := buildNotifyScript(tt.title, tt.body, tt.sound)
		if got != tt.want {
			t.Errorf("buildNotifyScript(%q, %q, %v) = %q, want %q", tt.title, tt.body, tt.sound, got, tt.want)
		}
	}
}

func TestNotify_RequiresApproval(t *testing.T) {
	tool := &NotifyTool{}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}

func TestNotify_DesktopHandler_Delivered(t *testing.T) {
	tool := &NotifyTool{}
	var (
		called   bool
		gotTitle string
		gotBody  string
		gotSound bool
	)
	handler := NotifyHandler(func(title, body string, sound bool) bool {
		called = true
		gotTitle, gotBody, gotSound = title, body, sound
		return true
	})
	ctx := WithNotifyHandler(context.Background(), handler)

	result, err := tool.Run(ctx, `{"title":"T","body":"B","sound":true,"description":"test delivery"}`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	if !called {
		t.Fatal("expected NotifyHandler to be called")
	}
	if gotTitle != "T" || gotBody != "B" || gotSound != true {
		t.Errorf("handler args = (%q,%q,%v), want (T,B,true)", gotTitle, gotBody, gotSound)
	}
	if result.Content != "notification sent" {
		t.Errorf("unexpected content: %q", result.Content)
	}
}

func TestNotify_DesktopHandler_BodyFromMessageAlias(t *testing.T) {
	tool := &NotifyTool{}
	var gotBody string
	handler := NotifyHandler(func(title, body string, sound bool) bool {
		gotBody = body
		return true
	})
	ctx := WithNotifyHandler(context.Background(), handler)

	if _, err := tool.Run(ctx, `{"title":"T","message":"from-alias","description":"test alias"}`); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if gotBody != "from-alias" {
		t.Errorf("expected body from message alias, got %q", gotBody)
	}
}

func TestNotify_DesktopHandler_FallsBackWhenHeadless(t *testing.T) {
	// When the handler returns false (no Desktop attached), Run must fall
	// through to the osascript path. We cancel the context from inside the
	// handler so exec.CommandContext bails out immediately without actually
	// posting a real notification on the developer's machine.
	tool := &NotifyTool{}
	var called bool
	ctx, cancel := context.WithCancel(context.Background())
	handler := NotifyHandler(func(title, body string, sound bool) bool {
		called = true
		cancel()
		return false
	})
	ctx = WithNotifyHandler(ctx, handler)

	_, err := tool.Run(ctx, `{"title":"T","body":"B","description":"test fallback"}`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !called {
		t.Fatal("expected handler to be consulted before osascript fallback")
	}
}

func TestNotify_NoHandler_UnchangedBehavior(t *testing.T) {
	// Backward-compat: when no NotifyHandler is in context, Run should reach
	// the osascript path. We cancel the parent context to prevent an actual
	// notification side effect, and verify that Run returns without panicking.
	tool := &NotifyTool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.Run(ctx, `{"title":"T","body":"B","description":"test error"}`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
}

func TestNotifyHandlerFrom_NilWhenAbsent(t *testing.T) {
	if h := NotifyHandlerFrom(context.Background()); h != nil {
		t.Error("expected nil handler from bare context")
	}
}

func TestWithNotifyHandler_NilIsNoop(t *testing.T) {
	ctx := WithNotifyHandler(context.Background(), nil)
	if h := NotifyHandlerFrom(ctx); h != nil {
		t.Error("expected nil handler after WithNotifyHandler(nil)")
	}
}
