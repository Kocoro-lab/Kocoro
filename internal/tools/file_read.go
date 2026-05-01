package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// imageReadExtensions are file extensions that file_read returns as vision image blocks.
var imageReadExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// maxImageReadSize is the maximum file size for image reads (20 MB).
const maxImageReadSize = 20 * 1024 * 1024

// maxPDFPages is the maximum number of pages rendered per file_read call.
// Kept low (2) to avoid exceeding gateway body size limits — each page is
// ~100-300KB as JPEG. Agent can use offset to read more pages.
const maxPDFPages = 2

// pdfPageMaxDim is the max pixel dimension for rendered PDF pages.
const pdfPageMaxDim = 1024

// fileReadMaxTokens is the hard cap on text file_read output. Files (or
// offset+limit slices) whose estimated token count exceeds this return an
// error pointing the agent at offset/limit instead of letting the loop's
// 50K spill fallback drop a 2K preview into context. ~100B error vs ~2K
// spill preview ≈ 95% per-call savings on oversized reads. Mirrors CC's
// DEFAULT_MAX_OUTPUT_TOKENS in src/tools/FileReadTool/limits.ts:18.
const fileReadMaxTokens = 25000

const fileReadNoLimitMaxBytes = 256 * 1024

type FileReadTool struct{}

type fileReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (t *FileReadTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:               "file_read",
		MaxResultSizeChars: agent.UnlimitedToolResultSizeChars,
		Description:        "Read a file's contents with line numbers. Use offset and limit for large files. Repeat reads of the same (path, offset, limit) within one session return a short \"unchanged since last read\" stub when the file has not been modified — to force a fresh read, modify the file or pass a different offset/limit range. For image files (png/jpg/gif/webp), returns the image for vision analysis. For PDF files, renders pages as images for vision analysis (use offset for start page, limit for max pages).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Absolute or relative file path"},
				"offset": map[string]any{"type": "integer", "description": "Start line (0-based, default 0). For PDF: start page (0-based)."},
				"limit":  map[string]any{"type": "integer", "description": "Max lines to read (default: all). For PDF: max pages to render (default: 2)."},
			},
		},
		Required: []string{"path"},
	}
}

func (t *FileReadTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args fileReadArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}
	resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"file_read: no session working directory is set. Pass an absolute path.",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("file_read: %v", resolveErr)), nil
	}
	args.Path = resolved

	// Image files: return as vision image block instead of text lines.
	ext := strings.ToLower(filepath.Ext(args.Path))
	if imageReadExtensions[ext] {
		return t.readImage(args.Path)
	}

	// PDF files: render pages as images for vision analysis.
	if ext == ".pdf" {
		return t.readPDF(args.Path, args.Offset, args.Limit)
	}

	// Dedup check: when the same (path, offset, limit) was read earlier in
	// the session AND the file's mtime+size are unchanged, return a ~120B
	// stub instead of replaying the full content. Skipped above for
	// images/PDFs (they return image blocks, not text). No-op when there's
	// no ReadTracker in context (tool called outside the agent loop).
	info, statErr := os.Stat(args.Path)
	if statErr == nil {
		if hit, stub := agent.CheckFileReadDedup(ctx, args.Path, args.Offset, args.Limit, info.ModTime(), info.Size()); hit {
			return agent.ToolResult{Content: stub}, nil
		}
	}

	start := args.Offset
	if start < 0 {
		start = 0
	}
	if args.Limit <= 0 && statErr == nil && info.Size() > fileReadNoLimitMaxBytes {
		return agent.ToolResult{
			IsError: true,
			Content: fmt.Sprintf(
				"file_read: file is too large (%d bytes). Use offset+limit to read a smaller range, e.g. {\"offset\":0,\"limit\":200}.",
				info.Size(),
			),
		}, nil
	}

	var (
		lines      []string
		totalLines int
		err        error
	)
	if args.Limit > 0 {
		lines, totalLines, _, err = readTextLineRange(args.Path, start, args.Limit)
		if err != nil {
			if os.IsPermission(err) {
				return agent.PermissionError(fmt.Sprintf("cannot read %s: permission denied", args.Path)), nil
			}
			return agent.ToolResult{Content: fmt.Sprintf("error reading file: %v", err), IsError: true}, nil
		}
	} else {
		data, err := os.ReadFile(args.Path)
		if err != nil {
			if os.IsPermission(err) {
				return agent.PermissionError(fmt.Sprintf("cannot read %s: permission denied", args.Path)), nil
			}
			return agent.ToolResult{Content: fmt.Sprintf("error reading file: %v", err), IsError: true}, nil
		}
		lines = strings.Split(string(data), "\n")
		totalLines = len(lines)
		if start > len(lines) {
			start = len(lines)
		}
	}

	// Estimate output tokens on the requested slice (NOT the whole file —
	// asking for limit=100 of a 10K-line file should succeed). chars/3 is
	// a coarse but safe estimate for English/code text.
	var sliceChars int
	for i := range lines {
		sliceChars += len(lines[i]) + 1 // +1 for newline
	}
	if estTokens := sliceChars / 3; estTokens > fileReadMaxTokens {
		return agent.ToolResult{
			IsError: true,
			Content: fmt.Sprintf(
				"file_read: requested range too large (~%d tokens, max %d). File has %d lines; use offset+limit to read smaller chunks (e.g. limit=200 reads ~200 lines).",
				estTokens, fileReadMaxTokens, totalLines,
			),
		}, nil
	}

	var sb strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&sb, "%4d | %s\n", start+i+1, line)
	}
	// Record this read for future dedup. Stat may have failed earlier
	// (race with file removal); skip recording in that case.
	if statErr == nil {
		agent.RecordFileRead(ctx, args.Path, args.Offset, args.Limit, info.ModTime(), info.Size())
	}
	return agent.ToolResult{Content: sb.String()}, nil
}

func readTextLineRange(path string, offset, limit int) (lines []string, totalLines int, reachedEOF bool, err error) {
	if offset < 0 {
		offset = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	end := -1
	if limit > 0 {
		end = offset + limit
	}
	for scanner.Scan() {
		if end >= 0 && totalLines >= end {
			return lines, totalLines, false, scanner.Err()
		}
		text := scanner.Text()
		if totalLines >= offset {
			lines = append(lines, text)
		}
		totalLines++
	}
	if err := scanner.Err(); err != nil {
		return nil, totalLines, false, err
	}
	return lines, totalLines, true, nil
}

// readImage reads an image file and returns it as a vision-compatible image block.
func (t *FileReadTool) readImage(path string) (agent.ToolResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot read %s: permission denied", path)), nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("error reading image: %v", err), IsError: true}, nil
	}
	if info.Size() > maxImageReadSize {
		return agent.ToolResult{
			Content: fmt.Sprintf("image too large (%d bytes, max %d bytes). Resize the image first, then retry.", info.Size(), maxImageReadSize),
			IsError: true,
		}, nil
	}

	block, err := EncodeImage(path)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("error encoding image: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{
		Content: fmt.Sprintf("[Image: %s (%d bytes)]", filepath.Base(path), info.Size()),
		Images:  []agent.ImageBlock{block},
	}, nil
}

// readPDF renders PDF pages to images using macOS PDFKit (via swift) and returns
// them as vision-compatible image blocks. startPage is 0-based, maxPages defaults
// to maxPDFPages. This uses the macOS-native Swift PDFKit which is always available.
func (t *FileReadTool) readPDF(path string, startPage, maxPages int) (agent.ToolResult, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot read %s: permission denied", path)), nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("error reading PDF: %v", err), IsError: true}, nil
	}

	if startPage < 0 {
		startPage = 0
	}
	if maxPages <= 0 || maxPages > maxPDFPages {
		maxPages = maxPDFPages
	}

	tmpDir, err := os.MkdirTemp("", "shannon-pdf-*")
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("error creating temp dir: %v", err), IsError: true}, nil
	}
	defer os.RemoveAll(tmpDir)

	// Swift script that renders PDF pages to PNG using PDFKit.
	swiftCode := fmt.Sprintf(`
import PDFKit
import AppKit

let url = URL(fileURLWithPath: %q)
guard let doc = PDFDocument(url: url) else {
    print("ERROR:cannot open PDF (file may be corrupted or password-protected)")
    exit(1)
}
if doc.isEncrypted && !doc.unlock(withPassword: "") {
    print("ERROR:PDF is password-protected")
    exit(1)
}
print("PAGES:\(doc.pageCount)")

let start = %d
let count = min(%d, doc.pageCount - start)
if start >= doc.pageCount {
    print("ERROR:offset \(start) exceeds page count \(doc.pageCount)")
    exit(1)
}

for i in start..<(start + count) {
    guard let page = doc.page(at: i) else { continue }
    let bounds = page.bounds(for: .mediaBox)
    let scale: CGFloat = 2.0
    let maxDim: CGFloat = 8192
    let width = Int(min(bounds.width * scale, maxDim))
    let height = Int(min(bounds.height * scale, maxDim))

    let image = NSImage(size: NSSize(width: width, height: height))
    image.lockFocus()
    if let ctx = NSGraphicsContext.current?.cgContext {
        ctx.setFillColor(NSColor.white.cgColor)
        ctx.fill(CGRect(x: 0, y: 0, width: width, height: height))
        ctx.scaleBy(x: scale, y: scale)
        page.draw(with: .mediaBox, to: ctx)
    }
    image.unlockFocus()

    if let tiff = image.tiffRepresentation,
       let rep = NSBitmapImageRep(data: tiff),
       let jpg = rep.representation(using: .jpeg, properties: [.compressionFactor: 0.8]) {
        let outPath = %q + "/page_\(i).jpg"
        do {
            try jpg.write(to: URL(fileURLWithPath: outPath))
            print("RENDERED:\(outPath)")
        } catch {
            print("ERROR:failed to write page \(i): \(error)")
        }
    }
}
`, path, startPage, maxPages, tmpDir)

	if _, lookErr := exec.LookPath("swift"); lookErr != nil {
		return agent.ToolResult{
			Content: "PDF rendering requires macOS with Xcode Command Line Tools (swift not found). Use bash with python/pymupdf to extract content instead.",
			IsError: true,
		}, nil
	}

	out, err := exec.Command("swift", "-e", swiftCode).CombinedOutput()
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("PDF rendering failed: %v\n%s", err, string(out)),
			IsError: true,
		}, nil
	}

	// Parse output: PAGES:<n> and RENDERED:<path> lines.
	var totalPages int
	var renderedPaths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PAGES:") {
			totalPages, _ = strconv.Atoi(strings.TrimPrefix(line, "PAGES:"))
		} else if strings.HasPrefix(line, "RENDERED:") {
			renderedPaths = append(renderedPaths, strings.TrimPrefix(line, "RENDERED:"))
		} else if strings.HasPrefix(line, "ERROR:") {
			return agent.ToolResult{
				Content: fmt.Sprintf("PDF error: %s", strings.TrimPrefix(line, "ERROR:")),
				IsError: true,
			}, nil
		}
	}

	if len(renderedPaths) == 0 {
		return agent.ToolResult{Content: "PDF rendered no pages", IsError: true}, nil
	}

	// Resize and encode each rendered page as an image block.
	var images []agent.ImageBlock
	for _, p := range renderedPaths {
		// Resize to keep base64 size reasonable for gateway body limits.
		// If resize fails, continue with the original (larger) image.
		if err := ResizeImage(p, pdfPageMaxDim); err != nil {
			log.Printf("WARNING: PDF page resize failed: %v", err)
		}
		block, encErr := EncodeImage(p)
		if encErr != nil {
			log.Printf("WARNING: PDF page encode failed: %v", encErr)
			continue
		}
		images = append(images, block)
	}

	if len(images) == 0 {
		return agent.ToolResult{
			Content: fmt.Sprintf("PDF pages rendered but failed to encode (%d pages)", len(renderedPaths)),
			IsError: true,
		}, nil
	}

	// Build summary text.
	var sb strings.Builder
	fmt.Fprintf(&sb, "[PDF: %s — %d total pages, showing pages %d–%d]",
		filepath.Base(path), totalPages, startPage+1, startPage+len(images))
	if skipped := len(renderedPaths) - len(images); skipped > 0 {
		fmt.Fprintf(&sb, "\n[Warning: %d page(s) failed to encode and were skipped]", skipped)
	}
	if startPage+len(images) < totalPages {
		fmt.Fprintf(&sb, "\n[Use offset=%d to read the next pages]", startPage+len(images))
	}

	return agent.ToolResult{
		Content: sb.String(),
		Images:  images,
	}, nil
}

func (t *FileReadTool) RequiresApproval() bool { return true }

func (t *FileReadTool) IsReadOnlyCall(string) bool { return true }

func (t *FileReadTool) IsSafeArgs(argsJSON string) bool {
	var args fileReadArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	return isPathUnderCWD(args.Path)
}

func (t *FileReadTool) IsSafeArgsWithContext(ctx context.Context, argsJSON string) bool {
	var args fileReadArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	return isPathUnderSessionCWD(ctx, args.Path)
}
