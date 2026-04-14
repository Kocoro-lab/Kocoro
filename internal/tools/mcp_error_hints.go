package tools

import "strings"

// normalizeMCPResult rewrites raw MCP-server tool results into content the
// model can act on without having to infer intent from terse playwright-mcp
// strings. Two kinds of rewrite:
//
//   - Error path: append a terse, imperative [hint] explaining what almost
//     certainly just happened and what to do next. Real runs showed the
//     agent retrying browser_file_upload after a successful upload because
//     the error said "no related modal state present" — technically correct,
//     but the model read it as a transient failure and retried, triggering
//     the loop detector.
//   - Success path: append a [hint] when the playwright-mcp response body
//     looks like a definitive completion but has no visible effect in the
//     page snapshot (uploads close the file chooser and leave the composer
//     with just a thumbnail, which the snapshot may elide as a graphical
//     fragment). Without this, the model often fails to notice the upload
//     succeeded and re-runs it.
//
// Returns the possibly-rewritten content; isError is returned to the caller
// unchanged so audit/loop-detection behavior is preserved.
func normalizeMCPResult(serverName, toolName, content string, isError bool) string {
	if content == "" {
		return content
	}
	if toolName != "browser_file_upload" {
		return content
	}

	if isError {
		if strings.Contains(content, "related modal state present") {
			// Put the hint before the raw error so the model reads the
			// actionable direction first. The loop detector fires fast on
			// consecutive identical calls; we lose if the hint is buried.
			return "[hint] STOP calling browser_file_upload blindly. The file chooser is no longer open, so this tool cannot run again until you reopen the chooser from the page. Do NOT retry with the same path unless you first reopen the chooser. Next action: (a) call browser_snapshot to verify whether the attachment is already present near the composer; or (b) if it is not present, reopen the chooser from the page UI and then retry.\n\n" + content
		}
		if strings.Contains(content, "outside allowed roots") {
			return "[hint] The path is outside the directories advertised to playwright-mcp as workspace roots. Use a path under ~/.shannon/tmp/attachments/ (materialized inline attachments), ~/Downloads, ~/Desktop, or ~/Documents. If the file is elsewhere, copy it into one of those directories with bash and retry with the copied path.\n\n" + content
		}
		return content
	}

	// Success path: playwright-mcp prints the setFiles call and an often-
	// empty page snapshot. Tell the model this is a completion.
	if strings.Contains(content, "fileChooser.setFiles(") {
		return content + "\n\n[hint] The file has been attached to the composer. DO NOT call browser_file_upload again for this file. The file chooser is now closed. Next step: type your message in the composer (browser_type) and submit it (browser_press_key Enter or click the send button)."
	}
	return content
}
