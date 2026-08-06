package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

// Post-compaction file restoration: ShapeHistory drops the middle of the
// history, and with it the full text of files the model read there — content
// that is still sitting on disk. Re-reading the most recent reads after a
// compaction restores exact identifiers and code the summary inevitably
// paraphrases (2026-08-04 e2e: decoy identifiers were permanently lost with
// the first compaction; the model honestly answered CANNOT RECALL).
const (
	// restoreMaxFiles bounds how many files are re-injected per compaction.
	//   - Workload: long tool-heavy sessions where dozens of files were read.
	//   - Symptom when it binds: only the 5 most recent files come back; older
	//     reads must be re-read by the model on demand.
	//   - Override: raise together with restoreTotalTokenCap — the trigger
	//     budget below is the real ceiling.
	restoreMaxFiles = 5
	// restoreFileTokenCap caps a single re-read (≈5K tokens at chars/3.5).
	//   - Symptom when it binds: a large file comes back head-only with a
	//     truncation marker; the model can re-read the rest on demand.
	//   - Override: raise for agents whose work depends on whole large files.
	restoreFileTokenCap = 5_000
	// restoreTotalTokenCap caps the whole restoration payload (≈50K tokens).
	//   - Symptom when it binds: fewer than restoreMaxFiles files restored.
	//   - Override: raise only with a matching context-window increase; the
	//     compaction just freed (trigger − landing) tokens of hysteresis band
	//     and the payload must stay well inside that. On the absolute-buffer
	//     regime (≥ ~900K windows) the band is two buffers (120K), so 50K
	//     leaves more than one large turn pair of slack — the relation is
	//     pinned by TestRestoreCapFitsAbsoluteHysteresisBand so the constants
	//     cannot drift apart again.
	restoreTotalTokenCap = 50_000
	// restoreMinBudgetTokens is the floor below which restoration declines
	// entirely: shaping landed so close to the trigger line (e.g. minKeepLast
	// floor on an overhead-dominated session) that any payload would
	// immediately re-arm the compaction it just paid for.
	restoreMinBudgetTokens = 1_000
	// restoreMaxFileBytes skips pathologically large files before reading.
	restoreMaxFileBytes = 5 << 20
)

// RecentRead is one entry of ReadTracker.RecentReads: the most recent
// file_read range recorded for a path.
type RecentRead struct {
	Path   string
	Offset int
	Limit  int
	When   time.Time
}

// RecentReads returns up to max per-path read records, most recent first.
// When a path was read at several ranges, the most recent range wins — for
// sequential large-file reads that is the range freshest in the model's
// (pre-compaction) attention.
func (rt *ReadTracker) RecentReads(max int) []RecentRead {
	if rt == nil || max <= 0 {
		return nil
	}
	rt.mu.Lock()
	latest := make(map[string]RecentRead, len(rt.lastReads))
	for key, entry := range rt.lastReads {
		if cur, ok := latest[key.path]; !ok || entry.when.After(cur.When) {
			latest[key.path] = RecentRead{Path: key.path, Offset: entry.offset, Limit: entry.limit, When: entry.when}
		}
	}
	rt.mu.Unlock()

	out := make([]RecentRead, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].When.After(out[j].When) })
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// restoreExcludedBasenames are re-injected through the system prompt /
// scaffold every turn already; restoring them here would duplicate them.
var restoreExcludedBasenames = map[string]bool{
	"memory.md": true,
	"agent.md":  true,
}

// restoreExcludedExtensions are formats file_read serves as vision blocks
// (readImage/readPDF record them in the tracker too). Re-reading them here
// would interpolate raw binary bytes as prompt text — json.Marshal replaces
// the invalid UTF-8 with U+FFFD, so it fails as silent token waste. The
// utf8.Valid check in readRestoreRange backstops extensions not listed here.
// Keep in sync with imageReadExtensions in internal/tools/file_read.go —
// the sets cannot be shared because tools imports agent.
var restoreExcludedExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".heic": true, ".heif": true, ".avif": true, ".bmp": true, ".tiff": true,
	".pdf": true,
}

// restoreHeaderPrefix marks each restored file section; it doubles as the
// dedup key a later compaction in the same Run uses to avoid re-injecting a
// file whose restore block still survives in the kept tail. The "[restored]"
// sentinel keeps the dedup parser from misreading ordinary markdown headings
// INSIDE restored file content as restored paths — a doc containing a
// path-shaped heading would otherwise silently suppress that file's
// restoration at the next compaction.
const restoreHeaderPrefix = "## [restored] "

const restoreIntro = "Context was compacted. The most recently read files were re-read from disk to preserve continuity (fresh content — a file may have changed since the earlier read):"

// keptFileReadPaths collects the normalized paths whose content already
// survives in the shaped history: file_read tool_use blocks in the kept tail,
// plus files named by an earlier restore block (a second compaction in the
// same Run must not inject the same file twice). Known limitation: a
// surviving read whose result was a dedup stub ("file unchanged since last
// read") also suppresses restoration even though the full text it points at
// may have been dropped — acceptable because the stub itself tells the model
// how to re-read.
func keptFileReadPaths(messages []client.Message, cwd string) map[string]bool {
	out := make(map[string]bool)
	collectRestoreBlockPaths := func(text string) {
		if !strings.Contains(text, restoreIntro) {
			return
		}
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, restoreHeaderPrefix) {
				continue
			}
			path := strings.TrimPrefix(line, restoreHeaderPrefix)
			if i := strings.LastIndex(path, " (lines "); i > 0 {
				path = path[:i]
			}
			if p := normalizePathWithCWD(strings.TrimSpace(path), cwd); p != "" {
				out[p] = true
			}
		}
	}
	for _, m := range messages {
		if !m.Content.HasBlocks() {
			collectRestoreBlockPaths(m.Content.Text())
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "text" {
				collectRestoreBlockPaths(b.Text)
				continue
			}
			if b.Type != "tool_use" || b.Name != "file_read" {
				continue
			}
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(b.Input, &args); err != nil || args.Path == "" {
				continue
			}
			if p := normalizePathWithCWD(args.Path, cwd); p != "" {
				out[p] = true
			}
		}
	}
	return out
}

// readRestoreRange re-reads the recorded line range from disk. When the
// recorded offset no longer exists (file shrank), it falls back to the head —
// fresh content beats nothing. limit<=0 means "to the end". Binary content
// (extension or invalid UTF-8) is refused: this path re-injects TEXT; vision
// formats have their own encoding pipeline.
func readRestoreRange(path string, offset, limit int) (content string, first, last int, err error) {
	if restoreExcludedExtensions[strings.ToLower(filepath.Ext(path))] {
		return "", 0, 0, fmt.Errorf("skipped: non-text format")
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > restoreMaxFileBytes {
		if err == nil {
			err = fmt.Errorf("skipped: dir or over %d bytes", restoreMaxFileBytes)
		}
		return "", 0, 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, 0, err
	}
	if !utf8.Valid(data) {
		return "", 0, 0, fmt.Errorf("skipped: not valid UTF-8")
	}
	lines := strings.Split(string(data), "\n")
	// A newline-terminated file splits into a trailing "" — drop it so the
	// reported line range matches what a reader would count.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "", 0, 0, fmt.Errorf("skipped: empty file")
	}
	if offset < 0 || offset >= len(lines) {
		offset = 0
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], "\n"), offset + 1, end, nil
}

// clipWithMarker bounds s at maxBytes on a rune boundary, appending a marker
// naming how to get the rest. Byte-based on purpose: the caller's budget is
// rune-derived (chars/3.5), so clipping by bytes only ever under-shoots —
// safe direction for a budget.
func clipWithMarker(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[... truncated for restoration budget — use file_read for the rest ...]"
}

// buildPostCompactionFileRestore assembles a user message re-injecting the
// most recently read files after an applied PROACTIVE or PREFLIGHT
// compaction; both callers pass a.estOverhead(). The reactive (post-400)
// path deliberately does NOT call this at all: its evidence floor is only a
// lower bound on the true overhead, so any budget computed there can
// overshoot into the one prompt where reactiveCompacted makes a second
// overflow terminal. Returns ok=false when there is nothing to restore or no
// headroom: the payload must keep the calibrated estimate under the
// compaction trigger line (it deliberately MAY consume part of the 90/80
// hysteresis band — see CompactTriggerTokens).
// BuildPostCompactionFileRestore exposes the restoration payload to external
// compaction drivers (TUI /compact) so a manual compaction keeps the same
// file-content recovery the in-loop proactive/preflight paths have. Same
// budget discipline: the payload must keep the estimate under the trigger
// line, or restoration declines.
func (a *AgentLoop) BuildPostCompactionFileRestore(shaped []client.Message, overheadTokens int) (client.Message, bool) {
	return a.buildPostCompactionFileRestore(shaped, overheadTokens)
}

func (a *AgentLoop) buildPostCompactionFileRestore(shaped []client.Message, overheadTokens int) (client.Message, bool) {
	rt := a.readTracker
	if rt == nil || a.contextWindow <= 0 {
		return client.Message{}, false
	}
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	budget := ctxwin.CompactTriggerTokens(a.contextWindow) - ctxwin.EstimateTokens(shaped) - overheadTokens
	if budget > restoreTotalTokenCap {
		budget = restoreTotalTokenCap
	}
	if budget < restoreMinBudgetTokens {
		return client.Message{}, false
	}
	// chars/3.5 mirrors ctxwin.EstimateTokens so the payload budget and the
	// next request's estimate agree.
	charBudget := int(float64(budget) * 3.5)
	perFileChars := int(float64(restoreFileTokenCap) * 3.5)

	kept := keptFileReadPaths(shaped, rt.cwd)
	var sb strings.Builder
	files := 0
	for _, read := range rt.RecentReads(restoreMaxFiles * 4) {
		if files >= restoreMaxFiles || charBudget <= 0 {
			break
		}
		if restoreExcludedBasenames[strings.ToLower(filepath.Base(read.Path))] {
			continue
		}
		if kept[read.Path] {
			continue
		}
		content, first, last, err := readRestoreRange(read.Path, read.Offset, read.Limit)
		if err != nil || strings.TrimSpace(content) == "" {
			continue
		}
		clip := perFileChars
		if charBudget < clip {
			clip = charBudget
		}
		content = clipWithMarker(content, clip)
		// A literal closing tag inside file content would end the
		// system-reminder block early and reframe the rest as instructions.
		content = strings.ReplaceAll(content, "</system-reminder>", "[/system-reminder]")
		section := fmt.Sprintf("%s%s (lines %d-%d)\n%s\n\n", restoreHeaderPrefix, read.Path, first, last, content)
		sb.WriteString(section)
		charBudget -= len(section)
		files++
	}
	if files == 0 {
		return client.Message{}, false
	}

	text := "<system-reminder>\n" + restoreIntro + "\n\n" + sb.String() + "</system-reminder>"
	fmt.Fprintf(os.Stderr, "[agent] post-compaction file restore: %d file(s), ~%d chars\n", files, len(text))
	return client.Message{Role: "user", Content: client.NewTextContent(text)}, true
}
