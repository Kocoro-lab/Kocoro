package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/images"
)

// fakeImageEdit is an in-memory imageEdit so the tool tests can assert
// arg-validation and error-classification behavior without an HTTP server.
type fakeImageEdit struct {
	resp   *images.GenerateResponse
	err    error
	gotReq images.EditRequest
	calls  int
}

func (f *fakeImageEdit) Edit(ctx context.Context, req images.EditRequest) (*images.GenerateResponse, error) {
	f.calls++
	f.gotReq = req
	return f.resp, f.err
}

const editTestSrc = "https://static.kocoro.ai/public/abc/src.png"

func TestEditImageInvalidJSON(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	res, err := tool.Run(context.Background(), `{"description":"test image operation","prompt":}`) // malformed JSON
	if err != nil {
		t.Fatalf("Run returned error (should embed in ToolResult): %v", err)
	}
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
}

func TestEditImageEmptyPrompt(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	args := []string{
		`{"description":"test image operation","image_urls":["` + editTestSrc + `"]}`,
		`{"description":"test image operation","prompt":"","image_urls":["` + editTestSrc + `"]}`,
		`{"description":"test image operation","prompt":"   ","image_urls":["` + editTestSrc + `"]}`,
	}
	for _, a := range args {
		res, _ := tool.Run(context.Background(), a)
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("args=%q: expected validation error, got %+v", a, res)
		}
	}
}

func TestEditImagePromptTooLong(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	long := strings.Repeat("a", imagePromptMaxLen+1)
	res, _ := tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"`+long+`","image_urls":["`+editTestSrc+`"]}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for over-length prompt, got %+v", res)
	}
}

// TestEditImagePromptRuneVsByte mirrors TestGenerateImagePromptRuneVsByte —
// rune-counted enforcement so multibyte prompts up to 32000 runes are accepted
// even though byte length exceeds the same number.
func TestEditImagePromptRuneVsByte(t *testing.T) {
	fake := &fakeImageEdit{
		resp: &images.GenerateResponse{
			Images: []images.Image{{URL: "https://cdn/x.png", ContentType: "image/png"}},
			Model:  "gpt-image-2",
			Size:   "1024x1024",
		},
	}
	tool := NewEditImageTool(fake)

	cjk := strings.Repeat("漢", 16000)
	res, _ := tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"`+cjk+`","image_urls":["`+editTestSrc+`"]}`)
	if res.IsError {
		t.Fatalf("16000-rune CJK prompt must be accepted, got %+v", res)
	}

	over := strings.Repeat("漢", imagePromptMaxLen+1)
	res, _ = tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"`+over+`","image_urls":["`+editTestSrc+`"]}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("32001-rune prompt must be rejected, got %+v", res)
	}
	if !strings.Contains(res.Content, "32001") {
		t.Errorf("error message should report rune count 32001, got %q", res.Content)
	}
}

func TestEditImageMissingImageURLs(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	for _, a := range []string{
		`{"description":"test image operation","prompt":"x"}`,                 // missing entirely
		`{"description":"test image operation","prompt":"x","image_urls":[]}`, // empty array
	} {
		res, _ := tool.Run(context.Background(), a)
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("args=%q: expected validation error, got %+v", a, res)
		}
		if !strings.Contains(res.Content, "image_urls") {
			t.Errorf("args=%q: error must mention image_urls: %q", a, res.Content)
		}
	}
}

func TestEditImageTooManyImageURLs(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	urls := make([]string, editImageURLsMax+1)
	for i := range urls {
		urls[i] = `"` + editTestSrc + `"`
	}
	args := `{"description":"test image operation","prompt":"x","image_urls":[` + strings.Join(urls, ",") + `]}`
	res, _ := tool.Run(context.Background(), args)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for >%d URLs, got %+v", editImageURLsMax, res)
	}
}

func TestEditImageNonCDNURL(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	cases := []string{
		// pure external URL
		`{"description":"test image operation","prompt":"x","image_urls":["https://example.com/x.png"]}`,
		// http (not https)
		`{"description":"test image operation","prompt":"x","image_urls":["http://static.kocoro.ai/x.png"]}`,
		// CDN host but wrong domain
		`{"description":"test image operation","prompt":"x","image_urls":["https://cdn.kocoro.com/x.png"]}`,
		// mixed: first valid, second external — must still reject
		`{"description":"test image operation","prompt":"x","image_urls":["` + editTestSrc + `","https://example.com/y.png"]}`,
	}
	for _, a := range cases {
		res, _ := tool.Run(context.Background(), a)
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("args=%s: expected validation error, got %+v", a, res)
		}
		if !strings.Contains(res.Content, "static.kocoro.ai") {
			t.Errorf("args=%s: error must point user to CDN prefix, got %q", a, res.Content)
		}
	}
}

func TestEditImageNonCDNURLDoesNotCallClient(t *testing.T) {
	// Defense in depth: a non-CDN URL must short-circuit before any HTTP call,
	// otherwise we waste a paid round-trip.
	fake := &fakeImageEdit{}
	tool := NewEditImageTool(fake)
	_, _ = tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"x","image_urls":["https://example.com/x.png"]}`)
	if fake.calls != 0 {
		t.Errorf("client must not be called when URL fails prefix check; calls=%d", fake.calls)
	}
}

func TestEditImageNOutOfRange(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	res, _ := tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"x","image_urls":["`+editTestSrc+`"],"n":11}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for n=11, got %+v", res)
	}
	res, _ = tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"x","image_urls":["`+editTestSrc+`"],"n":-1}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for n=-1, got %+v", res)
	}
}

func TestEditImageInvalidEnum(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	cases := []string{
		`{"description":"test image operation","prompt":"x","image_urls":["` + editTestSrc + `"],"size":"4096x4096"}`,
		`{"description":"test image operation","prompt":"x","image_urls":["` + editTestSrc + `"],"quality":"ultra"}`,
		`{"description":"test image operation","prompt":"x","image_urls":["` + editTestSrc + `"],"background":"chrome"}`,
	}
	for _, args := range cases {
		res, _ := tool.Run(context.Background(), args)
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("args=%s: expected validation error, got %+v", args, res)
		}
	}
}

func TestEditImageHappyPath(t *testing.T) {
	fake := &fakeImageEdit{
		resp: &images.GenerateResponse{
			Images: []images.Image{
				{URL: "https://cdn/edited.png", Key: "public/x/edited.png", SizeBytes: 4321, ContentType: "image/png"},
			},
			Model: "gpt-image-2",
			Size:  "1024x1024",
		},
	}
	tool := NewEditImageTool(fake)
	res, _ := tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"add a hat to the cat","image_urls":["`+editTestSrc+`"],"quality":"low"}`)
	if res.IsError {
		t.Fatalf("happy path returned error: %+v", res)
	}
	if !strings.Contains(res.Content, "https://cdn/edited.png") {
		t.Errorf("Content missing URL: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Generated 1 image") {
		t.Errorf("Content missing count: %q", res.Content)
	}
	if fake.gotReq.Prompt != "add a hat to the cat" || fake.gotReq.Quality != "low" {
		t.Errorf("client received wrong request: %+v", fake.gotReq)
	}
	if len(fake.gotReq.ImageURLs) != 1 || fake.gotReq.ImageURLs[0] != editTestSrc {
		t.Errorf("client received wrong image_urls: %+v", fake.gotReq.ImageURLs)
	}
}

func TestEditImageMultiSourceHappyPath(t *testing.T) {
	fake := &fakeImageEdit{
		resp: &images.GenerateResponse{
			Images: []images.Image{
				{URL: "https://cdn/combined.png", ContentType: "image/png", SizeBytes: 9000},
			},
			Model: "gpt-image-2",
			Size:  "1024x1024",
		},
	}
	tool := NewEditImageTool(fake)
	srcs := []string{
		`"` + editTestSrc + `"`,
		`"https://static.kocoro.ai/public/def/two.png"`,
		`"https://static.kocoro.ai/public/ghi/three.png"`,
		`"https://static.kocoro.ai/public/jkl/four.png"`,
	}
	args := `{"description":"test image operation","prompt":"combine into one","image_urls":[` + strings.Join(srcs, ",") + `]}`
	res, _ := tool.Run(context.Background(), args)
	if res.IsError {
		t.Fatalf("4-source happy path returned error: %+v", res)
	}
	if len(fake.gotReq.ImageURLs) != 4 {
		t.Errorf("expected 4 image_urls forwarded, got %d", len(fake.gotReq.ImageURLs))
	}
}

func TestEditImageErrorClassification(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantCat agent.ErrorCategory
	}{
		{"invalid_image_url", images.ErrInvalidImageURL, agent.ErrCategoryBusiness},
		{"source_too_large", images.ErrSourceTooLarge, agent.ErrCategoryValidation},
		{"unauthorized", images.ErrUnauthorized, agent.ErrCategoryPermission},
		{"endpoint_not_found", images.ErrEndpointNotFound, agent.ErrCategoryBusiness},
		{"bad_request", images.ErrBadRequest, agent.ErrCategoryValidation},
		{"request_too_large", images.ErrRequestTooLarge, agent.ErrCategoryValidation},
		{"upstream_timeout", images.ErrUpstreamTimeout, agent.ErrCategoryBusiness},
		{"content_rejected", images.ErrContentRejected, agent.ErrCategoryBusiness},
		{"server_config", images.ErrServerConfig, agent.ErrCategoryBusiness},
		{"transient", images.ErrTransient, agent.ErrCategoryTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewEditImageTool(&fakeImageEdit{err: tc.err})
			res, _ := tool.Run(context.Background(),
				`{"description":"test image operation","prompt":"x","image_urls":["`+editTestSrc+`"]}`)
			if !res.IsError {
				t.Fatalf("expected IsError, got %+v", res)
			}
			if res.ErrorCategory != tc.wantCat {
				t.Errorf("category = %q, want %q", res.ErrorCategory, tc.wantCat)
			}
		})
	}
}

func TestEditImageUnknownErrorFallsThrough(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{err: errors.New("boom")})
	res, _ := tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"x","image_urls":["`+editTestSrc+`"]}`)
	if !res.IsError {
		t.Fatalf("expected IsError, got %+v", res)
	}
	// Default branch keeps IsError=true but doesn't tag a category.
	if res.ErrorCategory != "" {
		t.Errorf("expected no category for unknown error, got %q", res.ErrorCategory)
	}
}

func TestEditImageRequiresApproval(t *testing.T) {
	tool := NewEditImageTool(&fakeImageEdit{})
	if !tool.RequiresApproval() {
		t.Error("RequiresApproval must be true (paid + permanent public URL)")
	}
	if tool.IsSafeArgs(`{"description":"test image operation","prompt":"x","image_urls":["` + editTestSrc + `"]}`) {
		t.Error("IsSafeArgs must be false")
	}
	// SafeChecker contract assertion mirrors generate_image.
	var _ agent.SafeChecker = tool
}
