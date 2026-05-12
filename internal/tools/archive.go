// Package tools — archive.go implements archive_inspect / archive_extract
// (plan §2 P1). Goal: let the LLM enumerate and selectively extract zip / tar
// / tar.gz archives with zero external dependencies, while preventing the
// well-known attack surface (path traversal, zipbomb, symlink races, etc.).
//
// archive_inspect: read-only, no approval; returns entry list + total size.
// archive_extract: side effects, requires approval; atomic via staging dir.
//
// Both tools support .zip, .tar, .tar.gz, .tgz. Other formats return a clear
// error; archive_extract intentionally rejects encrypted zips rather than
// silently skipping entries.
package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// Archive safety caps (plan §2 P1). Conservative defaults; we'd rather refuse
// a borderline archive than burn the user's disk on a hostile payload.
const (
	maxArchiveEntries     = 500
	maxExtractedFileSize  = 50 * 1024 * 1024  // 50 MB per entry
	maxExtractedTotalSize = 200 * 1024 * 1024 // 200 MB across entries
)

// ErrPathTraversal indicates an archive entry's resolved path escapes the
// staging directory. The check is staging-prefix-based (see plan §2 P1
// rationale: failed-extract cleanup only nukes staging, so the isolation
// boundary is staging — not dest).
var ErrPathTraversal = errors.New("archive entry would escape staging directory")

// ArchiveFormat enumerates supported archive types.
type ArchiveFormat string

const (
	ArchiveZip   ArchiveFormat = "zip"
	ArchiveTar   ArchiveFormat = "tar"
	ArchiveTarGz ArchiveFormat = "tar.gz"
)

// detectArchiveFormat sniffs the extension. We do not magic-byte sniff —
// archive_inspect / archive_extract have no business operating on arbitrary
// file types, and extension-based dispatch is what users expect.
func detectArchiveFormat(path string) (ArchiveFormat, error) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return ArchiveZip, nil
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return ArchiveTarGz, nil
	case strings.HasSuffix(lower, ".tar"):
		return ArchiveTar, nil
	default:
		return "", fmt.Errorf("unsupported archive format (need .zip / .tar / .tar.gz / .tgz)")
	}
}

// ArchiveEntry is the metadata we surface to the LLM for both inspect and
// extract responses. Permissions are surfaced as octal string for readability.
type ArchiveEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Mode     string `json:"mode,omitempty"`
	Symlink  string `json:"symlink_target,omitempty"`
	Skipped  string `json:"skipped_reason,omitempty"`
	Modified string `json:"modified,omitempty"`
}

// ---- archive_inspect ----

type ArchiveInspectTool struct{}

type archiveInspectArgs struct {
	Path string `json:"path"`
}

func (t *ArchiveInspectTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "archive_inspect",
		Description: "List the contents of a .zip / .tar / .tar.gz / .tgz archive without extracting. " +
			"Returns the entry list (name, size, is_dir, mode) and totals. Read-only; no approval required. " +
			"Useful for sizing a job before running archive_extract.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the archive file (absolute, or relative to session cwd).",
				},
			},
		},
		Required: []string{"path"},
	}
}

func (t *ArchiveInspectTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args archiveInspectArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return agent.ValidationError("path is required"), nil
	}
	resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError("archive_inspect: no session working directory is set. Pass an absolute path."), nil
		}
		return agent.ValidationError(fmt.Sprintf("archive_inspect: %v", resolveErr)), nil
	}
	args.Path = resolved

	format, err := detectArchiveFormat(args.Path)
	if err != nil {
		return agent.ValidationError(err.Error()), nil
	}

	entries, totalSize, err := inspectArchive(args.Path, format)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("archive_inspect failed: %v", err), IsError: true}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Archive: %s\nFormat: %s\nEntries: %d\nTotal uncompressed size: %d bytes\n\n",
		args.Path, format, len(entries), totalSize)
	for _, e := range entries {
		marker := "f"
		if e.IsDir {
			marker = "d"
		} else if e.Symlink != "" {
			marker = "l"
		}
		fmt.Fprintf(&sb, "%s %12d %s\t%s", marker, e.Size, e.Mode, e.Name)
		if e.Symlink != "" {
			fmt.Fprintf(&sb, " -> %s", e.Symlink)
		}
		if e.Skipped != "" {
			fmt.Fprintf(&sb, "  [WARN: %s]", e.Skipped)
		}
		sb.WriteByte('\n')
	}
	return agent.ToolResult{Content: sb.String()}, nil
}

func (t *ArchiveInspectTool) RequiresApproval() bool { return false }

func (t *ArchiveInspectTool) IsReadOnlyCall(string) bool { return true }

// ---- archive_extract ----

type ArchiveExtractTool struct{}

type archiveExtractArgs struct {
	Path      string `json:"path"`
	Dest      string `json:"dest"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

func (t *ArchiveExtractTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "archive_extract",
		Description: "Extract a .zip / .tar / .tar.gz / .tgz archive to dest. " +
			"dest must NOT exist unless overwrite=true (in which case dest is replaced atomically). " +
			"Single-layer extraction only — nested archives stay as files. Symlinks, absolute-path entries, " +
			"setuid/setgid bits, device files, and encrypted zips are rejected. Total uncompressed payload " +
			"is capped at 200 MB; per-entry at 50 MB; entries at 500.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the archive file (absolute, or relative to session cwd).",
				},
				"dest": map[string]any{
					"type":        "string",
					"description": "Destination directory to create. Must not already exist unless overwrite=true.",
				},
				"overwrite": map[string]any{
					"type":        "boolean",
					"description": "If true and dest already exists, dest is replaced atomically. Defaults to false.",
				},
			},
		},
		Required: []string{"path", "dest"},
	}
}

func (t *ArchiveExtractTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args archiveExtractArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return agent.ValidationError("path is required"), nil
	}
	if strings.TrimSpace(args.Dest) == "" {
		return agent.ValidationError("dest is required"), nil
	}

	resolvedSrc, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError("archive_extract: no session working directory is set. Pass an absolute path."), nil
		}
		return agent.ValidationError(fmt.Sprintf("archive_extract: %v", resolveErr)), nil
	}
	resolvedDest, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Dest)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError("archive_extract: no session working directory is set. Pass an absolute dest path."), nil
		}
		return agent.ValidationError(fmt.Sprintf("archive_extract: %v", resolveErr)), nil
	}

	format, err := detectArchiveFormat(resolvedSrc)
	if err != nil {
		return agent.ValidationError(err.Error()), nil
	}

	// Pre-check encrypted zips / corrupted headers BEFORE creating a staging
	// directory. This keeps the failure path clean — no orphan tmp dirs when
	// the user feeds us garbage.
	if err := preExtractValidate(resolvedSrc, format); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("archive_extract: %v", err), IsError: true}, nil
	}

	// dest must not exist unless overwrite=true (plan §2 P1: dest semantics).
	destInfo, destErr := os.Stat(resolvedDest)
	if destErr == nil {
		if !args.Overwrite {
			return agent.ValidationError(fmt.Sprintf(
				"archive_extract: dest %s already exists; pass overwrite=true to replace it atomically", resolvedDest)), nil
		}
		if !destInfo.IsDir() {
			return agent.ValidationError(fmt.Sprintf(
				"archive_extract: dest %s exists and is not a directory; refusing to replace a file", resolvedDest)), nil
		}
	} else if !os.IsNotExist(destErr) {
		return agent.ToolResult{Content: fmt.Sprintf("archive_extract: stat dest: %v", destErr), IsError: true}, nil
	}

	// Create staging dir in dest's parent directory. Putting staging INSIDE
	// dest would mean failed extractions leave half-populated dest on disk;
	// putting it next to dest means a single os.RemoveAll(staging) reverts
	// everything cleanly on failure.
	parent := filepath.Dir(resolvedDest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("archive_extract: create dest parent: %v", err), IsError: true}, nil
	}

	staging, err := makeStagingDir(parent)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("archive_extract: create staging: %v", err), IsError: true}, nil
	}
	defer os.RemoveAll(staging) // safety net; cleared on successful commit

	entries, totalBytes, extractErr := extractArchive(staging, resolvedSrc, format)
	if extractErr != nil {
		return agent.ToolResult{Content: fmt.Sprintf("archive_extract failed: %v", extractErr), IsError: true}, nil
	}

	// Commit: if overwrite, atomically swap. Otherwise rename staging → dest.
	if destErr == nil && args.Overwrite {
		// Cross-fs rename guard: if the old dest is on a different fs from
		// staging (rare — they're sibling dirs by construction, but symlinks
		// could change that), fall back to remove-then-rename.
		backup := resolvedDest + ".old-" + randomSuffix()
		if err := os.Rename(resolvedDest, backup); err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("archive_extract: pre-rename existing dest: %v", err), IsError: true}, nil
		}
		if err := os.Rename(staging, resolvedDest); err != nil {
			// Restore on failure.
			_ = os.Rename(backup, resolvedDest)
			return agent.ToolResult{Content: fmt.Sprintf("archive_extract: commit rename: %v", err), IsError: true}, nil
		}
		_ = os.RemoveAll(backup)
	} else {
		if err := os.Rename(staging, resolvedDest); err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("archive_extract: commit rename: %v", err), IsError: true}, nil
		}
	}
	// Renaming staging into place means the deferred RemoveAll(staging) above
	// becomes a no-op (path no longer exists). No need to disarm it.

	var sb strings.Builder
	fmt.Fprintf(&sb, "Extracted %d entries (%d bytes total) from %s to %s\nFormat: %s\n\n",
		len(entries), totalBytes, resolvedSrc, resolvedDest, format)
	for _, e := range entries {
		marker := "f"
		if e.IsDir {
			marker = "d"
		}
		fmt.Fprintf(&sb, "%s %12d  %s", marker, e.Size, e.Name)
		if e.Skipped != "" {
			fmt.Fprintf(&sb, "  [SKIPPED: %s]", e.Skipped)
		}
		sb.WriteByte('\n')
	}
	return agent.ToolResult{Content: sb.String()}, nil
}

func (t *ArchiveExtractTool) RequiresApproval() bool { return true }

func (t *ArchiveExtractTool) IsReadOnlyCall(string) bool { return false }

// ---- Implementation helpers ----

func makeStagingDir(parent string) (string, error) {
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(parent, ".extracting-"+randomSuffix())
		if err := os.Mkdir(candidate, 0o755); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique staging directory")
}

func randomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Crypto-rand on macOS/linux essentially cannot fail; fall back to
		// process pid to keep the failure non-fatal.
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(buf[:])
}

// preExtractValidate sniffs the archive header for fatal conditions (encrypted
// zips, bad gzip wrappers, totally unreadable file) without creating any
// staging state. It's the cheap "fail before we start scribbling on disk"
// check.
func preExtractValidate(path string, format ArchiveFormat) error {
	switch format {
	case ArchiveZip:
		zr, err := zip.OpenReader(path)
		if err != nil {
			return fmt.Errorf("open zip: %w", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			// Encrypted zip detection: zip "general purpose bit flag" 0x1.
			if f.Flags&0x1 != 0 {
				return fmt.Errorf("archive contains encrypted entries (entry %q); password-protected archives are not supported", f.Name)
			}
		}
		return nil
	case ArchiveTar:
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		tr := tar.NewReader(fh)
		// Read at least one header to catch corruption early.
		if _, err := tr.Next(); err != nil && err != io.EOF {
			return fmt.Errorf("tar header: %w", err)
		}
		return nil
	case ArchiveTarGz:
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return fmt.Errorf("gzip header: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		if _, err := tr.Next(); err != nil && err != io.EOF {
			return fmt.Errorf("tar header: %w", err)
		}
		return nil
	}
	return fmt.Errorf("unsupported archive format %q", format)
}

func inspectArchive(path string, format ArchiveFormat) ([]ArchiveEntry, int64, error) {
	switch format {
	case ArchiveZip:
		return inspectZip(path)
	case ArchiveTar, ArchiveTarGz:
		return inspectTar(path, format)
	}
	return nil, 0, fmt.Errorf("unsupported archive format %q", format)
}

func inspectZip(path string) ([]ArchiveEntry, int64, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	entries := make([]ArchiveEntry, 0, len(zr.File))
	var total int64
	for i, f := range zr.File {
		if i >= maxArchiveEntries {
			entries = append(entries, ArchiveEntry{
				Name:    fmt.Sprintf("[... %d more entries truncated ...]", len(zr.File)-i),
				Skipped: fmt.Sprintf("max entries cap = %d", maxArchiveEntries),
			})
			break
		}
		entry := ArchiveEntry{
			Name:  f.Name,
			Size:  int64(f.UncompressedSize64),
			IsDir: f.FileInfo().IsDir(),
			Mode:  fmt.Sprintf("%04o", f.Mode().Perm()),
		}
		if !f.Modified.IsZero() {
			entry.Modified = f.Modified.UTC().Format("2006-01-02T15:04:05Z")
		}
		if f.Mode()&os.ModeSymlink != 0 {
			entry.Symlink = "<unread (would be unsafe to extract)>"
		}
		if f.Flags&0x1 != 0 {
			entry.Skipped = "encrypted"
		}
		entries = append(entries, entry)
		if !entry.IsDir {
			total += entry.Size
		}
	}
	return entries, total, nil
}

func inspectTar(path string, format ArchiveFormat) ([]ArchiveEntry, int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer fh.Close()

	var tr *tar.Reader
	switch format {
	case ArchiveTarGz:
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return nil, 0, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		tr = tar.NewReader(gz)
	default:
		tr = tar.NewReader(fh)
	}

	var entries []ArchiveEntry
	var total int64
	for i := 0; ; i++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return entries, total, fmt.Errorf("tar header: %w", err)
		}
		if i >= maxArchiveEntries {
			entries = append(entries, ArchiveEntry{
				Name:    "[... entries truncated ...]",
				Skipped: fmt.Sprintf("max entries cap = %d", maxArchiveEntries),
			})
			break
		}
		entry := ArchiveEntry{
			Name:  hdr.Name,
			Size:  hdr.Size,
			IsDir: hdr.Typeflag == tar.TypeDir,
			Mode:  fmt.Sprintf("%04o", os.FileMode(hdr.Mode).Perm()),
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			entry.Symlink = hdr.Linkname
		case tar.TypeBlock, tar.TypeChar, tar.TypeFifo:
			entry.Skipped = "device/fifo entry"
		}
		if !hdr.ModTime.IsZero() {
			entry.Modified = hdr.ModTime.UTC().Format("2006-01-02T15:04:05Z")
		}
		entries = append(entries, entry)
		if entry.IsDir || entry.Symlink != "" {
			continue
		}
		total += hdr.Size
	}
	return entries, total, nil
}

func extractArchive(staging, src string, format ArchiveFormat) ([]ArchiveEntry, int64, error) {
	switch format {
	case ArchiveZip:
		return extractZip(staging, src)
	case ArchiveTar, ArchiveTarGz:
		return extractTar(staging, src, format)
	}
	return nil, 0, fmt.Errorf("unsupported archive format %q", format)
}

// validateEntryPath enforces plan §2 P1 path-traversal rule: the cleaned
// absolute path must live strictly inside the staging dir (with trailing
// separator so /staging-evil isn't mistakenly accepted as /staging prefix).
func validateEntryPath(staging, entryName string) (string, error) {
	if entryName == "" {
		return "", fmt.Errorf("empty entry name")
	}
	if filepath.IsAbs(entryName) {
		return "", fmt.Errorf("absolute path entry %q rejected", entryName)
	}
	stagingAbs, err := filepath.Abs(staging)
	if err != nil {
		return "", fmt.Errorf("staging abs: %w", err)
	}
	// filepath.Join applies Clean(), which collapses .. — but Clean can't
	// catch entries that cross the boundary, only normalize them. The prefix
	// check below is the actual escape detection.
	full := filepath.Join(staging, entryName)
	cleanedAbs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}
	if !strings.HasPrefix(cleanedAbs+string(os.PathSeparator), stagingAbs+string(os.PathSeparator)) {
		return "", ErrPathTraversal
	}
	return cleanedAbs, nil
}

func extractZip(staging, src string) ([]ArchiveEntry, int64, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, 0, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	if len(zr.File) > maxArchiveEntries {
		return nil, 0, fmt.Errorf("archive has %d entries; cap is %d", len(zr.File), maxArchiveEntries)
	}

	var entries []ArchiveEntry
	var totalBytes int64

	for _, f := range zr.File {
		// Reject things we don't process even before we touch disk.
		if f.Flags&0x1 != 0 {
			return entries, totalBytes, fmt.Errorf("entry %q is encrypted; password-protected archives unsupported", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return entries, totalBytes, fmt.Errorf("entry %q is a symlink; symlink entries are rejected for safety", f.Name)
		}
		if mode&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return entries, totalBytes, fmt.Errorf("entry %q has setuid/setgid bits; rejected for safety", f.Name)
		}
		if mode&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 {
			return entries, totalBytes, fmt.Errorf("entry %q is a device/fifo/socket; rejected for safety", f.Name)
		}

		// Path traversal validation — staging-prefix-anchored.
		full, err := validateEntryPath(staging, f.Name)
		if err != nil {
			return entries, totalBytes, fmt.Errorf("entry %q: %w", f.Name, err)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return entries, totalBytes, fmt.Errorf("mkdir %q: %w", f.Name, err)
			}
			entries = append(entries, ArchiveEntry{Name: f.Name, IsDir: true, Mode: fmt.Sprintf("%04o", mode.Perm())})
			continue
		}

		if int64(f.UncompressedSize64) > maxExtractedFileSize {
			return entries, totalBytes, fmt.Errorf("entry %q exceeds per-file cap (%d > %d)", f.Name, f.UncompressedSize64, maxExtractedFileSize)
		}
		if totalBytes+int64(f.UncompressedSize64) > maxExtractedTotalSize {
			return entries, totalBytes, fmt.Errorf("entry %q would exceed total cap (%d + %d > %d)", f.Name, totalBytes, f.UncompressedSize64, maxExtractedTotalSize)
		}

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return entries, totalBytes, fmt.Errorf("mkdir parent %q: %w", f.Name, err)
		}

		written, err := copyZipEntry(f, full, maxExtractedFileSize)
		if err != nil {
			return entries, totalBytes, fmt.Errorf("entry %q: %w", f.Name, err)
		}
		totalBytes += written
		if totalBytes > maxExtractedTotalSize {
			return entries, totalBytes, fmt.Errorf("entry %q pushed total over cap (%d > %d)", f.Name, totalBytes, maxExtractedTotalSize)
		}
		entries = append(entries, ArchiveEntry{
			Name: f.Name, Size: written, Mode: fmt.Sprintf("%04o", mode.Perm()),
		})
	}
	return entries, totalBytes, nil
}

func copyZipEntry(f *zip.File, dest string, perEntryCap int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("open entry: %w", err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create dest: %w", err)
	}
	// LimitReader caps the per-entry payload. perEntryCap+1 so we can tell
	// whether the source actually exceeded the limit (n > cap → bomb).
	n, copyErr := io.Copy(out, io.LimitReader(rc, perEntryCap+1))
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dest)
		return 0, fmt.Errorf("copy: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(dest)
		return 0, fmt.Errorf("close: %w", closeErr)
	}
	if n > perEntryCap {
		os.Remove(dest)
		return 0, fmt.Errorf("entry exceeds per-file cap (%d > %d) — potential zipbomb", n, perEntryCap)
	}
	// Sanity check vs. the header's uncompressed size when present. zip
	// headers are not authoritative but a wide mismatch is suspicious.
	return n, nil
}

func extractTar(staging, src string, format ArchiveFormat) ([]ArchiveEntry, int64, error) {
	fh, err := os.Open(src)
	if err != nil {
		return nil, 0, err
	}
	defer fh.Close()

	var tr *tar.Reader
	switch format {
	case ArchiveTarGz:
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return nil, 0, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		tr = tar.NewReader(gz)
	default:
		tr = tar.NewReader(fh)
	}

	var entries []ArchiveEntry
	var totalBytes int64
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return entries, totalBytes, fmt.Errorf("tar header: %w", err)
		}
		count++
		if count > maxArchiveEntries {
			return entries, totalBytes, fmt.Errorf("archive exceeds entry cap %d", maxArchiveEntries)
		}

		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			return entries, totalBytes, fmt.Errorf("entry %q is a symlink/hardlink; rejected for safety", hdr.Name)
		case tar.TypeBlock, tar.TypeChar, tar.TypeFifo:
			return entries, totalBytes, fmt.Errorf("entry %q is a device/fifo; rejected for safety", hdr.Name)
		}
		mode := os.FileMode(hdr.Mode)
		if mode&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return entries, totalBytes, fmt.Errorf("entry %q has setuid/setgid bits; rejected for safety", hdr.Name)
		}

		full, err := validateEntryPath(staging, hdr.Name)
		if err != nil {
			return entries, totalBytes, fmt.Errorf("entry %q: %w", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return entries, totalBytes, fmt.Errorf("mkdir %q: %w", hdr.Name, err)
			}
			entries = append(entries, ArchiveEntry{Name: hdr.Name, IsDir: true, Mode: fmt.Sprintf("%04o", mode.Perm())})
			continue
		case tar.TypeReg:
		default:
			// Unknown type — skip it rather than die (most archives have only
			// regular + dir; sparse / xattr extension types we can ignore).
			entries = append(entries, ArchiveEntry{Name: hdr.Name, Skipped: fmt.Sprintf("unsupported tar typeflag %d", hdr.Typeflag)})
			continue
		}

		if hdr.Size > maxExtractedFileSize {
			return entries, totalBytes, fmt.Errorf("entry %q exceeds per-file cap (%d > %d)", hdr.Name, hdr.Size, maxExtractedFileSize)
		}
		if totalBytes+hdr.Size > maxExtractedTotalSize {
			return entries, totalBytes, fmt.Errorf("entry %q would exceed total cap (%d + %d > %d)", hdr.Name, totalBytes, hdr.Size, maxExtractedTotalSize)
		}

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return entries, totalBytes, fmt.Errorf("mkdir parent %q: %w", hdr.Name, err)
		}
		out, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return entries, totalBytes, fmt.Errorf("create %q: %w", hdr.Name, err)
		}
		n, copyErr := io.Copy(out, io.LimitReader(tr, maxExtractedFileSize+1))
		closeErr := out.Close()
		if copyErr != nil {
			os.Remove(full)
			return entries, totalBytes, fmt.Errorf("copy %q: %w", hdr.Name, copyErr)
		}
		if closeErr != nil {
			os.Remove(full)
			return entries, totalBytes, fmt.Errorf("close %q: %w", hdr.Name, closeErr)
		}
		if n > maxExtractedFileSize {
			os.Remove(full)
			return entries, totalBytes, fmt.Errorf("entry %q exceeds per-file cap (%d > %d) — potential zipbomb", hdr.Name, n, maxExtractedFileSize)
		}
		totalBytes += n
		if totalBytes > maxExtractedTotalSize {
			os.Remove(full)
			return entries, totalBytes, fmt.Errorf("entry %q pushed total over cap (%d > %d)", hdr.Name, totalBytes, maxExtractedTotalSize)
		}
		entries = append(entries, ArchiveEntry{Name: hdr.Name, Size: n, Mode: fmt.Sprintf("%04o", mode.Perm())})
	}
	return entries, totalBytes, nil
}
