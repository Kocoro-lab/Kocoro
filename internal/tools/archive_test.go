package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// helper: build a zip file at path with the given (name -> bytes) map. Names
// MAY contain "/" path separators. Files matching the predicate
// markAsSymlink are written with the symlink Mode bit set so we can exercise
// the rejection path.
func writeTestZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer fh.Close()
	zw := zip.NewWriter(fh)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create(%q): %v", name, err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// writeTestZipEncrypted produces a zip whose first entry has the encrypted
// flag (0x1) set on the local header. We don't actually encrypt the bytes —
// archive_extract should refuse based on the flag alone.
func writeTestZipEncrypted(t *testing.T, path string) {
	t.Helper()
	// Build a normal zip and then byte-patch the flag. archive/zip writer
	// doesn't expose encryption; this is the cheapest way to force the flag.
	plain := filepath.Join(t.TempDir(), "plain.zip")
	writeTestZip(t, plain, map[string][]byte{"a.txt": []byte("hello")})

	raw, err := os.ReadFile(plain)
	if err != nil {
		t.Fatalf("read plain: %v", err)
	}
	// Local file header signature: 0x04034b50. Flags at offset +6 (uint16,
	// little-endian). We set bit 0 to force encrypted.
	sig := []byte{0x50, 0x4b, 0x03, 0x04}
	idx := bytes.Index(raw, sig)
	if idx < 0 {
		t.Fatalf("could not locate local header signature in plain zip")
	}
	raw[idx+6] |= 0x01
	// Also patch the central directory header (sig 0x02014b50) flags at +8.
	cdSig := []byte{0x50, 0x4b, 0x01, 0x02}
	cdIdx := bytes.Index(raw, cdSig)
	if cdIdx < 0 {
		t.Fatalf("could not locate central directory signature")
	}
	raw[cdIdx+8] |= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write encrypted zip: %v", err)
	}
}

func writeTestTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tgz: %v", err)
	}
	defer fh.Close()
	gz := gzip.NewWriter(fh)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     e.Mode,
			Size:     int64(len(e.Body)),
			Typeflag: e.Type,
			Linkname: e.LinkName,
		}
		if e.Type == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if e.Type == tar.TypeReg || e.Type == 0 {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatalf("tar write: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
}

type tarEntry struct {
	Name     string
	Mode     int64
	Body     []byte
	Type     byte
	LinkName string
}

func runExtract(t *testing.T, src, dest string) (agent.ToolResult, error) {
	t.Helper()
	tool := &ArchiveExtractTool{}
	args, _ := json.Marshal(map[string]any{"path": src, "dest": dest})
	return tool.Run(context.Background(), string(args))
}

func runInspect(t *testing.T, src string) (agent.ToolResult, error) {
	t.Helper()
	tool := &ArchiveInspectTool{}
	args, _ := json.Marshal(map[string]any{"path": src})
	return tool.Run(context.Background(), string(args))
}

// ---- Happy path ----

func TestArchiveInspect_Zip_Basic(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.zip")
	writeTestZip(t, src, map[string][]byte{
		"a.txt":     []byte("hello"),
		"sub/b.txt": []byte("world"),
	})
	res, err := runInspect(t, src)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Entries: 2") {
		t.Errorf("expected entry count in output; got %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.txt") || !strings.Contains(res.Content, "sub/b.txt") {
		t.Errorf("missing entry name; got %s", res.Content)
	}
}

func TestArchiveExtract_Zip_Basic(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "x.zip")
	writeTestZip(t, src, map[string][]byte{
		"a.txt":     []byte("hello"),
		"sub/b.txt": []byte("world"),
	})
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "out")
	res, err := runExtract(t, src, dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(got) != "hello" {
		t.Errorf("a.txt mismatch: %q", string(got))
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "sub", "b.txt")); string(got) != "world" {
		t.Errorf("sub/b.txt mismatch: %q", string(got))
	}
}

func TestArchiveExtract_TarGz_Basic(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.tar.gz")
	writeTestTarGz(t, src, []tarEntry{
		{Name: "foo.txt", Mode: 0o644, Body: []byte("foo")},
		{Name: "dir/", Mode: 0o755, Type: tar.TypeDir},
		{Name: "dir/bar.txt", Mode: 0o644, Body: []byte("bar")},
	})
	dest := filepath.Join(t.TempDir(), "out")
	res, err := runExtract(t, src, dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "foo.txt")); string(got) != "foo" {
		t.Errorf("foo.txt mismatch: %q", string(got))
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "dir/bar.txt")); string(got) != "bar" {
		t.Errorf("dir/bar.txt mismatch: %q", string(got))
	}
}

// ---- dest semantics ----

func TestArchiveExtract_DestMustNotExist(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.zip")
	writeTestZip(t, src, map[string][]byte{"a.txt": []byte("hi")})
	dest := t.TempDir() // already exists
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected validation error when dest exists; got %s", res.Content)
	}
	if !strings.Contains(res.Content, "overwrite=true") {
		t.Errorf("error should mention overwrite hint; got %s", res.Content)
	}
}

func TestArchiveExtract_OverwriteAtomicSwap(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.zip")
	writeTestZip(t, src, map[string][]byte{"a.txt": []byte("v2")})
	dest := t.TempDir()
	// seed with v1 content
	if err := os.WriteFile(filepath.Join(dest, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ArchiveExtractTool{}
	args, _ := json.Marshal(map[string]any{"path": src, "dest": dest, "overwrite": true})
	res, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dest, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old content should be replaced; old.txt still exists: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(got) != "v2" {
		t.Errorf("a.txt mismatch after overwrite: %q", string(got))
	}
}

// ---- Path traversal ----

func TestArchiveExtract_PathTraversalRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "evil.zip")
	writeTestZip(t, src, map[string][]byte{
		"../escape.txt": []byte("pwn"),
	})
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for path traversal, got %s", res.Content)
	}
	if !strings.Contains(res.Content, "escape") && !strings.Contains(res.Content, "staging directory") {
		t.Errorf("error should describe traversal; got %s", res.Content)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest should not exist after rejected extract: %v", err)
	}
}

func TestArchiveExtract_AbsolutePathRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "abs.zip")
	writeTestZip(t, src, map[string][]byte{
		"/etc/passwd": []byte("nope"),
	})
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for absolute path, got %s", res.Content)
	}
	if !strings.Contains(res.Content, "absolute") {
		t.Errorf("error should mention absolute path; got %s", res.Content)
	}
}

// ---- Symlink rejection (tar; zip symlinks are tricky to author in pure Go) ----

func TestArchiveExtract_TarSymlinkRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "syms.tar.gz")
	writeTestTarGz(t, src, []tarEntry{
		{Name: "link", Type: tar.TypeSymlink, LinkName: "/etc/passwd"},
	})
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for symlink entry, got %s", res.Content)
	}
	if !strings.Contains(res.Content, "symlink") {
		t.Errorf("error should mention symlink; got %s", res.Content)
	}
}

func TestArchiveExtract_TarDeviceRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "dev.tar.gz")
	writeTestTarGz(t, src, []tarEntry{
		{Name: "fifo", Type: tar.TypeFifo},
	})
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for fifo entry, got %s", res.Content)
	}
	if !strings.Contains(res.Content, "device") {
		t.Errorf("error should mention device/fifo; got %s", res.Content)
	}
}

// ---- Encrypted zip rejection ----

func TestArchiveExtract_EncryptedZipRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "enc.zip")
	writeTestZipEncrypted(t, src)
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for encrypted zip, got %s", res.Content)
	}
	if !strings.Contains(strings.ToLower(res.Content), "encrypt") {
		t.Errorf("error should mention encryption; got %s", res.Content)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest must not be created when pre-validate fails: %v", err)
	}
}

// ---- Corrupt headers ----

func TestArchiveExtract_CorruptHeaderRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "corrupt.zip")
	if err := os.WriteFile(src, []byte("not a zip file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for corrupt zip, got %s", res.Content)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest must not be created when pre-validate fails: %v", err)
	}
}

// ---- Zipbomb ----

func TestArchiveExtract_ZipbombSingleFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bomb.zip")
	// Create a zip with one entry whose declared body exceeds per-file cap.
	fh, _ := os.Create(src)
	zw := zip.NewWriter(fh)
	w, _ := zw.Create("big.bin")
	// Write maxExtractedFileSize+1 bytes (zeros — compresses to tiny on disk).
	big := bytes.Repeat([]byte("a"), maxExtractedFileSize+1024)
	w.Write(big)
	zw.Close()
	fh.Close()

	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for oversized entry, got %s", res.Content)
	}
	if !strings.Contains(res.Content, "cap") {
		t.Errorf("error should mention cap; got %s", res.Content)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest should not exist after rejected extract: %v", err)
	}
}

func TestArchiveExtract_ZipbombTotalCap(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bomb.zip")
	fh, _ := os.Create(src)
	zw := zip.NewWriter(fh)
	// Author multiple entries each under the per-file cap but totaling over
	// the global cap. perFile=40MB, totalCap=200MB → 6 entries triggers it.
	perFile := bytes.Repeat([]byte("a"), 40*1024*1024)
	for i := 0; i < 6; i++ {
		w, _ := zw.Create(fmt.Sprintf("f%d.bin", i))
		w.Write(perFile)
	}
	zw.Close()
	fh.Close()

	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for total-size bomb, got %s", res.Content)
	}
	if !strings.Contains(res.Content, "total cap") {
		t.Errorf("error should mention total cap; got %s", res.Content)
	}
}

// ---- Nested archives stay as files ----

func TestArchiveExtract_NestedArchiveSingleLayer(t *testing.T) {
	innerPath := filepath.Join(t.TempDir(), "inner.zip")
	writeTestZip(t, innerPath, map[string][]byte{"inner.txt": []byte("inner content")})
	innerBytes, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatal(err)
	}
	outerPath := filepath.Join(t.TempDir(), "outer.zip")
	writeTestZip(t, outerPath, map[string][]byte{
		"nested.zip": innerBytes,
		"top.txt":    []byte("top content"),
	})
	dest := filepath.Join(t.TempDir(), "out")
	res, err := runExtract(t, outerPath, dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	// nested.zip must exist as a file (not recursively extracted)
	info, err := os.Stat(filepath.Join(dest, "nested.zip"))
	if err != nil {
		t.Fatalf("nested.zip should be a file: %v", err)
	}
	if info.IsDir() {
		t.Errorf("nested.zip should NOT have been recursively extracted")
	}
	// And its bytes should match the original inner zip.
	got, _ := os.ReadFile(filepath.Join(dest, "nested.zip"))
	if !bytes.Equal(got, innerBytes) {
		t.Errorf("nested zip bytes corrupted by extraction")
	}
}

// ---- Format detection ----

func TestArchiveExtract_UnsupportedFormat(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.rar")
	os.WriteFile(src, []byte("Rar!"), 0o600)
	dest := filepath.Join(t.TempDir(), "out")
	res, _ := runExtract(t, src, dest)
	if !res.IsError {
		t.Fatalf("expected error for unsupported format; got %s", res.Content)
	}
	if !strings.Contains(res.Content, "unsupported") {
		t.Errorf("error should mention unsupported; got %s", res.Content)
	}
}

// ---- validateEntryPath unit test ----

func TestValidateEntryPath(t *testing.T) {
	staging := t.TempDir()
	cases := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{"simple file", "foo.txt", false},
		{"nested file", "a/b/c.txt", false},
		{"current dir prefix", "./a/b.txt", false},
		{"parent escape", "../foo", true},
		{"deep escape", "a/../../foo", true},
		{"absolute", "/etc/passwd", true},
		{"hidden file", ".env", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateEntryPath(staging, tc.entry)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateEntryPath(%q) err=%v, wantErr=%v", tc.entry, err, tc.wantErr)
			}
		})
	}
}

// ---- ToolInfo ----

func TestArchiveTools_ToolInfo(t *testing.T) {
	inspect := (&ArchiveInspectTool{}).Info()
	extract := (&ArchiveExtractTool{}).Info()

	if inspect.Name != "archive_inspect" {
		t.Errorf("inspect tool name = %q", inspect.Name)
	}
	if extract.Name != "archive_extract" {
		t.Errorf("extract tool name = %q", extract.Name)
	}
	if (&ArchiveInspectTool{}).RequiresApproval() {
		t.Error("archive_inspect must not require approval (read-only)")
	}
	if !(&ArchiveExtractTool{}).RequiresApproval() {
		t.Error("archive_extract must require approval (mutates fs)")
	}
}

