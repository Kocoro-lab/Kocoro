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
	//   - Override: raise together with restoreTotalTokenCap — the landing
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
	//     compaction just freed (trigger − landing) × window tokens and the
	//     payload must stay well inside that.
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

// keptFileReadPaths collects the normalized paths of file_read tool_use
// blocks that survived shaping. Known limitation: a surviving read whose
// result was a dedup stub ("file unchanged since last read") also suppresses
// restoration even though the full text it points at may have been dropped —
// acceptable because the stub itself tells the model how to re-read.
func keptFileReadPaths(messages []client.Message, cwd string) map[string]bool {
	out := make(map[string]bool)
	for _, m := range messages {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
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
// fresh content beats nothing. limit<=0 means "to the end".
func readRestoreRange(path string, offset, limit int) (content string, first, last int, err error) {
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
	lines := strings.Split(string(data), "\n")
	if offset < 0 || offset >= len(lines) {
		offset = 0
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], "\n"), offset + 1, end, nil
}

// clipRunesWithMarker bounds s at maxChars bytes on a rune boundary,
// appending a marker naming how to get the rest.
func clipRunesWithMarker(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := maxChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[... truncated for restoration budget — use file_read for the rest ...]"
}

// buildPostCompactionFileRestore assembles a user message re-injecting the
// most recently read files after an APPLIED compaction. Returns ok=false when
// there is nothing to restore or no headroom: the payload must keep the
// calibrated estimate under the compaction trigger line, or the restoration
// itself would re-arm the compaction it just paid for (it deliberately MAY
// consume part of the 90/80 hysteresis band — see CompactTriggerTokens).
func (a *AgentLoop) buildPostCompactionFileRestore(shaped []client.Message) (client.Message, bool) {
	rt := a.readTracker
	if rt == nil || a.contextWindow <= 0 {
		return client.Message{}, false
	}
	budget := ctxwin.CompactTriggerTokens(a.contextWindow) - ctxwin.EstimateTokens(shaped) - a.estOverhead()
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
		content = clipRunesWithMarker(content, clip)
		fmt.Fprintf(&sb, "## %s (lines %d-%d)\n%s\n\n", read.Path, first, last, content)
		charBudget -= len(content)
		files++
	}
	if files == 0 {
		return client.Message{}, false
	}

	text := "<system-reminder>\nContext was compacted. The most recently read files were re-read from disk to preserve continuity (fresh content — a file may have changed since the earlier read):\n\n" +
		sb.String() + "</system-reminder>"
	fmt.Fprintf(os.Stderr, "[agent] post-compaction file restore: %d file(s), ~%d chars\n", files, len(text))
	return client.Message{Role: "user", Content: client.NewTextContent(text)}, true
}
