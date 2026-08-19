package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// fakeXMediaUploads records Upload/Delete calls so orchestration tests can
// assert ordering and best-effort cleanup behavior.
type fakeXMediaUploads struct {
	uploadOpts []uploads.UploadOptions
	uploadResp *uploads.UploadResponse
	uploadErr  error
	deleted    []string
	deleteErr  error
}

func (f *fakeXMediaUploads) Upload(ctx context.Context, openBody func() (io.ReadCloser, error),
	opts uploads.UploadOptions) (*uploads.UploadResponse, error) {
	f.uploadOpts = append(f.uploadOpts, opts)
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	if openBody != nil {
		if rc, err := openBody(); err == nil {
			_, _ = io.Copy(io.Discard, rc)
			rc.Close()
		}
	}
	return f.uploadResp, nil
}

func (f *fakeXMediaUploads) Delete(ctx context.Context, id string) (*uploads.DeleteResponse, error) {
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &uploads.DeleteResponse{Deleted: true, ID: id}, nil
}

type xMediaExecuteCall struct {
	name           string
	args           map[string]any
	requestID      string
	idempotencyKey string
}

// newXMediaFixture builds the tool with recording fakes and a temp media file
// of the requested size. Returns the tool, both recorders, and the file path.
func newXMediaFixture(t *testing.T, filename string, size int) (*XUploadMediaTool, *fakeXMediaUploads, *[]xMediaExecuteCall, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, size), 0o644); err != nil {
		t.Fatalf("write temp media: %v", err)
	}
	uploader := &fakeXMediaUploads{
		uploadResp: &uploads.UploadResponse{
			ID:  "upload-1",
			URL: "https://static.example/cdn/media.png",
		},
	}
	calls := &[]xMediaExecuteCall{}
	execute := func(ctx context.Context, name string, args map[string]any, requestID, idempotencyKey string) (*client.ToolExecuteResponse, error) {
		*calls = append(*calls, xMediaExecuteCall{name: name, args: args, requestID: requestID, idempotencyKey: idempotencyKey})
		return &client.ToolExecuteResponse{
			Success: true,
			Output:  []byte(`{"media_id":"1234567890","expires_after_secs":86400}`),
		}, nil
	}
	return NewXUploadMediaTool(uploader, execute), uploader, calls, path
}

func runXMediaTool(t *testing.T, tool *XUploadMediaTool, args map[string]any) agent.ToolResult {
	t.Helper()
	if _, ok := args["description"]; !ok {
		args["description"] = "Upload image to X"
	}
	argsJSON, err := jsonMarshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return res
}

func TestXUploadMedia_SuccessOrchestration(t *testing.T) {
	tool, uploader, calls, path := newXMediaFixture(t, "photo.png", 1024)

	res := runXMediaTool(t, tool, map[string]any{"file_path": path, "alt_text": "a red bird"})
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if !strings.Contains(res.Content, "media_id: 1234567890") {
		t.Errorf("result missing media_id: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Expires in about") {
		t.Errorf("result missing expiry hint: %s", res.Content)
	}

	if len(uploader.uploadOpts) != 1 {
		t.Fatalf("uploads = %d, want 1", len(uploader.uploadOpts))
	}
	opts := uploader.uploadOpts[0]
	if opts.Kind != uploads.KindImage || opts.ContentType != "image/png" ||
		opts.Filename != "photo.png" || string(opts.Metadata) != `{"purpose":"x_media"}` {
		t.Errorf("upload opts = %#v", opts)
	}

	if len(*calls) != 1 {
		t.Fatalf("execute calls = %d, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.name != "x_upload_media" ||
		call.args["media_url"] != "https://static.example/cdn/media.png" ||
		call.args["alt_text"] != "a red bird" {
		t.Errorf("execute call = %#v", call)
	}

	if len(uploader.deleted) != 1 || uploader.deleted[0] != "upload-1" {
		t.Errorf("staged upload not cleaned up: deleted=%v", uploader.deleted)
	}
}

func TestXUploadMedia_AltTextOmittedWhenEmpty(t *testing.T) {
	tool, _, calls, path := newXMediaFixture(t, "photo.jpg", 512)

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if _, present := (*calls)[0].args["alt_text"]; present {
		t.Errorf("alt_text key sent despite empty input: %#v", (*calls)[0].args)
	}
}

func TestXUploadMedia_PurposePassthrough(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantSent any // nil = key must be absent
	}{
		{"dm sent on the wire", map[string]any{"purpose": "dm"}, "dm"},
		{"post omitted (Cloud default)", map[string]any{"purpose": "post"}, nil},
		{"absent omitted", map[string]any{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, _, calls, path := newXMediaFixture(t, "photo.png", 128)
			tc.args["file_path"] = path
			res := runXMediaTool(t, tool, tc.args)
			if res.IsError {
				t.Fatalf("unexpected error result: %s", res.Content)
			}
			got, present := (*calls)[0].args["purpose"]
			if tc.wantSent == nil {
				if present {
					t.Errorf("purpose key sent when it should be omitted: %#v", (*calls)[0].args)
				}
				return
			}
			if got != tc.wantSent {
				t.Errorf("purpose = %v, want %v", got, tc.wantSent)
			}
		})
	}
}

func TestXUploadMedia_InvalidPurposeRejected(t *testing.T) {
	tool, uploader, calls, path := newXMediaFixture(t, "photo.png", 128)

	res := runXMediaTool(t, tool, map[string]any{"file_path": path, "purpose": "story"})
	if !res.IsError || !strings.Contains(res.Content, `invalid purpose "story"`) {
		t.Errorf("got (%v, %s), want invalid-purpose rejection", res.IsError, res.Content)
	}
	if len(uploader.uploadOpts) != 0 || len(*calls) != 0 {
		t.Error("invalid purpose must fail before upload or execute")
	}
}

func TestXUploadMedia_RequestIDFromToolInvocation(t *testing.T) {
	tool, _, calls, path := newXMediaFixture(t, "photo.png", 256)

	ctx := agent.ContextWithToolInvocation(context.Background(), agent.ToolInvocation{
		ToolName:  "x_upload_media",
		ToolUseID: "toolu_abc",
	})
	argsJSON, _ := jsonMarshal(map[string]any{
		"file_path":   path,
		"description": "Upload image to X",
	})
	if _, err := tool.Run(ctx, argsJSON); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if (*calls)[0].requestID != "toolu_abc" {
		t.Errorf("requestID = %q, want toolu_abc", (*calls)[0].requestID)
	}
}

func TestXUploadMedia_ExecuteFailureStillDeletesStagedUpload(t *testing.T) {
	tool, uploader, _, path := newXMediaFixture(t, "photo.png", 256)
	tool.execute = func(context.Context, string, map[string]any, string, string) (*client.ToolExecuteResponse, error) {
		return nil, &client.IntegrationToolAPIError{StatusCode: 403, Code: "tool_not_allowed"}
	}

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if !res.IsError {
		t.Fatal("expected error result on execute failure")
	}
	if !strings.Contains(res.Content, "not connected") {
		t.Errorf("tool_not_allowed mapping missing guidance: %s", res.Content)
	}
	if len(uploader.deleted) != 1 || uploader.deleted[0] != "upload-1" {
		t.Errorf("staged upload must be cleaned up even on execute failure: %v", uploader.deleted)
	}
}

// X deterministically rejecting the media (provider_rejected + error_detail)
// is an ordinary fixable error carrying the vendor's reason — and the staging
// upload is still cleaned up.
func TestXUploadMedia_ProviderRejectedSurfacesDetailAndCleansUp(t *testing.T) {
	tool, uploader, _, path := newXMediaFixture(t, "photo.png", 256)
	tool.execute = func(context.Context, string, map[string]any, string, string) (*client.ToolExecuteResponse, error) {
		return nil, &client.IntegrationToolAPIError{
			StatusCode:  403,
			Code:        "provider_rejected",
			ErrorDetail: "animated GIF exceeds frame limit",
		}
	}

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if !res.IsError || res.SideEffectOutcomeUnknown {
		t.Fatalf("want ordinary error, got %#v", res)
	}
	if !strings.Contains(res.Content, "animated GIF exceeds frame limit") ||
		!strings.Contains(res.Content, "NOT uploaded") {
		t.Fatalf("content missing detail / no-upload statement: %s", res.Content)
	}
	if len(uploader.deleted) != 1 {
		t.Errorf("staged upload must still be cleaned up: %v", uploader.deleted)
	}
}

func TestXUploadMedia_DeleteFailureDoesNotAffectResult(t *testing.T) {
	tool, uploader, _, path := newXMediaFixture(t, "photo.png", 256)
	uploader.deleteErr = errors.New("gateway down")

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if res.IsError {
		t.Fatalf("delete failure must not override the primary result: %s", res.Content)
	}
	if !strings.Contains(res.Content, "media_id: 1234567890") {
		t.Errorf("result missing media_id: %s", res.Content)
	}
}

func TestXUploadMedia_UploadFailureSkipsExecuteAndDelete(t *testing.T) {
	tool, uploader, calls, path := newXMediaFixture(t, "photo.png", 256)
	uploader.uploadErr = uploads.ErrUnauthorized

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if !res.IsError {
		t.Fatal("expected error result on staging failure")
	}
	if len(*calls) != 0 {
		t.Error("execute must not run after a failed staging upload")
	}
	if len(uploader.deleted) != 0 {
		t.Error("nothing was staged; delete must not run")
	}
}

func TestXUploadMedia_EmptyUploadIDSkipsDelete(t *testing.T) {
	tool, uploader, _, path := newXMediaFixture(t, "photo.png", 256)
	uploader.uploadResp.ID = "" // older Cloud without the id field

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if len(uploader.deleted) != 0 {
		t.Errorf("delete must be skipped without an upload id: %v", uploader.deleted)
	}
}

func TestXUploadMedia_MissingMediaIDIsError(t *testing.T) {
	tool, uploader, _, path := newXMediaFixture(t, "photo.png", 256)
	tool.execute = func(context.Context, string, map[string]any, string, string) (*client.ToolExecuteResponse, error) {
		return &client.ToolExecuteResponse{Success: true, Output: []byte(`{"ok":true}`)}, nil
	}

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if !res.IsError || !strings.Contains(res.Content, "no media_id") {
		t.Errorf("expected missing-media_id error, got: %v %s", res.IsError, res.Content)
	}
	if len(uploader.deleted) != 1 {
		t.Errorf("staged upload must still be cleaned up: %v", uploader.deleted)
	}
}

func TestXUploadMedia_GuardsBlockSensitiveAndNonMediaPaths(t *testing.T) {
	tool, uploader, calls, _ := newXMediaFixture(t, "unused.png", 16)

	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sensitivePNG := filepath.Join(secretsDir, "leak.png")
	if err := os.WriteFile(sensitivePNG, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	textFile := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(textFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"sensitive path segment", sensitivePNG, "sensitive segment"},
		{"non-media extension", textFile, "unsupported media type"},
		{"missing file", filepath.Join(dir, "nope.png"), "file not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runXMediaTool(t, tool, map[string]any{"file_path": tc.path})
			if !res.IsError || !strings.Contains(res.Content, tc.want) {
				t.Errorf("got (%v, %s), want error containing %q", res.IsError, res.Content, tc.want)
			}
		})
	}
	if len(uploader.uploadOpts) != 0 || len(*calls) != 0 {
		t.Error("guard rejections must not reach upload or execute")
	}
}

func TestXUploadMedia_SymlinkToSensitiveTargetBlocked(t *testing.T) {
	tool, uploader, _, _ := newXMediaFixture(t, "unused.png", 16)

	dir := t.TempDir()
	target := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "cute.png")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	res := runXMediaTool(t, tool, map[string]any{"file_path": link})
	if !res.IsError {
		t.Fatal("symlink to a sensitive target must be blocked")
	}
	if len(uploader.uploadOpts) != 0 {
		t.Error("blocked symlink must not reach the uploader")
	}
}

func TestXUploadMedia_SizeLimits(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		size     int
		wantErr  bool
	}{
		{"png over 5MB rejected", "big.png", int(xMediaMaxImageBytes) + 1, true},
		{"gif under 15MB accepted", "anim.gif", 6 << 20, false},
		{"gif over 15MB rejected", "huge.gif", int(xMediaMaxGIFBytes) + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, uploader, _, path := newXMediaFixture(t, tc.filename, tc.size)
			res := runXMediaTool(t, tool, map[string]any{"file_path": path})
			if tc.wantErr {
				if !res.IsError || !strings.Contains(res.Content, "too large") {
					t.Errorf("got (%v, %s), want size rejection", res.IsError, res.Content)
				}
				if len(uploader.uploadOpts) != 0 {
					t.Error("oversize file must fail before any upload")
				}
				return
			}
			if res.IsError {
				t.Errorf("unexpected rejection: %s", res.Content)
			}
			if len(uploader.uploadOpts) != 1 || uploader.uploadOpts[0].ContentType != "image/gif" {
				t.Errorf("gif upload opts = %#v", uploader.uploadOpts)
			}
		})
	}
}

func TestXUploadMedia_RequiredFields(t *testing.T) {
	tool, _, _, _ := newXMediaFixture(t, "unused.png", 16)

	res := runXMediaTool(t, tool, map[string]any{"file_path": ""})
	if !res.IsError || !strings.Contains(res.Content, "file_path is required") {
		t.Errorf("missing file_path: got (%v, %s)", res.IsError, res.Content)
	}

	argsJSON, _ := jsonMarshal(map[string]any{"file_path": "/tmp/a.png", "description": ""})
	res2, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res2.IsError || !strings.Contains(res2.Content, "description") {
		t.Errorf("missing description: got (%v, %s)", res2.IsError, res2.Content)
	}
}

func TestXUploadMedia_ApprovalAndExposureContract(t *testing.T) {
	tool, _, _, _ := newXMediaFixture(t, "unused.png", 16)
	if !tool.RequiresApproval() {
		t.Error("x_upload_media must require approval")
	}
	if tool.IsSafeArgs(`{"file_path":"/tmp/a.png"}`) {
		t.Error("x_upload_media must never be safe on args alone")
	}
	if tool.ToolExposure() != agent.ToolExposureDeferred {
		t.Error("x_upload_media must be Deferred")
	}
}

func TestXUploadMedia_UsagePropagatesFromExecuteResponse(t *testing.T) {
	tool, _, _, path := newXMediaFixture(t, "photo.png", 256)
	tool.execute = func(context.Context, string, map[string]any, string, string) (*client.ToolExecuteResponse, error) {
		return &client.ToolExecuteResponse{
			Success: true,
			Output:  []byte(`{"media_id":"42"}`),
			Usage: &client.ToolUsage{
				Provider: "x", CostModel: "x-list-price-estimate-v2",
				Units: 1, UnitType: "requests", CostUSD: 0.005,
			},
		}, nil
	}

	res := runXMediaTool(t, tool, map[string]any{"file_path": path})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Usage == nil || res.Usage.Provider != "x" || res.Usage.CostUSD != 0.005 ||
		res.Usage.Units != 1 || res.Usage.UnitType != "requests" {
		t.Errorf("usage = %#v", res.Usage)
	}
}

// TestXUploadMedia_AmbiguousOutcomesAreOutcomeUnknown pins the ambiguity
// classification: results that cannot prove the media never reached X must
// carry SideEffectOutcomeUnknown so the journal records outcome_unknown and
// the same-turn retry latch arms — never an ordinary error the model could
// blindly re-send (duplicate media_id, double charge). Proven-no-dispatch
// failures stay ordinary retryable errors.
func TestXUploadMedia_AmbiguousOutcomesAreOutcomeUnknown(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantUnknown bool
	}{
		{"outcome_unknown code", &client.IntegrationToolAPIError{StatusCode: 409, Code: "outcome_unknown"}, true},
		{"call_in_progress code", &client.IntegrationToolAPIError{StatusCode: 409, Code: "call_in_progress"}, true},
		{"post-dispatch 500", &client.IntegrationToolAPIError{StatusCode: 500, Code: "internal_error"}, true},
		{"transport may have dispatched", &client.ToolDispatchError{MayHaveDispatched: true, Err: errors.New("response lost")}, true},
		{"429 rejected before execution", &client.IntegrationToolAPIError{StatusCode: 429, Code: "rate_limited"}, false},
		{"pre-dispatch transport failure", &client.ToolDispatchError{MayHaveDispatched: false, Retryable: true, Err: errors.New("connection refused")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, uploader, _, path := newXMediaFixture(t, "photo.png", 128)
			execErr := tc.err
			tool.execute = func(context.Context, string, map[string]any, string, string) (*client.ToolExecuteResponse, error) {
				return nil, execErr
			}
			res := runXMediaTool(t, tool, map[string]any{"file_path": path})
			if !res.IsError {
				t.Fatalf("want error result, got %#v", res)
			}
			if res.SideEffectOutcomeUnknown != tc.wantUnknown {
				t.Fatalf("SideEffectOutcomeUnknown = %v, want %v (content: %s)",
					res.SideEffectOutcomeUnknown, tc.wantUnknown, res.Content)
			}
			if tc.wantUnknown && !strings.Contains(res.Content, "outcome UNKNOWN") {
				t.Fatalf("unknown result missing narratable wording: %s", res.Content)
			}
			if len(uploader.deleted) != 1 {
				t.Errorf("staged upload cleanup must run regardless of outcome: %v", uploader.deleted)
			}
		})
	}
}

// TestXUploadMedia_JournalBackedRunSendsIdempotencyKeyAndArmsLatch pins both
// review findings end to end through the real dispatcher: (a) a
// journal-backed call forwards the durable IdempotencyKey to the Cloud
// execute seam (the material contract's provider-level dedup key), and (b) an
// outcome_unknown execute journals outcome_unknown and arms the same-turn
// retry latch so a byte-identical repeat never re-stages or re-dispatches.
func TestXUploadMedia_JournalBackedRunSendsIdempotencyKeyAndArmsLatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 128), 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeXMediaUploads{
		uploadResp: &uploads.UploadResponse{ID: "upload-1", URL: "https://static.example/cdn/m.png"},
	}
	var execCalls []xMediaExecuteCall
	execute := func(_ context.Context, name string, args map[string]any, requestID, idempotencyKey string) (*client.ToolExecuteResponse, error) {
		execCalls = append(execCalls, xMediaExecuteCall{name: name, args: args, requestID: requestID, idempotencyKey: idempotencyKey})
		return nil, &client.IntegrationToolAPIError{StatusCode: 409, Code: "outcome_unknown"}
	}
	reg := agent.NewToolRegistry()
	reg.Register(NewXUploadMediaTool(uploader, execute))

	argsJSON := `{"file_path":` + strconv.Quote(path) + `,"description":"upload to X"}`
	llm := &xMediaScriptedLLM{argsJSON: argsJSON}
	journal := &integrationIdentityJournal{}
	loop := agent.NewAgentLoop(llm, reg, "medium", t.TempDir(), 6, 2000, 200, nil, nil, nil)
	loop.SetCheckpointFunc(func(context.Context) error { return nil })
	loop.SetSideEffectExecutionJournal(journal)
	loop.SetHandler(xMediaApproveAllHandler{})

	text, _, err := loop.Run(context.Background(), "upload the photo", nil, nil)
	if err != nil || text != "done" {
		t.Fatalf("Run = (%q, %v), want narrated completion", text, err)
	}
	// (a) durable identity forwarded on the wire seam.
	if len(execCalls) != 1 {
		t.Fatalf("execute calls = %d, want 1 (latched repeat must not dispatch)", len(execCalls))
	}
	if execCalls[0].requestID != "exec-x-create-1" || execCalls[0].idempotencyKey != "idem-x-create-1" {
		t.Fatalf("identity = (%q, %q), want the journal's durable pair", execCalls[0].requestID, execCalls[0].idempotencyKey)
	}
	// (b) ambiguity journaled and the latch armed: the identical second call
	// never re-staged an upload (one Upload, one cleanup Delete only).
	if journal.committed != 0 || journal.unknown != 1 {
		t.Fatalf("journal committed=%d unknown=%d, want 0/1", journal.committed, journal.unknown)
	}
	if len(uploader.uploadOpts) != 1 || len(uploader.deleted) != 1 {
		t.Fatalf("uploads=%d deletes=%d, want 1/1 (latched repeat must not re-stage)",
			len(uploader.uploadOpts), len(uploader.deleted))
	}
}

// xMediaScriptedLLM issues the same x_upload_media call on two consecutive
// turns (the second is the byte-identical automatic retry the latch must
// reject) and then ends the turn.
type xMediaScriptedLLM struct {
	calls    int
	argsJSON string
}

func (c *xMediaScriptedLLM) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	c.calls++
	if c.calls == 1 {
		// x_upload_media is explicit Deferred: warm it first like production.
		return &client.CompletionResponse{
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				ID: "toolu_search_media", Name: "tool_search",
				Arguments: json.RawMessage(`{"query":"select:x_upload_media"}`),
			}},
		}, nil
	}
	if c.calls <= 3 {
		return &client.CompletionResponse{
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				ID:        fmt.Sprintf("toolu_media_%d", c.calls),
				Name:      "x_upload_media",
				Arguments: json.RawMessage(c.argsJSON),
			}},
		}, nil
	}
	return &client.CompletionResponse{OutputText: "done", FinishReason: "stop"}, nil
}

func (c *xMediaScriptedLLM) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

// xMediaApproveAllHandler approves every tool call so the Run-level tests can
// exercise the dispatcher path of an approval-requiring tool.
type xMediaApproveAllHandler struct{}

func (xMediaApproveAllHandler) OnToolCall(string, string, string) {}
func (xMediaApproveAllHandler) OnToolResult(string, string, string, agent.ToolResult, time.Duration) {
}
func (xMediaApproveAllHandler) OnText(string)                        {}
func (xMediaApproveAllHandler) OnPreamble(string)                    {}
func (xMediaApproveAllHandler) OnStreamDelta(string)                 {}
func (xMediaApproveAllHandler) OnApprovalNeeded(string, string) bool { return true }
func (xMediaApproveAllHandler) OnUsage(agent.TurnUsage)              {}
func (xMediaApproveAllHandler) OnCloudAgent(string, string, string)  {}
func (xMediaApproveAllHandler) OnCloudProgress(int, int)             {}
func (xMediaApproveAllHandler) OnCloudPlan(string, string, bool)     {}
