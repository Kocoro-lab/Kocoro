package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// listUploader is the seam ListPublishedFilesTool talks to. *uploads.Client
// implements it; tests inject a fake to assert table-rendering without
// standing up an HTTP server.
type listUploader interface {
	List(ctx context.Context, limit, offset int) (*uploads.ListResponse, error)
}

// retractUploader is the seam RetractPublishedFileTool talks to.
type retractUploader interface {
	Delete(ctx context.Context, id string) (*uploads.DeleteResponse, error)
}

// listMaxLimit mirrors Cloud's server-side clamp on GET /api/v1/uploads. We
// pre-clamp locally so the LLM gets a clear validation error instead of
// silently receiving fewer rows than asked for.
const listMaxLimit = 100

// listDefaultLimit is the cloud default when limit is omitted / 0.
const listDefaultLimit = 20

// --- list_my_published_files ---

type ListPublishedFilesTool struct {
	client listUploader
}

func NewListPublishedFilesTool(client listUploader) *ListPublishedFilesTool {
	return &ListPublishedFilesTool{client: client}
}

type listPublishedArgs struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

func (t *ListPublishedFilesTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "list_my_published_files",
		Description: "List the current user's still-active files previously published via " +
			"`publish_to_web`. Returns up to 100 entries per page, newest first. " +
			"Read-only — no approval needed.\n\n" +
			"USE when the user asks 'what have I shared?', 'find that landing page I " +
			"sent yesterday', or before calling `retract_published_file` (the LLM " +
			"needs an `id` from this list — the public URL alone is not enough).\n\n" +
			"DO NOT USE for:\n" +
			"  - Counting all-time uploads. `total_count` is the active (non-retracted) " +
			"count under the current filter; retracted files are not listed.\n" +
			"  - Files published before this feature shipped — they are not tracked and " +
			"cannot be managed via this tool.\n\n" +
			"Output is a human-readable list keyed by `id` (UUID). Pass that id to " +
			"`retract_published_file` to delete.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max entries to return (default 20). Cloud clamps to [1, 100].",
					"minimum":     1,
					"maximum":     listMaxLimit,
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Skip the first N entries (default 0). Combine with `limit` to paginate when `total_count` exceeds the page.",
					"minimum":     0,
				},
			},
		},
	}
}

func (t *ListPublishedFilesTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args listPublishedArgs
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	limit := args.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	if limit > listMaxLimit {
		limit = listMaxLimit
	}
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}

	res, err := t.client.List(ctx, limit, offset)
	if err != nil {
		return classifyListErr(err), nil
	}

	if len(res.Uploads) == 0 {
		if offset > 0 {
			return agent.ToolResult{Content: fmt.Sprintf(
				"No files at offset %d. total_count = %d.", offset, res.TotalCount,
			)}, nil
		}
		return agent.ToolResult{Content: "You haven't published any files via publish_to_web yet, or all of them have been retracted."}, nil
	}

	var sb strings.Builder
	for i, u := range res.Uploads {
		fmt.Fprintf(&sb, "[%d] %s\n", offset+i+1, u.ID)
		fmt.Fprintf(&sb, "    %s  (%s, %s)  created %s\n",
			u.Filename, humanizeBytes(u.Size), u.ContentType, u.CreatedAt)
		fmt.Fprintf(&sb, "    %s\n\n", u.URL)
	}
	end := offset + len(res.Uploads)
	fmt.Fprintf(&sb, "Showing %d-%d of %d active file(s).", offset+1, end, res.TotalCount)
	if end < res.TotalCount {
		fmt.Fprintf(&sb, " Call again with offset=%d to see the next page.", end)
	}
	return agent.ToolResult{Content: sb.String()}, nil
}

func (t *ListPublishedFilesTool) RequiresApproval() bool { return false }

// IsReadOnlyCall lets the agent loop batch this with other read-only tools
// (file_read, grep, directory_list, etc.) in parallel rather than serializing
// against any write-effect call. The List endpoint is pure GET and idempotent.
func (t *ListPublishedFilesTool) IsReadOnlyCall(string) bool { return true }

var _ agent.ReadOnlyChecker = (*ListPublishedFilesTool)(nil)

// --- retract_published_file ---

type RetractPublishedFileTool struct {
	client retractUploader
}

func NewRetractPublishedFileTool(client retractUploader) *RetractPublishedFileTool {
	return &RetractPublishedFileTool{client: client}
}

type retractArgs struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
}

func (t *RetractPublishedFileTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "retract_published_file",
		Description: "⚠️ Retract (delete) a previously published file from Shannon Cloud's " +
			"public CDN. Soft-deletes the DB row and hard-deletes the S3 object — " +
			"this is NOT reversible.\n\n" +
			"USE when the user explicitly asks to retract / delete / revoke / unpublish " +
			"a file they previously shared. Always confirm with the user which file " +
			"before calling — if there's any ambiguity, list first with " +
			"`list_my_published_files` and ask which `id` to retract.\n\n" +
			"DO NOT USE for:\n" +
			"  - Files the current user did not publish — cross-user retracts return " +
			"a 'not found' error by design (cloud avoids existence leaks).\n" +
			"  - Files published before this feature shipped — they cannot be retracted " +
			"via the API; tell the user this is a pre-existing limitation.\n\n" +
			"Caveats:\n" +
			"  - CDN edge nodes may still serve cached content for up to 5 minutes " +
			"after a successful retract. The success message reports the exact " +
			"window. Surface this to the user if they ask 'why is the URL still " +
			"working?'.\n" +
			"  - Pass the `id` (UUID) from `list_my_published_files`, NOT the URL." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The file's id (UUID) obtained from `list_my_published_files`. NOT the public URL.",
				},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"id", "description"},
	}
}

func (t *RetractPublishedFileTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args retractArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return agent.ValidationError(
			"id is required. Get it from `list_my_published_files` — do not pass the URL.",
		), nil
	}
	if strings.HasPrefix(id, "http://") || strings.HasPrefix(id, "https://") {
		return agent.ValidationError(
			"id looks like a URL. Call `list_my_published_files` first and pass the UUID `id` field instead.",
		), nil
	}

	res, err := t.client.Delete(ctx, id)
	if err != nil {
		return classifyRetractErr(err), nil
	}
	// Today's cloud contract makes `deleted=false` on a 2xx unreachable, but
	// guard so a future partial-failure mode (or a contract drift) does not
	// silently render "Retracted." to the user when nothing was actually
	// deleted. One line of insurance — caught by a code reviewer.
	if !res.Deleted {
		return agent.BusinessError(fmt.Sprintf(
			"retract_published_file: cloud responded 2xx but deleted=false (id=%s). "+
				"This is unexpected — the file may NOT have been removed. Re-run "+
				"`list_my_published_files` to confirm and tell the user the result is uncertain.",
			res.ID)), nil
	}

	return agent.ToolResult{
		Content: fmt.Sprintf(
			"Retracted.\nID: %s\nS3 deletion: complete.\nCDN eviction window: %ds — the public URL may still serve cached content from edge nodes for up to %ds. Tell the user this if they check the URL immediately.",
			res.ID, res.CDNEvictionSeconds, res.CDNEvictionSeconds,
		),
	}, nil
}

func (t *RetractPublishedFileTool) RequiresApproval() bool { return true }

// No SafeChecker implementation: every call falls through to the standard
// approval prompt. `always_allow_tools` (per-agent or global) will skip the
// prompt once the user opts in — that's intentional. Retract is destructive
// but not paid and not capable of creating permanent public outputs, so the
// stricter `DisallowsAutoApproval` denylist (publish_to_web / generate_image /
// edit_image) does NOT include retract.

// classifyListErr maps the typed errors from internal/uploads onto the agent's
// ToolResult error categories.
func classifyListErr(err error) agent.ToolResult {
	switch {
	case errors.Is(err, uploads.ErrUnauthorized):
		return agent.PermissionError(fmt.Sprintf(
			"list_my_published_files: %v — check that cloud.api_key is configured and valid.", err))
	case errors.Is(err, uploads.ErrEndpointNotFound):
		return agent.BusinessError(fmt.Sprintf(
			"list_my_published_files: %v. The cloud deployment does not yet expose GET /api/v1/uploads. Tell the user this is a server-side limitation; retrying will not help.", err))
	case errors.Is(err, uploads.ErrTransient):
		return agent.TransientError(fmt.Sprintf("list_my_published_files: %v", err))
	default:
		return agent.ToolResult{
			Content: fmt.Sprintf("list_my_published_files error: %v", err),
			IsError: true,
		}
	}
}

// classifyRetractErr is the retract version. 404 → friendly BusinessError that
// surfaces all three cloud-conflated causes ("not yours / already retracted /
// invalid id") so the LLM can relay the right thing to the user.
//
// Note: ErrEndpointNotFound is intentionally NOT handled here — the delete op
// in uploads.classifyError maps every 404 (including a bare proxy 404 from a
// not-deployed cloud) onto ErrNotFound, so this code path cannot surface
// ErrEndpointNotFound. Adding a defensive branch would be dead code.
func classifyRetractErr(err error) agent.ToolResult {
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		return agent.BusinessError(
			"retract_published_file: file not found. The cloud returns this for three " +
				"conflated reasons — the id doesn't exist, the file has already been " +
				"retracted, or it belongs to a different user. Confirm by calling " +
				"`list_my_published_files` and check that the id is present.")
	case errors.Is(err, uploads.ErrUnauthorized):
		return agent.PermissionError(fmt.Sprintf(
			"retract_published_file: %v — check that cloud.api_key is configured and valid.", err))
	case errors.Is(err, uploads.ErrTransient):
		return agent.TransientError(fmt.Sprintf("retract_published_file: %v", err))
	default:
		return agent.ToolResult{
			Content: fmt.Sprintf("retract_published_file error: %v", err),
			IsError: true,
		}
	}
}

// humanizeBytes renders a byte count as a short human-readable string
// ("1.2 KB", "17.8 MB"). Used in list output so the LLM can relay sizes to
// the user without converting itself.
func humanizeBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
