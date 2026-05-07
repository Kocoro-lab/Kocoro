package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// publishMaxBytes mirrors the server-side 50 MiB cap. We pre-check locally so
// large files fail fast with a clear ValidationError rather than wasting a
// 50 MiB POST that ends in 413.
const publishMaxBytes int64 = 50 << 20

// publishMinPurposeLen — purposes shorter than this are too short to convey
// intent; LLMs tend to fill "x" / "share" when the field is mandatory but
// they're not thinking. We force at least one informative phrase.
const publishMinPurposeLen = 10

// uploader is the seam the tool talks to. *uploads.Client implements it; tests
// inject a fake to assert error classification without standing up an HTTP server.
type uploader interface {
	Upload(ctx context.Context, filename, contentType string,
		openBody func() (io.ReadCloser, error)) (*uploads.UploadResponse, error)
}

// pathDenyComponents are case-insensitive path-segment matches. Any one of
// these appearing as its own segment in the resolved path rejects the upload.
// Anchored to whole segments so "secrets/" inside a path is denied but
// "secret-recipes.html" is not.
var pathDenyComponents = map[string]bool{
	".env":        true,
	".envrc":      true,
	".ssh":        true,
	".aws":        true,
	".gcp":        true,
	".azure":      true,
	".npmrc":      true,
	".netrc":      true,
	".pgpass":     true,
	".gitconfig":  true,
	".kube":       true,
	"credentials": true,
	"credential":  true,
	"secrets":     true,
	"secret":      true,
	"id_rsa":      true,
	"id_ed25519":  true,
	"id_ecdsa":    true,
	"id_dsa":      true,
}

// pathDenySuffixes match the file's basename extension, including disguised
// double extensions such as cert.key.txt. Catches private-key and
// signing-material formats that should never be published.
var pathDenySuffixes = map[string]bool{
	".pem":      true,
	".key":      true,
	".p12":      true,
	".pfx":      true,
	".jks":      true,
	".keystore": true,
	".asc":      true,
	".gpg":      true,
}

// defaultExtAllowlist is the baseline set of extensions allowed for publish.
// Skewed toward "things you'd share with an external recipient" — documents,
// images, public data, media. Source code, configs, and archives are
// deliberately excluded because they're the most common accidental leak.
var defaultExtAllowlist = map[string]bool{
	// documents
	".html": true, ".htm": true, ".md": true, ".txt": true, ".pdf": true,
	// images
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true,
	// data / reports
	".csv": true, ".json": true,
	// media
	".mp4": true, ".mp3": true, ".wav": true, ".webm": true,
}

// vaguePurposes are placeholder strings LLMs fall back to when forced to fill
// the field but not actually thinking about why. Reject these explicitly.
var vaguePurposes = map[string]bool{
	"share":   true,
	"send":    true,
	"send it": true,
	"upload":  true,
	"test":    true,
	"todo":    true,
	"asdf":    true,
	"foo":     true,
	"bar":     true,
	"x":       true,
	"y":       true,
	"hi":      true,
	"hello":   true,

	"for testing":     true,
	"for test":        true,
	"share with team": true,
	"share with user": true,
	"send to user":    true,
	"show the user":   true,
	"for the user":    true,
	"for review":      true,
}

type PublishToWebTool struct {
	client       uploader
	extAllowlist map[string]bool
}

// NewPublishToWebTool constructs the tool with the given uploads client and
// the effective extension allowlist. Pass nil for extAllowlist to use the
// default set unmodified.
func NewPublishToWebTool(client uploader, extAllowlist map[string]bool) *PublishToWebTool {
	if extAllowlist == nil {
		extAllowlist = defaultExtAllowlist
	}
	return &PublishToWebTool{client: client, extAllowlist: extAllowlist}
}

type publishArgs struct {
	Path        string `json:"path"`
	Purpose     string `json:"purpose"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

func (t *PublishToWebTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "publish_to_web",
		Description: "⚠️ Publishes a local file to a PUBLIC, PERMANENT URL on Shannon Cloud.\n\n" +
			"Anyone who obtains the URL can read the file. There is no DELETE — once\n" +
			"published, the file stays accessible forever.\n\n" +
			"USE ONLY when the user explicitly asked to share / publish / send the file\n" +
			"to an external recipient (Slack/LINE/Feishu reply, web link, email, etc.).\n\n" +
			"DO NOT USE for:\n" +
			"  - Backup, sync, or 'just in case' uploads\n" +
			"  - Source code, config files, .env, credentials, private keys, logs\n" +
			"  - Sharing files between agent runs (use file_read/file_write locally)\n" +
			"  - Inline previews inside Kocoro Desktop (use the kocoro-generative-ui\n" +
			"    skill's html-artifact blocks instead — those render in-app, no public URL)\n\n" +
			"The 'purpose' parameter is shown to the user during approval. Be specific\n" +
			"about WHY this file needs to be public (e.g. 'send landing page draft to\n" +
			"user via Slack reply'). Vague purposes ('share', 'send it', 'test') are rejected.\n\n" +
			"Constraints:\n" +
			"  - 50 MiB hard limit per file\n" +
			"  - Allowed extensions: html, md, txt, pdf, png, jpg, jpeg, gif, webp, svg, csv, json, mp4, mp3, wav, webm\n" +
			"  - Each upload returns a fresh URL — no idempotent re-upload",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Local file path. Relative paths resolve against the session CWD.",
				},
				"purpose": map[string]any{
					"type":        "string",
					"minLength":   publishMinPurposeLen,
					"maxLength":   500,
					"description": "One sentence: why does this file need to be PUBLIC? Shown to the user during approval. Be specific. Vague answers ('share', 'send it', 'test') will be rejected.",
				},
				"filename": map[string]any{
					"type":        "string",
					"description": "Optional. Override the filename embedded in the public URL. Defaults to basename(path).",
				},
				"content_type": map[string]any{
					"type":        "string",
					"description": "Optional. Override Content-Type. Server falls back to extension sniff.",
				},
			},
		},
		Required: []string{"path", "purpose"},
	}
}

func (t *PublishToWebTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args publishArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return agent.ValidationError("path is required"), nil
	}
	if res, ok := validatePurpose(args.Purpose); !ok {
		return res, nil
	}

	resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"publish_to_web: no session working directory is set. Pass an absolute path.",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("publish_to_web: %v", resolveErr)), nil
	}

	// First-pass guards on the user-supplied path (string-only, no filesystem).
	// This catches obvious cases (".pem" suffix, "/credentials/" segment, ".go"
	// extension) before any filesystem call so non-existent paths still get a
	// clear BusinessError instead of a confusing "file not found" message.
	if res, ok := checkPathBlocked(resolved); !ok {
		return res, nil
	}
	if res, ok := t.checkExtension(resolved); !ok {
		return res, nil
	}

	// Second pass: resolve symlinks and re-run the same guards on the real
	// path. Without this, an attacker (or a careless rm/mv chain) could expose
	// a forbidden file via a symlink whose own path looks innocent — e.g.
	// `ln -s ~/.ssh/id_rsa /tmp/cute.html` would pass the first-pass checks.
	realPath, evalErr := filepath.EvalSymlinks(resolved)
	if evalErr != nil {
		if os.IsNotExist(evalErr) {
			return agent.ValidationError(fmt.Sprintf("file not found: %s", resolved)), nil
		}
		return agent.ValidationError(fmt.Sprintf("publish_to_web: cannot resolve path %s: %v", resolved, evalErr)), nil
	}
	if realPath != resolved {
		if res, ok := checkPathBlocked(realPath); !ok {
			return res, nil
		}
		if res, ok := t.checkExtension(realPath); !ok {
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
	if info.Size() > publishMaxBytes {
		return agent.ValidationError(fmt.Sprintf(
			"file too large: %d bytes (max %d / 50 MiB). Server will reject; reduce or split the file.",
			info.Size(), publishMaxBytes)), nil
	}

	filename := args.Filename
	if filename == "" {
		filename = filepath.Base(resolved)
	}

	openBody := func() (io.ReadCloser, error) { return os.Open(resolved) }

	res, err := t.client.Upload(ctx, filename, args.ContentType, openBody)
	if err != nil {
		return classifyUploadErr(err), nil
	}

	return agent.ToolResult{
		Content: fmt.Sprintf(
			"Published.\nURL: %s\nSize: %d bytes\nContent-Type: %s\nPurpose: %s",
			res.URL, res.Size, res.ContentType, args.Purpose,
		),
	}, nil
}

func (t *PublishToWebTool) RequiresApproval() bool { return true }

// IsSafeArgs always returns false: publish_to_web side-effects (a permanent
// public URL) are never auto-approvable, no matter the args.
func (t *PublishToWebTool) IsSafeArgs(string) bool { return false }

var _ agent.SafeChecker = (*PublishToWebTool)(nil)

// validatePurpose enforces the purpose-field hygiene: not blank, not too short,
// not a placeholder. Returns ok=false with the ToolResult to return verbatim.
func validatePurpose(purpose string) (agent.ToolResult, bool) {
	trimmed := strings.TrimSpace(purpose)
	if trimmed == "" {
		return agent.ValidationError(
			"purpose is required. Briefly state why this file needs to be PUBLIC " +
				"(e.g. 'send landing page draft to user via Slack reply').",
		), false
	}
	if vaguePurposes[normalizePurpose(trimmed)] {
		return agent.ValidationError(
			"purpose is too vague. State the actual reason this file needs to be public " +
				"(who is the recipient, what channel, what for).",
		), false
	}
	if len(trimmed) < publishMinPurposeLen {
		return agent.ValidationError(fmt.Sprintf(
			"purpose too short (%d chars, min %d). Be specific about why this file needs to be public.",
			len(trimmed), publishMinPurposeLen,
		)), false
	}
	return agent.ToolResult{}, true
}

func normalizePurpose(purpose string) string {
	return strings.ToLower(strings.Join(strings.Fields(purpose), " "))
}

// checkPathBlocked rejects the upload if the resolved path traverses any
// segment in pathDenyComponents (case-insensitive, exact-segment match), the
// basename matches a sensitive-file pattern, or the basename has a denied
// suffix.
func checkPathBlocked(resolved string) (agent.ToolResult, bool) {
	clean := filepath.Clean(resolved)
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if seg == "" {
			continue
		}
		if pathDenyComponents[strings.ToLower(seg)] {
			return agent.BusinessError(fmt.Sprintf(
				"refusing to publish: path contains sensitive segment %q (path: %s)",
				seg, clean,
			)), false
		}
	}
	base := strings.ToLower(filepath.Base(clean))
	if permissions.IsSensitiveFile(base) {
		return agent.BusinessError(fmt.Sprintf(
			"refusing to publish: filename %q matches the sensitive-file blocklist (path: %s)",
			filepath.Base(clean), clean,
		)), false
	}
	for suffix := range pathDenySuffixes {
		if strings.HasSuffix(base, suffix) || strings.Contains(base, suffix+".") {
			return agent.BusinessError(fmt.Sprintf(
				"refusing to publish: file extension %q is in the credential/key blocklist (path: %s)",
				suffix, clean,
			)), false
		}
	}
	return agent.ToolResult{}, true
}

// checkExtension rejects the upload if the file extension is not in the
// effective allowlist (default + cloud.publish_allowed_extensions config).
func (t *PublishToWebTool) checkExtension(resolved string) (agent.ToolResult, bool) {
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext == "" {
		return agent.BusinessError(
			"refusing to publish: file has no extension. Allowed extensions: " +
				summariseAllowlist(t.extAllowlist),
		), false
	}
	if !t.extAllowlist[ext] {
		return agent.BusinessError(fmt.Sprintf(
			"refusing to publish: extension %q is not in the publish allowlist. "+
				"Allowed: %s. To allow more, set cloud.publish_allowed_extensions in config.",
			ext, summariseAllowlist(t.extAllowlist),
		)), false
	}
	return agent.ToolResult{}, true
}

// summariseAllowlist returns a stable, comma-separated rendering of the
// allowlist for error messages.
func summariseAllowlist(allow map[string]bool) string {
	exts := make([]string, 0, len(allow))
	for ext := range allow {
		exts = append(exts, ext)
	}
	// stable order
	for i := 1; i < len(exts); i++ {
		for j := i; j > 0 && exts[j-1] > exts[j]; j-- {
			exts[j-1], exts[j] = exts[j], exts[j-1]
		}
	}
	return strings.Join(exts, ", ")
}

// classifyUploadErr maps the typed errors from internal/uploads onto the
// agent's ToolResult error categories so the loop can decide retry/escalate.
func classifyUploadErr(err error) agent.ToolResult {
	switch {
	case errors.Is(err, uploads.ErrUnauthorized):
		return agent.PermissionError(fmt.Sprintf(
			"publish_to_web: %v — check that cloud.api_key is configured and valid.", err))
	case errors.Is(err, uploads.ErrEndpointNotFound):
		return agent.BusinessError(fmt.Sprintf(
			"publish_to_web: %v. The gateway at cloud.endpoint responded, but does not "+
				"expose POST /api/v1/uploads. Confirm the Shannon Cloud deployment "+
				"includes the uploads handler, or point cloud.endpoint at an environment "+
				"that does. Tell the user to ask the cloud team — this is not a "+
				"client-side issue and retrying will not help.", err))
	case errors.Is(err, uploads.ErrFileTooLarge):
		return agent.ValidationError(fmt.Sprintf("publish_to_web: %v", err))
	case errors.Is(err, uploads.ErrBadRequest):
		return agent.ValidationError(fmt.Sprintf("publish_to_web: %v", err))
	case errors.Is(err, uploads.ErrServerConfig):
		return agent.BusinessError(fmt.Sprintf(
			"publish_to_web: cloud upload not configured server-side: %v", err))
	case errors.Is(err, uploads.ErrTransient):
		return agent.TransientError(fmt.Sprintf("publish_to_web: %v", err))
	default:
		return agent.ToolResult{
			Content: fmt.Sprintf("publish_to_web error: %v", err),
			IsError: true,
		}
	}
}
