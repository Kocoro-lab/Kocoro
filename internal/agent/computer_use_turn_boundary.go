package agent

import (
	"encoding/json"
	"strings"
)

func isDesktopControlToolName(name string) bool {
	return isGUIToolName(name) || name == "ghostty"
}

func toolSearchMayLoadDesktopControl(argsJSON string) bool {
	var args struct {
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return false
	}
	query := strings.TrimSpace(strings.ToLower(args.Query))
	if query == "" {
		return false
	}
	if strings.HasPrefix(query, "select:") {
		for _, name := range strings.Split(strings.TrimPrefix(query, "select:"), ",") {
			if isDesktopControlToolName(strings.TrimSpace(name)) {
				return true
			}
		}
		return false
	}
	if isDesktopControlToolName(query) {
		return true
	}
	for _, marker := range []string{
		"browser", "playwright", "computer", "desktop", "gui",
		"screenshot", "accessibility", "applescript", "ghostty",
	} {
		if strings.Contains(query, marker) {
			return true
		}
	}
	return false
}

func isAlternateDesktopControlCall(toolName string, argsJSON string) bool {
	if isDesktopControlToolName(toolName) {
		return true
	}
	return toolName == "tool_search" && toolSearchMayLoadDesktopControl(argsJSON)
}

func alternateDesktopControlBlockedResult() ToolResult {
	return ToolResult{
		Content: "computer_use_error: alternate_desktop_control_blocked\n" +
			"message: one goal-level computer_use task already owns this turn; a second desktop-control task or fallback would duplicate its private recovery and verification\n" +
			"recovery: use the original computer_use result and report any partial or unverified outcome honestly; wait for a new user turn before attempting desktop control again",
		IsError:       true,
		ErrorCategory: ErrCategoryBusiness,
	}
}
