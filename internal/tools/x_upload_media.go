package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// X's own per-media caps (docs.x.com media upload): images 5 MB, GIFs 15 MB.
// Enforced locally so an oversize file fails fast with a clear message before
// any network transfer. When these bind, the fix is on the user's side
// (compress/resize); X rejects larger files regardless of what we send.
const (
	xMediaMaxImageBytes int64 = 5 << 20
	xMediaMaxGIFBytes   int64 = 15 << 20
)

// xMediaContentTypes doubles as the media extension allowlist (v1 = images +
// GIF; video is a follow-up with its own transfer lane) and the explicit
// Content-Type sent on the CDN upload so the transfer URL serves the right
// MIME without relying on server-side extension sniffing.
var xMediaContentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// xMediaUploader is the uploads-client seam: Upload stages the file on the
// CDN, Delete retracts the staging copy after the X hand-off.
type xMediaUploader interface {
	Upload(ctx context.Context, openBody func() (io.ReadCloser, error),
		opts uploads.UploadOptions) (*uploads.UploadResponse, error)
	Delete(ctx context.Context, id string) (*uploads.DeleteResponse, error)
}

// xMediaExecuteFn is the Cloud execute seam. Production binds
// GatewayClient.ExecuteIntegrationToolWithIdentity against the Cloud-side
// x_upload_media integration tool (POST
// /api/v1/integrations/tools/x_upload_media/execute), which fetches the staged
// media_url and runs X's INIT/APPEND/FINALIZE flow.
type xMediaExecuteFn func(ctx context.Context, name string, args map[string]any, requestID string) (*client.ToolExecuteResponse, error)

// XUploadMediaTool uploads one local image to X and returns the media_id for
// the X posting tools. Internally: local guards → stage on the Cloud CDN →
// Cloud x_upload_media execute (media_url) → best-effort delete of the staging
// upload → media_id. The model never sees the intermediate URL flow, so it
// cannot skip the finalize step or leak a half-uploaded id.
type XUploadMediaTool struct {
	uploads   xMediaUploader
	execute   xMediaExecuteFn
	authGuard *authSensitiveToolGuard
}

func (t *XUploadMediaTool) setAuthSensitiveToolGuard(guard *authSensitiveToolGuard) {
	t.authGuard = guard
}

// NewXUploadMediaTool wires the two seams. Tests inject fakes; production goes
// through RegisterXUploadMediaTool.
func NewXUploadMediaTool(uploader xMediaUploader, execute xMediaExecuteFn) *XUploadMediaTool {
	return &XUploadMediaTool{uploads: uploader, execute: execute}
}

type xUploadMediaArgs struct {
	FilePath    string `json:"file_path"`
	AltText     string `json:"alt_text,omitempty"`
	Description string `json:"description,omitempty"`
}

func (t *XUploadMediaTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "x_upload_media",
		Description: "Uploads a LOCAL image file to X (Twitter) and returns a media_id.\n\n" +
			"Use this BEFORE the X posting/DM tools when the user wants to attach a\n" +
			"local image: call it once per file, then pass the returned media_id(s)\n" +
			"to the posting tool. Requires an active X connection (Settings →\n" +
			"Integrations). The file is staged through Shannon Cloud for the\n" +
			"transfer and the staging copy is removed afterwards.\n\n" +
			"Formats: jpg, jpeg, png, gif, webp. Size limits (X's own): images\n" +
			"5 MB, GIFs 15 MB. Video is not supported yet.\n\n" +
			"media_id values expire on X (typically within ~24 hours) — upload\n" +
			"shortly before posting, and do not stockpile ids." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Local image file path. Relative paths resolve against the session CWD.",
				},
				"alt_text": map[string]any{
					"type":        "string",
					"description": "Optional accessibility description of the image, attached to the media on X.",
				},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"file_path", "description"},
	}
}

func (t *XUploadMediaTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	return runAuthSensitiveTool(t.authGuard, func() (agent.ToolResult, error) {
		return t.run(ctx, argsJSON)
	})
}

func (t *XUploadMediaTool) run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args xUploadMediaArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.FilePath) == "" {
		return agent.ValidationError("file_path is required"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("x_upload_media: missing required `description` parameter"), nil
	}

	resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.FilePath)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"x_upload_media: no session working directory is set. Pass an absolute path.",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("x_upload_media: %v", resolveErr)), nil
	}

	// Same two-pass guard structure as publish_to_web: string-only checks on
	// the supplied path first, then again on the symlink-resolved real path so
	// `ln -s ~/.ssh/id_rsa cute.png` cannot smuggle a forbidden file out.
	if res, ok := checkPathBlocked(resolved); !ok {
		return res, nil
	}
	if res, ok := checkXMediaExtension(resolved); !ok {
		return res, nil
	}
	realPath, evalErr := filepath.EvalSymlinks(resolved)
	if evalErr != nil {
		if os.IsNotExist(evalErr) {
			return agent.ValidationError(fmt.Sprintf("file not found: %s", resolved)), nil
		}
		return agent.ValidationError(fmt.Sprintf("x_upload_media: cannot resolve path %s: %v", resolved, evalErr)), nil
	}
	if realPath != resolved {
		if res, ok := checkPathBlocked(realPath); !ok {
			return res, nil
		}
		if res, ok := checkXMediaExtension(realPath); !ok {
			return res, nil
		}
	}
	resolved = realPath

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.ValidationError(fmt.Sprintf("file not found: %s", resolved)), nil
		}
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot stat %s: permission denied", resolved)), nil
		}
		return agent.ValidationError(fmt.Sprintf("stat error: %v", err)), nil
	}
	if info.IsDir() {
		return agent.ValidationError(fmt.Sprintf("path is a directory, not a file: %s", resolved)), nil
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	maxBytes := xMediaMaxImageBytes
	limitLabel := "5 MB (X image limit)"
	if ext == ".gif" {
		maxBytes = xMediaMaxGIFBytes
		limitLabel = "15 MB (X GIF limit)"
	}
	if info.Size() > maxBytes {
		return agent.ValidationError(fmt.Sprintf(
			"file too large for X: %d bytes exceeds %s. Compress or resize the file first.",
			info.Size(), limitLabel)), nil
	}

	requestID := ""
	if execution, ok := agent.SideEffectExecutionFromContext(ctx); ok {
		requestID = execution.ExecutionID
	} else if invocation, ok := agent.ToolInvocationFromContext(ctx); ok {
		requestID = invocation.ToolUseID
	}

	openBody := func() (io.ReadCloser, error) { return os.Open(resolved) }
	staged, err := t.uploads.Upload(ctx, openBody, uploads.UploadOptions{
		Filename:    filepath.Base(resolved),
		ContentType: xMediaContentTypes[ext],
		Kind:        uploads.KindImage,
		Metadata:    json.RawMessage(`{"purpose":"x_media"}`),
	})
	if err != nil {
		return classifyXMediaStagingErr(err), nil
	}
	// The staging copy exists only to hand bytes to Cloud; retract it whether
	// or not the X transfer succeeds. Best-effort — a failed delete is logged
	// and never overrides the primary result.
	defer t.cleanupStagedUpload(ctx, staged.ID)

	execArgs := map[string]any{"media_url": staged.URL}
	if strings.TrimSpace(args.AltText) != "" {
		execArgs["alt_text"] = args.AltText
	}
	resp, execErr := t.execute(ctx, "x_upload_media", execArgs, requestID)
	if execErr != nil {
		return classifyXMediaExecuteErr(execErr), nil
	}

	toolUsage := convertAndEmitXMediaUsage(ctx, resp.Usage)

	if resp.Error != nil && *resp.Error != "" {
		return agent.ToolResult{Content: *resp.Error, IsError: true, Usage: toolUsage}, nil
	}

	var out struct {
		MediaID          string `json:"media_id"`
		ExpiresAfterSecs int64  `json:"expires_after_secs"`
	}
	_ = json.Unmarshal(resp.Output, &out)
	if out.MediaID == "" {
		return agent.ToolResult{
			Content: fmt.Sprintf(
				"x_upload_media: Cloud reported success but returned no media_id. Raw output: %s",
				strings.TrimSpace(string(resp.Output))),
			IsError: true,
			Usage:   toolUsage,
		}, nil
	}

	expiry := "X media ids expire (typically within ~24 hours); use it in the posting tool soon."
	if out.ExpiresAfterSecs > 0 {
		expiry = fmt.Sprintf("Expires in about %s; use it in the posting tool before then.",
			(time.Duration(out.ExpiresAfterSecs) * time.Second).String())
	}
	content := fmt.Sprintf("Media uploaded to X.\nmedia_id: %s\n%s", out.MediaID, expiry)
	if strings.TrimSpace(args.AltText) != "" {
		content += "\nAlt text attached."
	}
	return agent.ToolResult{Content: content, Usage: toolUsage}, nil
}

// convertAndEmitXMediaUsage mirrors ServerTool.Run's usage handling so the
// Cloud-reported X media charge lands on the ToolResult for audit attribution
// AND in the per-run usage accumulator via EmitUsage.
func convertAndEmitXMediaUsage(ctx context.Context, u *client.ToolUsage) *agent.ToolUsage {
	if u == nil {
		return nil
	}
	totalTokens := u.TotalTokens
	if totalTokens == 0 {
		totalTokens = u.Tokens
	}
	if totalTokens == 0 {
		totalTokens = u.InputTokens + u.OutputTokens
	}
	model := u.Model
	if model == "" {
		model = u.CostModel
	}
	agent.EmitUsage(ctx, agent.TurnUsage{
		Provider:     u.Provider,
		CostModel:    u.CostModel,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  totalTokens,
		CostUSD:      u.CostUSD,
		Model:        model,
		Units:        u.Units,
		UnitType:     u.UnitType,
	})
	return &agent.ToolUsage{
		Provider:     u.Provider,
		Model:        model,
		CostModel:    u.CostModel,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  totalTokens,
		CostUSD:      u.CostUSD,
		Units:        u.Units,
		UnitType:     u.UnitType,
	}
}

// cleanupStagedUpload retracts the CDN staging copy. Runs on
// context.WithoutCancel so a cancelled tool call still cleans up; bounded so a
// dead gateway cannot hang the deferred path.
func (t *XUploadMediaTool) cleanupStagedUpload(ctx context.Context, id string) {
	if id == "" {
		// Older Cloud deployments omit the upload id; the staging copy stays in
		// the user's library where list/retract tools can still reach it.
		log.Printf("x_upload_media: staged upload has no id; skipping cleanup")
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := t.uploads.Delete(cleanupCtx, id); err != nil {
		log.Printf("x_upload_media: failed to delete staged upload %s: %v", id, err)
	}
}

// checkXMediaExtension rejects files outside the X media allowlist.
func checkXMediaExtension(resolved string) (agent.ToolResult, bool) {
	ext := strings.ToLower(filepath.Ext(resolved))
	if _, ok := xMediaContentTypes[ext]; !ok {
		return agent.ValidationError(fmt.Sprintf(
			"unsupported media type %q — X accepts jpg, jpeg, png, gif, webp (video not supported yet)",
			ext)), false
	}
	return agent.ToolResult{}, true
}

// classifyXMediaStagingErr maps uploads-client errors from the CDN staging
// step. Mirrors publish_to_web's mapping with x_upload_media wording.
func classifyXMediaStagingErr(err error) agent.ToolResult {
	switch {
	case errors.Is(err, uploads.ErrUnauthorized):
		return agent.PermissionError(fmt.Sprintf(
			"x_upload_media: %v — check that the daemon is signed in with a valid API key.", err))
	case errors.Is(err, uploads.ErrFileTooLarge), errors.Is(err, uploads.ErrBadRequest):
		return agent.ValidationError(fmt.Sprintf("x_upload_media: %v", err))
	case errors.Is(err, uploads.ErrEndpointNotFound), errors.Is(err, uploads.ErrServerConfig):
		return agent.BusinessError(fmt.Sprintf(
			"x_upload_media: media staging is unavailable on this Cloud deployment: %v", err))
	case errors.Is(err, uploads.ErrTransient):
		return agent.TransientError(fmt.Sprintf("x_upload_media: %v", err))
	default:
		return agent.ToolResult{Content: fmt.Sprintf("x_upload_media staging error: %v", err), IsError: true}
	}
}

// classifyXMediaExecuteErr maps Cloud integration-execute errors from the X
// transfer step. Structured codes take precedence; status classes back-fill.
func classifyXMediaExecuteErr(err error) agent.ToolResult {
	var integrationErr *client.IntegrationToolAPIError
	if errors.As(err, &integrationErr) {
		msg := fmt.Sprintf("x_upload_media: %v", err)
		switch integrationErr.Code {
		case "auth_expired", "connection_not_found", "connection_inactive":
			return agent.PermissionError(msg + " — reconnect the X integration from Settings → Integrations")
		case "tool_not_allowed", "feature_disabled":
			return agent.BusinessError(msg + " — the X integration is not connected or media upload is not enabled")
		case "integration_limit_exceeded":
			return agent.BusinessError(msg + ": the integration usage limit was reached")
		case "provider_unavailable":
			return agent.TransientError(msg + ": X is temporarily unavailable")
		}
		if integrationErr.StatusCode >= http.StatusInternalServerError ||
			integrationErr.StatusCode == http.StatusTooManyRequests {
			return agent.TransientError(msg)
		}
		if integrationErr.StatusCode == http.StatusBadRequest ||
			integrationErr.StatusCode == http.StatusUnprocessableEntity {
			return agent.ValidationError(msg)
		}
		return agent.BusinessError(msg)
	}
	return agent.ToolResult{Content: fmt.Sprintf("x_upload_media error: %v", err), IsError: true}
}

func (t *XUploadMediaTool) RequiresApproval() bool { return true }

// IsSafeArgs always returns false: the call spends X API quota and sends a
// local file off the machine — never auto-approvable on args alone.
func (t *XUploadMediaTool) IsSafeArgs(string) bool { return false }

var _ agent.SafeChecker = (*XUploadMediaTool)(nil)
