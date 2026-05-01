package agent

import (
	"fmt"
	"sort"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type ContextBloatOptions struct {
	RecentToolResultBytes int
}

func buildContextBloatReminder(messages []client.Message, opts ContextBloatOptions) string {
	if opts.RecentToolResultBytes <= 0 {
		opts.RecentToolResultBytes = 20000
	}
	nameByID := toolUseNameByID(messages)
	bytesByTool := make(map[string]int)
	for _, msg := range messages {
		if msg.Role != "user" || !msg.Content.HasBlocks() {
			continue
		}
		for _, block := range msg.Content.Blocks() {
			if block.Type != "tool_result" {
				continue
			}
			toolName := nameByID[block.ToolUseID]
			if toolName == "" {
				continue
			}
			bytesByTool[toolName] += len(client.ToolResultText(block))
		}
	}
	type pair struct {
		name string
		n    int
	}
	var pairs []pair
	for name, n := range bytesByTool {
		if n >= opts.RecentToolResultBytes {
			pairs = append(pairs, pair{name: name, n: n})
		}
	}
	if len(pairs) == 0 {
		return ""
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].n > pairs[j].n })
	top := pairs[0]
	switch top.name {
	case "file_read":
		return fmt.Sprintf("<system-reminder>Large file_read output is dominating context (~%d chars). Prefer offset+limit, grep, or a narrower path before reading more.</system-reminder>", top.n)
	case "grep":
		return fmt.Sprintf("<system-reminder>Large grep output is dominating context (~%d chars). Prefer files_with_matches, head_limit, type, glob, or count before requesting content.</system-reminder>", top.n)
	case "bash":
		return fmt.Sprintf("<system-reminder>Large bash output is dominating context (~%d chars). Redirect noisy output to a file and inspect small slices.</system-reminder>", top.n)
	default:
		return fmt.Sprintf("<system-reminder>Large %s tool output is dominating context (~%d chars). Narrow the next tool call before producing more output.</system-reminder>", top.name, top.n)
	}
}
