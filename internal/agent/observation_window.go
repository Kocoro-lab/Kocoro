package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// Browser/GUI agent loops re-send the full accumulated observation history on
// every iteration. Page/DOM snapshots are large, so a long browser session
// grows the per-iteration prompt far faster than the model's short outputs
// justify. The two helpers here bound that growth without changing the wire
// contract: truncateObservation caps a single observation at capture time, and
// filterOldObservations keeps only the most recent N observations at full
// fidelity while stubbing the rest. Both are purely client-side context
// assembly — the OpenAI-compatible messages array shape is unchanged.

// observationStubPrefix marks a browser/GUI observation whose text has aged out
// of the sliding window. Used for idempotency (already-stubbed content is not
// re-stubbed) and by tests.
const observationStubPrefix = "[elided browser observation:"

// defaultObservationWindow is the number of most-recent browser/GUI
// observations kept at full fidelity.
//
//   - Workload: multi-page browser tasks (navigate → snapshot → click → …) that
//     run tens of iterations, each re-sending every prior page snapshot.
//   - Symptom when it binds: the 4th-oldest and earlier observations show as a
//     one-line stub instead of full page text; the model works from the 3 most
//     recent snapshots plus its own notes.
//   - Override: agent.observation_window (0 disables the window entirely).
const defaultObservationWindow = 3

// defaultBrowserObservationMaxChars caps a single browser/GUI observation at
// capture time (runes). Smaller than the generic tools.result_truncation
// (30000) because page/DOM dumps compress poorly and the useful signal is
// front-loaded.
//
//   - Workload: a single full-page snapshot / DOM dump that spikes well past
//     the generic per-result cap.
//   - Symptom when it binds: the observation ends with a "[browser observation
//     truncated: N chars total]" marker; trailing page markup is dropped.
//   - Override: tools.browser_result_truncation (0 falls back to the generic cap).
const defaultBrowserObservationMaxChars = 24000

// defaultMaxRecentImages is the count-based old-image pruning default: keep the
// last N image-bearing messages at full fidelity, replacing older screenshots
// with a text placeholder (see filterOldImages). Applies to ALL images (browser
// screenshots, user uploads, file_read images), so it stays generous to avoid
// degrading multi-turn non-browser vision.
//
//   - Workload: batch-vision ("describe these N screenshots") and long browser
//     sessions that snapshot the viewport repeatedly.
//   - Symptom when it binds: screenshots older than the last N show as
//     "[previous screenshot removed to save context]".
//   - Override: agent.max_recent_images. With Layer-1 compression each image is
//     <= 5 MB base64 (~6-12K tokens), so 50 images ~= 300-600K tokens, under the
//     1M-token default context window. Was 5 pre-#135 — too conservative for
//     batch-vision ("describe these N screenshots") use cases.
const defaultMaxRecentImages = 50

// defaultMaxRecentBrowserImages keeps only the most recent browser/GUI
// screenshot in history (nested in a GUI tool_result). Older browser
// screenshots are the least useful context — the agent acts on the current
// viewport — yet the most expensive per image. Scoped by isGUIToolName so
// user-uploaded images and non-GUI tool images stay under the looser global
// filterOldImages budget.
//
//   - Workload: multi-step browser sessions that screenshot after each action.
//   - Symptom when it binds: browser screenshots older than the most recent
//     show as "[previous screenshot removed to save context]".
//   - Override: agent.max_recent_browser_images (0 disables this browser-scoped
//     filter; non-browser images remain governed by agent.max_recent_images).
const defaultMaxRecentBrowserImages = 1

// browserObservationStub is the one-line placeholder that replaces an old
// browser/GUI observation's text once it ages out of the sliding window.
func browserObservationStub(tool string, chars int) string {
	return fmt.Sprintf("%s %s, was %d chars]", observationStubPrefix, tool, chars)
}

// truncateObservation caps a browser/GUI observation at `max` runes at capture
// time, inclusive of an explicit marker that discloses the original size.
// Unlike truncateStr (which appends a bare "..."), the marker is self-describing
// so the model and the audit log can see how much page/DOM text was dropped.
//
// The cap is inclusive: the returned string never exceeds `max` runes, so the
// "each observation <= cap" guarantee holds literally. max <= 0 means "no cap"
// (returns the input unchanged) so a misconfigured 0 never blanks an observation.
func truncateObservation(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	marker := fmt.Sprintf("\n[browser observation truncated: %d chars total]", len(runes))
	keep := max - utf8.RuneCountInString(marker)
	if keep < 0 {
		// Cap smaller than the marker itself — return the marker truncated to
		// max runes so the "never exceeds max" contract still holds. max is in
		// (0, len(marker)) here (max <= 0 and len <= max returned earlier), so
		// the slice is in range. Not reachable at the default 24000.
		return string([]rune(marker)[:max])
	}
	return string(runes[:keep]) + marker
}

// collectObservationToolUseIDs returns ordered (oldest-first) tool_use IDs from
// assistant messages whose tool is a browser/GUI observation producer
// (isGUIToolName), together with an ID -> tool-name map for stub labelling.
// Mirrors collectCompactableToolUseIDs (timebasedcompact.go) but scoped to the
// browser/GUI family — the bulky page/DOM snapshots that dominate accumulated
// context.
func collectObservationToolUseIDs(messages []client.Message) (ids []string, names map[string]string) {
	names = map[string]string{}
	for _, m := range messages {
		if m.Role != "assistant" || !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_use" && b.ID != "" && isGUIToolName(b.Name) {
				ids = append(ids, b.ID)
				names[b.ID] = b.Name
			}
		}
	}
	return ids, names
}

// filterOldObservations keeps full fidelity only for the most recent `keep`
// browser/GUI observations; older ones have their tool_result text replaced
// with a one-line stub. This bounds the accumulation of re-sent page snapshots
// across a long browser agent loop (the dominant per-iteration context cost)
// without breaking the API contract: tool_result blocks and their tool_use_id
// pairing are preserved — only the content string is replaced.
//
// Scope guards keep it safe:
//   - keep <= 0 disables it entirely (no-op).
//   - Only browser/GUI tools (isGUIToolName) are touched; file_read, bash,
//     grep, http and conversational user/assistant turns are never trimmed here.
//   - Only string tool_result content is stubbed; screenshot/image results use
//     nested blocks and are aged out separately by filterOldImages.
//   - Idempotent: already-stubbed content is skipped so re-runs don't redirty
//     bytes or fragment the prompt-cache prefix.
//
// Returns the number of observations stubbed. Mirrors the shape of
// timeBasedCompact so the two read alike.
func filterOldObservations(messages []client.Message, keep int) int {
	if keep <= 0 {
		return 0
	}
	ids, names := collectObservationToolUseIDs(messages)
	if len(ids) <= keep {
		return 0
	}
	keepFrom := len(ids) - keep
	stubSet := make(map[string]bool, keepFrom)
	for _, id := range ids[:keepFrom] {
		stubSet[id] = true
	}

	stubbed := 0
	for i, m := range messages {
		if m.Role != "user" || !m.Content.HasBlocks() {
			continue
		}
		blocks := m.Content.Blocks()
		newBlocks := make([]client.ContentBlock, len(blocks))
		touched := false
		for j, b := range blocks {
			if b.Type == "tool_result" && stubSet[b.ToolUseID] {
				// Only string content is bulky page text; nested-block results
				// (screenshots) are handled by filterOldImages, so leave them.
				if existing, ok := b.ToolContent.(string); ok && !strings.HasPrefix(existing, observationStubPrefix) {
					b.ToolContent = browserObservationStub(names[b.ToolUseID], utf8.RuneCountInString(existing))
					touched = true
					stubbed++
				}
			}
			newBlocks[j] = b
		}
		if touched {
			oldContent := messages[i].Content
			messages[i].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("obs_window", i, oldContent, messages[i].Content)
		}
	}
	return stubbed
}

// toolResultHasImage reports whether a tool_result block carries a nested image.
func toolResultHasImage(b client.ContentBlock) bool {
	if b.Type != "tool_result" {
		return false
	}
	nested, ok := b.ToolContent.([]client.ContentBlock)
	if !ok {
		return false
	}
	for _, nb := range nested {
		if nb.Type == "image" {
			return true
		}
	}
	return false
}

// filterOldBrowserImages keeps only the most recent `keep` browser/GUI
// screenshots (images nested in a tool_result whose tool_use is isGUIToolName);
// older browser screenshots have their images replaced with the standard
// screenshot placeholder. This is the browser-scoped counterpart to the global
// filterOldImages: it deliberately leaves user-uploaded images (top-level image
// blocks) and non-GUI tool images (e.g. file_read) untouched, so those stay
// governed by the looser global budget. Uses the same isGUIToolName tool_use-id
// scoping as filterOldObservations.
//
// keep <= 0 disables the browser-scoped filter (no-op). Returns the number of
// browser screenshots stripped.
func filterOldBrowserImages(messages []client.Message, keep int) int {
	if keep <= 0 {
		return 0
	}
	ids, _ := collectObservationToolUseIDs(messages)
	if len(ids) == 0 {
		return 0
	}
	guiSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		guiSet[id] = true
	}

	// Indices of messages carrying a browser screenshot, newest first.
	var imageIndices []int
	for i := len(messages) - 1; i >= 0; i-- {
		if !messages[i].Content.HasBlocks() {
			continue
		}
		for _, b := range messages[i].Content.Blocks() {
			if guiSet[b.ToolUseID] && toolResultHasImage(b) {
				imageIndices = append(imageIndices, i)
				break
			}
		}
	}
	if len(imageIndices) <= keep {
		return 0
	}

	stripped := 0
	for _, idx := range imageIndices[keep:] {
		blocks := messages[idx].Content.Blocks()
		newBlocks := make([]client.ContentBlock, len(blocks))
		touched := false
		for j, b := range blocks {
			if guiSet[b.ToolUseID] && toolResultHasImage(b) {
				newBlocks[j] = stripImagesFromToolResult(b)
				touched = true
				stripped++
			} else {
				newBlocks[j] = b
			}
		}
		if touched {
			oldContent := messages[idx].Content
			messages[idx].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("browser_img_strip", idx, oldContent, messages[idx].Content)
		}
	}
	return stripped
}
