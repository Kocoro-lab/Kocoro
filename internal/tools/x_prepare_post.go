package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const xPreparePostMaxRunes = 100_000

// XPreparePostTool builds an official X Web Intent locally. It deliberately
// has no browser opener or HTTP client: generating a review link cannot publish
// anything, and only the user's later click may hand it to the system browser.
type XPreparePostTool struct{}

type xPreparePostArgs struct {
	Text string `json:"text"`
}

func (*XPreparePostTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "x_prepare_post",
		Description: "Prepare a user-clickable X composer link from an agreed draft. This tool never publishes, never opens a browser, does not require an X connection, and makes no network call. Return the review link to the user and never use browser or computer automation to click X's Post button. Nothing was posted by this tool.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{
					"type":        "string",
					"description": "Exact draft text to prefill in X's composer. It is not published until the user reviews it and clicks Post on X.",
				},
			},
		},
		Required: []string{"text"},
	}
}

func (*XPreparePostTool) RequiresApproval() bool            { return false }
func (*XPreparePostTool) IsReadOnlyCall(string) bool        { return true }
func (*XPreparePostTool) IsConcurrencySafeCall(string) bool { return true }
func (*XPreparePostTool) StopsAgentLoop() bool              { return true }

func (t *XPreparePostTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args xPreparePostArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid input: %v", err)), nil
	}
	if strings.TrimSpace(args.Text) == "" {
		return agent.ValidationError("text must not be empty"), nil
	}
	if utf8.RuneCountInString(args.Text) > xPreparePostMaxRunes {
		return agent.ValidationError(fmt.Sprintf("text exceeds the local safety limit of %d characters; it was not truncated", xPreparePostMaxRunes)), nil
	}

	intent := url.URL{Scheme: "https", Host: "x.com", Path: "/intent/tweet"}
	query := intent.Query()
	query.Set("text", args.Text)
	intent.RawQuery = query.Encode()
	link := intent.String()
	return agent.ToolResult{Content: fmt.Sprintf(
		`{"published":false,"message":"Nothing was posted. Review the draft on X and click Post yourself.","url":%q,"markdown":"[Review and post on X](%s)"}`,
		link, link,
	), StopAgentLoop: true, TerminalUserMessage: fmt.Sprintf(
		"[Review and post on X](%s)\n\nNothing was posted. Review the draft and click Post on X yourself.",
		link,
	)}, nil
}

func (*XPreparePostTool) AuditSummaries(string, string) (string, string) {
	return "X post draft supplied (content omitted)", "X Web Intent prepared; nothing posted"
}
