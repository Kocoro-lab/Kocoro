package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/images"
)

// fakeImageGen is an in-memory imageGen so the tool tests can assert
// arg-validation and error-classification behavior without an HTTP server.
type fakeImageGen struct {
	resp   *images.GenerateResponse
	err    error
	gotReq images.GenerateRequest
	calls  int
}

func (f *fakeImageGen) Generate(ctx context.Context, req images.GenerateRequest) (*images.GenerateResponse, error) {
	f.calls++
	f.gotReq = req
	return f.resp, f.err
}

func TestGenerateImageInvalidJSON(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{})
	res, err := tool.Run(context.Background(), `{"description":"test image operation","prompt":}`) // malformed JSON
	if err != nil {
		t.Fatalf("Run returned error (should embed in ToolResult): %v", err)
	}
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
}

func TestGenerateImageEmptyPrompt(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{})
	for _, args := range []string{`{"description":"test image operation"}`, `{"description":"test image operation","prompt":""}`, `{"description":"test image operation","prompt":"   "}`} {
		res, _ := tool.Run(context.Background(), args)
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("args=%q: expected validation error, got %+v", args, res)
		}
	}
}

func TestGenerateImagePromptTooLong(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{})
	long := strings.Repeat("a", imagePromptMaxLen+1)
	res, _ := tool.Run(context.Background(), `{"description":"test image operation","prompt":"`+long+`"}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for over-length prompt, got %+v", res)
	}
}

// TestGenerateImagePromptRuneVsByte guards the rune-count enforcement.
// A CJK prompt of 16000 runes is ~48000 bytes — over the byte limit but
// well under the rune limit (32000), so it MUST be accepted. Using len()
// (bytes) would have rejected it client-side before the server got a chance.
func TestGenerateImagePromptRuneVsByte(t *testing.T) {
	fake := &fakeImageGen{
		resp: &images.GenerateResponse{
			Images: []images.Image{{URL: "https://cdn/x.png", ContentType: "image/png"}},
			Model:  "gpt-image-2",
			Size:   "1024x1024",
		},
	}
	tool := NewGenerateImageTool(fake)

	// 16000 CJK runes (each 3 bytes in UTF-8) → 48000 bytes, 16000 runes.
	cjk := strings.Repeat("漢", 16000)
	res, _ := tool.Run(context.Background(), `{"description":"test image operation","prompt":"`+cjk+`"}`)
	if res.IsError {
		t.Fatalf("16000-rune CJK prompt must be accepted (well under 32000 max), got %+v", res)
	}

	// At exactly 32000 runes — boundary, still accepted.
	at := strings.Repeat("漢", imagePromptMaxLen)
	res, _ = tool.Run(context.Background(), `{"description":"test image operation","prompt":"`+at+`"}`)
	if res.IsError {
		t.Fatalf("32000-rune CJK prompt at boundary must be accepted, got %+v", res)
	}

	// 32001 runes — one over, must reject. Rune count, not byte count, in
	// the error message.
	over := strings.Repeat("漢", imagePromptMaxLen+1)
	res, _ = tool.Run(context.Background(), `{"description":"test image operation","prompt":"`+over+`"}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("32001-rune prompt must be rejected, got %+v", res)
	}
	if !strings.Contains(res.Content, "32001") {
		t.Errorf("error message should report rune count 32001, got %q", res.Content)
	}
}

func TestGenerateImageNOutOfRange(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{})
	res, _ := tool.Run(context.Background(), `{"description":"test image operation","prompt":"x","n":11}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for n=11, got %+v", res)
	}
	res, _ = tool.Run(context.Background(), `{"description":"test image operation","prompt":"x","n":-1}`)
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error for n=-1, got %+v", res)
	}
}

func TestGenerateImageInvalidEnum(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{})
	cases := []string{
		`{"description":"test image operation","prompt":"x","size":"4096x4096"}`,
		`{"description":"test image operation","prompt":"x","quality":"ultra"}`,
		`{"description":"test image operation","prompt":"x","background":"chrome"}`,
	}
	for _, args := range cases {
		res, _ := tool.Run(context.Background(), args)
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("args=%s: expected validation error, got %+v", args, res)
		}
	}
}

func TestGenerateImageHappyPath(t *testing.T) {
	fake := &fakeImageGen{
		resp: &images.GenerateResponse{
			Images: []images.Image{
				{URL: "https://cdn/a.png", Key: "public/x/a.png", SizeBytes: 1234, ContentType: "image/png"},
			},
			Model: "gpt-image-2",
			Size:  "1024x1024",
		},
	}
	tool := NewGenerateImageTool(fake)
	res, _ := tool.Run(context.Background(),
		`{"description":"test image operation","prompt":"a cyberpunk cat","size":"1024x1024","quality":"low","n":1}`)
	if res.IsError {
		t.Fatalf("happy path returned error: %+v", res)
	}
	if !strings.Contains(res.Content, "https://cdn/a.png") {
		t.Errorf("Content missing URL: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Generated 1 image") {
		t.Errorf("Content missing count: %q", res.Content)
	}
	if !strings.Contains(res.Content, "image/png") {
		t.Errorf("Content missing content-type: %q", res.Content)
	}
	if fake.gotReq.Prompt != "a cyberpunk cat" || fake.gotReq.Quality != "low" {
		t.Errorf("client received wrong request: %+v", fake.gotReq)
	}
}

func TestGenerateImageMultiImageOutput(t *testing.T) {
	fake := &fakeImageGen{
		resp: &images.GenerateResponse{
			Images: []images.Image{
				{URL: "https://cdn/a.png", ContentType: "image/png", SizeBytes: 1},
				{URL: "https://cdn/b.png", ContentType: "image/png", SizeBytes: 2},
				{URL: "https://cdn/c.png", ContentType: "image/png", SizeBytes: 3},
			},
			Model: "gpt-image-2",
			Size:  "1024x1024",
		},
	}
	tool := NewGenerateImageTool(fake)
	res, _ := tool.Run(context.Background(), `{"description":"test image operation","prompt":"three cats","n":3}`)
	if res.IsError {
		t.Fatalf("error: %+v", res)
	}
	for _, want := range []string{"a.png", "b.png", "c.png", "Generated 3 image"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("Content missing %q: %q", want, res.Content)
		}
	}
}

func TestGenerateImageErrorClassification(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantCat agent.ErrorCategory
	}{
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
			tool := NewGenerateImageTool(&fakeImageGen{err: tc.err})
			res, _ := tool.Run(context.Background(), `{"description":"test image operation","prompt":"x"}`)
			if !res.IsError {
				t.Fatalf("expected IsError, got %+v", res)
			}
			if res.ErrorCategory != tc.wantCat {
				t.Errorf("category = %q, want %q", res.ErrorCategory, tc.wantCat)
			}
		})
	}
}

func TestGenerateImageUnknownErrorFallsThrough(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{err: errors.New("boom")})
	res, _ := tool.Run(context.Background(), `{"description":"test image operation","prompt":"x"}`)
	if !res.IsError {
		t.Fatalf("expected IsError, got %+v", res)
	}
	// Default branch keeps IsError=true but doesn't tag a category.
	if res.ErrorCategory != "" {
		t.Errorf("expected no category for unknown error, got %q", res.ErrorCategory)
	}
}

func TestGenerateImageRequiresApproval(t *testing.T) {
	tool := NewGenerateImageTool(&fakeImageGen{})
	if !tool.RequiresApproval() {
		t.Error("RequiresApproval must be true (paid + permanent public URL)")
	}
	if tool.IsSafeArgs(`{"description":"test image operation","prompt":"x"}`) {
		t.Error("IsSafeArgs must be false")
	}
	// SafeChecker contract assertion mirrors the publish_to_web pattern.
	var _ agent.SafeChecker = tool
}
