package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

func pdJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPresentDeliverable_ValidFileUnderCWD(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "q3-deck.pptx")
	if err := os.WriteFile(file, []byte("PK\x03\x04 fake pptx bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got *Deliverable
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	ctx = WithDeliverableHandler(ctx, func(d Deliverable) bool {
		dd := d
		got = &dd
		return true
	})

	res, err := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{
		"path":  "q3-deck.pptx",
		"title": "Q3 Sales Deck",
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %s", res.Content)
	}
	if got == nil {
		t.Fatal("deliverable handler was not invoked")
	}
	if got.Path != file {
		t.Errorf("path = %q, want %q", got.Path, file)
	}
	if got.Filename != "q3-deck.pptx" {
		t.Errorf("filename = %q, want q3-deck.pptx", got.Filename)
	}
	if got.Title != "Q3 Sales Deck" {
		t.Errorf("title = %q, want Q3 Sales Deck", got.Title)
	}
	want := "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	if got.MIME != want {
		t.Errorf("mime = %q, want %q", got.MIME, want)
	}
	if got.ByteSize == 0 {
		t.Error("byte_size should be > 0")
	}
	if !strings.HasPrefix(got.ID, "dlv_") {
		t.Errorf("id = %q, want dlv_ prefix", got.ID)
	}
	var parsed presentDeliverableResult
	if err := json.Unmarshal([]byte(res.Content), &parsed); err != nil {
		t.Fatalf("result content is not pure JSON: %v (content=%s)", err, res.Content)
	}
	if !parsed.Delivered {
		t.Errorf("delivered = false, want true")
	}
	if parsed.Deliverable.Path != file {
		t.Errorf("result path = %q, want %q", parsed.Deliverable.Path, file)
	}
}

func TestPresentDeliverable_TitleDefaultsToFilename(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got *Deliverable
	ctx := WithDeliverableHandler(cwdctx.WithSessionCWD(context.Background(), dir), func(d Deliverable) bool {
		dd := d
		got = &dd
		return true
	})
	res, _ := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{"path": "report.pdf"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if got.Title != "report.pdf" {
		t.Errorf("title = %q, want report.pdf (filename fallback)", got.Title)
	}
	if got.MIME != "application/pdf" {
		t.Errorf("mime = %q, want application/pdf", got.MIME)
	}
}

func TestPresentDeliverable_AcceptsFileOutsideCWD(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir() // sibling temp dir, NOT under the session CWD `dir`
	file := filepath.Join(outside, "deck.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got *Deliverable
	ctx := WithDeliverableHandler(cwdctx.WithSessionCWD(context.Background(), dir), func(d Deliverable) bool {
		dd := d
		got = &dd
		return true
	})
	// The CWD restriction was removed: a real regular file outside the session
	// working directory is accepted (still vouched as a real file).
	res, _ := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{"path": file}))
	if res.IsError {
		t.Fatalf("expected acceptance for an out-of-CWD real file, got error: %s", res.Content)
	}
	got2 := got
	if got2 == nil {
		t.Fatal("handler should be called for an out-of-CWD real file")
	}
	if got2.Path != file {
		t.Errorf("path = %q, want %q", got2.Path, file)
	}
}

func TestPresentDeliverable_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	res, _ := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{"path": "nope.pdf"}))
	if !res.IsError {
		t.Fatalf("expected error for missing file, got: %s", res.Content)
	}
}

func TestPresentDeliverable_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	res, _ := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{"path": "sub"}))
	if !res.IsError {
		t.Fatalf("expected error for a directory, got: %s", res.Content)
	}
}

func TestPresentDeliverable_RejectsRelativeWithoutCWD(t *testing.T) {
	res, _ := (&PresentDeliverableTool{}).Run(context.Background(), pdJSON(t, map[string]any{"path": "x.pdf"}))
	if !res.IsError {
		t.Fatalf("expected error without a session CWD, got: %s", res.Content)
	}
}

func TestPresentDeliverable_AcceptsSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.pdf")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.pdf")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	called := false
	ctx := WithDeliverableHandler(cwdctx.WithSessionCWD(context.Background(), dir), func(d Deliverable) bool {
		called = true
		return true
	})
	res, _ := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{"path": "link.pdf"}))
	// CWD restriction removed: a symlink resolving to a real file is accepted.
	if res.IsError {
		t.Fatalf("expected acceptance of a symlinked real file, got error: %s", res.Content)
	}
	if !called {
		t.Error("handler should be called for a symlinked real file")
	}
}

func TestPresentDeliverable_NoHandlerStillRecordsParseableMetadata(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "page.html")
	if err := os.WriteFile(file, []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := cwdctx.WithSessionCWD(context.Background(), dir) // no handler injected
	res, err := (&PresentDeliverableTool{}).Run(ctx, pdJSON(t, map[string]any{"path": "page.html"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("expected success without a handler, got: %s", res.Content)
	}
	var parsed presentDeliverableResult
	if err := json.Unmarshal([]byte(res.Content), &parsed); err != nil {
		t.Fatalf("metadata not parseable: %v (content=%s)", err, res.Content)
	}
	if parsed.Delivered {
		t.Error("delivered = true, want false without a handler")
	}
	if parsed.Deliverable.Filename != "page.html" {
		t.Errorf("filename = %q, want page.html", parsed.Deliverable.Filename)
	}
	if parsed.Deliverable.MIME != "text/html" {
		t.Errorf("mime = %q, want text/html", parsed.Deliverable.MIME)
	}
}
