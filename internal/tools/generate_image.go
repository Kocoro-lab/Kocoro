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

// imageGen abstracts the *images.Client surface this tool depends on so
// tests can inject a fake without standing up an HTTP server.
type imageGen interface {
	Generate(ctx context.Context, req images.GenerateRequest) (*images.GenerateResponse, error)
}

const (
	imagePromptMaxLen = 32000
	imageNMin         = 1
	imageNMax         = 10
)

// imageValidSizes / imageValidQuality / imageValidBackground mirror the API
// spec's enums. Validating client-side saves a 400 round-trip and produces
// friendlier error text than upstream codes.
var (
	imageValidSizes      = map[string]bool{"1024x1024": true, "1024x1536": true, "1536x1024": true, "auto": true}
	imageValidQuality    = map[string]bool{"auto": true, "low": true, "medium": true, "high": true}
	imageValidBackground = map[string]bool{"transparent": true, "opaque": true, "auto": true}
)

type GenerateImageTool struct {
	client imageGen
}

func NewGenerateImageTool(client imageGen) *GenerateImageTool {
	return &GenerateImageTool{client: client}
}

type generateImageArgs struct {
	Prompt      string `json:"prompt"`
	Description string `json:"description,omitempty"`
	Size        string `json:"size,omitempty"`
	Quality     string `json:"quality,omitempty"`
	N           int    `json:"n,omitempty"`
	Background  string `json:"background,omitempty"`
}

func (t *GenerateImageTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "generate_image",
		Description: "⚠️ Generates an image from a text prompt via Shannon Cloud. Returns a permanent " +
			"public CDN URL — anyone with the URL can view it; there is no DELETE.\n\n" +
			"USE FOR: photorealistic images, illustrations, banners, decorative artwork, " +
			"or any time the user asks 'draw / generate / paint / make me a picture of X'.\n\n" +
			"DO NOT USE for:\n" +
			"  - Charts / diagrams / data visualization → use the kocoro-generative-ui skill\n" +
			"    (renders inline as SVG/HTML, no public URL).\n" +
			"  - Editing or annotating an existing image (this tool is text-to-image only;\n" +
			"    no input image is supported).\n\n" +
			"Latency vs. quality:\n" +
			"  - quality=low      ~30–50s\n" +
			"  - quality=medium   ~60–90s\n" +
			"  - quality=auto     ~80–150s (default)\n" +
			"  - quality=high     ~120–180s\n" +
			"Pick the lowest quality that satisfies the request.\n\n" +
			"Cost: each call consumes Shannon Cloud image-generation credits. Use n=1 unless " +
			"the user explicitly asks for multiple variants." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   imagePromptMaxLen,
					"description": "Detailed description of the image. Be specific about subject, style, composition, lighting.",
				},
				"description": agent.DescriptionFieldSpec,
				"size": map[string]any{
					"type":        "string",
					"enum":        []string{"1024x1024", "1024x1536", "1536x1024", "auto"},
					"description": "Image dimensions. Default: 1024x1024.",
				},
				"quality": map[string]any{
					"type":        "string",
					"enum":        []string{"auto", "low", "medium", "high"},
					"description": "Generation quality. Higher = slower and more expensive. Default: auto.",
				},
				"n": map[string]any{
					"type":        "integer",
					"minimum":     imageNMin,
					"maximum":     imageNMax,
					"description": "Number of images to generate. Default: 1. Each image is a separate paid generation.",
				},
				"background": map[string]any{
					"type":        "string",
					"enum":        []string{"transparent", "opaque", "auto"},
					"description": "Background mode. Use 'transparent' for logos / icons that need to composite over other content.",
				},
			},
		},
		Required: []string{"prompt", "description"},
	}
}

func (t *GenerateImageTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args generateImageArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	// Explicit required-field checks so the missing-field messages stay useful.
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return agent.ValidationError("prompt is required"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("generate_image: missing required `description` parameter"), nil
	}
	// API spec says "1..32000 chars" — rune-counted to match JSON Schema
	// maxLength semantics. Using len() (bytes) would reject CJK / emoji
	// prompts at ~10000 visible characters, well before the server would.
	if runeCount := utf8.RuneCountInString(prompt); runeCount > imagePromptMaxLen {
		return agent.ValidationError(fmt.Sprintf(
			"prompt too long: %d chars (max %d). Trim the prompt to the essentials.",
			runeCount, imagePromptMaxLen,
		)), nil
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

	res, err := t.client.Generate(ctx, images.GenerateRequest{
		Prompt:     prompt,
		Size:       args.Size,
		Quality:    args.Quality,
		N:          args.N,
		Background: args.Background,
	})
	if err != nil {
		return classifyGenerateErr(err), nil
	}

	return agent.ToolResult{Content: formatGenerateResult(res)}, nil
}

func (t *GenerateImageTool) RequiresApproval() bool { return true }

// IsSafeArgs always returns false: generate_image side-effects (a permanent
// public URL plus paid quota consumption) are never auto-approvable.
func (t *GenerateImageTool) IsSafeArgs(string) bool { return false }

var _ agent.SafeChecker = (*GenerateImageTool)(nil)

// formatGenerateResult is the LLM-facing tool_result content. Compact and
// machine-readable: one URL line per image, then a single metadata line
// describing the model/size. The LLM wraps the URL into markdown ![](url)
// in its assistant reply for the user-visible output.
func formatGenerateResult(res *images.GenerateResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Generated %d image(s).\n", len(res.Images))
	for _, img := range res.Images {
		fmt.Fprintf(&b, "URL: %s\n", img.URL)
	}
	first := res.Images[0]
	fmt.Fprintf(&b, "Size: %s | Content-Type: %s | %d bytes\n", res.Size, first.ContentType, first.SizeBytes)
	if res.Model != "" {
		fmt.Fprintf(&b, "Model: %s", res.Model)
	}
	return strings.TrimRight(b.String(), "\n")
}

// classifyGenerateErr maps the typed errors from internal/images onto the
// agent's ToolResult error categories. The 504/502-no_images_returned cases
// land as BusinessError with explicit "fix-the-args" guidance because
// retrying the same arguments would just consume more paid quota.
func classifyGenerateErr(err error) agent.ToolResult {
	switch {
	case errors.Is(err, images.ErrUnauthorized):
		return agent.PermissionError(fmt.Sprintf(
			"generate_image: %v — check that cloud.api_key is configured and valid.", err))
	case errors.Is(err, images.ErrEndpointNotFound):
		return agent.BusinessError(fmt.Sprintf(
			"generate_image: %v. The gateway at cloud.endpoint responded but does not "+
				"expose POST /api/v1/images/generations. Confirm the Shannon Cloud "+
				"deployment includes the images handler, or point cloud.endpoint at an "+
				"environment that does. Tell the user to ask the cloud team — this is "+
				"not a client-side issue and retrying will not help.", err))
	case errors.Is(err, images.ErrBadRequest), errors.Is(err, images.ErrRequestTooLarge):
		return agent.ValidationError(fmt.Sprintf("generate_image: %v", err))
	case errors.Is(err, images.ErrUpstreamTimeout):
		return agent.BusinessError(fmt.Sprintf(
			"generate_image: %v. The upstream model exceeded its 500s budget. "+
				"Do not retry with the same args — drop quality (high → medium / low) "+
				"or n (>1 → 1) before trying again.", err))
	case errors.Is(err, images.ErrContentRejected):
		return agent.BusinessError(fmt.Sprintf(
			"generate_image: %v. The upstream returned no images, typically because "+
				"the prompt hit a content-moderation filter. Revise the prompt — "+
				"retrying the same text will hit the same outcome.", err))
	case errors.Is(err, images.ErrServerConfig):
		return agent.BusinessError(fmt.Sprintf(
			"generate_image: image generation not configured server-side: %v", err))
	case errors.Is(err, images.ErrTransient):
		return agent.TransientError(fmt.Sprintf("generate_image: %v", err))
	default:
		return agent.ToolResult{
			Content: fmt.Sprintf("generate_image error: %v", err),
			IsError: true,
		}
	}
}
