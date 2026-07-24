package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// NotifyHandler delivers a notify tool call through an attached daemon client
// (typically the Desktop app) instead of shelling out to osascript. It returns
// true when the notification was delivered — in which case the tool skips the
// osascript fallback — and false when no client is attached, which tells the
// tool to fall back to osascript for headless mode.
type NotifyHandler func(title, body string, sound bool) bool

type notifyHandlerKey struct{}

// WithNotifyHandler returns a context carrying a NotifyHandler. The daemon
// runner attaches one per run so that notify tool calls from scheduled or
// interactive agents can be routed through the Desktop's UNUserNotificationCenter
// with correct app attribution and click-through.
func WithNotifyHandler(ctx context.Context, h NotifyHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, notifyHandlerKey{}, h)
}

// NotifyHandlerFrom returns the NotifyHandler from ctx, or nil if none is set.
func NotifyHandlerFrom(ctx context.Context) NotifyHandler {
	h, _ := ctx.Value(notifyHandlerKey{}).(NotifyHandler)
	return h
}

type NotifyTool struct{}

type notifyArgs struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body,omitempty"`
	Message     string `json:"message,omitempty"` // alias for body
	Sound       bool   `json:"sound,omitempty"`
}

func (t *NotifyTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "notify",
		Description: "Send a macOS desktop notification using osascript." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "Notification title"},
				"description": agent.DescriptionFieldSpec,
				"body":        map[string]any{"type": "string", "description": "Notification body text (alias: message)"},
				"message":     map[string]any{"type": "string", "description": "Alias for body"},
				"sound":       map[string]any{"type": "boolean", "description": "Play notification sound (default: false)"},
			},
		},
		Required: []string{"title", "description"},
	}
}

func (t *NotifyTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args notifyArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Title) == "" {
		return agent.ValidationError("notify: missing required `title` parameter"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("notify: missing required `description` parameter"), nil
	}

	body := args.Body
	if body == "" {
		body = args.Message
	}

	if err := SendBanner(ctx, args.Title, body, args.Sound); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("notification error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{Content: "notification sent"}, nil
}

// SendBanner delivers a macOS notification banner using the Desktop route when
// a NotifyHandler is attached to ctx; otherwise falls back to osascript so the
// banner still fires in headless mode. Shared by the notify tool and the
// daemon's reply-complete banner so both honor the same delivery contract.
//
// The osascript fallback is macOS-only: on Linux/Windows it returns an
// `executable file not found` error. Callers that fire SendBanner implicitly
// (rather than via an agent-invoked tool) should gate on `runtime.GOOS ==
// "darwin"` to keep headless deployments quiet.
func SendBanner(ctx context.Context, title, body string, sound bool) error {
	if h := NotifyHandlerFrom(ctx); h != nil {
		if h(title, body, sound) {
			return nil
		}
	}
	// The Desktop route above is cross-platform; the osascript fallback below is
	// macOS-only. On other platforms without a handler, return a clean error
	// rather than a cryptic `exec: "osascript": file not found`.
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop notifications require macOS or the Kocoro Desktop app")
	}
	script := buildNotifyScript(title, body, sound)
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if out := strings.TrimSpace(string(output)); out != "" {
			return fmt.Errorf("%w: %s", err, out)
		}
		return err
	}
	return nil
}

func buildNotifyScript(title, body string, sound bool) string {
	title = escapeAppleScript(title)
	body = escapeAppleScript(body)

	script := fmt.Sprintf(`display notification "%s" with title "%s"`, body, title)
	if sound {
		script += ` sound name "default"`
	}
	return script
}

func (t *NotifyTool) RequiresApproval() bool { return true }

func (t *NotifyTool) IsReadOnlyCall(string) bool { return false }
