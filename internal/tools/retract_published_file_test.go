package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// --- fakes ---

type fakeListUploader struct {
	gotLimit, gotOffset int
	resp                *uploads.ListResponse
	err                 error
}

func (f *fakeListUploader) List(_ context.Context, limit, offset int) (*uploads.ListResponse, error) {
	f.gotLimit = limit
	f.gotOffset = offset
	return f.resp, f.err
}

type fakeRetractUploader struct {
	gotID string
	resp  *uploads.DeleteResponse
	err   error
}

func (f *fakeRetractUploader) Delete(_ context.Context, id string) (*uploads.DeleteResponse, error) {
	f.gotID = id
	return f.resp, f.err
}

// --- ListPublishedFilesTool ---

func TestListPublishedFilesTool_Info_NotRequiringApproval(t *testing.T) {
	tool := NewListPublishedFilesTool(&fakeListUploader{})
	if tool.RequiresApproval() {
		t.Errorf("RequiresApproval() = true, want false (read-only)")
	}
	info := tool.Info()
	if info.Name != "list_my_published_files" {
		t.Errorf("Name = %q", info.Name)
	}
	if len(info.Required) != 0 {
		t.Errorf("Required = %v, want empty (both params have defaults)", info.Required)
	}
	if _, ok := info.Parameters["properties"].(map[string]any)["limit"]; !ok {
		t.Errorf("schema missing 'limit' property")
	}
}

func TestListPublishedFilesTool_IsReadOnlyCall(t *testing.T) {
	// list_my_published_files must declare itself read-only so the agent
	// loop's read-only batcher can fan it out in parallel with file_read /
	// grep / directory_list etc. If this test fails after a refactor, either
	// (a) restore the IsReadOnlyCall implementation, or (b) remove the
	// agent.ReadOnlyChecker interface assertion at the bottom of the source
	// file — the GET endpoint is pure-read and should stay batchable.
	tool := NewListPublishedFilesTool(&fakeListUploader{})
	roc, ok := agent.Tool(tool).(interface{ IsReadOnlyCall(string) bool })
	if !ok {
		t.Fatal("ListPublishedFilesTool must implement IsReadOnlyCall")
	}
	if !roc.IsReadOnlyCall(`{}`) {
		t.Error("IsReadOnlyCall returned false; GET /uploads is pure-read")
	}
}

func TestRetractPublishedFileTool_NotReadOnly(t *testing.T) {
	// Defense in depth: retract is destructive. It must NOT implement
	// ReadOnlyChecker, otherwise the agent loop would batch it with read-only
	// tools and violate the read-before-write barrier.
	tool := NewRetractPublishedFileTool(&fakeRetractUploader{})
	if _, ok := agent.Tool(tool).(interface{ IsReadOnlyCall(string) bool }); ok {
		t.Error("RetractPublishedFileTool must NOT implement ReadOnlyChecker — it is destructive")
	}
}

func TestListPublishedFilesTool_Run_RendersTable(t *testing.T) {
	fake := &fakeListUploader{
		resp: &uploads.ListResponse{
			Uploads: []uploads.UploadEntry{
				{ID: "6b28b36c-d218-448f-adb5-e256218fb025", URL: "https://x/y/a.html", Filename: "a.html", ContentType: "text/html", Size: 2688, CreatedAt: "2026-05-14T07:19:09Z"},
				{ID: "abcd-1234", URL: "https://x/y/b.png", Filename: "b.png", ContentType: "image/png", Size: 1500000, CreatedAt: "2026-05-13T11:00:00Z"},
			},
			TotalCount: 2,
		},
	}
	tool := NewListPublishedFilesTool(fake)
	out, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.IsError {
		t.Fatalf("IsError = true: %s", out.Content)
	}
	// Each id should appear in the rendering.
	if !strings.Contains(out.Content, "6b28b36c-d218-448f-adb5-e256218fb025") {
		t.Errorf("output missing first id; got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "abcd-1234") {
		t.Errorf("output missing second id; got:\n%s", out.Content)
	}
	// File sizes should be human-readable.
	if !strings.Contains(out.Content, "2.6 KB") {
		t.Errorf("output missing 2.6 KB rendering of 2688 B; got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "1.4 MB") {
		t.Errorf("output missing MB rendering of 1500000 B; got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "Showing 1-2 of 2") {
		t.Errorf("output missing summary line; got:\n%s", out.Content)
	}
}

func TestListPublishedFilesTool_Run_DefaultLimit(t *testing.T) {
	fake := &fakeListUploader{resp: &uploads.ListResponse{Uploads: []uploads.UploadEntry{}, TotalCount: 0}}
	tool := NewListPublishedFilesTool(fake)
	_, _ = tool.Run(context.Background(), `{}`)
	if fake.gotLimit != listDefaultLimit {
		t.Errorf("limit = %d, want default %d", fake.gotLimit, listDefaultLimit)
	}
	if fake.gotOffset != 0 {
		t.Errorf("offset = %d, want 0", fake.gotOffset)
	}
}

func TestListPublishedFilesTool_Run_ClampsLimitToMax(t *testing.T) {
	fake := &fakeListUploader{resp: &uploads.ListResponse{Uploads: []uploads.UploadEntry{}, TotalCount: 0}}
	tool := NewListPublishedFilesTool(fake)
	_, _ = tool.Run(context.Background(), `{"limit": 9999}`)
	if fake.gotLimit != listMaxLimit {
		t.Errorf("limit = %d, want clamped to %d", fake.gotLimit, listMaxLimit)
	}
}

func TestListPublishedFilesTool_Run_EmptyShowsFriendlyMessage(t *testing.T) {
	fake := &fakeListUploader{resp: &uploads.ListResponse{Uploads: []uploads.UploadEntry{}, TotalCount: 0}}
	tool := NewListPublishedFilesTool(fake)
	out, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.IsError {
		t.Fatalf("IsError = true on empty list")
	}
	if !strings.Contains(out.Content, "haven't published") {
		t.Errorf("empty-list message missing user-friendly text; got:\n%s", out.Content)
	}
}

func TestListPublishedFilesTool_Run_PaginatedHint(t *testing.T) {
	fake := &fakeListUploader{
		resp: &uploads.ListResponse{
			Uploads:    []uploads.UploadEntry{{ID: "a", URL: "x", Filename: "f", ContentType: "text/plain", Size: 1, CreatedAt: "now"}},
			TotalCount: 5,
		},
	}
	tool := NewListPublishedFilesTool(fake)
	out, _ := tool.Run(context.Background(), `{"limit": 1, "offset": 0}`)
	if !strings.Contains(out.Content, "offset=1") {
		t.Errorf("expected next-page hint with offset=1; got:\n%s", out.Content)
	}
}

func TestListPublishedFilesTool_Run_UnauthorizedMapsToPermissionError(t *testing.T) {
	fake := &fakeListUploader{err: uploads.ErrUnauthorized}
	tool := NewListPublishedFilesTool(fake)
	out, _ := tool.Run(context.Background(), `{}`)
	if !out.IsError {
		t.Errorf("expected IsError = true")
	}
	if out.ErrorCategory != agent.ErrCategoryPermission {
		t.Errorf("ErrorCategory = %s, want permission", out.ErrorCategory)
	}
}

// --- RetractPublishedFileTool ---

func TestRetractPublishedFileTool_Info_RequiresApprovalWithDescription(t *testing.T) {
	tool := NewRetractPublishedFileTool(&fakeRetractUploader{})
	if !tool.RequiresApproval() {
		t.Errorf("RequiresApproval() = false, want true")
	}
	info := tool.Info()
	if info.Name != "retract_published_file" {
		t.Errorf("Name = %q", info.Name)
	}
	// Required must include both "id" and "description".
	gotRequired := map[string]bool{}
	for _, r := range info.Required {
		gotRequired[r] = true
	}
	if !gotRequired["id"] {
		t.Errorf("Required missing 'id'; got %v", info.Required)
	}
	if !gotRequired["description"] {
		t.Errorf("Required missing 'description'; got %v", info.Required)
	}
	// Schema must declare a 'description' property — checked here so a future
	// refactor that drops the helper still surfaces.
	props, _ := info.Parameters["properties"].(map[string]any)
	if _, ok := props["description"]; !ok {
		t.Errorf("Parameters.properties missing 'description' field")
	}
}

func TestRetractPublishedFileTool_Run_HappyPath(t *testing.T) {
	fake := &fakeRetractUploader{
		resp: &uploads.DeleteResponse{Deleted: true, ID: "abc", CDNEvictionSeconds: 300},
	}
	tool := NewRetractPublishedFileTool(fake)
	out, err := tool.Run(context.Background(), `{"id":"abc","description":"撤回测试文件"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.IsError {
		t.Errorf("IsError = true: %s", out.Content)
	}
	if fake.gotID != "abc" {
		t.Errorf("Delete called with id = %q, want %q", fake.gotID, "abc")
	}
	if !strings.Contains(out.Content, "Retracted") {
		t.Errorf("success message missing 'Retracted'; got: %s", out.Content)
	}
	if !strings.Contains(out.Content, "300") {
		t.Errorf("success message missing eviction window; got: %s", out.Content)
	}
}

func TestRetractPublishedFileTool_Run_EmptyDescriptionStillExecutes(t *testing.T) {
	// Approval-description spec invariant (CLAUDE.md): the daemon does NOT
	// block execution when `description` is missing or empty. The schema
	// declares it required so the model is nudged to populate it, but tools
	// must NOT enforce — `args` JSON is the source of truth for audit logs
	// and rewriting it would record fiction. UI clients fall back to raw
	// args when description is empty.
	//
	// Symmetric to TestRetractPublishedFileTool_Run_MissingIDIsValidationError
	// (which DOES enforce, because no id means there's nothing to delete).
	fake := &fakeRetractUploader{
		resp: &uploads.DeleteResponse{Deleted: true, ID: "abc", CDNEvictionSeconds: 300},
	}
	tool := NewRetractPublishedFileTool(fake)
	// Missing description entirely.
	out, _ := tool.Run(context.Background(), `{"id":"abc"}`)
	if out.IsError {
		t.Errorf("missing description must not block execution; got error: %s", out.Content)
	}
	// Empty-string description.
	out, _ = tool.Run(context.Background(), `{"id":"abc","description":""}`)
	if out.IsError {
		t.Errorf("empty description must not block execution; got error: %s", out.Content)
	}
	// Whitespace-only description.
	out, _ = tool.Run(context.Background(), `{"id":"abc","description":"   "}`)
	if out.IsError {
		t.Errorf("whitespace-only description must not block execution; got error: %s", out.Content)
	}
}

func TestRetractPublishedFileTool_Run_DeletedFalseOn2xxIsBusinessError(t *testing.T) {
	// Cloud's current contract says deleted=true on every 2xx; the only path
	// to "deleted=false" is a 4xx/5xx with a typed error. But if cloud ever
	// adds a partial-failure mode, we must not silently render "Retracted."
	// to the user. This test pins the defensive guard.
	fake := &fakeRetractUploader{
		resp: &uploads.DeleteResponse{Deleted: false, ID: "abc", CDNEvictionSeconds: 300},
	}
	tool := NewRetractPublishedFileTool(fake)
	out, _ := tool.Run(context.Background(), `{"id":"abc","description":"x"}`)
	if !out.IsError {
		t.Fatal("expected IsError=true when deleted=false on 2xx")
	}
	if out.ErrorCategory != agent.ErrCategoryBusiness {
		t.Errorf("ErrorCategory = %s, want business", out.ErrorCategory)
	}
	if !strings.Contains(out.Content, "uncertain") {
		t.Errorf("user-facing message should signal uncertainty; got: %s", out.Content)
	}
}

func TestRetractPublishedFileTool_Run_MissingIDIsValidationError(t *testing.T) {
	tool := NewRetractPublishedFileTool(&fakeRetractUploader{})
	out, _ := tool.Run(context.Background(), `{"description":"x"}`)
	if !out.IsError {
		t.Fatal("expected IsError = true")
	}
	if out.ErrorCategory != agent.ErrCategoryValidation {
		t.Errorf("ErrorCategory = %s, want validation", out.ErrorCategory)
	}
}

func TestRetractPublishedFileTool_Run_URLAsIDIsValidationError(t *testing.T) {
	// Common LLM mistake: passing the public URL instead of the UUID.
	fake := &fakeRetractUploader{}
	tool := NewRetractPublishedFileTool(fake)
	out, _ := tool.Run(context.Background(), `{"id":"https://static.kocoro.ai/public/x.html","description":"撤回"}`)
	if !out.IsError {
		t.Fatal("expected IsError = true")
	}
	if out.ErrorCategory != agent.ErrCategoryValidation {
		t.Errorf("ErrorCategory = %s, want validation", out.ErrorCategory)
	}
	if fake.gotID != "" {
		t.Errorf("Delete called with %q; expected client to short-circuit on URL-looking id", fake.gotID)
	}
}

func TestRetractPublishedFileTool_Run_NotFoundFriendlyError(t *testing.T) {
	fake := &fakeRetractUploader{err: uploads.ErrNotFound}
	tool := NewRetractPublishedFileTool(fake)
	out, _ := tool.Run(context.Background(), `{"id":"nonexistent","description":"撤回"}`)
	if !out.IsError {
		t.Fatal("expected IsError = true")
	}
	if out.ErrorCategory != agent.ErrCategoryBusiness {
		t.Errorf("ErrorCategory = %s, want business", out.ErrorCategory)
	}
	// User-facing message must mention all three conflated reasons.
	if !strings.Contains(out.Content, "already been retracted") {
		t.Errorf("message missing 'already retracted' wording; got: %s", out.Content)
	}
}

func TestRetractPublishedFileTool_Run_UnauthorizedMapsToPermissionError(t *testing.T) {
	fake := &fakeRetractUploader{err: uploads.ErrUnauthorized}
	tool := NewRetractPublishedFileTool(fake)
	out, _ := tool.Run(context.Background(), `{"id":"abc","description":"x"}`)
	if out.ErrorCategory != agent.ErrCategoryPermission {
		t.Errorf("ErrorCategory = %s, want permission", out.ErrorCategory)
	}
}

func TestRetractPublishedFileTool_Run_TransientMapsToTransientError(t *testing.T) {
	fake := &fakeRetractUploader{err: uploads.ErrTransient}
	tool := NewRetractPublishedFileTool(fake)
	out, _ := tool.Run(context.Background(), `{"id":"abc","description":"x"}`)
	if out.ErrorCategory != agent.ErrCategoryTransient {
		t.Errorf("ErrorCategory = %s, want transient", out.ErrorCategory)
	}
	if !out.IsRetryable {
		t.Errorf("IsRetryable = false, want true for transient errors")
	}
}

// TestRetractPublishedFileTool_NotInAutoApprovalDenyList enforces the Q2
// decision: retract is destructive but NOT in the high-risk denylist that
// blocks "always allow". It must remain togglable per the user's preference.
// If a future change accidentally adds it to agent.DisallowsAutoApproval,
// this test fails so the policy regression is caught.
func TestRetractPublishedFileTool_NotInAutoApprovalDenyList(t *testing.T) {
	if agent.DisallowsAutoApproval("retract_published_file") {
		t.Errorf("retract_published_file is now in the high-risk auto-approval denylist — " +
			"that breaks the documented 'always allow works for retract' UX. " +
			"If this is intentional, update README + recipes.md and remove this test.")
	}
	// Sanity: list is read-only, never approval, definitely not in the denylist.
	if agent.DisallowsAutoApproval("list_my_published_files") {
		t.Errorf("list_my_published_files in the denylist is meaningless (it does not require approval)")
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2688, "2.6 KB"},
		{1500000, "1.4 MB"},
		{2 * 1024 * 1024 * 1024, "2.0 GB"},
	}
	for _, c := range cases {
		got := humanizeBytes(c.n)
		if got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
