// Package tools — doc_extract.go implements four document-extraction convenience
// tools: pdf_to_text, docx_to_text, xlsx_to_text, pptx_to_text. Each tool
// shells out to an external "primary" extractor (poppler / pandoc / xlsx2csv)
// and falls back to a zero-dependency unzip + raw-XML strip path when the
// primary is missing, so the LLM can read common Office formats in a single
// turn regardless of host setup.
//
// Design notes:
//
//   - All four tools are read-only and never require approval. They run only
//     trusted binaries (no user-supplied argv) with a fixed argument slice;
//     paths are resolved through cwdctx.ResolveFilesystemPath, mirroring the
//     archive_inspect / archive_extract pattern.
//   - Output is capped at maxDocExtractRunes runes (defined below) measured via
//     utf8.RuneCountInString; truncated output gets a "[Truncated: ...]" tail
//     marker so the LLM knows the result is partial.
//   - Primary tools that aren't installed produce a structured install hint
//     rather than a hard error — the agent falls through to the unzip fallback
//     for Office formats, or surfaces the hint to the user for PDF (no
//     zero-dep PDF text extractor in the Go stdlib).
//   - exec.Command is invoked with a fixed argv slice for every path. We
//     never construct a shell command string from user input, so the
//     well-known "$(...)" / "; rm -rf /" attack surface does not apply here.
package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// Maximum runes returned by any doc-extract tool. Chosen to mirror the
// per-result spill threshold in agent/spill.go (50K characters) plus headroom
// — 100K runes is approximately the upper end of what fits in one turn's
// tool_result before per-turn aggregate caps trim things further.
const maxDocExtractRunes = 100_000

// Timeout for the primary extractor subprocess. Long enough for big docs,
// short enough that a wedged binary cannot park the agent loop indefinitely.
const docExtractTimeout = 60 * time.Second

// xmlTagStripper matches XML tags so the fallback paths can produce
// human-readable raw text from Office Open XML payloads. We pre-compile once
// at package load.
var xmlTagStripper = regexp.MustCompile(`<[^>]+>`)

// ----------------------------------------------------------------------------
// Shared helpers
// ----------------------------------------------------------------------------

// resolveDocPath unifies the "absolute or cwd-relative path" handling across
// all four tools. Returns a ToolResult to be returned directly on validation
// errors; callers should check `errResult != nil`.
func resolveDocPath(ctx context.Context, toolName, raw string) (resolved string, errResult *agent.ToolResult) {
	if strings.TrimSpace(raw) == "" {
		r := agent.ValidationError("path is required")
		return "", &r
	}
	resolved, err := cwdctx.ResolveFilesystemPath(ctx, raw)
	if err != nil {
		if errors.Is(err, cwdctx.ErrNoSessionCWD) {
			r := agent.ValidationError(fmt.Sprintf("%s: no session working directory is set. Pass an absolute path.", toolName))
			return "", &r
		}
		r := agent.ValidationError(fmt.Sprintf("%s: %v", toolName, err))
		return "", &r
	}
	return resolved, nil
}

// capRunes truncates s to maxDocExtractRunes runes (UTF-8 safe) and appends a
// truncation marker. If s is already within budget, returned unchanged.
func capRunes(s string) string {
	count := utf8.RuneCountInString(s)
	if count <= maxDocExtractRunes {
		return s
	}
	// Walk runes to find the byte offset at the cap.
	kept := 0
	for i := range s {
		if kept == maxDocExtractRunes {
			return s[:i] + fmt.Sprintf("\n[Truncated: original_len=%d, kept=%d]", count, maxDocExtractRunes)
		}
		kept++
	}
	// Unreachable in practice (count > cap guaranteed walk to trigger return).
	return s
}

// runPrimary invokes a primary extractor with a fixed argv slice. Returns
// (stdout, true) on success. On failure, returns ("", false) so callers can
// fall through to a fallback path or emit an install hint. The exit-status
// error is intentionally discarded — pdftotext/pandoc/xlsx2csv exit codes
// don't map cleanly enough across versions to be worth surfacing, and the
// fallback path is a more useful response than "exit 1 from xlsx2csv".
func runPrimary(ctx context.Context, name string, args []string) (string, bool) {
	runCtx, cancel := context.WithTimeout(ctx, docExtractTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return stdout.String(), true
}

// readZipMember opens path as a zip archive and returns the bytes of the
// first entry matching exact name `member` (or, for glob matching, an entry
// whose name passes filepath.Match against `glob`). Used by the Office
// fallbacks (which crack the .docx/.xlsx/.pptx zip container directly).
func readZipMembers(path string, match func(name string) bool) ([][]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	var out [][]byte
	for _, f := range zr.File {
		if !match(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}
		// Per-entry size guard: cap at 5x the rune cap (bytes, before
		// XML tag strip) so a single oversized member can't blow memory.
		data, err := io.ReadAll(io.LimitReader(rc, int64(maxDocExtractRunes)*5))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}
		out = append(out, data)
	}
	return out, nil
}

// stripXMLTags removes XML tags from b and collapses runs of whitespace so
// the residual text is readable. Quality is "good enough for the LLM" — the
// agent should reach for pandoc / xlsx2csv via the install hint when it
// needs structure-preserving output.
func stripXMLTags(raw []byte) string {
	noTags := xmlTagStripper.ReplaceAll(raw, []byte(" "))
	// Collapse whitespace runs. We avoid regexp here for speed on large docs.
	var b strings.Builder
	b.Grow(len(noTags))
	prevSpace := false
	for _, r := range string(noTags) {
		if r == ' ' || r == '\t' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		if r == '\n' {
			b.WriteByte('\n')
			prevSpace = true
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// ----------------------------------------------------------------------------
// pdf_to_text
// ----------------------------------------------------------------------------

type PDFToTextTool struct{}

type pdfToTextArgs struct {
	Path  string `json:"path"`
	Pages string `json:"pages,omitempty"`
}

func (t *PDFToTextTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "pdf_to_text",
		Description: "Extract plain text from a PDF using poppler's pdftotext. " +
			"Optional pages selector: \"all\" (default), \"5\" (single page), or \"1-10\" (range). " +
			"Read-only; no approval. Requires `pdftotext` on PATH — if missing, returns an install hint " +
			"(`brew install poppler` on macOS) and suggests uploading the PDF directly so cloud can " +
			"render it as a native Anthropic document block instead. Output capped at 100K characters.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the PDF file (absolute, or relative to session cwd).",
				},
				"pages": map[string]any{
					"type":        "string",
					"description": "Page selector: \"all\" (default), a single page number (\"5\"), or a range (\"1-10\"). Out-of-range pages are silently clamped by pdftotext.",
				},
			},
		},
		Required: []string{"path"},
	}
}

func (t *PDFToTextTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args pdfToTextArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	resolved, errRes := resolveDocPath(ctx, "pdf_to_text", args.Path)
	if errRes != nil {
		return *errRes, nil
	}

	pageArgs, err := pdfPageFlags(args.Pages)
	if err != nil {
		return agent.ValidationError(fmt.Sprintf("pdf_to_text: %v", err)), nil
	}

	if _, err := exec.LookPath("pdftotext"); err != nil {
		return agent.ToolResult{
			Content: "[pdf_to_text] pdftotext (poppler) is not installed. Install with `brew install poppler` on macOS, " +
				"or upload the PDF directly so cloud renders it as a native Anthropic document block " +
				"(no host-side extractor needed in that path).",
			IsError:       true,
			ErrorCategory: agent.ErrCategoryBusiness,
		}, nil
	}

	cmdArgs := append([]string{"-layout"}, pageArgs...)
	cmdArgs = append(cmdArgs, resolved, "-")
	out, ok := runPrimary(ctx, "pdftotext", cmdArgs)
	if !ok {
		return agent.ToolResult{
			Content: fmt.Sprintf("[pdf_to_text] pdftotext failed on %s. Verify the file is a valid PDF and not encrypted.", resolved),
			IsError: true,
		}, nil
	}
	note := "[Extracted via pdftotext -layout]"
	return agent.ToolResult{Content: note + "\n\n" + capRunes(out)}, nil
}

func (t *PDFToTextTool) RequiresApproval() bool { return false }

func (t *PDFToTextTool) IsReadOnlyCall(string) bool { return true }

// pdfPageFlags translates the friendly pages arg into pdftotext's -f / -l
// options. "all" / "" -> no flags. "5" -> -f 5 -l 5. "1-10" -> -f 1 -l 10.
func pdfPageFlags(pages string) ([]string, error) {
	p := strings.TrimSpace(pages)
	if p == "" || strings.EqualFold(p, "all") {
		return nil, nil
	}
	if strings.Contains(p, "-") {
		parts := strings.SplitN(p, "-", 2)
		first, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		last, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || first < 1 || last < first {
			return nil, fmt.Errorf("invalid pages range %q (expected forms: \"all\", \"5\", or \"1-10\")", pages)
		}
		return []string{"-f", strconv.Itoa(first), "-l", strconv.Itoa(last)}, nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 {
		return nil, fmt.Errorf("invalid pages value %q (expected forms: \"all\", \"5\", or \"1-10\")", pages)
	}
	return []string{"-f", strconv.Itoa(n), "-l", strconv.Itoa(n)}, nil
}

// ----------------------------------------------------------------------------
// docx_to_text
// ----------------------------------------------------------------------------

type DocxToTextTool struct{}

type docxToTextArgs struct {
	Path string `json:"path"`
}

func (t *DocxToTextTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "docx_to_text",
		Description: "Extract plain text from a .docx file. Prefers pandoc (`pandoc -t plain --wrap=preserve`) " +
			"for clean output; falls back to unzipping the .docx and stripping XML tags from word/document.xml " +
			"when pandoc is unavailable. Read-only; no approval. The fallback path is good enough for LLM reading " +
			"but loses tables / structure — install pandoc (`brew install pandoc`) for better fidelity. " +
			"Output capped at 100K characters.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the .docx file (absolute, or relative to session cwd).",
				},
			},
		},
		Required: []string{"path"},
	}
}

func (t *DocxToTextTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args docxToTextArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	resolved, errRes := resolveDocPath(ctx, "docx_to_text", args.Path)
	if errRes != nil {
		return *errRes, nil
	}

	// Primary: pandoc.
	if _, err := exec.LookPath("pandoc"); err == nil {
		if out, ok := runPrimary(ctx, "pandoc", []string{"-t", "plain", "--wrap=preserve", resolved}); ok {
			return agent.ToolResult{Content: "[Extracted via pandoc]\n\n" + capRunes(out)}, nil
		}
		// pandoc present but failed — fall through to fallback so the agent
		// still gets something readable instead of a hard error.
	}

	// Fallback: unzip + XML strip.
	members, err := readZipMembers(resolved, func(name string) bool {
		return name == "word/document.xml"
	})
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("[docx_to_text] failed to read .docx as zip: %v. Install pandoc for better quality: brew install pandoc", err),
			IsError: true,
		}, nil
	}
	if len(members) == 0 {
		return agent.ToolResult{
			Content: "[docx_to_text] no word/document.xml found inside file (is this really a .docx?). Install pandoc for better quality: brew install pandoc",
			IsError: true,
		}, nil
	}
	text := stripXMLTags(members[0])
	note := "[Extracted via raw XML fallback. Install pandoc for better quality: brew install pandoc]"
	return agent.ToolResult{Content: note + "\n\n" + capRunes(text)}, nil
}

func (t *DocxToTextTool) RequiresApproval() bool { return false }

func (t *DocxToTextTool) IsReadOnlyCall(string) bool { return true }

// ----------------------------------------------------------------------------
// xlsx_to_text
// ----------------------------------------------------------------------------

type XlsxToTextTool struct{}

type xlsxToTextArgs struct {
	Path  string `json:"path"`
	Sheet string `json:"sheet,omitempty"`
}

func (t *XlsxToTextTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "xlsx_to_text",
		Description: "Extract CSV/plain-text from a .xlsx workbook. Prefers xlsx2csv (`pip install xlsx2csv`); " +
			"falls back to unzipping the .xlsx and reading sharedStrings.xml + sheet XML directly. " +
			"Sheet selector: \"all\" (default, primary path emits one CSV block per sheet via `-a`), a sheet name, " +
			"or a 1-based index. The fallback path is intentionally minimal — it surfaces raw cell strings " +
			"without column/row alignment; install xlsx2csv for proper CSV. Read-only; no approval. " +
			"Output capped at 100K characters.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the .xlsx file (absolute, or relative to session cwd).",
				},
				"sheet": map[string]any{
					"type":        "string",
					"description": "Sheet selector: \"all\" (default), a sheet name (\"Sheet1\"), or a 1-based index (\"1\").",
				},
			},
		},
		Required: []string{"path"},
	}
}

func (t *XlsxToTextTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args xlsxToTextArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	resolved, errRes := resolveDocPath(ctx, "xlsx_to_text", args.Path)
	if errRes != nil {
		return *errRes, nil
	}

	// Primary: xlsx2csv.
	if _, err := exec.LookPath("xlsx2csv"); err == nil {
		cmdArgs := xlsxSheetFlags(args.Sheet)
		cmdArgs = append(cmdArgs, resolved)
		if out, ok := runPrimary(ctx, "xlsx2csv", cmdArgs); ok {
			return agent.ToolResult{Content: "[Extracted via xlsx2csv]\n\n" + capRunes(out)}, nil
		}
		// xlsx2csv installed but failed — fall through to fallback.
	}

	// Fallback: crack .xlsx zip and stitch sharedStrings + sheet XML together.
	// We intentionally don't attempt column/row alignment — quality is "raw
	// data visible to the LLM"; install xlsx2csv for proper CSV.
	text, err := xlsxFallbackText(resolved)
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("[xlsx_to_text] failed to read .xlsx as zip: %v. Install xlsx2csv for proper CSV: pip install xlsx2csv", err),
			IsError: true,
		}, nil
	}
	note := "[Extracted via raw XML fallback. Install xlsx2csv for proper CSV: pip install xlsx2csv]"
	return agent.ToolResult{Content: note + "\n\n" + capRunes(text)}, nil
}

func (t *XlsxToTextTool) RequiresApproval() bool { return false }

func (t *XlsxToTextTool) IsReadOnlyCall(string) bool { return true }

// xlsxSheetFlags maps the friendly sheet arg into xlsx2csv flags.
// "all" / "" -> -a (emit all sheets, separated). Numeric -> -s N. Name -> -n NAME.
func xlsxSheetFlags(sheet string) []string {
	s := strings.TrimSpace(sheet)
	if s == "" || strings.EqualFold(s, "all") {
		return []string{"-a"}
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return []string{"-s", strconv.Itoa(n)}
	}
	return []string{"-n", s}
}

// xlsxFallbackText reads sharedStrings.xml and every sheet under xl/worksheets/
// from the .xlsx zip, stripping XML tags. Output is grouped per sheet so the
// LLM can at least see boundaries. We don't try to reconstruct cell layout —
// install xlsx2csv for that.
func xlsxFallbackText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	var shared []byte
	var sheets []struct {
		name string
		data []byte
	}
	for _, f := range zr.File {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("open sharedStrings: %w", err)
			}
			shared, err = io.ReadAll(io.LimitReader(rc, int64(maxDocExtractRunes)*5))
			rc.Close()
			if err != nil {
				return "", fmt.Errorf("read sharedStrings: %w", err)
			}
		case strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml"):
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("open %s: %w", f.Name, err)
			}
			data, err := io.ReadAll(io.LimitReader(rc, int64(maxDocExtractRunes)*5))
			rc.Close()
			if err != nil {
				return "", fmt.Errorf("read %s: %w", f.Name, err)
			}
			sheets = append(sheets, struct {
				name string
				data []byte
			}{name: filepath.Base(f.Name), data: data})
		}
	}

	var b strings.Builder
	if len(shared) > 0 {
		b.WriteString("=== Shared strings ===\n")
		b.WriteString(stripXMLTags(shared))
		b.WriteString("\n\n")
	}
	for _, s := range sheets {
		b.WriteString("=== Sheet: ")
		b.WriteString(s.name)
		b.WriteString(" ===\n")
		b.WriteString(stripXMLTags(s.data))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String()), nil
}

// ----------------------------------------------------------------------------
// pptx_to_text
// ----------------------------------------------------------------------------

type PptxToTextTool struct{}

type pptxToTextArgs struct {
	Path string `json:"path"`
}

func (t *PptxToTextTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "pptx_to_text",
		Description: "Extract plain text from a .pptx file. Prefers pandoc (`pandoc -t plain`); " +
			"falls back to unzipping the .pptx and stripping XML tags from each ppt/slides/slide*.xml. " +
			"The fallback gives slide-by-slide raw text without preserving layout — install pandoc " +
			"(`brew install pandoc`) for better fidelity. Read-only; no approval. " +
			"Output capped at 100K characters.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the .pptx file (absolute, or relative to session cwd).",
				},
			},
		},
		Required: []string{"path"},
	}
}

func (t *PptxToTextTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args pptxToTextArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	resolved, errRes := resolveDocPath(ctx, "pptx_to_text", args.Path)
	if errRes != nil {
		return *errRes, nil
	}

	// Primary: pandoc.
	if _, err := exec.LookPath("pandoc"); err == nil {
		if out, ok := runPrimary(ctx, "pandoc", []string{"-t", "plain", resolved}); ok {
			return agent.ToolResult{Content: "[Extracted via pandoc]\n\n" + capRunes(out)}, nil
		}
	}

	// Fallback: unzip + XML strip across all slides.
	members, err := readZipMembers(resolved, func(name string) bool {
		return strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml")
	})
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("[pptx_to_text] failed to read .pptx as zip: %v. Install pandoc for better quality: brew install pandoc", err),
			IsError: true,
		}, nil
	}
	if len(members) == 0 {
		return agent.ToolResult{
			Content: "[pptx_to_text] no ppt/slides/slide*.xml entries found (is this really a .pptx?). Install pandoc for better quality: brew install pandoc",
			IsError: true,
		}, nil
	}
	var b strings.Builder
	for i, m := range members {
		fmt.Fprintf(&b, "=== Slide %d ===\n", i+1)
		b.WriteString(stripXMLTags(m))
		b.WriteString("\n\n")
	}
	note := "[Extracted via raw XML fallback. Install pandoc for better quality: brew install pandoc]"
	return agent.ToolResult{Content: note + "\n\n" + capRunes(strings.TrimSpace(b.String()))}, nil
}

func (t *PptxToTextTool) RequiresApproval() bool { return false }

func (t *PptxToTextTool) IsReadOnlyCall(string) bool { return true }
