package tools

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// ----------------------------------------------------------------------------
// Sample-file builders. The four Office formats are all "zip + XML", so we
// construct minimal but valid containers in-memory at test time. This avoids
// committing binary blobs and keeps the test repeatable.
// ----------------------------------------------------------------------------

// writeDocx creates the smallest valid .docx containing the given paragraph
// text at `path`. Only word/document.xml is populated; the minimal package
// is enough for both the unzip+strip fallback and a real `pandoc` if present
// (pandoc is fairly lenient about missing parts on read).
func writeDocx(t *testing.T, path, paragraph string) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create docx: %v", err)
	}
	defer fh.Close()
	zw := zip.NewWriter(fh)
	must := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip Write(%q): %v", name, err)
		}
	}
	must("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)
	must("_rels/.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`)
	must("word/document.xml", `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>`+paragraph+`</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// writeXlsx creates a minimal .xlsx with one shared string and one sheet
// referencing it via cell A1.
func writeXlsx(t *testing.T, path, cell string) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create xlsx: %v", err)
	}
	defer fh.Close()
	zw := zip.NewWriter(fh)
	must := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip Write(%q): %v", name, err)
		}
	}
	must("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`)
	must("_rels/.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`)
	must("xl/workbook.xml", `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	must("xl/_rels/workbook.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/></Relationships>`)
	must("xl/sharedStrings.xml", `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="1" uniqueCount="1"><si><t>`+cell+`</t></si></sst>`)
	must("xl/worksheets/sheet1.xml", `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c></row></sheetData></worksheet>`)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// writePptx creates a minimal .pptx with one slide containing the given text.
func writePptx(t *testing.T, path, slideText string) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create pptx: %v", err)
	}
	defer fh.Close()
	zw := zip.NewWriter(fh)
	must := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip Write(%q): %v", name, err)
		}
	}
	must("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/></Types>`)
	must("_rels/.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`)
	must("ppt/presentation.xml", `<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`)
	must("ppt/slides/slide1.xml", `<?xml version="1.0"?><p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>`+slideText+`</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// writePDF creates a minimal valid PDF containing the given single line of
// text. Hand-built to avoid pulling in a PDF library just for tests. Only
// works with `pdftotext` (poppler); we skip the PDF-primary test when
// pdftotext is absent.
func writePDF(t *testing.T, path, line string) {
	t.Helper()
	// Minimal PDF 1.4: catalog → pages → page → text content stream.
	// The text stream uses the (Tj) operator on a Helvetica font. This is the
	// shortest PDF that pdftotext reliably emits the expected string for.
	streamContent := "BT /F1 24 Tf 100 700 Td (" + line + ") Tj ET"
	streamObj := "<< /Length " + itoa(len(streamContent)) + " >>\nstream\n" + streamContent + "\nendstream"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		streamObj,
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objs {
		offsets = append(offsets, b.Len())
		b.WriteString(itoa(i+1) + " 0 obj\n")
		b.WriteString(obj)
		b.WriteString("\nendobj\n")
	}
	xrefOff := b.Len()
	b.WriteString("xref\n0 ")
	b.WriteString(itoa(len(objs) + 1))
	b.WriteString("\n0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		// xref entries are exactly 20 chars: "%010d %05d n \n"
		entry := pad10(off) + " 00000 n \n"
		b.WriteString(entry)
	}
	b.WriteString("trailer\n<< /Size ")
	b.WriteString(itoa(len(objs) + 1))
	b.WriteString(" /Root 1 0 R >>\nstartxref\n")
	b.WriteString(itoa(xrefOff))
	b.WriteString("\n%%EOF\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
}

func itoa(n int) string  { return strings.TrimSpace(jsonNumber(n)) }
func pad10(n int) string { return strings.Repeat("0", 10-len(itoa(n))) + itoa(n) }
func jsonNumber(n int) string {
	// avoid strconv import noise in helpers
	b, _ := json.Marshal(n)
	return string(b)
}

// ----------------------------------------------------------------------------
// Path resolution + validation
// ----------------------------------------------------------------------------

func TestDocExtract_RequiresPath(t *testing.T) {
	for _, tool := range []agent.Tool{
		&PDFToTextTool{}, &DocxToTextTool{}, &XlsxToTextTool{}, &PptxToTextTool{},
	} {
		res, _ := tool.Run(context.Background(), `{}`)
		if !res.IsError {
			t.Errorf("%s: expected validation error on missing path, got %q", tool.Info().Name, res.Content)
		}
	}
}

func TestDocExtract_InvalidJSON(t *testing.T) {
	for _, tool := range []agent.Tool{
		&PDFToTextTool{}, &DocxToTextTool{}, &XlsxToTextTool{}, &PptxToTextTool{},
	} {
		res, _ := tool.Run(context.Background(), `not json`)
		if !res.IsError {
			t.Errorf("%s: expected validation error on bad JSON, got %q", tool.Info().Name, res.Content)
		}
	}
}

func TestDocExtract_RelativePathWithoutCWD(t *testing.T) {
	// No cwdctx in context.Background() → expect ErrNoSessionCWD → validation error.
	for _, tool := range []agent.Tool{
		&PDFToTextTool{}, &DocxToTextTool{}, &XlsxToTextTool{}, &PptxToTextTool{},
	} {
		args, _ := json.Marshal(map[string]any{"path": "relative.docx"})
		res, _ := tool.Run(context.Background(), string(args))
		if !res.IsError {
			t.Errorf("%s: expected error for relative path without cwd, got %q", tool.Info().Name, res.Content)
		}
	}
}

// ----------------------------------------------------------------------------
// docx_to_text
// ----------------------------------------------------------------------------

func TestDocxToText_PrimaryPandoc(t *testing.T) {
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc not installed; skipping primary path")
	}
	path := filepath.Join(t.TempDir(), "sample.docx")
	writeDocx(t, path, "Hello docx")

	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&DocxToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello docx") {
		t.Errorf("output missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Extracted via pandoc") {
		t.Errorf("expected pandoc note; got %q", res.Content)
	}
}

func TestDocxToText_FallbackWhenPandocMissing(t *testing.T) {
	// Force LookPath("pandoc") to fail by emptying PATH for this test.
	t.Setenv("PATH", "")

	path := filepath.Join(t.TempDir(), "sample.docx")
	writeDocx(t, path, "Hello docx fallback")

	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&DocxToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success on fallback, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello docx fallback") {
		t.Errorf("fallback missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "raw XML fallback") {
		t.Errorf("expected raw XML fallback note; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "brew install pandoc") {
		t.Errorf("expected install hint mentioning pandoc; got %q", res.Content)
	}
}

func TestDocxToText_FallbackNotADocx(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "not.docx")
	if err := os.WriteFile(path, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&DocxToTextTool{}).Run(context.Background(), string(args))
	if !res.IsError {
		t.Fatalf("expected error for non-zip docx, got %q", res.Content)
	}
}

// ----------------------------------------------------------------------------
// xlsx_to_text
// ----------------------------------------------------------------------------

func TestXlsxToText_PrimaryXlsx2csv(t *testing.T) {
	if _, err := exec.LookPath("xlsx2csv"); err != nil {
		t.Skip("xlsx2csv not installed; skipping primary path")
	}
	path := filepath.Join(t.TempDir(), "sample.xlsx")
	writeXlsx(t, path, "Hello xlsx")
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&XlsxToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello xlsx") {
		t.Errorf("output missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Extracted via xlsx2csv") {
		t.Errorf("expected xlsx2csv note; got %q", res.Content)
	}
}

func TestXlsxToText_FallbackWhenXlsx2csvMissing(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "sample.xlsx")
	writeXlsx(t, path, "Hello xlsx fallback")
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&XlsxToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success on fallback, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello xlsx fallback") {
		t.Errorf("fallback missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "raw XML fallback") {
		t.Errorf("expected raw XML fallback note; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "pip install xlsx2csv") {
		t.Errorf("expected install hint; got %q", res.Content)
	}
}

// ----------------------------------------------------------------------------
// pptx_to_text
// ----------------------------------------------------------------------------

func TestPptxToText_PrimaryPandoc(t *testing.T) {
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc not installed; skipping primary path")
	}
	path := filepath.Join(t.TempDir(), "sample.pptx")
	writePptx(t, path, "Hello pptx")
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&PptxToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello pptx") {
		t.Errorf("output missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Extracted via pandoc") {
		t.Errorf("expected pandoc note; got %q", res.Content)
	}
}

func TestPptxToText_FallbackWhenPandocMissing(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "sample.pptx")
	writePptx(t, path, "Hello pptx fallback")
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&PptxToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success on fallback, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello pptx fallback") {
		t.Errorf("fallback missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "raw XML fallback") {
		t.Errorf("expected raw XML fallback note; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Slide 1") {
		t.Errorf("expected per-slide separator; got %q", res.Content)
	}
}

// ----------------------------------------------------------------------------
// pdf_to_text
// ----------------------------------------------------------------------------

func TestPDFToText_Primary(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed; skipping primary path")
	}
	path := filepath.Join(t.TempDir(), "sample.pdf")
	writePDF(t, path, "Hello pdf")
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&PDFToTextTool{}).Run(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Hello pdf") {
		t.Errorf("output missing expected text; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Extracted via pdftotext") {
		t.Errorf("expected pdftotext note; got %q", res.Content)
	}
}

func TestPDFToText_MissingExtractor(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "sample.pdf")
	writePDF(t, path, "Hello pdf")
	args, _ := json.Marshal(map[string]any{"path": path})
	res, _ := (&PDFToTextTool{}).Run(context.Background(), string(args))
	if !res.IsError {
		t.Fatalf("expected error when pdftotext missing, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "brew install poppler") {
		t.Errorf("expected poppler install hint; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Anthropic document block") {
		t.Errorf("expected hint about cloud document block; got %q", res.Content)
	}
}

func TestPDFToText_InvalidPagesRange(t *testing.T) {
	// Don't need a real pdf — argv validation runs before LookPath/file read
	// in our implementation order. We pass an obviously-not-a-pdf path so
	// even if validation order changed, the test still flags the bad pages.
	path := filepath.Join(t.TempDir(), "not.pdf")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"abc", "0", "5-3", "10-abc"} {
		args, _ := json.Marshal(map[string]any{"path": path, "pages": p})
		res, _ := (&PDFToTextTool{}).Run(context.Background(), string(args))
		if !res.IsError {
			t.Errorf("pages=%q: expected validation error, got %q", p, res.Content)
		}
	}
}

func TestPDFPageFlags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		ok   bool
	}{
		{"", nil, true},
		{"all", nil, true},
		{"ALL", nil, true},
		{"5", []string{"-f", "5", "-l", "5"}, true},
		{"1-10", []string{"-f", "1", "-l", "10"}, true},
		{"abc", nil, false},
		{"0", nil, false},
		{"5-3", nil, false},
		{"-5", nil, false},
	}
	for _, c := range cases {
		got, err := pdfPageFlags(c.in)
		if c.ok != (err == nil) {
			t.Errorf("pdfPageFlags(%q) error mismatch: err=%v want_ok=%v", c.in, err, c.ok)
			continue
		}
		if !sliceEqual(got, c.want) {
			t.Errorf("pdfPageFlags(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------------
// Truncation
// ----------------------------------------------------------------------------

func TestCapRunes_TruncatesWithMarker(t *testing.T) {
	// Construct a string longer than the cap and verify the marker shape.
	long := strings.Repeat("a", maxDocExtractRunes+1234)
	out := capRunes(long)
	if !strings.Contains(out, "[Truncated:") {
		t.Fatalf("expected truncation marker; got tail %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "original_len=") {
		t.Fatalf("expected original_len in marker; got tail %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "kept=100000") {
		t.Fatalf("expected kept=100000 in marker; got tail %q", out[max(0, len(out)-200):])
	}
	// The body (before marker) must contain exactly maxDocExtractRunes runes.
	idx := strings.Index(out, "\n[Truncated:")
	if idx < 0 {
		t.Fatalf("could not find truncation marker boundary")
	}
	body := out[:idx]
	if utf8.RuneCountInString(body) != maxDocExtractRunes {
		t.Errorf("expected body rune count = %d, got %d", maxDocExtractRunes, utf8.RuneCountInString(body))
	}
}

func TestCapRunes_NoOpUnderBudget(t *testing.T) {
	short := strings.Repeat("a", 1000)
	if got := capRunes(short); got != short {
		t.Errorf("capRunes mutated short string")
	}
}

func TestCapRunes_MultibyteRuneSafe(t *testing.T) {
	// 4-byte rune (U+1F600) repeated past the cap. Truncation must land on
	// a rune boundary, not split a 4-byte sequence mid-byte.
	emoji := "\U0001F600"
	long := strings.Repeat(emoji, maxDocExtractRunes+10)
	out := capRunes(long)
	idx := strings.Index(out, "\n[Truncated:")
	if idx < 0 {
		t.Fatalf("missing truncation marker")
	}
	body := out[:idx]
	if !utf8.ValidString(body) {
		t.Errorf("truncated body is not valid UTF-8 — rune was split mid-bytes")
	}
}

// ----------------------------------------------------------------------------
// Tool registration / API surface
// ----------------------------------------------------------------------------

func TestDocExtractTools_DontRequireApproval(t *testing.T) {
	for _, tool := range []agent.Tool{
		&PDFToTextTool{}, &DocxToTextTool{}, &XlsxToTextTool{}, &PptxToTextTool{},
	} {
		if tool.RequiresApproval() {
			t.Errorf("%s should NOT require approval (read-only extraction)", tool.Info().Name)
		}
	}
}

func TestDocExtractTools_AreReadOnly(t *testing.T) {
	for _, tool := range []agent.Tool{
		&PDFToTextTool{}, &DocxToTextTool{}, &XlsxToTextTool{}, &PptxToTextTool{},
	} {
		// All four implement IsReadOnlyCall and must return true for any args.
		checker, ok := tool.(agent.ReadOnlyChecker)
		if !ok {
			t.Errorf("%s must implement ReadOnlyChecker", tool.Info().Name)
			continue
		}
		if !checker.IsReadOnlyCall(`{}`) {
			t.Errorf("%s.IsReadOnlyCall returned false", tool.Info().Name)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
