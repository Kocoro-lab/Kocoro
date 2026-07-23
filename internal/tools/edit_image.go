package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/images"
)

// imageEdit abstracts the *images.Client.Edit surface this tool depends on so
// tests can inject a fake without standing up an HTTP server.
type imageEdit interface {
	Edit(ctx context.Context, req images.EditRequest) (*images.GenerateResponse, error)
}

// kocoroCDNPrefix is the only URL prefix the /api/v1/images/edits endpoint
// accepts for image_urls entries. Pre-validating client-side avoids a wasted
// HTTP round-trip and gives the LLM an immediate, friendlier error than the
// upstream's "invalid_image_url" code.
const kocoroCDNPrefix = "https://static.kocoro.ai/"

const (
	editImageURLsMin = 1
	editImageURLsMax = 4
)

type EditImageTool struct {
	client imageEdit
}

func NewEditImageTool(client imageEdit) *EditImageTool {
	return &EditImageTool{client: client}
}

type editImageArgs struct {
	Prompt      string   `json:"prompt"`
	ImageURLs   []string `json:"image_urls"`
	Description string   `json:"description,omitempty"`
	Size        string   `json:"size,omitempty"`
	Quality     string   `json:"quality,omitempty"`
	N           int      `json:"n,omitempty"`
	Background  string   `json:"background,omitempty"`
}

func (t *EditImageTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "edit_image",
		Description: "⚠️ Edits one or more existing images via Shannon Cloud (POST " +
			"/api/v1/images/edits, backed by gpt-image-2). Returns a permanent " +
			"public CDN URL for the modified image — anyone with the URL can view " +
			"it; there is no DELETE.\n\n" +
			"REQUIREMENT: every image_urls entry MUST start with " +
			"https://static.kocoro.ai/ (Shannon CDN). External URLs are rejected. " +
			"If the user provides an external URL, first call publish_to_web to " +
			"upload it, or generate_image to produce one — then pass the resulting " +
			"CDN URL here.\n\n" +
			"USE FOR:\n" +
			"  - 'modify this image / change X to Y / redraw in cartoon style'\n" +
			"  - 'add Z to the background / replace the sky / remove the watermark'\n" +
			"  - 'combine these N images into one' (up to 4 sources)\n\n" +
			"DO NOT USE for:\n" +
			"  - Text-to-image from scratch → use generate_image (no source needed)\n" +
			"  - Charts / diagrams / data visualization → use the kocoro-generative-ui skill\n\n" +
			"NO MASK is supported — describe the region in natural language " +
			"(\"change the cat's color to orange\", \"add a moon in the upper-left\", " +
			"\"remove the text in the bottom-right corner\").\n\n" +
			"Latency vs. quality (single source, 1024×1024):\n" +
			"  - quality=low      ~40–70s\n" +
			"  - quality=auto     ~100–180s (default)\n" +
			"  - quality=high     ~150–250s\n" +
			"4-image edits add 50–100% to those numbers; quality=high with 4 sources " +
			"can take 200–350s. Pick the lowest quality that satisfies the request.\n\n" +
			"Cost: each call consumes Shannon Cloud image-generation credits, and " +
			"each source image charges ~85 image-tokens on top of the prompt. Use " +
			"n=1 unless the user explicitly asks for multiple variants." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   imagePromptMaxLen,
					"description": "Natural-language modification instruction. Be specific about what to change and where (\"change the cat's color to orange\", \"add a moon in the upper-left corner\").",
				},
				"description": agent.DescriptionFieldSpec,
				"image_urls": map[string]any{
					"type":        "array",
					"minItems":    editImageURLsMin,
					"maxItems":    editImageURLsMax,
					"items":       map[string]any{"type": "string"},
					"description": "1–4 source image URLs. EVERY entry MUST start with https://static.kocoro.ai/ (Shannon CDN). External URLs are rejected by the server.",
				},
				"size": map[string]any{
					"type":        "string",
					"enum":        []string{"1024x1024", "1024x1536", "1536x1024", "auto"},
					"description": "Output image dimensions. Default: 1024x1024.",
				},
				"quality": map[string]any{
					"type":        "string",
					"enum":        []string{"auto", "low", "medium", "high"},
					"description": "Edit quality. Higher = slower and more expensive. Default: auto.",
				},
				"n": map[string]any{
					"type":        "integer",
					"minimum":     imageNMin,
					"maximum":     imageNMax,
					"description": "Number of edited variants to produce. Default: 1. Each output is a separate paid generation.",
				},
				"background": map[string]any{
					"type":        "string",
					"enum":        []string{"transparent", "opaque", "auto"},
					"description": "Background mode. Use 'transparent' for output that should composite over other content.",
				},
			},
		},
		Required: []string{"prompt", "image_urls", "description"},
	}
}

func (t *EditImageTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args editImageArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	// Explicit required-field checks (not the generic gate) so each missing
	// field keeps its actionable recovery hint.
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return agent.ValidationError("prompt is required"), nil
	}
	if len(args.ImageURLs) < editImageURLsMin {
		return agent.ValidationError(
			"image_urls is required and must contain at least 1 URL. " +
				"If the user provided an external URL, first call publish_to_web to " +
				"upload it, or generate_image to produce one, then retry edit_image " +
				"with the resulting https://static.kocoro.ai/ URL.",
		), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("edit_image: missing required `description` parameter"), nil
	}

	// API spec says "1..32000 chars" — rune-counted to match JSON Schema
	// maxLength semantics. See generate_image.go for the rationale.
	if runeCount := utf8.RuneCountInString(prompt); runeCount > imagePromptMaxLen {
		return agent.ValidationError(fmt.Sprintf(
			"prompt too long: %d chars (max %d). Trim the prompt to the essentials.",
			runeCount, imagePromptMaxLen,
		)), nil
	}

	if len(args.ImageURLs) > editImageURLsMax {
		return agent.ValidationError(fmt.Sprintf(
			"image_urls has %d entries (max %d). Drop the lowest-priority sources before retrying.",
			len(args.ImageURLs), editImageURLsMax,
		)), nil
	}
	for i, u := range args.ImageURLs {
		if !strings.HasPrefix(u, kocoroCDNPrefix) {
			return agent.ValidationError(fmt.Sprintf(
				"image_urls[%d] = %q is not a Shannon CDN URL. Every entry must start "+
					"with %s. If this is an external URL, first call publish_to_web to "+
					"upload it (or generate_image to produce a fresh one) — then pass the "+
					"returned URL here. Do not retry edit_image with the original URL.",
				i, u, kocoroCDNPrefix,
			)), nil
		}
	}

	if args.Size != "" && !imageValidSizes[args.Size] {
		return agent.ValidationError(fmt.Sprintf(
			"invalid size %q. Must be one of: 1024x1024, 1024x1536, 1536x1024, auto.", args.Size,
		)), nil
	}
	if args.Quality != "" && !imageValidQuality[args.Quality] {
		return agent.ValidationError(fmt.Sprintf(
			"invalid quality %q. Must be one of: auto, low, medium, high.", args.Quality,
		)), nil
	}
	if args.Background != "" && !imageValidBackground[args.Background] {
		return agent.ValidationError(fmt.Sprintf(
			"invalid background %q. Must be one of: transparent, opaque, auto.", args.Background,
		)), nil
	}
	if args.N < 0 || args.N > imageNMax {
		return agent.ValidationError(fmt.Sprintf(
			"invalid n=%d (must be 1..%d). Use n=1 unless the user asked for variants.",
			args.N, imageNMax,
		)), nil
	}

	res, err := t.client.Edit(ctx, images.EditRequest{
		Prompt:     prompt,
		ImageURLs:  args.ImageURLs,
		Size:       args.Size,
		Quality:    args.Quality,
		N:          args.N,
		Background: args.Background,
	})
	if err != nil {
		return classifyEditErr(err), nil
	}

	return agent.ToolResult{Content: formatGenerateResult(res)}, nil
}

func (t *EditImageTool) RequiresApproval() bool { return true }

// IsSafeArgs always returns false: edit_image side-effects (a permanent
// public URL plus paid quota consumption) are never auto-approvable.
func (t *EditImageTool) IsSafeArgs(string) bool { return false }

var _ agent.SafeChecker = (*EditImageTool)(nil)

// classifyEditErr maps the typed errors from internal/images onto the
// agent's ToolResult error categories. Adds two edits-specific branches
// (ErrInvalidImageURL, ErrSourceTooLarge) on top of the same set used by
// generate_image; everything else falls through to the shared classifier.
func classifyEditErr(err error) agent.ToolResult {
	switch {
	case errors.Is(err, images.ErrInvalidImageURL):
		// BusinessError (not Validation): even when client-side prefix check
		// passed, the server may reject for orthogonal reasons (path missing,
		// bucket mismatch). The fix is "rebuild the URL pipeline", not "fix
		// the JSON" — and retrying the same args wastes a round trip.
		return agent.BusinessError(fmt.Sprintf(
			"edit_image: %v. The server rejected one of the image_urls — every URL "+
				"must start with %s. Use generate_image or publish_to_web to obtain "+
				"a CDN URL first; do not retry with the same args.",
			err, kocoroCDNPrefix))
	case errors.Is(err, images.ErrSourceTooLarge):
		return agent.ValidationError(fmt.Sprintf(
			"edit_image: %v. One source image exceeds OpenAI's 25 MiB hard cap. "+
				"Re-publish a smaller / compressed version first, then retry.",
			err))
	case errors.Is(err, images.ErrUnauthorized):
		return agent.PermissionError(fmt.Sprintf(
			"edit_image: %v — check that cloud.api_key is configured and valid.", err))
	case errors.Is(err, images.ErrEndpointNotFound):
		return agent.BusinessError(fmt.Sprintf(
			"edit_image: %v. The gateway at cloud.endpoint responded but does not "+
				"expose POST /api/v1/images/edits. Confirm the Shannon Cloud "+
				"deployment includes the images handler, or point cloud.endpoint at an "+
				"environment that does. Tell the user to ask the cloud team — this is "+
				"not a client-side issue and retrying will not help.", err))
	case errors.Is(err, images.ErrBadRequest), errors.Is(err, images.ErrRequestTooLarge):
		return agent.ValidationError(fmt.Sprintf("edit_image: %v", err))
	case errors.Is(err, images.ErrUpstreamTimeout):
		return agent.BusinessError(fmt.Sprintf(
			"edit_image: %v. The upstream model exceeded its 500s budget. "+
				"Do not retry with the same args — drop quality (high → medium / low), "+
				"reduce n (>1 → 1), or use fewer source images before trying again.", err))
	case errors.Is(err, images.ErrContentRejected):
		return agent.BusinessError(fmt.Sprintf(
			"edit_image: %v. The upstream returned no images, typically because "+
				"the prompt or one of the source images hit a content-moderation filter. "+
				"Revise the prompt or source — retrying the same input will hit the same outcome.", err))
	case errors.Is(err, images.ErrServerConfig):
		return agent.BusinessError(fmt.Sprintf(
			"edit_image: image generation not configured server-side: %v", err))
	case errors.Is(err, images.ErrTransient):
		return agent.TransientError(fmt.Sprintf("edit_image: %v", err))
	default:
		return agent.ToolResult{
			Content: fmt.Sprintf("edit_image error: %v", err),
			IsError: true,
		}
	}
}
