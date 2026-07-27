package agent

import (
	"encoding/json"
	"strings"
)

func isTerminalGoalComputerUseFailure(
	toolName string,
	result ToolResult,
) bool {
	return toolName == "computer_use" &&
		result.IsError &&
		strings.Contains(result.Content, "computer_use_error:")
}

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
			"message: a goal-level computer_use task already owns this turn and ended with a terminal executor failure; alternate desktop-control paths are disabled to avoid acting on unverified partial state\n" +
			"recovery: report the original computer_use failure and any possible partial completion honestly; wait for a new user turn before attempting desktop control again",
		IsError:       true,
		ErrorCategory: ErrCategoryBusiness,
	}
}
