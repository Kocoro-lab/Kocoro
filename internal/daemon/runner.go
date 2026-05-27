package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cloudflow"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

var (
	disconnectPlaywrightAfterIdleFn = func(mgr *mcp.ClientManager, d time.Duration) {
		mgr.DisconnectAfterIdle("playwright", d)
	}
	disconnectPlaywrightNowFn = func(mgr *mcp.ClientManager) {
		mgr.Disconnect("playwright")
	}
	stopPlaywrightChromeAndWaitFn = mcp.StopCDPChromeAndWait
)

// RequestContentBlock represents a content block in the POST /message request.
// Supported types: "text" and "image" (passed through to LLM), "file_ref" (resolved by daemon).
type RequestContentBlock struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	Source   *client.ImageSource `json:"source,omitempty"`
	FilePath string              `json:"file_path,omitempty"`
	Filename string              `json:"filename,omitempty"`
	ByteSize int64               `json:"byte_size,omitempty"`
}

// RunAgentRequest is the input for RunAgent.
type RunAgentRequest struct {
	Text           string                `json:"text"`
	Content        []RequestContentBlock `json:"content,omitempty"` // multimodal content blocks (optional)
	Agent          string                `json:"agent,omitempty"`
	SessionID      string                `json:"session_id,omitempty"`
	NewSession     bool                  `json:"new_session,omitempty"`
	Source         string                `json:"source,omitempty"`    // "slack", "line", "kocoro", "webhook" (legacy "shanclaw" still accepted by router for one release)
	Sender         string                `json:"sender,omitempty"`    // user identifier from channel
	Channel        string                `json:"channel,omitempty"`   // channel/thread source context
	ThreadID       string                `json:"thread_id,omitempty"` // thread context for messaging platforms
	CWD            string                `json:"cwd,omitempty"`       // absolute project path override
	RouteKey       string                `json:"-"`                   // internal routing key
	Ephemeral      bool                  `json:"-"`                   // caller owns persistence + events
	ModelOverride  string                `json:"-"`                   // overrides agent model tier
	BypassRouting  bool                  `json:"-"`                   // skip route lock (heartbeat runs)
	SessionHistory []client.Message      `json:"-"`                   // pre-loaded history for LLM context (BypassRouting runs)
	OmitHistory    bool                  `json:"-"`                   // skip sess.HistoryForLoop() snapshot; LLM sees empty history. Set by scheduler for stateless schedules.
	StickyContext  string                `json:"-"`                   // 额外的 sticky context，注入系统提示（对用户不可见）
	Files          []RemoteFile          `json:"-"`                   // remote file attachments from Cloud (WS only)

	// IM message lifecycle plumbing for the run's PRIMARY user message (first
	// turn). Mid-run follow-ups carry their own copies on InjectedMessage.
	// CloudMessageID is the Cloud envelope id; IMStatusContext is the opaque
	// platform reaction context (echoed verbatim in MESSAGE_LIFECYCLE events).
	// Both empty for non-IM sources (TUI/CLI/webhook/cron).
	CloudMessageID  string          `json:"-"`
	IMStatusContext json.RawMessage `json:"-"`
}

// Validate checks that the request has the minimum required fields.
func (r *RunAgentRequest) Validate() error {
	if strings.TrimSpace(r.Text) == "" && len(r.Content) == 0 {
		return fmt.Errorf("text or content is required")
	}
	if r.Agent != "" {
		if err := agents.ValidateAgentName(r.Agent); err != nil {
			return err
		}
	}
	if r.CWD != "" {
		if err := cwdctx.ValidateCWD(r.CWD); err != nil {
			return fmt.Errorf("invalid cwd: %w", err)
		}
	}
	return nil
}

// ComputeRouteKey builds the route key for session cache/locking decisions.
func ComputeRouteKey(req RunAgentRequest) string {
	if req.BypassRouting {
		return ""
	}
	if req.SessionID != "" {
		return "session:" + sanitizeRouteValue(req.SessionID)
	}
	if shouldBypassNamedAgentRoute(req.Source) {
		return ""
	}
	if IsMessagingPlatform(req.Source) && req.ThreadID != "" {
		if req.Agent != "" {
			return "agent:" + req.Agent + ":" + sanitizeRouteValue(req.Source) + ":" + sanitizeRouteValue(req.ThreadID)
		}
		return "default:" + sanitizeRouteValue(req.Source) + ":" + sanitizeRouteValue(req.ThreadID)
	}
	// Messaging platform without thread but with sender: suffix sender so
	// concurrent users in a shared channel don't collide on one session.
	// Group channels that want a single shared session must use a thread,
	// which the thread-scoped branch above handles.
	if IsMessagingPlatform(req.Source) && req.Sender != "" {
		if req.Agent != "" {
			return "agent:" + req.Agent + ":" + sanitizeRouteValue(req.Source) + ":" + sanitizeRouteValue(req.Channel) + ":" + sanitizeRouteValue(req.Sender)
		}
		return "default:" + sanitizeRouteValue(req.Source) + ":" + sanitizeRouteValue(req.Channel) + ":" + sanitizeRouteValue(req.Sender)
	}
	if req.Agent != "" {
		return "agent:" + req.Agent
	}
	if req.NewSession || shouldBypassRouteCache(req.Source) {
		return ""
	}
	if req.Source != "" && req.Channel != "" {
		return "default:" + sanitizeRouteValue(req.Source) + ":" + sanitizeRouteValue(req.Channel)
	}
	return ""
}

func shouldBypassNamedAgentRoute(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case ChannelWeb, "webhook", "cron", ChannelSystem:
		return true
	default:
		return false
	}
}

func isPlainAgentRouteKey(routeKey string) bool {
	if !strings.HasPrefix(routeKey, "agent:") {
		return false
	}
	return !strings.Contains(strings.TrimPrefix(routeKey, "agent:"), ":")
}

func shouldBypassRouteCache(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", ChannelWeb, "webhook", "cron", ChannelSchedule, ChannelSystem:
		return true
	default:
		return false
	}
}

func shouldPersistRouteKey(routeKey string) bool {
	if routeKey == "" || strings.HasPrefix(routeKey, "session:") {
		return false
	}
	return !isPlainAgentRouteKey(routeKey)
}

func sanitizeRouteValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return url.PathEscape(trimmed)
}

// resolveContentBlocks converts request content blocks into client.ContentBlock
// values suitable for the LLM. "text" and "image" blocks are passed through;
// "file_ref" blocks are resolved by reading the referenced file from disk.
func resolveContentBlocks(blocks []RequestContentBlock) []client.ContentBlock {
	out := make([]client.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, client.ContentBlock{Type: "text", Text: b.Text})
		case "image":
			// Layer 1 also covers direct inline image blocks pushed by cloud or
			// Desktop (bypassing resolveFileRef). Without this, an oversize image
			// would be saved to session pre-loop at runner.go's pre-save user
			// message path, after which captureTurnBaseline locks it into baseline
			// forever.
			out = append(out, client.ContentBlock{
				Type:   "image",
				Source: tools.CompressInlineImageSource(b.Source),
			})
		case "document":
			out = append(out, client.ContentBlock{Type: "document", Source: b.Source})
		case "file_ref":
			out = append(out, resolveFileRef(b)...)
		}
	}
	return out
}

// ConvertFilesToInjected lowers daemon-layer RemoteFile (cloud wire format)
// to agent.InjectedFile (cycle-free agent-layer carrier). The inject-path
// priority order is:
//
//	ExtractedText > DocumentB64 > URL-download (image case).
//
// Note this is the REVERSE of downloadRemoteFiles' priority (which is
// DocumentB64 > ExtractedText > URL, per attachment.go's header comment).
// The main path prefers DocumentB64 to preserve PDF fidelity for native
// vision; the inject path prefers ExtractedText because mid-turn injects
// share context with the active turn and a cheaper-token text block
// usually wins over a fresh ~25 MB document. Real-world cloud mid-turn
// followups are typically "look at this image" or "here is the extract"
// — when ExtractedText is populated, the cloud already did the extraction
// work and we honor it.
//
// Returns nil for empty input; skips entries that can't be expressed and
// logs them rather than silently dropping. Non-image URL-only files are
// skipped intentionally — the inject path has no disk-cleanup hook for the
// file_ref/disk-download flow that downloadRemoteFiles uses, and the only
// real-world mid-turn attachment from cloud channels is an image followup
// ("here's the screenshot you asked about"). A future enhancement can wire
// document downloads through ensureDir() + the existing cleanup chain.
//
// Exported so the cmd/daemon.go WS handler can call it before queueing the
// InjectedMessage onto the route's inject channel.
func ConvertFilesToInjected(ctx context.Context, files []RemoteFile) []agent.InjectedFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]agent.InjectedFile, 0, len(files))
	for _, f := range files {
		switch {
		case f.ExtractedText != "":
			out = append(out, agent.InjectedFile{
				Type: "text",
				Data: f.ExtractedText,
			})
		case f.DocumentB64 != "":
			out = append(out, agent.InjectedFile{
				Type:      "document",
				MediaType: f.MimeType,
				Data:      f.DocumentB64,
			})
		case f.URL != "" && strings.HasPrefix(f.MimeType, "image/"):
			b64, err := downloadInjectedImageBase64(ctx, f.URL, f.AuthHeader)
			if err != nil {
				log.Printf("daemon: convertFilesToInjected: image download failed for %q: %v", f.Name, sanitizeError(err))
				continue
			}
			out = append(out, agent.InjectedFile{
				Type:      "image",
				MediaType: f.MimeType,
				Data:      b64,
			})
		default:
			log.Printf("daemon: convertFilesToInjected: skip unrepresentable file %q (mimetype=%s, has_url=%v)", f.Name, f.MimeType, f.URL != "")
		}
	}
	return out
}

// downloadInjectedImageBase64 fetches a remote image URL in memory and
// returns its base64-encoded body. Used by the mid-turn inject path which
// can't reuse downloadOneFile's disk-+-cleanup flow (no cleanup hook).
// Applies the same SSRF/Slack-rewrite/auth-header guards as downloadOneFile
// and caps decoded bytes at maxInlineImageDecodedBytes to match Anthropic's
// 5 MB inline-image ceiling (with a 4 MB safety margin via the 20 MB cap
// used by the rest of the inline-image path).
func downloadInjectedImageBase64(ctx context.Context, rawURL, authHeader string) (string, error) {
	dlURL := slackDownloadURL(rawURL)
	if err := urlValidator(dlURL); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return "", fmt.Errorf("bad URL: %w", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	httpClient := &http.Client{
		Timeout: downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if err := urlValidator(req.URL.String()); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			// Cross-host redirect handling. Cloud-supplied URLs aren't
			// directly user-controlled, but a malicious 302 from a compromised
			// upstream would otherwise leak the bot token to attacker.com.
			//
			// Two layers of leakage to defend against:
			//
			// 1. Stdlib's `Client.do` auto-copies the original request's
			//    Authorization header onto the redirected request before
			//    invoking CheckRedirect. The strip path is gated by two
			//    checks: `reqs[0].URL.Host != req.URL.Host` (port-aware)
			//    AND `!shouldCopyHeaderOnRedirect(...)` — the latter
			//    compares via idnaASCIIFromURL (hostname-only, port-blind)
			//    and returns true on hostname/subdomain match. So two
			//    httptest servers on 127.0.0.1:A vs 127.0.0.1:B trip the
			//    first check but pass the second, and stdlib silently
			//    propagates Authorization. We must DELETE that header on
			//    cross-host redirects, not merely refrain from re-setting.
			// 2. The pre-existing custom block below explicitly re-applied the
			//    original Authorization on every redirect, defeating stdlib's
			//    same-hostname filter for genuine cross-domain redirects too.
			//
			// We compare URL.Host (with port) so the httptest case behaves as
			// "cross-host". Same-host (including port) keeps the original
			// Authorization; cross-host strips it.
			if req.URL.Host == via[0].URL.Host {
				if auth := via[0].Header.Get("Authorization"); auth != "" {
					req.Header.Set("Authorization", auth)
				}
			} else {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return "", fmt.Errorf("got text/html response (auth may have failed)")
	}
	// Read with a hard limit slightly above the decoded cap so we can detect
	// oversize via the (read > cap) check without buffering arbitrary bytes.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxInlineImageDecodedBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxInlineImageDecodedBytes {
		return "", fmt.Errorf("image exceeds %d-byte inline cap", maxInlineImageDecodedBytes)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// imageExtensions are sent as base64 image content blocks to the LLM.
var imageExtensions = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
}

// resolveFileRef returns the appropriate content blocks for a file_ref.
// Images → model-visible path hint plus base64 image block so the agent has
// both a reusable file handle and inline vision access.
// Directories → hint suggests glob/grep/directory_list/bash to explore.
// Zip archives → hint suggests `bash unzip -l` for listing, `bash unzip` for extract.
// All other files → text hint with path so the agent reads via file_read tool.
func resolveFileRef(b RequestContentBlock) []client.ContentBlock {
	ext := strings.ToLower(filepath.Ext(b.Filename))

	// Directories: stat to confirm and emit a folder-aware hint. Tool calls
	// inside the directory are auto-approved by the agent loop's
	// userFilePaths matcher (prefix-match for IsDir entries).
	if info, err := os.Stat(b.FilePath); err == nil && info.IsDir() {
		return []client.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[User attached folder: %s at path: %s — use directory_list, glob, grep, file_read, or bash to explore. Tool calls against files inside this folder are auto-approved.]",
				b.Filename, b.FilePath),
		}}
	}

	// Zip archives: opaque file_ref. file_read can't usefully read a zip's
	// raw bytes, so route the model to bash unzip.
	if ext == ".zip" {
		return []client.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[User attached zip archive: %s (%d bytes) at path: %s — use `bash unzip -l <path>` to list contents, `bash unzip <path> -d <dir>` to extract.]",
				b.Filename, b.ByteSize, b.FilePath),
		}}
	}

	// Images must be inline base64 — Claude vision requires image data in the request body.
	if mimeType, ok := imageExtensions[ext]; ok {
		info, err := os.Stat(b.FilePath)
		if err != nil {
			log.Printf("WARNING: failed to read attached image %s: %v", b.FilePath, err)
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to read image %s]", b.Filename),
			}}
		}
		const maxInlineImage = 20 * 1024 * 1024 // pre-decode allocation guard
		if info.Size() > maxInlineImage {
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[User attached image: %s (%d bytes) at path: %s — too large for inline vision (max %d bytes). Use file_read or another file-based tool with this path.]",
					b.Filename, info.Size(), b.FilePath, maxInlineImage),
			}}
		}
		data, err := os.ReadFile(b.FilePath)
		if err != nil {
			log.Printf("WARNING: failed to read attached image %s: %v", b.FilePath, err)
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to read image %s]", b.Filename),
			}}
		}
		// Run through the shared compression pipeline so this path enforces the
		// same 5 MB inline limit as file_read / screenshot / etc. Without this,
		// Desktop drag-drop of a 6 MB PNG would bypass Layer 1.
		block, err := tools.EncodeImageBytes(data, mimeType)
		if err != nil {
			log.Printf("WARNING: failed to encode attached image %s: %v", b.FilePath, err)
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to encode image %s]", b.Filename),
			}}
		}
		return []client.ContentBlock{
			{
				Type: "text",
				// Preserve filename + size + path + path-reuse hint so the model
				// can still call file_read on the original if it needs vision
				// after edit / wants pixel-level access bypassing compression.
				Text: fmt.Sprintf("[User attached image: %s (%d bytes) at path: %s — the image is included inline below for vision. Use the path if a tool needs the original file.]",
					b.Filename, info.Size(), b.FilePath),
			},
			{
				Type: "image",
				Source: &client.ImageSource{
					Type:      "base64",
					MediaType: block.MediaType,
					Data:      block.Data,
				},
			},
		}
	}

	// PDF files: file_read natively renders PDF pages as images for vision.
	if ext == ".pdf" {
		return []client.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[User attached PDF: %s (%d bytes) at path: %s — use file_read to analyze (it renders PDF pages as images for vision). Use offset for start page, limit for max pages.]",
				b.Filename, b.ByteSize, b.FilePath),
		}}
	}

	// All other files: let the agent use file_read to access content on demand.
	return []client.ContentBlock{{
		Type: "text",
		Text: fmt.Sprintf("[User attached file: %s (%d bytes) at path: %s — use the file_read tool to read its contents]",
			b.Filename, b.ByteSize, b.FilePath),
	}}
}

// extractUserFilePaths collects file paths from file_ref content blocks.
// These paths represent files the user explicitly attached, so tool access
// to them should be auto-approved without prompting. Each path is stat'd so
// the agent loop can apply subtree matching for directory attachments and
// exact matching for file attachments.
func extractUserFilePaths(blocks []RequestContentBlock) []agent.UserAttachedPath {
	var paths []agent.UserAttachedPath
	for _, b := range blocks {
		if b.Type != "file_ref" || b.FilePath == "" {
			continue
		}
		isDir := false
		if info, err := os.Stat(b.FilePath); err == nil {
			isDir = info.IsDir()
		}
		paths = append(paths, agent.UserAttachedPath{Path: b.FilePath, IsDir: isDir})
	}
	return paths
}

// buildUserMsgContent creates the MessageContent for the user message.
// If resolved content contains non-text blocks (images), uses block array format.
// Otherwise, merges all text into a single string for maximum gateway compatibility.
func buildUserMsgContent(prompt string, resolvedContent []client.ContentBlock) client.MessageContent {
	if len(resolvedContent) == 0 {
		return client.NewTextContent(prompt)
	}

	// Check if any block requires array format (images, documents).
	needsBlocks := false
	for _, b := range resolvedContent {
		if b.Type != "text" {
			needsBlocks = true
			break
		}
	}

	if needsBlocks {
		blocks := resolvedContent
		if prompt != "" {
			blocks = append([]client.ContentBlock{{Type: "text", Text: prompt}}, blocks...)
		}
		return client.NewBlockContent(blocks)
	}

	// Text-only: merge into single string.
	var sb strings.Builder
	sb.WriteString(prompt)
	for _, b := range resolvedContent {
		if b.Text != "" {
			sb.WriteString("\n\n")
			sb.WriteString(b.Text)
		}
	}
	return client.NewTextContent(sb.String())
}

// hasPDFAttachment returns true if any file_ref block has a .pdf extension.
func hasPDFAttachment(blocks []RequestContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "file_ref" && strings.ToLower(filepath.Ext(b.Filename)) == ".pdf" {
			return true
		}
	}
	return false
}

// injectBundledSkill appends a bundled skill to the list if not already present.
func injectBundledSkill(existing []*skills.Skill, shannonDir, name string) []*skills.Skill {
	for _, s := range existing {
		if s.Name == name {
			return existing // already loaded
		}
	}
	src, err := skills.BundledSkillSource(shannonDir)
	if err != nil {
		log.Printf("daemon: failed to load bundled skill source for %q: %v", name, err)
		return existing
	}
	loaded, err := skills.LoadSkills(src)
	if err != nil {
		log.Printf("daemon: failed to load bundled skill %q: %v", name, err)
		return existing
	}
	for _, s := range loaded {
		if s.Name == name {
			return append(existing, s)
		}
	}
	return existing
}

// EnsureRouteKey computes and sets the route key if not already set.
func (req *RunAgentRequest) EnsureRouteKey() {
	if req == nil {
		return
	}
	if req.RouteKey == "" {
		req.RouteKey = ComputeRouteKey(*req)
	}
}

// outputFormatForSource maps a request source to an output format profile.
// Only explicit cloud-distributed channel sources use "plain" — Shannon Cloud
// handles final channel rendering for these (Slack mrkdwn, LINE Flex, etc.).
// Everything else (local, cron, schedule, web, unknown) defaults to "markdown".
//
// Shares its cloud-source definition with ensureCloudSessionTmpDir via
// isCloudSource; the two paths must agree on what "cloud-routed" means or the
// allocator and the formatter would drift apart silently.
func outputFormatForSource(source string) string {
	if isCloudSource(source) {
		return "plain"
	}
	return "markdown"
}

// silentBannerSources lists request sources whose `agent_reply` should NOT
// trigger the daemon's reply-complete macOS banner. Cloud-distributed channels
// (slack/line/feishu/lark/telegram/webhook) are filtered separately via
// isCloudSource — those deliver the reply elsewhere. The entries here are the
// autonomous local sources that fire frequently without a foregrounded user:
//   - heartbeat: per-agent self-pings on a timer (internal/heartbeat)
//   - watcher:   filesystem-change triggered runs (cmd/daemon.go watcher)
//   - mcp:       another MCP client owns the response; banner is noise
//
// schedule/cron stay opted-in — those are exactly the background completions
// the user wants surfaced.
var silentBannerSources = map[string]struct{}{
	"heartbeat": {},
	"watcher":   {},
	"mcp":       {},
}

// shouldEmitReplyBanner reports whether a reply-complete banner should fire
// for the given request source. Returns false for cloud-distributed channels
// (reply delivered elsewhere) and for autonomous local sources that would
// spam the notification center.
func shouldEmitReplyBanner(source string) bool {
	if isCloudSource(source) {
		return false
	}
	_, silent := silentBannerSources[strings.ToLower(strings.TrimSpace(source))]
	return !silent
}

// markdownStripRE matches the small set of markdown markers that read poorly
// in a macOS notification: backticks (inline code + fences), bold/italic
// asterisks and underscores, leading hashes for headers, and the `[text](url)`
// link wrapper (the captured `text` is restored by the replacement). Heavier
// constructs (tables, html, fenced code bodies) are out of scope — banners are
// 140 chars and the goal is readability, not faithful rendering.
var markdownStripRE = regexp.MustCompile(`\x60+|\*+|_+|^#{1,6}\s+|\[([^\]]+)\]\([^)]*\)`)

// stripMarkdownLite removes the markdown markers most likely to read as
// visible cruft in a banner. Idempotent and safe on plain text.
func stripMarkdownLite(s string) string {
	return markdownStripRE.ReplaceAllString(s, "$1")
}

// IsMessagingPlatform returns true for sources where the gateway delivers
// an explicit AgentName (or empty = use the daemon's default agent) and any
// "@<botname>" prefix in the message body is user-facing convention, not an
// agent-routing signal.
//
// Callers (e.g. cmd/daemon.go) should skip the @mention parsing fallback for
// these sources — otherwise a literal "@<botname> hello" arriving from a
// group chat parses the bot's display name as a local agent, which won't
// exist in the daemon's registry and surfaces as a confusing error to the
// end user.
//
// NOTE: keep this set aligned with cloudSourceSet in session_cwd.go. The
// invariants differ (CWD allocation vs. @mention handling), but if a channel
// is cloud-routed it almost always belongs in both lists.
func IsMessagingPlatform(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case ChannelSlack, ChannelFeishu, ChannelLark, ChannelWeCom,
		ChannelLINE, ChannelWeChat, ChannelTeams, ChannelDiscord,
		ChannelTelegram:
		return true
	}
	return false
}

// cacheSourceFromDaemonSource maps the daemon-level source (slack/webhook/
// cron/mcp/tui/...) to the cache_source string Shannon uses for prompt-cache
// TTL routing. Channel messages + interactive use → long bucket (1h). Fire-and-
// forget paths → short bucket (5m). See docs/cache-strategy.md.
//
// Unknown / unclassified sources deliberately fall through to "unknown" →
// Shannon routes unknown to 5m (fail cheap, not fail expensive).
func cacheSourceFromDaemonSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch s {
	case "slack", "line", "feishu", "lark", "wecom", "telegram":
		// Human-conversation channels: idle gaps > 5m are common, 1h pays off.
		return s
	case "tui", "kocoro", "shanclaw":
		// Interactive sessions: TUI and Kocoro Desktop both have idle gaps >> 5m.
		// "shanclaw" kept one release as belt-and-suspenders during Round-2 protocol rename;
		// removed in 7.4 after cloud confirms all daemons emit "kocoro".
		return s
	case "cache_bench":
		// Synthetic benchmark traffic — treat as long-bucket so bench measures
		// reflect the production channel-message configuration.
		return "cache_bench"
	case "webhook", "cron", "schedule", "mcp":
		// One-shot paths — each invocation starts fresh, no resume.
		return s
	default:
		return "unknown"
	}
}

func routeTitle(source, channel, sender string) string {
	if source == "" {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(source))
	if s == "" {
		return ""
	}
	switch s {
	case "desktop", "shanclaw", "kocoro":
		return ""
	}
	label := strings.ToUpper(s[:1]) + s[1:]

	// Use sender name when available (e.g. "Slack · Wayland")
	if sender != "" {
		return label + " · " + sender
	}
	// Fall back to channel if it differs from source (avoid "Slack slack")
	if channel != "" && strings.ToLower(channel) != s {
		return label + " · " + channel
	}
	return label
}

// RunAgentResult is the output from RunAgent.
type RunAgentResult struct {
	Reply     string        `json:"reply"`
	SessionID string        `json:"session_id"`
	Agent     string        `json:"agent"`
	Usage     RunAgentUsage `json:"usage"`
	// Partial=true + FailureCode indicate the run completed "softly" — the
	// reply is valid and should be shown, but the loop layer flagged it as
	// abnormal (e.g. loop-detector force-stop). Treat as a soft warning, not
	// an error.
	Partial     bool           `json:"partial,omitempty"`
	FailureCode runstatus.Code `json:"failure_code,omitempty"`

	// MessageStartIndex / MessageEndIndex pin the slice of sess.Messages this
	// invocation wrote. MessageStartIndex is len(sess.Messages) AFTER the
	// pre-loop user message was appended (when Source != "" && !Ephemeral) —
	// i.e. it points at the first ASSISTANT message this run will write, not
	// at the user message that triggered it. MessageEndIndex is
	// len(sess.Messages) after the run terminated. The downstream resolver
	// (SummarizeLastRun) emits only assistant turns, so user-message
	// inclusion is invisible to consumers, but document the actual semantics
	// for future callers.
	//
	// Scheduler stores these into Schedule.LastRunMessage{Start,End}Index so
	// schedule_show can return the precise turns from this run instead of the
	// session's tail (which, on named-agent shared sessions, may be later
	// interactive chat). Populated on both success and hard-error paths.
	MessageStartIndex int `json:"message_start_index,omitempty"`
	MessageEndIndex   int `json:"message_end_index,omitempty"`
}

// RunAgentUsage tracks token and cost information for a single agent run.
type RunAgentUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// computeReportedUsage builds the per-run usage block emitted to lifecycle
// observers (schedule_run, heartbeat callers). Prefers the handler's
// accumulator snapshot when the handler is a UsageProvider AND has recorded
// any LLM or tool spend — that snapshot folds in nested cloud_delegate spend
// and gateway tool billing that the loop's own TurnUsage misses.
//
// Used by both the success path (post-final-LLM) and the hard-error path
// (after an intermediate LLM call has already incurred cost). Keeping a
// single resolver guarantees the failed schedule_run event reports the same
// tokens/cost the success event would have reported up to the point of
// failure — otherwise the failed-run case looks free when it isn't.
//
// nil-safe on usage so the hard-error path can call it even if loop.Run
// returned (nil, nil, err) before producing a TurnUsage.
func computeReportedUsage(usage *agent.TurnUsage, handler agent.EventHandler) RunAgentUsage {
	var reported RunAgentUsage
	if usage != nil {
		reported = RunAgentUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
			CostUSD:      usage.CostUSD,
		}
	}
	if up, ok := handler.(agent.UsageProvider); ok {
		acc := up.Usage()
		llm := acc.LLM
		if llm.LLMCalls > 0 || llm.TotalTokens > 0 || llm.CostUSD > 0 || acc.ToolCostUSD > 0 {
			reported = RunAgentUsage{
				InputTokens:  llm.InputTokens,
				OutputTokens: llm.OutputTokens,
				TotalTokens:  llm.TotalTokens,
				CostUSD:      llm.CostUSD + acc.ToolCostUSD,
			}
		}
	}
	return reported
}

// ServerDeps holds shared dependencies required by both the WS callback
// and the HTTP server for running agent loops.
type ServerDeps struct {
	mu              sync.RWMutex // guards Config, Registry, Cleanup during reload
	Config          *config.Config
	GW              *client.GatewayClient
	Registry        *agent.ToolRegistry
	MCPManager      *mcp.ClientManager  // live MCP connections; swapped on reload
	Supervisor      *mcp.Supervisor     // MCP health supervisor; swapped on reload
	Cleanup         func()              // closes MCP connections; swapped on reload
	BaselineReg     *agent.ToolRegistry // local-only tools; refreshed on reload
	GatewayOverlay  []agent.Tool        // cached gateway tools; refreshed on reload
	PostOverlays    []agent.Tool        // cloud_delegate etc.; refreshed on reload
	ShannonDir      string
	AgentsDir       string
	Auditor         *audit.AuditLogger
	HookRunner      *hooks.HookRunner
	SessionCache    *SessionCache
	EventBus        *EventBus
	ScheduleManager *schedule.Manager
	WSClient        *Client              // WebSocket client for proactive messages
	SecretsStore    *skills.SecretsStore // skill secrets for env injection
	MemSvc          *memory.Service      // structured memory orchestrator (Phase 2.3)
	// ReadTrackerCache holds per-session ReadTrackers so file_read dedup
	// history persists across the per-message AgentLoop instances created
	// by RunAgent. nil-safe: callers can leave it unset (each turn falls
	// back to a fresh tracker, equivalent to pre-fix behavior).
	ReadTrackerCache *ReadTrackerCache
	// Suggestions is the per-session prompt-suggestion store shared between
	// the HTTP handler (server.go) and the post-Run hook in RunAgent.
	// Wired by NewServer after construction. nil-safe: when unset (e.g. CLI
	// fixtures that construct ServerDeps directly), the post-Run hook is a no-op.
	Suggestions *agent.SuggestionState

	// ApprovalTracker records which sessions are currently blocked on a
	// user approval prompt. Approval handlers (SSE + WS) Mark/Clear here so
	// the daemon HTTP layer can surface "awaiting_approval" without scanning
	// per-request brokers. nil-safe.
	ApprovalTracker *ApprovalTracker

	// suggestionRegisteredMu + suggestionRegistered dedupe the
	// SessionManager.OnSessionClose registration in RunAgent: without dedupe
	// each turn appends a fresh closure to the same session's close-handler
	// list, growing O(N) per N-turn session. The set is keyed by sessionID;
	// the registered closure deletes its own key when fired so a sessionID
	// reused after a previous SessionManager lifetime can re-register cleanly.
	suggestionRegisteredMu sync.Mutex
	suggestionRegistered   map[string]struct{}
}

// Snapshot returns current Config, Registry, and Supervisor under read lock.
// Callers use the returned values without holding the lock.
func (d *ServerDeps) Snapshot() (*config.Config, *agent.ToolRegistry, *mcp.Supervisor) {
	d.mu.RLock()
	cfg, reg, sup := d.Config, d.Registry, d.Supervisor
	d.mu.RUnlock()
	return cfg, reg, sup
}

// ShutdownCleanup captures and calls the current Cleanup function under lock,
// preventing races with concurrent reload swaps.
func (d *ServerDeps) ShutdownCleanup() {
	d.mu.Lock()
	cleanup := d.Cleanup
	d.Cleanup = nil
	d.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

// WriteLock acquires the write lock on ServerDeps. Used by daemon event
// handler to update in-memory config (e.g., always-allow persistence).
func (d *ServerDeps) WriteLock()   { d.mu.Lock() }
func (d *ServerDeps) WriteUnlock() { d.mu.Unlock() }

// RebuildLayers returns the cached rebuild layers under read lock.
func (d *ServerDeps) RebuildLayers() (*agent.ToolRegistry, []agent.Tool, []agent.Tool, *mcp.ClientManager) {
	d.mu.RLock()
	bl, gw, po, mgr := d.BaselineReg, d.GatewayOverlay, d.PostOverlays, d.MCPManager
	d.mu.RUnlock()
	return bl, gw, po, mgr
}

// cleanupPlaywrightAfterTurn runs at the end of every RunAgent invocation
// (via defer). Behavior depends on three orthogonal factors: the playwright
// MCP config (CDP vs stdio), the keep_alive flag, and whether this Run
// actually used the browser (tracked via mcp.ChromeUseLease on ctx).
//
// CDP path: gated on browser-was-used. Counter ALWAYS releases (no leak on
// keep_alive). When the last browser-using Run releases its lease, disconnect
// Playwright + blocking-stop Chrome atomically under the tracker mutex so
// concurrent acquires from new Runs wait until teardown finishes.
//
// Non-CDP path: idle-disconnect schedules every Run regardless of browser-use
// (preserves existing behavior). keep_alive=true short-circuits as before.
func cleanupPlaywrightAfterTurn(ctx context.Context, mgr *mcp.ClientManager) {
	lease := mcp.ChromeUseLeaseFrom(ctx)
	if mgr == nil {
		if lease != nil {
			lease.ReleaseOnly()
		}
		return
	}
	cfg, ok := mgr.ConfigFor("playwright")
	if !ok {
		if lease != nil {
			lease.ReleaseOnly()
		}
		return
	}

	if mcp.IsPlaywrightCDPMode(cfg) {
		if cfg.KeepAlive {
			// Release the lease even on keep_alive so the counter cannot leak;
			// no teardown by config.
			if lease != nil {
				lease.ReleaseOnly()
			}
			return
		}
		if lease == nil {
			// Defensive: no lease => no browser use this Run, nothing to release.
			return
		}
		// Fresh background context — the request ctx may already be cancelled
		// on error/timeout paths, but cleanup must still complete.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		torndown, err := lease.ReleaseAndMaybeTeardown(
			func() { disconnectPlaywrightNowFn(mgr) },
			stopPlaywrightChromeAndWaitFn,
			cleanupCtx,
		)
		if torndown {
			log.Printf("daemon: Playwright on-demand teardown completed")
		}
		if err != nil {
			log.Printf("daemon: Playwright teardown error: %v", err)
		}
		return
	}

	// Non-CDP path: unchanged behavior for keep_alive=true; idle-disconnect
	// schedules for keep_alive=false on every turn regardless of browser-use.
	if lease != nil {
		lease.ReleaseOnly()
	}
	if cfg.KeepAlive {
		return
	}
	disconnectPlaywrightAfterIdleFn(mgr, 5*time.Minute)
	log.Printf("daemon: Playwright idle disconnect scheduled (5m)")
}

// cleanupBrowserToolAfterTurn runs at the end of every RunAgent invocation
// (via defer) to tear down the chromedp BrowserTool's Chrome when the last
// in-flight Run releases its lease. Mirrors cleanupPlaywrightAfterTurn but for
// the local chromedp fallback used when Playwright MCP isn't healthy. Has no
// effect when the Run didn't touch the chromedp backend or when the tool isn't
// in the registry (Playwright connected at startup and removed it).
func cleanupBrowserToolAfterTurn(ctx context.Context) {
	lease := tools.BrowserUseLeaseFrom(ctx)
	if lease == nil {
		return
	}
	owner := lease.Owner()
	if owner == nil {
		// Lease was created but MarkUsedWith never called (no browser activity this turn).
		lease.ReleaseOnly()
		return
	}
	// Owner-aware release: per-owner gate in ReleaseAndMaybeTeardown ensures
	// teardown fires against this lease's owner, not whatever the registry
	// currently holds (which post-reload would be NEW).
	//
	// Callback selection:
	//   - owner.IsDeprecated() → full Cleanup() (kills chromedp AND pinchtab).
	//     register.go's cleanup gate skips this owner, so the lease path is
	//     the only one that will tear down a deprecated owner's pinchtab
	//     state. Using CleanupChromedp here would leak pinchtab forever.
	//   - else → CleanupChromedp (preserves long-lived pinchtab across turns;
	//     this is the unchanged per-turn semantics from before this PR).
	var teardown func() error
	if owner.IsDeprecated() {
		teardown = func() error {
			owner.Cleanup()
			return nil
		}
	} else {
		teardown = owner.CleanupChromedp
	}
	torndown, err := lease.ReleaseAndMaybeTeardown(teardown)
	if torndown {
		if err != nil {
			log.Printf("daemon: chromedp browser teardown error: %v", err)
		} else {
			log.Printf("daemon: chromedp browser on-demand teardown completed")
		}
	}
}

// isUnattendedRunStartProbe reports whether this RunAgent invocation is
// triggered automatically (heartbeat, scheduled job, cron, file watcher, MCP
// inbound call, external webhook) rather than by a live user interaction. The
// Playwright degraded-self-heal path skips ProbeNow's Chrome relaunch for
// unattended runs so a user idle at their desk never sees a Chrome window pop
// open from a background tick — UX invariant: no visible Chrome while idle.
//
// Webhooks are included because they're fired by external services (CI,
// GitHub, Slack events, etc.) without a live user watching — same UX class as
// heartbeat/schedule.
func isUnattendedRunStartProbe(req RunAgentRequest) bool {
	if req.BypassRouting {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(req.Source)) {
	case "heartbeat", "schedule", "cron", "watcher", "mcp", "webhook":
		return true
	default:
		return false
	}
}

// shouldSkipPlaywrightProbeChromeRelaunch is defense-in-depth for the
// unattended-degraded-keep_alive=false branch of the preflight probe.
// The async-startup flow added an outer guard in RunAgent (skip when
// mgr.IsConnected("playwright") is false) that already covers the
// common post-discovery-Disconnect case — by the time we reach this
// predicate, playwrightLive must be true. So in practice the only
// surviving live cfg.KeepAlive=false path through here would be a
// transient window between connect-success and PostConnectDisconnect-
// IfDiscoveryOnly running. Kept as a safety net: if some future
// refactor breaks the "discover then disconnect" handshake, an
// unattended turn still won't pop a Chrome window.
func shouldSkipPlaywrightProbeChromeRelaunch(before mcp.ServerHealth, cfg mcp.MCPServerConfig, req RunAgentRequest) bool {
	return before.State == mcp.StateDegraded &&
		mcp.IsPlaywrightCDPMode(cfg) &&
		!cfg.KeepAlive &&
		isUnattendedRunStartProbe(req)
}

func shouldTrackPlaywrightProbeChromeBefore(before mcp.ServerHealth, cfg mcp.MCPServerConfig, req RunAgentRequest) bool {
	return before.State == mcp.StateDegraded &&
		mcp.IsPlaywrightCDPMode(cfg) &&
		!cfg.KeepAlive &&
		!isUnattendedRunStartProbe(req)
}

// resumeNamedAgentColdStart resumes the latest persisted named-agent session.
// Returns true only when a session was actually loaded from disk; a fresh
// in-memory session pre-created by the route manager does not count as resumed.
func resumeNamedAgentColdStart(sessMgr *session.Manager) (bool, error) {
	latest, err := sessMgr.ResumeLatest()
	if err != nil {
		return false, err
	}
	if latest != nil {
		return true, nil
	}
	if sessMgr.Current() == nil {
		sessMgr.NewSession()
	}
	return false, nil
}

func resumeRoutedColdStart(sessMgr *session.Manager, routeKey string) (bool, error) {
	if !shouldPersistRouteKey(routeKey) {
		return false, nil
	}
	latest, err := sessMgr.ResumeLatestByRouteKey(routeKey)
	if err != nil {
		return false, err
	}
	if latest != nil {
		return true, nil
	}
	return false, nil
}

// applyAgentModelOverlayToLoop applies the loop-facing fields of the per-agent
// model overlay onto the AgentLoop. Called per-turn so reload picks up edits.
//
// SetModelTier and SetSpecificModel write to independent fields on the loop
// (modelTier vs specificModel). Call order does NOT decide precedence; the
// request-time resolver in loop.go:messagesForLLM picks specificModel when
// non-empty and falls back to modelTier otherwise. Both setters are applied
// so an operator can later switch between specific-pin and tier without
// unsetting the other. Idle timeout fields live in runCfg, not on the loop,
// and are handled inline at the call site.
func applyAgentModelOverlayToLoop(loop *agent.AgentLoop, ac *agents.AgentModelConfig) {
	if loop == nil || ac == nil {
		return
	}
	if ac.ModelTier != nil && *ac.ModelTier != "" {
		loop.SetModelTier(*ac.ModelTier)
	}
	if ac.Model != nil {
		loop.SetSpecificModel(*ac.Model)
	}
	if ac.MaxIterations != nil {
		loop.SetMaxIterations(*ac.MaxIterations)
	}
	if ac.Temperature != nil {
		loop.SetTemperature(*ac.Temperature)
	}
	if ac.MaxTokens != nil {
		loop.SetMaxTokens(*ac.MaxTokens)
	}
	if ac.ContextWindow != nil {
		loop.SetContextWindowExplicit(*ac.ContextWindow)
	}
}

// historySnapshotForRequest returns the conversation history that the agent
// loop should see. When req.OmitHistory is true (set by the scheduler for
// stateless schedules), the LLM gets an empty history even though the session
// file still records every turn. Otherwise, defers to session.HistoryForLoop()
// which strips loop-injected guardrail nudges.
func historySnapshotForRequest(sess *session.Session, req RunAgentRequest) []client.Message {
	if req.OmitHistory {
		return nil
	}
	return sess.HistoryForLoop()
}

// RunAgent executes a single agent turn using the shared dependencies.
// The caller provides an EventHandler to control streaming, approval, and
// event reporting (WS uses daemonEventHandler, HTTP uses httpEventHandler).
func RunAgent(ctx context.Context, deps *ServerDeps, req RunAgentRequest, handler agent.EventHandler) (*RunAgentResult, error) {
	// Phase 1: read supervisor atomically, probe if needed
	cfg, _, sup := deps.Snapshot()
	if cfg == nil || deps.GW == nil || deps.SessionCache == nil {
		return nil, fmt.Errorf("daemon not fully configured")
	}
	// Install ChromeUseLease + BrowserUseLease on ctx before any tool dispatch
	// happens. Defer the same end-of-turn manager lookup the success-path
	// cleanup used, so reloads during a turn keep the existing cleanup
	// semantics. The browser lease covers the chromedp BrowserTool fallback used
	// when Playwright MCP is unavailable.
	ctx = mcp.WithChromeUseLease(ctx)
	ctx = tools.WithBrowserUseLease(ctx)
	defer func() {
		_, _, _, mgr := deps.RebuildLayers()
		cleanupPlaywrightAfterTurn(ctx, mgr)
		cleanupBrowserToolAfterTurn(ctx)
	}()
	if sup != nil {
		var mgr *mcp.ClientManager
		if _, _, _, m := deps.RebuildLayers(); m != nil {
			mgr = m
			// Cancel any pending idle disconnect — a new turn is starting.
			mgr.CancelIdleDisconnect("playwright")
		}
		// Only probe+reconnect Playwright when there is actually a live
		// client to probe. Two cases trigger the skip:
		//
		//   (1) periodic probe marked it Disconnected (e.g. user closed
		//       Chrome) — ProbeNow on Disconnected would fire
		//       attemptReconnect → relaunch Chrome.
		//   (2) post-discovery Disconnect for keep_alive=false — the
		//       async startup flow leaves supervisor state at Degraded
		//       (the capability probe ran before Chrome was up) even
		//       though we deliberately removed the client. ProbeNow on
		//       Degraded + CDP mode fires maybeRelaunchDegradedCDPChrome
		//       and pops Chrome for a turn that may never touch browser
		//       tools.
		//
		// Tool invocation paths in mcp_tool.go handle their own
		// ensureChromeDebugPort lazily, so we never lose recovery
		// capability when the agent actually needs the browser.
		before := sup.HealthFor("playwright")
		playwrightLive := mgr != nil && mgr.IsConnected("playwright")
		if before.State != mcp.StateDisconnected && playwrightLive {
			var pwCfg mcp.MCPServerConfig
			hasPlaywrightCfg := false
			if mgr != nil {
				pwCfg, hasPlaywrightCfg = mgr.ConfigFor("playwright")
			}
			if hasPlaywrightCfg && shouldSkipPlaywrightProbeChromeRelaunch(before, pwCfg, req) {
				log.Printf("daemon: skipping unattended Playwright degraded probe relaunch")
			} else {
				if hasPlaywrightCfg && shouldTrackPlaywrightProbeChromeBefore(before, pwCfg, req) {
					// ProbeNow may launch Chrome and still return Degraded/timeout
					// while the extension finishes initializing. Mark before the
					// probe so keep_alive=false always gets an end-of-turn teardown.
					mcp.MarkChromeUsed(ctx)
				}
				sup.ProbeNow("playwright")
			}
		}
	}
	// Phase 2: re-snapshot to get post-swap registry
	cfg, baseReg, _ := deps.Snapshot()
	if baseReg == nil {
		return nil, fmt.Errorf("daemon not fully configured")
	}
	agentName := req.Agent
	prompt := req.Text

	// Download remote file attachments and convert to file_ref blocks.
	// Attachment files must survive across turns (non-image files become
	// file_read hints in session history). Cleanup uses sessMgr.OnClose
	// (append-style, fires on manager close) — not OnSessionClose (which
	// replaces per-session and would clobber previous turns' cleanup).
	// The defer is a safety net for early-return errors before sessMgr
	// is available; it's cancelled once OnClose takes ownership.
	var attachmentCleanup func()
	var attachmentRegistered bool
	defer func() {
		if !attachmentRegistered && attachmentCleanup != nil {
			attachmentCleanup()
		}
	}()
	if len(req.Content) > 0 {
		var inlineCleanup func()
		req.Content, inlineCleanup = materializeInlineImageBlocks(deps.ShannonDir, req.Content)
		attachmentCleanup = combineCleanup(attachmentCleanup, inlineCleanup)
	}
	if len(req.Files) > 0 {
		var fileBlocks []RequestContentBlock
		var remoteCleanup func()
		fileBlocks, remoteCleanup = downloadRemoteFiles(deps.ShannonDir, req.Files)
		attachmentCleanup = combineCleanup(attachmentCleanup, remoteCleanup)
		req.Content = append(req.Content, fileBlocks...)
		// Zero auth headers to prevent lingering tokens in memory.
		for i := range req.Files {
			req.Files[i].AuthHeader = ""
		}
	}

	// Resolve multimodal content blocks (if present).
	var resolvedContent []client.ContentBlock
	if len(req.Content) > 0 {
		resolvedContent = resolveContentBlocks(req.Content)
	}

	// "default" is not a real agent — it means "use base agent, no --agent flag".
	if agentName == "default" {
		agentName = ""
	}
	req.Agent = agentName
	explicitAgent := agentName != "" // explicitly requested, not parsed from @mention

	// Parse @mention if no explicit agent was provided.
	if agentName == "" {
		agentName, prompt = agents.ParseAgentMention(req.Text)
	}
	if prompt == "" {
		prompt = req.Text
	}

	var agentOverride *agents.Agent
	if agentName != "" {
		a, loadErr := agents.LoadAgent(deps.AgentsDir, agentName)
		if loadErr != nil {
			if explicitAgent {
				return nil, fmt.Errorf("agent not found: %s", agentName)
			}
			// @mention fallback: use default agent
			log.Printf("daemon: agent %q not found: %v, using default", agentName, loadErr)
			agentName = ""
			prompt = req.Text
		} else {
			agentOverride = a
		}
	}
	// Resolve agent-scoped slash command: "/cmd-name args" → command content.
	if agentOverride != nil && strings.HasPrefix(prompt, "/") {
		parts := strings.Fields(prompt)
		cmdName := strings.TrimPrefix(parts[0], "/")
		if content, ok := agentOverride.Commands[cmdName]; ok {
			args := ""
			if len(parts) > 1 {
				args = strings.Join(parts[1:], " ")
			}
			prompt = strings.ReplaceAll(content, "$ARGUMENTS", args)
		}
	}
	req.Text = prompt
	// Recompute route key after final agent resolution.
	// Callers may precompute a default/source-channel key before @mention parsing.
	// Recomputing here avoids cross-route contamination.
	req.RouteKey = ComputeRouteKey(req)

	sessionsDir := deps.SessionCache.SessionsDir(agentName)
	var sessMgr *session.Manager

	var route *routeEntry
	var routeDone chan struct{}
	var routeInjectCh chan agent.InjectedMessage
	// Empty route key = no cache entry for routing, always start a fresh local session.
	if req.RouteKey != "" {
		route = deps.SessionCache.LockRouteWithManager(req.RouteKey, sessionsDir)
		sessMgr = route.manager
		reqCtx, cancel := context.WithCancel(ctx)
		routeDone = make(chan struct{})
		routeInjectCh = make(chan agent.InjectedMessage, 10)
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, nil, "")
		ctx = reqCtx
		// Register cancel under sc.mu so CancelRoute sees it immediately.
		// Also fires cancel right away if CancelRoute already set cancelPending.
		deps.SessionCache.SetRouteCancel(req.RouteKey, cancel)
		defer func() {
			// Emit MESSAGE_LIFECYCLE "done" for the tail of this run's
			// drained-inflight IM messages and "cleared" for earlier entries,
			// then clear the slice. Runs before ClearRouteRunState so the
			// route is still externally "active" while lifecycle events go
			// out, mirroring the Task 8 "processing" emit ordering. nil-safe
			// — TakeDrainedInflight short-circuits when the slice is empty
			// (non-IM runs / no follow-ups drained).
			var ws LifecycleEventSender
			if deps.WSClient != nil {
				ws = deps.WSClient
			}
			EmitLifecycleOnRunCompletion(ws, deps.SessionCache, req.RouteKey)
			deps.SessionCache.ClearRouteRunState(req.RouteKey)
			closeRouteDone(routeDone)
			route.cancel = nil
			// Atomic store — SetRouteSessionID would re-acquire entry.mu
			// (held by the surrounding LockRouteWithManager) and deadlock.
			if current := sessMgr.Current(); current != nil {
				route.storeSessionID(current.ID)
			}
			deps.SessionCache.UnlockRoute(req.RouteKey)
		}()

		// Drain any pending mailbox messages and prepend their text to the
		// current prompt so the LLM sees the user's full intent in one user
		// turn. This consumes both crash-recovery rows (seeded at daemon
		// startup) and any HTTP/Desktop messages that piled up via the
		// /queue endpoint while the route was idle.
		//
		// Known limitation (P0-4 relaxed): mailbox messages are MarkConsumed
		// as soon as their text lands in the prompt string — they are NOT
		// guaranteed to survive a daemon crash that happens between this
		// point and session.Save. The trade-off bounds blast radius to
		// "messages enqueued via /queue or recovery, then daemon crashes
		// in the next ~10ms"; for Cloud-sourced messages the Cloud replay
		// buffer is the primary durability layer regardless.
		if pendingBatch := deps.SessionCache.DrainMailbox(req.RouteKey, 20); len(pendingBatch) > 0 {
			pendingIDs := make([]string, 0, len(pendingBatch))
			var b strings.Builder
			for _, m := range pendingBatch {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(m.Text)
				// Phase 4: surface any attachments as a bracketed hint so the
				// LLM is aware the user shipped files alongside this text.
				// Full RequestContentBlock integration (image compression /
				// file_ref / auto-approval) is the follow-up — for now the
				// text representation keeps the queue lossless on the prompt
				// path even if downstream tools won't process them yet.
				if len(m.Attachments) > 0 {
					b.WriteByte('\n')
					b.WriteString("[Attached: ")
					for i, att := range m.Attachments {
						if i > 0 {
							b.WriteString(", ")
						}
						if att.Kind != "" {
							b.WriteString(att.Kind)
							b.WriteString(":")
						}
						if att.OriginalURL != "" {
							b.WriteString(att.OriginalURL)
						} else if att.Nonce != "" {
							b.WriteString(att.Nonce)
						} else {
							b.WriteString("file")
						}
					}
					b.WriteByte(']')
				}
				pendingIDs = append(pendingIDs, m.ID)
			}
			if b.Len() > 0 {
				if prompt == "" {
					prompt = b.String()
				} else {
					prompt = b.String() + "\n" + prompt
				}
				req.Text = prompt
			}
			log.Printf("daemon: drained %d mailbox msg(s) into prompt for route %q", len(pendingBatch), req.RouteKey)
			if err := deps.SessionCache.MarkMailboxConsumed(pendingIDs); err != nil {
				log.Printf("daemon: mailbox mark consumed (%v): %v", pendingIDs, err)
			}
			if deps.EventBus != nil {
				payload, _ := json.Marshal(map[string]any{
					"route_key":    req.RouteKey,
					"consumed_ids": pendingIDs,
					"snapshot":     ToDTOs(deps.SessionCache.MailboxSnapshot(req.RouteKey)),
				})
				deps.EventBus.Emit(Event{Type: EventQueueFlushed, Payload: payload})
			}
		}
	} else {
		managerDir := sessionsDir
		if req.BypassRouting {
			tmpDir, tmpErr := os.MkdirTemp("", "heartbeat-*")
			if tmpErr != nil {
				return nil, fmt.Errorf("create temp session dir: %w", tmpErr)
			}
			defer os.RemoveAll(tmpDir)
			managerDir = tmpDir
		}
		sessMgr = session.NewManager(managerDir)
		defer func() {
			if err := sessMgr.Close(); err != nil {
				log.Printf("daemon: failed to close ephemeral session manager for %q: %v", managerDir, err)
			}
		}()
	}

	resumed := false
	switch {
	case req.NewSession && req.SessionID != "":
		// Client-minted ID path: the caller (Desktop) generated the UUID
		// before the first POST so subsequent follow-ups can carry the
		// same id without waiting for the daemon's `session_started`
		// event. Without this branch, NewSession=true would route to the
		// generic NewSession() below and the daemon would mint its own
		// id — defeating the whole point.
		//
		// Idempotency: a follow-up POST may STILL carry new_session=true
		// when the client's pending-marker hadn't been cleared by the
		// `session_started` SSE event yet (the race we're fixing).
		// Resume first so a second-POST-while-first-not-acked re-binds
		// to the existing session instead of wiping the in-progress
		// history with a fresh blank Session.
		if !session.IsValidSessionID(req.SessionID) {
			return nil, fmt.Errorf("invalid session_id format: %q", req.SessionID)
		}
		if _, err := sessMgr.Resume(req.SessionID); err == nil {
			resumed = true
		} else {
			sessMgr.NewSessionWithID(req.SessionID)
		}
	case req.SessionID != "":
		// Resume a specific session by ID (reuses cached manager to avoid DB handle leak).
		if _, err := sessMgr.Resume(req.SessionID); err != nil {
			return nil, fmt.Errorf("session not found: %s", req.SessionID)
		}
		resumed = true
	case req.NewSession || req.RouteKey == "":
		sessMgr.NewSession()
	case route != nil && route.loadSessionID() != "":
		routedSessionID := route.loadSessionID()
		if _, err := sessMgr.Resume(routedSessionID); err != nil {
			log.Printf("daemon: failed to resume routed session %q for %q: %v", routedSessionID, req.RouteKey, err)
			sessMgr.NewSession()
		} else {
			resumed = true
		}
	case shouldPersistRouteKey(req.RouteKey):
		if resumedRoute, err := resumeRoutedColdStart(sessMgr, req.RouteKey); err != nil {
			log.Printf("daemon: failed to resume persisted routed session for %q: %v", req.RouteKey, err)
			if sessMgr.Current() == nil {
				sessMgr.NewSession()
			}
		} else if resumedRoute {
			resumed = true
		} else {
			sessMgr.NewSession()
		}
	case isPlainAgentRouteKey(req.RouteKey):
		// Named-agent cold start (first run or after daemon restart).
		// route.sessionID is empty — resume latest from disk, or start fresh if none.
		if resumedLatest, err := resumeNamedAgentColdStart(sessMgr); err != nil {
			log.Printf("daemon: failed to resume latest named-agent session for %q: %v", req.RouteKey, err)
			if sessMgr.Current() == nil {
				sessMgr.NewSession()
			}
		} else {
			resumed = resumedLatest
		}
	default:
		sessMgr.NewSession()
	}
	sess := sessMgr.Current()
	if route != nil && sess != nil {
		route.storeSessionID(sess.ID)
	}

	// Ad-hoc route registration for default-agent / route-less runs.
	//
	// ComputeRouteKey returns "" for Desktop's default-agent path (source=
	// "desktop" with no channel/agent), which sends those runs through the
	// `route == nil` branch above without any SessionCache entry. That means
	// a subsequent POST /cancel or explicit POST /message injection with
	// `session_id` cannot reach the running loop; `RouteKeyForSession`
	// finds no match.
	//
	// To make cancellation and explicit mid-turn injection work for the
	// default-agent path we register a transient entry under
	// "session:<sess.ID>" once sessions have resolved. The entry carries
	// this run's cancel func + done chan + injectCh + activeCWD so
	// POST /cancel and POST /message can locate the active run via
	// session_id. POST /queue is queue-only and does not write injectCh.
	//
	// Note: only fires when `route == nil` AND sess.ID is non-empty — the
	// routed branch above already set up its own injectCh/cancel for runs
	// with a meaningful route_key (named agents, Slack threads, etc.).
	var adHocRouteKey string
	if route == nil && sess != nil && sess.ID != "" {
		reqCtx, cancel := context.WithCancel(ctx)
		ctx = reqCtx
		routeDone = make(chan struct{})
		routeInjectCh = make(chan agent.InjectedMessage, 10)
		// activeCWD is unresolved at this point (effectiveCWD is computed
		// further below). Register with "" — EnqueueMessage's CWD check
		// is gated on activeCWD != "" so a missing value is permissive.
		if key, ok := deps.SessionCache.RegisterAdHocSessionRoute(sess.ID, cancel, routeDone, routeInjectCh, ""); ok {
			adHocRouteKey = key
			log.Printf("daemon: ad-hoc route registered key=%s (default-agent run, session=%s)", adHocRouteKey, sess.ID)
			defer func() {
				deps.SessionCache.UnregisterAdHocSessionRoute(adHocRouteKey)
				closeRouteDone(routeDone)
			}()
		} else {
			cancel()
			// Falling back to running without ad-hoc registration is safe —
			// the loop still works, only mid-turn injection is unavailable.
			routeDone = nil
			routeInjectCh = nil
			log.Printf("daemon: ad-hoc route NOT registered (session=%s already has an active run); mid-turn injection disabled", sess.ID)
		}
	}

	// Clear any pending suggestion before this turn starts — the user is
	// sending a new message, so any prior suggestion is stale. If the user
	// had accepted via /suggestion/accept, that handler also clears (in T11.5);
	// this guard catches the "user typed something else instead of accepting"
	// path. Done HERE (not at function top) because sess.ID isn't available
	// until sessMgr.Current() returns above.
	if deps.Suggestions != nil && sess != nil {
		deps.Suggestions.Clear(sess.ID)
	}

	// Seed pre-loaded history for bypass-routed runs (e.g., heartbeat).
	// The throwaway manager has an empty session; this gives the LLM context.
	if len(req.SessionHistory) > 0 {
		sess.Messages = req.SessionHistory
	}

	// Resolve effective CWD: request > resumed session > agent config. When all
	// three are empty we deliberately do NOT invent a working directory for
	// most sources — the request runs with no filesystem scope, and filesystem
	// tools (glob, grep, file_read, directory_list) will refuse any relative
	// paths at the tool level. Web-only and pure-reasoning tasks are unaffected.
	//
	// Cloud-routed sources (slack/line/feishu/lark/telegram/webhook) are the
	// one exception: they arrive with no user shell and no persisted CWD, so a
	// tool like browser_snapshot(filename="x.md") has nowhere to land and
	// file_read("x.md") can't resolve it. For those we allocate a per-session
	// scratch dir under ~/.shannon/tmp/sessions/<id>/ as the lowest-priority
	// fallback. Any real CWD (request/resumed/agent) still wins.
	var sessionCWD string
	if resumed {
		sessionCWD = sess.CWD
	}
	var agentCWD string
	if agentOverride != nil && agentOverride.Config != nil {
		agentCWD = agentOverride.Config.CWD
	}
	effectiveCWD := cwdctx.ResolveEffectiveCWD(req.CWD, sessionCWD, agentCWD)
	var cloudSessionCWD string
	if effectiveCWD == "" {
		if dir, err := ensureCloudSessionTmpDir(deps.ShannonDir, sess.ID, req.Source); err != nil {
			log.Printf("daemon: failed to allocate cloud session cwd for %s: %v", sess.ID, err)
		} else if dir != "" {
			cloudSessionCWD = dir
			effectiveCWD = dir
		}
	}
	if effectiveCWD != "" {
		if err := cwdctx.ValidateCWD(effectiveCWD); err != nil {
			return nil, fmt.Errorf("invalid cwd: %w", err)
		}
	}
	if req.RouteKey != "" {
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, routeInjectCh, effectiveCWD)
	}
	runCfg, err := config.RuntimeConfigForCWD(cfg, effectiveCWD)
	if err != nil {
		return nil, fmt.Errorf("runtime config: %w", err)
	}
	// Only write back when we have a real CWD — avoid poisoning the session
	// with an empty value and avoid overwriting an existing non-empty session
	// CWD with an empty fallback. Cloud scratch dirs are deliberately NOT
	// persisted: they live under ~/.shannon/tmp/sessions/<id>/, get removed
	// on session close, and must be re-allocated on every resume. Persisting
	// them would leave sess.CWD pointing at a now-deleted path, and the next
	// run would fail ValidateCWD before it could recreate the scratch.
	if effectiveCWD != "" && cloudSessionCWD == "" {
		sess.CWD = effectiveCWD
	}
	ctx = cwdctx.WithSessionCWD(ctx, effectiveCWD)

	// Wrap the transport handler with a bus-emitting handler so every run
	// publishes progress events regardless of transport. See
	// docs/superpowers/specs/2026-04-23-event-bus-progress-coverage-design.md.
	bus := &busEventHandler{deps: deps, agent: agentName}
	handler = &multiHandler{handlers: []agent.EventHandler{handler, bus}}

	// Notify handler of resolved session ID so it can include it in EventBus payloads.
	if setter, ok := handler.(interface{ SetSessionID(string) }); ok {
		setter.SetSessionID(sess.ID)
	}

	// Route notify tool calls through the EventBus so attached SSE clients
	// (typically the Desktop app) render the banner via UNUserNotificationCenter
	// with correct app attribution and click-through routing. Falls back to
	// the direct osascript path only when EmitTo reports zero deliveries —
	// either because no client is subscribed, or because every subscriber's
	// buffer was full. Using EmitTo's delivery count (rather than a liveness
	// check) means a single stalled subscriber cannot swallow notifications
	// into a silent void.
	if deps.EventBus != nil {
		sessID := sess.ID
		notifyAgent := agentName
		notifySource := req.Source
		ctx = tools.WithNotifyHandler(ctx, func(title, body string, sound bool) bool {
			payload, _ := json.Marshal(map[string]any{
				"session_id": sessID,
				"agent":      notifyAgent,
				"source":     notifySource,
				"title":      title,
				"body":       body,
				"sound":      sound,
			})
			return deps.EventBus.EmitTo(Event{Type: EventNotification, Payload: payload}) > 0
		})
	}

	// Persist session to disk before loop.Run() so there's a record even if
	// the daemon crashes mid-execution. The final save after completion is
	// still needed to capture the assistant's reply.
	// Ephemeral requests skip persistence — the caller owns session lifecycle.
	if !req.Ephemeral {
		if shouldPersistRouteKey(req.RouteKey) {
			sess.RouteKey = req.RouteKey
		}
		if req.Source != "" && req.Channel != "" {
			sess.Source = req.Source
			sess.Channel = req.Channel
		}
		// Only set source-derived title for non-named-agent routes.
		// Named agents always get session.AgentTitle in the post-loop block.
		if sess.Title == "New session" && req.RouteKey != "" && !strings.HasPrefix(req.RouteKey, "agent:") {
			title := routeTitle(req.Source, req.Channel, req.Sender)
			if title != "" {
				sess.Title = title
			}
		}
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to pre-save session: %v", err)
		}
	}

	// Snapshot history BEFORE appending the user message so loop.Run(prompt, history)
	// does not receive the user message twice (once as prompt, once in history).
	// HistoryForLoop strips prior loop-injected guardrail nudges (MessageMeta
	// .SystemInjected) so they cannot leak into the current run's conversation
	// snapshot — see session.Session.HistoryForLoop for the full rationale.
	history := historySnapshotForRequest(sess, req)

	// For externally-sourced messages (Slack, LINE, etc.), persist the user message
	// before the agent loop so the UI can display it immediately on notification.
	// preLoopUserAppended tracks the in-memory append (not save success) to prevent
	// double-appending in the post-loop persist block.
	userMsgTime := time.Now()
	var preLoopUserAppended bool
	if !req.Ephemeral && req.Source != "" {
		source := req.Source
		if source == "" {
			source = "unknown"
		}
		msgID := generateMessageID()
		userMsgContent := buildUserMsgContent(prompt, resolvedContent)
		sess.Messages = append(sess.Messages,
			client.Message{Role: "user", Content: userMsgContent},
		)
		sess.MessageMeta = append(sess.MessageMeta,
			session.MessageMeta{Source: source, MessageID: msgID, Timestamp: session.TimePtr(userMsgTime)},
		)
		preLoopUserAppended = true
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to pre-save user message: %v", err)
		} else if deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"sender":     req.Sender,
				"session_id": sess.ID,
				"message_id": msgID,
				"text":       prompt,
			})
			deps.EventBus.Emit(Event{Type: EventMessageReceived, Payload: payload})
		}
	}

	// Clone and apply per-agent tool filter
	reg := tools.CloneWithRuntimeConfig(baseReg, runCfg)
	if agentOverride != nil {
		reg = tools.ApplyToolFilter(reg, agentOverride)
	}

	// Attach SecretsStore to the session-scoped bash tool so use_skill
	// activations can expose skill secrets as child-process env vars.
	// Baseline bash is created at daemon start before NewServer, so the
	// store has to be wired here, after CloneWithRuntimeConfig has
	// deep-copied bash for this run.
	if deps.SecretsStore != nil {
		if bashTool, ok := reg.Get("bash"); ok {
			if bt, ok := bashTool.(*tools.BashTool); ok {
				bt.SecretsStore = deps.SecretsStore
			}
		}
	}

	// Load skills (agent-scoped or global) and wire to registry
	var loadedSkills []*skills.Skill
	if agentOverride != nil {
		loadedSkills = agentOverride.Skills
	} else {
		var err error
		loadedSkills, err = agents.LoadGlobalSkills(deps.ShannonDir)
		if err != nil {
			log.Printf("WARNING: failed to load global skills: %v", err)
		}
	}

	// Auto-inject bundled skills based on attached file types.
	if hasPDFAttachment(req.Content) {
		loadedSkills = injectBundledSkill(loadedSkills, deps.ShannonDir, "pdf-reader")
	}

	// Per-request skill suppression for desktop-only skills on cloud channels.
	// Must run BEFORE every loadedSkills consumer (SetRegistrySkills below for
	// the use_skill tool's runtime lookup, plus SwitchAgent / SetSkills further
	// down for AgentLoop.agentSkills which feeds buildSkillListing + semantic
	// discovery). Filtering at this single producer-side point keeps the three
	// LLM-facing exposure paths consistent — see internal/daemon/skill_filter.go.
	loadedSkills = filterSkillsForSource(loadedSkills, req.Source)

	tools.SetRegistrySkills(reg, loadedSkills)

	// Always expose local session search for daemon-served agents.
	// Use the per-agent manager so searches are scoped to that agent's sessions.
	tools.RegisterSessionSearch(reg, sessMgr)

	// memory_recall — talks to the structured memory sidecar when ready and
	// falls back to session keyword search + MEMORY.md grep otherwise. Always
	// register; the tool itself decides whether to use the service or fallback
	// based on the service's Status().
	var memSvc tools.MemoryQuerier
	if deps.MemSvc != nil {
		memSvc = deps.MemSvc
	}
	tools.RegisterMemoryTool(reg, memSvc, &daemonFallback{sessionMgr: sessMgr})

	loop := agent.NewAgentLoop(deps.GW, reg, runCfg.ModelTier, deps.ShannonDir,
		runCfg.Agent.MaxIterations, runCfg.Tools.ResultTruncation, runCfg.Tools.ArgsTruncation,
		&runCfg.Permissions, deps.Auditor, deps.HookRunner)
	loop.SetMaxTokens(runCfg.Agent.MaxTokens)
	loop.SetTemperature(runCfg.Agent.Temperature)
	// Seed the soft context window from the configured model or the last
	// model observed on this session, falling back to the static config.
	// Without this, every routed daemon turn would re-seed at the static
	// default and force the first preflight check to assume the wrong cap
	// until maybeAutoAdjustContextWindow runs after the first response.
	loop.SetContextWindow(agent.SeedContextWindowFromModels(
		runCfg.Agent.Model, sess.LastSeenModel(),
		agent.ContextWindowFloorForProvider(runCfg.Provider, runCfg.Agent.ContextWindow)))
	// Streaming on: bypasses Shannon Cloud's MAX_NON_STREAMING=16384 cap in
	// llm-service/llm_provider/anthropic_provider.py, raising effective max
	// output to the model's full limit (e.g. Sonnet 4.6 = 64K). Without this,
	// the trailing tool_use truncation handled above triggers on routine large
	// file_write calls; with streaming, it becomes a rare edge case (still
	// possible past 64K, but the model has 4x the budget before clipping).
	// Streaming fallback to Complete() is built into the agent loop, so a
	// gateway that rejects streaming degrades gracefully. WS/SSE/bus handlers
	// all implement OnStreamDelta — the WS+bus paths are no-ops (clients see
	// the final message either way), SSE forwards deltas for real-time UI.
	loop.SetEnableStreaming(true)
	loop.SetDeltaProvider(agent.NewTemporalDelta())
	loop.SetCacheSource(cacheSourceFromDaemonSource(req.Source))
	loop.SetSkillDiscovery(runCfg.Agent.SkillDiscoveryEnabled())
	if deps.MemSvc != nil {
		var helperLLM client.LLMClient
		if deps.GW != nil {
			helperLLM = deps.GW
		}
		loop.SetMemoryPreflight(tools.NewMemoryPreflight(deps.MemSvc, helperLLM))
	}
	loop.SetTimeBasedCompactConfig(agent.TimeBasedCompactConfig{
		Enabled:             runCfg.Agent.TimeBasedCompact.Enabled,
		GapThresholdMinutes: runCfg.Agent.TimeBasedCompact.GapThresholdMinutes,
		KeepRecent:          runCfg.Agent.TimeBasedCompact.KeepRecent,
	})
	if agentOverride != nil {
		scopedMCPCtx := tools.ResolveMCPContext(runCfg, agentOverride)
		agentDir := filepath.Join(deps.ShannonDir, "agents", agentName)
		loop.SwitchAgent(agentOverride.Prompt, agentDir, nil, scopedMCPCtx, loadedSkills)
		// SwitchAgent resets alwaysAllowTools to nil. Inject the union of
		// global (~/.shannon/config.yaml permissions.always_allow_tools) and
		// per-agent (agents/<name>/config.yaml permissions.always_allow_tools)
		// — global lets the user authorize a tool once and have it apply to
		// every agent; per-agent narrows trust to a single agent.
		// SetAlwaysAllowTools dedups internally so simple append is fine.
		merged := append([]string(nil), runCfg.Permissions.AlwaysAllowTools...)
		if agentOverride.Config != nil && agentOverride.Config.Permissions != nil {
			merged = append(merged, agentOverride.Config.Permissions.AlwaysAllowTools...)
		}
		loop.SetAlwaysAllowTools(merged)
	} else {
		loop.SetMemoryDir(filepath.Join(deps.ShannonDir, "memory"))
		if loadedSkills != nil {
			loop.SetSkills(loadedSkills)
		}
		scopedMCPCtx := tools.ResolveMCPContext(runCfg)
		if scopedMCPCtx != "" {
			loop.SetMCPContext(scopedMCPCtx)
		}
		// Default agent: only the global list applies (no per-agent config to
		// merge from). Solves the "Default agent re-prompts every bash command"
		// pain since global always_allow_tools persists across daemon restarts.
		loop.SetAlwaysAllowTools(runCfg.Permissions.AlwaysAllowTools)
	}
	if runCfg.Agent.Model != "" {
		loop.SetSpecificModel(runCfg.Agent.Model)
	}
	if runCfg.Agent.Thinking {
		if runCfg.Agent.ThinkingMode == "enabled" {
			loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: runCfg.Agent.ThinkingBudget})
		} else {
			loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		}
	}
	if runCfg.Agent.ReasoningEffort != "" {
		loop.SetReasoningEffort(runCfg.Agent.ReasoningEffort)
	}
	// Per-agent model config overrides
	if agentOverride != nil && agentOverride.Config != nil && agentOverride.Config.Agent != nil {
		ac := agentOverride.Config.Agent
		applyAgentModelOverlayToLoop(loop, ac)
		if ac.IdleSoftTimeoutSecs != nil {
			runCfg.Agent.IdleSoftTimeoutSecs = *ac.IdleSoftTimeoutSecs
		}
		if ac.IdleHardTimeoutSecs != nil {
			runCfg.Agent.IdleHardTimeoutSecs = *ac.IdleHardTimeoutSecs
		}
	}
	// Apply idle-timeout config AFTER per-agent overrides have been folded
	// into runCfg, otherwise agent-level opt-in/override silently does nothing.
	loop.SetIdleTimeouts(runCfg.Agent.IdleSoftTimeoutSecs, runCfg.Agent.IdleHardTimeoutSecs)
	if req.ModelOverride != "" {
		loop.SetModelTier(req.ModelOverride)
	}
	// Inject session metadata as sticky context so it survives compaction.
	{
		var parts []string
		if req.Source != "" {
			parts = append(parts, "Source: "+req.Source)
		}
		if req.Channel != "" {
			parts = append(parts, "Channel: "+req.Channel)
		}
		if req.Sender != "" {
			parts = append(parts, "Sender: "+req.Sender)
		}
		if agentName != "" {
			parts = append(parts, "Agent: "+agentName)
		}
		if req.StickyContext != "" {
			parts = append(parts, req.StickyContext)
		}
		if len(parts) > 0 {
			loop.SetStickyContext(strings.Join(parts, "\n"))
		}
	}

	// Output format: cloud-distributed channels use "plain" (Shannon Cloud
	// handles final channel rendering). Local sources keep "markdown" (default).
	loop.SetOutputFormat(outputFormatForSource(req.Source))

	loop.SetHandler(handler)

	// Wire handler and agent context to the per-run cloud_delegate copy.
	// Must use reg (cloned), not baseReg (shared), to avoid race across routes.
	if ct, ok := reg.Get("cloud_delegate"); ok {
		if cdt, ok := ct.(*tools.CloudDelegateTool); ok {
			cdt.SetHandler(handler)
			if agentOverride != nil {
				cdt.SetAgentContext(agentName, agentOverride.Prompt)
			} else {
				cdt.SetAgentContext("", "")
			}
		}
	}

	if routeInjectCh != nil {
		loop.SetInjectCh(routeInjectCh)
	}
	// IM message lifecycle: wire the per-run emitter so the agent loop can
	// fire "processing" + record drained-inflight entries for IM-sourced user
	// messages (Cloud needs the original IMStatusContext to map the entry
	// back to a platform reaction). Non-IM runs construct the emitter too —
	// it short-circuits internally on empty IMStatusContext, so the
	// architectural plumbing stays uniform and we avoid per-source branching.
	// nil deps.WSClient is tolerated (tests / fixtures construct ServerDeps
	// without a WS client); the emitter still records bookkeeping for the
	// drained-inflight slice Task 9 consumes at run completion.
	if req.RouteKey != "" {
		var ws LifecycleEventSender
		if deps.WSClient != nil {
			ws = deps.WSClient
		}
		loop.SetLifecycleEmitter(NewRunLifecycleEmitter(ws, deps.SessionCache, req.RouteKey))
	}
	// First-turn lifecycle: the run's primary user message moves into the
	// LLM turn on iter 0; expose its IM plumbing to the loop so it can emit
	// "processing" exactly once at first-turn entry. Empty fields short-
	// circuit inside the loop's first-turn check.
	loop.SetFirstTurnLifecycle(req.CloudMessageID, req.IMStatusContext)
	// Wire mailbox row consumption for legacy injected mailbox IDs. The
	// modern POST /queue path is queue-only, but this keeps older injected
	// ID paths idempotent if they appear in a live loop.
	if req.RouteKey != "" {
		routeKey := req.RouteKey
		loop.SetMailboxConsumeFn(func(ids []string) {
			if len(ids) == 0 {
				return
			}
			if err := deps.SessionCache.MarkMailboxConsumed(ids); err != nil {
				log.Printf("daemon: MarkMailboxConsumed(%v): %v", ids, err)
			}
			if deps.EventBus != nil {
				payload, _ := json.Marshal(map[string]any{
					"route_key":    routeKey,
					"consumed_ids": ids,
					"snapshot":     ToDTOs(deps.SessionCache.MailboxSnapshot(routeKey)),
				})
				deps.EventBus.Emit(Event{Type: EventQueueFlushed, Payload: payload})
			}
		})
	}
	loop.SetSessionID(sess.ID)
	// Make the caller's agent name available to tools via ctx. schedule_create
	// reads this so a schedule built from an agent conversation defaults to
	// that agent (keeping results reachable via session_search inside the
	// same agent) instead of silently routing to the default agent's pool.
	loop.SetAgentName(agentName)
	loop.SetToolResultBudgetState(sess.ToolResultReplacements, sess.ToolResultSeen)
	// Inject the per-session ReadTracker so file_read dedup history persists
	// across the per-message AgentLoop instances created here. nil-safe: an
	// unset cache returns a fresh tracker, which keeps the pre-fix behavior.
	loop.SetReadTracker(deps.ReadTrackerCache.GetOrCreate(sess.ID))
	loop.SetSessionCWD(effectiveCWD)
	loop.SetWorkingSet(sessMgr.WorkingSet(sess.ID))
	// Always set (even nil) to clear paths from a previous run on a reused loop.
	loop.SetUserFilePaths(extractUserFilePaths(req.Content))
	sessMgr.OnSessionClose(sess.ID, loop.SpillCleanupFunc())
	sessionID := sess.ID
	sessMgr.OnSessionClose(sessionID, func() { deps.ReadTrackerCache.Forget(sessionID) })

	// file:// preview bridge: lazily-started loopback HTTP server that
	// rewrites browser_navigate(file://...) into http://127.0.0.1/<token>/…
	// so Playwright's Chromium deny-list doesn't strand the agent.
	//
	// Allowlist: the bridge only serves files already reachable by the
	// agent's other tools — the effective session CWD subtree plus any
	// explicit user-attached files. This prevents browser_navigate from
	// becoming an escape hatch that reads arbitrary local files outside
	// the normal file-access boundary.
	filePreview := tools.NewFilePreviewBridge()
	if effectiveCWD != "" {
		filePreview.AllowRoot(effectiveCWD)
	}
	for _, p := range extractUserFilePaths(req.Content) {
		// Both APIs are stat-based and silently ignore wrong-type inputs:
		// AllowFile no-ops on directories, AllowRoot no-ops on regular files.
		// Calling both lets folder attachments grant subtree access while
		// file attachments stay exact-match.
		filePreview.AllowFile(p.Path)
		if p.IsDir {
			filePreview.AllowRoot(p.Path)
		}
	}
	sessMgr.OnSessionClose(sess.ID, func() { _ = filePreview.Close() })
	if cloudSessionCWD != "" {
		// Reclaim the per-session scratch dir when the session is closed
		// (SessionCache eviction, daemon shutdown). Artifacts live across turns
		// of the same session but don't accumulate across sessions.
		sessMgr.OnSessionClose(sess.ID, cloudSessionTmpCleanup(cloudSessionCWD))
	}
	// Tear down per-session suggestion state on explicit session close
	// (session delete/switch, TUI quit, daemon shutdown). Forget drops the
	// gens counter too — the slow leak Clear would otherwise produce across
	// long-running daemons with churning sessions.
	//
	// Dedup the registration: SessionManager.OnSessionClose appends callbacks,
	// so without this guard each turn of an N-turn session would push N
	// closures doing the same work. The dedup map is keyed by sessionID; the
	// registered closure removes its own key when fired so a sessionID reused
	// after a SessionManager lifetime can re-register cleanly.
	if deps.Suggestions != nil {
		deps.suggestionRegisteredMu.Lock()
		if deps.suggestionRegistered == nil {
			deps.suggestionRegistered = make(map[string]struct{})
		}
		_, already := deps.suggestionRegistered[sessionID]
		if !already {
			deps.suggestionRegistered[sessionID] = struct{}{}
		}
		deps.suggestionRegisteredMu.Unlock()
		if !already {
			sessMgr.OnSessionClose(sessionID, func() {
				deps.Suggestions.Forget(sessionID)
				deps.suggestionRegisteredMu.Lock()
				delete(deps.suggestionRegistered, sessionID)
				deps.suggestionRegisteredMu.Unlock()
			})
		}
	}
	ctx = tools.WithFilePreview(ctx, filePreview)
	if attachmentCleanup != nil {
		attachmentRegistered = true // cancel the defer safety net
		sessMgr.OnClose(attachmentCleanup)
	}

	// Turn persistence: capture the session state at turn start so both the
	// mid-turn checkpoint hook and the post-turn final save can rebuild
	// messages + usage idempotently from (baseline + current loop state).
	// This is the single source of truth — no append-on-top anywhere in
	// the turn's persistence path, which would otherwise double-write any
	// transcript that crossed a checkpoint boundary.
	checkpointSource := req.Source
	if checkpointSource == "" {
		checkpointSource = "unknown"
	}
	turnBase := captureTurnBaseline(sess, checkpointSource, preLoopUserAppended)
	// The daemon handler implements agent.UsageProvider; extract once so
	// callsites pass a strongly-typed provider (or nil) to applyTurnState.
	var turnUsage usageProvider
	if up, ok := handler.(agent.UsageProvider); ok {
		turnUsage = up
	}
	loop.SetCheckpointMinInterval(2 * time.Second) // debounce in the loop, not here
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		applyTurnState(sess, loop, turnUsage, turnBase)
		sess.InProgress = true
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: mid-turn checkpoint save failed: %v", err)
			// Return the error so AgentLoop.maybeCheckpoint keeps the
			// dirty flag set and the next fire point retries.
			return err
		}
		return nil
	})

	result, usage, runErr := loop.Run(ctx, prompt, resolvedContent, history)
	status := loop.LastRunStatus()
	if runErr != nil && !isSoftRunError(runErr) {
		// Hard error — save a user-friendly error message so the session isn't
		// left with a dangling user message and no assistant reply.
		// Full error detail goes to the log; session/UI gets a clean summary.
		log.Printf("daemon: agent %s run error: %v", agentName, runErr)
		if status.FailureCode == runstatus.CodeNone {
			status.FailureCode = runstatus.CodeFromError(runErr)
		}
		userErr := FriendlyAgentError(runErr)
		savedSessionID := ""
		if !req.Ephemeral && result == "" {
			// Use the same idempotent rebuild as the mid-turn checkpoint
			// and the normal final save: reset messages+usage to
			// (baseline + current snapshot), then append the friendly
			// error stub on top. This handles three previously-broken cases:
			//   (a) a prior checkpoint already persisted partial transcript
			//       — we must not duplicate it by appending the error on
			//       top of what's already there.
			//   (b) a dirty checkpoint was debounced just before the error
			//       — rebuilding from RunMessages picks up the trailing
			//       batches that never got their own save.
			//   (c) usage was already folded by a checkpoint — AddUsage
			//       would double-count, so use baseline+current instead.
			applyTurnMessages(sess, loop, turnBase)
			sess.Messages = append(sess.Messages,
				client.Message{Role: "assistant", Content: client.NewTextContent(userErr)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: req.Source, Timestamp: session.TimePtr(time.Now())},
			)
			applyTurnUsage(sess, turnUsage, turnBase)
			// Persist tool-result budget state so dedup/replacement bookkeeping
			// from this crashed turn survives resume; mid-turn checkpoints
			// already update it via applyTurnState, but a turn can fail before
			// the first checkpoint fires.
			sess.ToolResultReplacements = loop.ToolResultReplacements()
			sess.ToolResultSeen = loop.ToolResultSeen()
			sess.InProgress = false // hard-error path: turn is over, clear marker
			if err := sessMgr.Save(); err != nil {
				log.Printf("daemon: failed to save error session: %v", err)
			} else {
				savedSessionID = sess.ID
			}
		}
		if deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":          agentName,
				"source":         req.Source,
				"session_id":     savedSessionID,
				"error":          fmt.Sprintf("agent run failed: %v", runErr),
				"friendly_error": userErr,
				"failure_code":   status.FailureCode,
			})
			deps.EventBus.Emit(Event{Type: EventAgentError, Payload: payload})
		}
		// Return a partial result alongside the error so schedulers (and any
		// other lifecycle observer) can stamp "last run pointed at session X"
		// even when the LLM call hard-errored. Production callers (cmd/daemon.go
		// and heartbeat.go) gate on err first and never deref result on error,
		// so this is a wire-safe upgrade.
		//
		// Usage uses the same resolver as the success path so a turn that
		// spent tokens before failing on a later LLM call doesn't report
		// $0 / 0 tokens in the failed schedule_run event.
		return &RunAgentResult{
			SessionID:         savedSessionID,
			Agent:             agentName,
			Usage:             computeReportedUsage(usage, handler),
			FailureCode:       status.FailureCode,
			MessageStartIndex: turnBase.msgCount,
			MessageEndIndex:   len(sess.Messages),
		}, fmt.Errorf("agent error for %s: %w", agentName, runErr)
	}
	if errors.Is(runErr, agent.ErrMaxIterReached) {
		log.Printf("daemon: agent %s hit iteration limit, saving partial result", agentName)
	}

	// Tracks persistence outcome so the return value can blank SessionID on
	// failure (in addition to the agent_reply gate inside the block below).
	// Stays nil for ephemeral requests, which is the desired "no failure" state.
	var saveErr error

	// Ephemeral requests skip post-run persistence — the caller owns session lifecycle.
	if !req.Ephemeral {
		// Set title from first user message (named agents get a fixed title).
		if sess.Title == "New session" {
			if agentName != "" {
				sess.Title = session.AgentTitle(agentName)
			} else {
				sess.Title = session.Title(prompt)
			}
		}

		// Final save uses the same (baseline + current snapshot) rebuild as
		// mid-turn checkpoints, so a turn that produced checkpoints never
		// gets its transcript double-written here.
		if len(loop.RunMessages()) > 0 {
			applyTurnMessages(sess, loop, turnBase)
		} else {
			// Fallback: flat text (early LLM error with nothing accumulated).
			// Truncate to baseline first so this path is also idempotent
			// under the (unusual) case where a prior checkpoint ran.
			if len(sess.Messages) > turnBase.msgCount {
				sess.Messages = sess.Messages[:turnBase.msgCount]
			}
			if len(sess.MessageMeta) > turnBase.metaCount {
				sess.MessageMeta = sess.MessageMeta[:turnBase.metaCount]
			}
			if !preLoopUserAppended {
				fallbackContent := buildUserMsgContent(prompt, resolvedContent)
				sess.Messages = append(sess.Messages,
					client.Message{Role: "user", Content: fallbackContent},
				)
				sess.MessageMeta = append(sess.MessageMeta,
					session.MessageMeta{Source: checkpointSource, Timestamp: session.TimePtr(userMsgTime)},
				)
			}
			replyTime := time.Now()
			sess.Messages = append(sess.Messages,
				client.Message{Role: "assistant", Content: client.NewTextContent(result)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: checkpointSource, Timestamp: session.TimePtr(replyTime)},
			)
		}
		applyTurnUsage(sess, turnUsage, turnBase) // idempotent: baseline + current
		// Persist tool-result budget state. Mid-turn checkpoints (applyTurnState)
		// also update these, but a fast turn that finishes before the first
		// checkpoint fires would otherwise lose new dedup/replacement entries.
		sess.ToolResultReplacements = loop.ToolResultReplacements()
		sess.ToolResultSeen = loop.ToolResultSeen()
		sess.InProgress = false // turn completed — clear mid-turn crash marker
		saveErr = sessMgr.Save()
		if saveErr != nil {
			log.Printf("daemon: failed to save session: %v", saveErr)
			if deps.EventBus != nil {
				payload, _ := json.Marshal(map[string]any{
					"agent":        agentName,
					"source":       req.Source,
					"session_id":   sess.ID,
					"error":        fmt.Sprintf("session save failed: %v", saveErr),
					"failure_code": runstatus.CodeUnexpected,
				})
				deps.EventBus.Emit(Event{Type: EventAgentError, Payload: payload})
			}
		}

		// Only emit agent_reply when the session actually persisted. If the
		// save failed, the conversation is not on disk and downstream
		// consumers (e.g. desktop schedule notifications that click through
		// to the session) would point at a session that cannot be loaded.
		if saveErr == nil && deps.EventBus != nil {
			payload := map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"session_id": sess.ID,
				"text":       result,
			}
			// Soft-warning semantics: force-stop exits still emit a normal
			// agent_reply, but carry partial/failure_code so consumers can
			// show a non-error "stopped early" hint next to the text.
			if status.Partial {
				payload["partial"] = true
				payload["failure_code"] = status.FailureCode
			}
			payloadBytes, _ := json.Marshal(payload)
			deps.EventBus.Emit(Event{Type: EventAgentReply, Payload: payloadBytes})

			// Reply-complete banner: routes through tools.SendBanner so it honors
			// the same Desktop-handler-or-osascript-fallback contract as the
			// notify tool. Skip when there is nothing to show or the source
			// already delivers the reply elsewhere (cloud channels) or fires
			// autonomously and would spam (heartbeat/watcher/mcp). The osascript
			// fallback is macOS-only — skip the call on other platforms to keep
			// headless Linux daemons silent instead of log-spamming a missing
			// binary on every turn.
			if runtime.GOOS == "darwin" && result != "" && shouldEmitReplyBanner(req.Source) {
				title := "Kocoro"
				if agentName != "" {
					title = "Kocoro · " + agentName
				}
				body := truncate(stripMarkdownLite(audit.RedactSecrets(result)), 140)
				if err := tools.SendBanner(ctx, title, body, false); err != nil {
					log.Printf("daemon: reply-complete banner failed (session=%s): %v", sess.ID, err)
				}
			}
		}

		// Post-turn prompt suggestion (fire-and-forget). Gated by all of:
		//   - agent.prompt_suggestion.enabled
		//   - SuggestionState wired through deps (NewServer wires it; CLI
		//     fixtures that build ServerDeps directly leave it nil — no-op)
		//   - session was actually persisted (saveErr == nil) — otherwise the
		//     HTTP handler that polls /suggestion would 404 on the session
		//   - ShouldGenerateSuggestion passes (MinTurns, cache-cold gate,
		//     not partial/errored, not plan-mode)
		// The captured request snapshot is the last successful main-turn
		// dispatch (LastSentRequest); forking from it gives byte-equal
		// prefix and warm-cache pricing on the suggestion call.
		if saveErr == nil && deps.Suggestions != nil && cfg != nil && cfg.Agent.PromptSuggestion.Enabled {
			ps := cfg.Agent.PromptSuggestion
			completedTurns := countAssistantTurns(sess.Messages)
			// Judge cache warmth on the LAST main-turn LLM call, not the
			// turn-aggregate `usage` — a multi-tool turn that started cold
			// but ended warm (last iter ~100% cache_read) should NOT be
			// gated out by the cumulative count. The suggestion fork pivots
			// from the last sent request, so the last iter's warmth is the
			// authoritative signal. Fall back to turn-aggregate when the
			// loop didn't expose a last-iter snapshot (LastLLMUsage returns
			// ok=false only before the first call, which can't happen on this
			// success path, so the fallback is purely defensive).
			var uncached int
			if last, ok := loop.LastLLMUsage(); ok {
				uncached = last.InputTokens - last.CacheReadTokens
			} else {
				uncached = usage.InputTokens - usage.CacheReadTokens
			}
			if uncached < 0 {
				uncached = 0
			}
			args := agent.ShouldGenerateArgs{
				Enabled:                  ps.Enabled,
				CompletedTurns:           completedTurns,
				MinTurns:                 ps.MinTurns,
				LastTurnUncachedTokens:   uncached,
				CacheColdThresholdTokens: ps.CacheColdThresholdTokens,
				LastTurnHadError:         status.Partial || status.FailureCode != runstatus.CodeNone,
				PlanMode:                 false, // plan-mode tracking lands in a future task
			}
			if agent.ShouldGenerateSuggestion(args) {
				if mainReq, ok := loop.LastSentRequest(); ok {
					// context.Background(): the goroutine outlives the
					// request ctx (HTTP handler / WS dispatch returns
					// before the forked call completes). Cancellation
					// happens via daemon shutdown, not request lifecycle.
					//
					// result is the assistant reply from loop.Run — the
					// forked call appends it to main so the model
					// predicts the user's NEXT message after seeing the
					// assistant's response (not a stale follow-up to
					// the prior user turn).
					go fireSuggestionAfterRun(context.Background(), deps, agentName, sess.ID, mainReq, result)
				}
			}
		}
	}

	// Prefer handler-accumulated LLM totals (includes cloud_delegate nested
	// spend) for the model token fields. Tool billing rolls into CostUSD
	// on top of LLM cost but never into the token fields, so
	// input_tokens+output_tokens==total_tokens stays true for API consumers.
	// Resolver shared with the hard-error path so the two stay byte-equal.
	reportedUsage := computeReportedUsage(usage, handler)
	log.Printf("daemon: reply to %s (%d tokens, $%.4f)", agentName, reportedUsage.TotalTokens, reportedUsage.CostUSD)

	// On save failure, blank SessionID so HTTP/SSE clients can't click through
	// to a session that isn't on disk (matches the agent_reply gate above).
	returnedSessionID := sess.ID
	if saveErr != nil {
		returnedSessionID = ""
	}
	return &RunAgentResult{
		Reply:             result,
		SessionID:         returnedSessionID,
		Agent:             agentName,
		Usage:             reportedUsage,
		Partial:           status.Partial,
		FailureCode:       status.FailureCode,
		MessageStartIndex: turnBase.msgCount,
		MessageEndIndex:   len(sess.Messages),
	}, nil
}

func generateMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "msg-" + hex.EncodeToString(b)
}

// countAssistantTurns counts assistant messages in the persisted session.
// Used by the post-Run hook's MinTurns gate. The system message and any
// guardrail/preflight user injections are not counted — only the model's
// own responses are turns.
func countAssistantTurns(messages []client.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

// fireSuggestionAfterRun runs in a detached goroutine after the main turn
// completes successfully. It generates a forked prompt suggestion, stores it
// in SuggestionState, emits an SSE event, and writes audit rows that record
// the forked call's cache stats (T12 baked into T10 per plan).
//
// Failure semantics: any error — gateway transport, panic, nil dependency —
// is swallowed. The suggestion path must never crash the daemon or surface
// errors to the user; if the suggestion fails, the next /suggestion poll
// returns 404 and Desktop hides the ghost text.
//
// assistantReply is the text the assistant just generated this turn (return
// value of loop.Run). It must be non-empty — otherwise we skip, since the
// model has nothing to anchor the next-message prediction on. We append it
// to main.Messages as an assistant turn so the forked LLM call sees the
// conversation state Desktop is about to show the user. Without this the
// snapshot captured by LastSentRequest() reflects "before assistant
// responded" and the suggestion predicts a stale follow-up.
//
// Cache impact: the appended assistant message is uncached (~few hundred
// input tokens at warm-cache pricing per token). The cached PREFIX —
// main.Messages and everything before it — is byte-identical to the main
// turn's request, so prompt-cache lookup still hits.
//
// Cache-audit detail (the InputSummary field): the audit row carries the
// forked-call model + cache_read_tokens / cache_creation_tokens so operators
// can verify the suggestion path is hitting the main turn's prompt cache.
// Without this telemetry there's no signal that the feature is paying
// warm-cache pricing as designed.
func fireSuggestionAfterRun(ctx context.Context, deps *ServerDeps, agentName, sessionID string, main client.CompletionRequest, assistantReply string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("daemon: prompt_suggestion panic: %v", rec)
		}
	}()

	if assistantReply == "" {
		// No assistant text to anchor the prediction. Could be a tool-only
		// turn or some unusual partial state — skip rather than emit a
		// low-quality suggestion. Next turn will retry.
		return
	}

	// Augment main with the just-completed assistant reply. Allocate a fresh
	// Messages slice so this never aliases the loop's internal snapshot
	// (LastSentRequest already deep-copies on read, but rely on local
	// allocation for clarity).
	augmented := make([]client.Message, 0, len(main.Messages)+1)
	augmented = append(augmented, main.Messages...)
	augmented = append(augmented, client.Message{
		Role:    "assistant",
		Content: client.NewTextContent(assistantReply),
	})
	main.Messages = augmented

	// Capture the generation BEFORE the gateway call. If a Clear fires
	// while this goroutine is in flight (new turn / session close), the
	// SetIfFresh below will drop the write rather than resurrect a stale
	// suggestion the user has already moved past.
	observedGen := deps.Suggestions.CurrentGen(sessionID)

	res, err := agent.GenerateSuggestionWithUsage(ctx, deps.GW, main)
	if err != nil {
		// Transport / gateway failure — silent. Audit a row for diagnosability
		// only if Auditor is wired; the model is empty here.
		if deps.Auditor != nil {
			deps.Auditor.Log(audit.AuditEntry{
				Timestamp:    time.Now(),
				SessionID:    sessionID,
				Event:        "prompt_suggestion_error",
				InputSummary: fmt.Sprintf("agent=%s err=%v", agentName, err),
			})
		}
		return
	}
	if res.Text == "" {
		// Filter rejection or empty model output — record cost (the gateway
		// call still cost tokens) but skip the SSE event.
		if deps.Auditor != nil {
			deps.Auditor.Log(audit.AuditEntry{
				Timestamp:           time.Now(),
				SessionID:           sessionID,
				Event:               "prompt_suggestion_filtered",
				Model:               res.Model,
				InputTokens:         res.Usage.InputTokens,
				OutputTokens:        res.Usage.OutputTokens,
				CacheReadTokens:     res.Usage.CacheReadTokens,
				CacheCreationTokens: res.Usage.CacheCreationTokens,
				CostUSD:             res.Usage.CostUSD,
				InputSummary:        fmt.Sprintf("agent=%s", agentName),
			})
		}
		return
	}

	if !deps.Suggestions.SetIfFresh(sessionID, observedGen, res.Text, time.Now()) {
		// A Clear fired during the gateway call (new turn started or
		// session closed). Drop the suggestion silently — the user has
		// already moved past it, and resurrecting it would confuse the UI.
		// Audit row would be noise here; the Clear caller already knows
		// about the lifecycle transition.
		return
	}

	if deps.EventBus != nil {
		payload, _ := json.Marshal(map[string]any{
			"session_id": sessionID,
			"agent":      agentName,
			"text":       res.Text,
		})
		deps.EventBus.Emit(Event{Type: EventSuggestionReady, Payload: payload})
	}

	if deps.Auditor != nil {
		deps.Auditor.Log(audit.AuditEntry{
			Timestamp:           time.Now(),
			SessionID:           sessionID,
			Event:               "prompt_suggestion_generated",
			Model:               res.Model,
			InputTokens:         res.Usage.InputTokens,
			OutputTokens:        res.Usage.OutputTokens,
			CacheReadTokens:     res.Usage.CacheReadTokens,
			CacheCreationTokens: res.Usage.CacheCreationTokens,
			CostUSD:             res.Usage.CostUSD,
			InputSummary:        fmt.Sprintf("agent=%s text_len=%d", agentName, len(res.Text)),
		})
	}
}

// ErrSlashRouteBusy is returned when a slash request lands on a route key
// that already has another run in flight. Callers translate this to an SSE
// error event with reason="active_run_not_ready".
var ErrSlashRouteBusy = errors.New("slash command rejected: another run is active on this route")

// RunSlashWorkflow handles a /research or /swarm HTTP request by dispatching
// directly to Shannon Cloud's Gateway via cloudflow.Run. It mirrors RunAgent's
// return shape so callers can encode the result identically (the SSE writer's
// existing event: done payload works without modification).
//
// Differences from RunAgent:
//   - No agent-loop execution; cloudflow.Run replaces the loop body.
//   - No session-history replay into the LLM (Cloud carries its own memory).
//
// Same as RunAgent (intentionally, to avoid transcript corruption on
// concurrent same-session writes):
//   - Honors req.Agent — slash routes through the agent's SessionsDir and
//     resumes the agent's long-lived session.
//   - Acquires a SessionCache route lock so concurrent slash+RunAgent or
//     slash+slash on the same route serialize via the existing locking model.
//   - Persists user + assistant messages to the local transcript; emits the
//     same EventMessageReceived bus event RunAgent emits at runner.go:837-846.
//
// Caller MUST have already validated that req.Text is a slash command and
// that req.Content is empty (rejected at the HTTP layer).
func RunSlashWorkflow(ctx context.Context, deps *ServerDeps, req RunAgentRequest, cmd *cloudflow.SlashCommand, handler agent.EventHandler) (*RunAgentResult, error) {
	cfg, _, _ := deps.Snapshot()
	if deps.GW == nil {
		return nil, fmt.Errorf("gateway not configured")
	}

	// Honor cfg.Cloud.Timeout the same way register.go:458-461 does for
	// cloud_delegate. Zero falls back to 1 hour so slash and tool paths default
	// identically (cloudflow's own zero-fallback is 30 minutes — diverges from
	// the cloud_delegate baseline if we don't compute this here).
	slashTimeout := time.Duration(cfg.Cloud.Timeout) * time.Second
	if slashTimeout <= 0 {
		slashTimeout = time.Hour
	}

	agentName := req.Agent // honors named-agent lane; "" = default
	sessionsDir := deps.SessionCache.SessionsDir(agentName)

	// Use the same route-key + lock machinery RunAgent uses (runner.go:611-660),
	// but fail fast instead of canceling or waiting for an active route.
	// `route` is hoisted out of the locking block so the resolution switch
	// below can warm-resume from route.sessionID across slash invocations.
	var sessMgr *session.Manager
	var route *routeEntry
	// slashCtx is the cancellable context passed to cloudflow.Run. For routed
	// requests we register slashCancel via SetRouteCancel so POST /cancel can
	// stop the run. For unrouted requests slashCtx == ctx (no cancel target).
	slashCtx := ctx
	if req.RouteKey != "" {
		var busy bool
		route, busy = deps.SessionCache.TryLockRouteWithManager(req.RouteKey, sessionsDir)
		if busy {
			return nil, ErrSlashRouteBusy
		}
		sessMgr = route.manager

		// Wrap ctx so POST /cancel → CancelRoute → slashCancel propagates through
		// cloudflow.Run → StreamSSE. Without this, CancelRoute only sets
		// cancelPending=true (a flag nothing reads) and the cloud workflow
		// continues until its own deadline. Mirrors RunAgent at runner.go:620-628.
		var slashCancel context.CancelFunc
		slashCtx, slashCancel = context.WithCancel(ctx)
		defer slashCancel()

		// Publish run state so concurrent regular POST /message calls on the same
		// route see "active run in progress" via InjectMessage (returns InjectBusy)
		// instead of falling through to start a parallel RunAgent. Mirrors
		// RunAgent's pattern at runner.go:621-624 / 629-631.
		routeDone := make(chan struct{})
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, nil, "")
		deps.SessionCache.SetRouteCancel(req.RouteKey, slashCancel)
		defer func() {
			// Drain lifecycle done/cleared for any IM messages this slash run
			// consumed. In practice slash workflows are HTTP-initiated and the
			// slice is empty, but emitting unconditionally keeps the contract
			// uniform with RunAgent and stays a no-op when there is nothing
			// to emit.
			var ws LifecycleEventSender
			if deps.WSClient != nil {
				ws = deps.WSClient
			}
			EmitLifecycleOnRunCompletion(ws, deps.SessionCache, req.RouteKey)
			// Clear cancel registration before unlock so the next caller registers fresh.
			deps.SessionCache.SetRouteCancel(req.RouteKey, nil)
			deps.SessionCache.ClearRouteRunState(req.RouteKey)
			closeRouteDone(routeDone)
			if current := sessMgr.Current(); current != nil {
				route.storeSessionID(current.ID)
			}
			deps.SessionCache.UnlockRoute(req.RouteKey)
		}()
	} else {
		sessMgr = session.NewManager(sessionsDir)
		defer func() {
			if err := sessMgr.Close(); err != nil {
				log.Printf("daemon: failed to close session manager: %v", err)
			}
		}()
	}

	// Resume / new-session — mirrors RunAgent's switch at runner.go:1310-1373
	// (client-minted ID > pure SessionID resume > NewSession/no-route >
	// warm-resume > agent cold-start > default new).
	switch {
	case req.NewSession && req.SessionID != "":
		// Client-minted ID path: Desktop generates the UUID before the first
		// POST so subsequent follow-ups can carry the same id without waiting
		// for the daemon's `session_started` SSE event. Without this branch
		// the request falls through to `case req.SessionID != ""` below and
		// fails with "session not found" because the session file does not
		// exist yet — the bug that surfaced as `/research quick` and `/swarm`
		// erroring on the very first message of a fresh chat.
		//
		// Idempotency mirrors RunAgent: a follow-up POST may STILL carry
		// new_session=true when the client's pending-marker hadn't been
		// cleared by `session_started` yet. Resume first so a second POST
		// re-binds to the existing session instead of wiping the in-progress
		// history with a fresh blank Session.
		if !session.IsValidSessionID(req.SessionID) {
			return nil, fmt.Errorf("invalid session_id format: %q", req.SessionID)
		}
		if _, err := sessMgr.Resume(req.SessionID); err != nil {
			sessMgr.NewSessionWithID(req.SessionID)
		}
	case req.SessionID != "":
		if _, err := sessMgr.Resume(req.SessionID); err != nil {
			return nil, fmt.Errorf("session not found: %s", req.SessionID)
		}
	case req.NewSession || req.RouteKey == "":
		sessMgr.NewSession()
	case route != nil && route.loadSessionID() != "":
		// Warm resume: a prior run on this route stored its session ID; reuse it
		// so subsequent slash calls on the same routed lane (default:source:channel
		// or agent:foo) append to one continuous local transcript instead of
		// forking a fresh session each time.
		warmSessionID := route.loadSessionID()
		if _, err := sessMgr.Resume(warmSessionID); err != nil {
			log.Printf("daemon: failed to resume routed session %q for %q: %v", warmSessionID, req.RouteKey, err)
			sessMgr.NewSession()
		}
	case shouldPersistRouteKey(req.RouteKey):
		if resumedRoute, err := resumeRoutedColdStart(sessMgr, req.RouteKey); err != nil {
			log.Printf("daemon: failed to resume persisted routed session for %q: %v", req.RouteKey, err)
			if sessMgr.Current() == nil {
				sessMgr.NewSession()
			}
		} else if !resumedRoute {
			sessMgr.NewSession()
		}
	case isPlainAgentRouteKey(req.RouteKey):
		// Named-agent cold start — resume latest from disk, or NewSession if none.
		if _, err := resumeNamedAgentColdStart(sessMgr); err != nil {
			log.Printf("daemon: failed to resume latest named-agent session: %v", err)
			if sessMgr.Current() == nil {
				sessMgr.NewSession()
			}
		}
	default:
		sessMgr.NewSession()
	}
	sess := sessMgr.Current()
	if route != nil && sess != nil {
		route.storeSessionID(sess.ID)
	}

	// Notify the handler of the resolved session ID. Mirrors the RunAgent
	// path at runner.go:946-948 — without this, any approval prompt that
	// surfaces inside the cloud workflow would Mark the ApprovalTracker
	// with an empty sessionID and be invisible to GET /approvals on
	// reconnect. Today /research and /swarm rarely trigger user-facing
	// approvals (most tools auto-route via Gateway), but the asymmetry is
	// cheap to remove and keeps the contract consistent.
	if sess != nil {
		if setter, ok := handler.(interface{ SetSessionID(string) }); ok {
			setter.SetSessionID(sess.ID)
		}
	}

	// Stamp session metadata before persisting — mirrors runner.go:791-803.
	// This makes the session searchable/displayable by source+channel and gives
	// it a stable human-readable title.
	if !req.Ephemeral {
		if shouldPersistRouteKey(req.RouteKey) {
			sess.RouteKey = req.RouteKey
		}
		if req.Source != "" && req.Channel != "" {
			sess.Source = req.Source
			sess.Channel = req.Channel
		}
		// Title precedence (matches RunAgent's combined behavior at lines
		// 798-803 + 1147-1152): named agent > route source/channel > derived
		// from query.
		if sess.Title == "New session" {
			switch {
			case agentName != "":
				sess.Title = session.AgentTitle(agentName)
			case req.RouteKey != "":
				if t := routeTitle(req.Source, req.Channel, req.Sender); t != "" {
					sess.Title = t
				} else {
					sess.Title = session.Title(cmd.Query)
				}
			default:
				sess.Title = session.Title(cmd.Query)
			}
		}
	}

	// Persist the user message (matching runner.go:820-848 verbatim).
	if !req.Ephemeral {
		userMsgID := generateMessageID()
		sess.Messages = append(sess.Messages,
			client.Message{Role: "user", Content: client.NewTextContent(req.Text)},
		)
		sess.MessageMeta = append(sess.MessageMeta,
			session.MessageMeta{Source: req.Source, MessageID: userMsgID, Timestamp: session.TimePtr(time.Now())},
		)
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to pre-save user message: %v", err)
		} else if deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"sender":     req.Sender,
				"session_id": sess.ID,
				"message_id": userMsgID,
				"text":       req.Text,
			})
			deps.EventBus.Emit(Event{Type: EventMessageReceived, Payload: payload})
		}
	}

	apiKey := cfg.APIKey
	if deps.GW != nil {
		apiKey = deps.GW.APIKey()
	}
	cf := cloudflow.Request{
		Gateway:      deps.GW,
		APIKey:       apiKey,
		Query:        cmd.Query,
		WorkflowType: cmd.Type,
		Strategy:     cmd.Strategy,
		SessionID:    sess.ID,
		Timeout:      slashTimeout,
	}
	res, err := cloudflow.Run(slashCtx, cf, handler)
	if err != nil {
		return nil, err
	}

	// Persist assistant message.
	if !req.Ephemeral {
		sess.Messages = append(sess.Messages,
			client.Message{Role: "assistant", Content: client.NewTextContent(res.FinalText)},
		)
		sess.MessageMeta = append(sess.MessageMeta,
			session.MessageMeta{Source: "cloud", Timestamp: session.TimePtr(time.Now())},
		)
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to save assistant message: %v", err)
		}
	}

	// RunAgentUsage has exactly four fields (runner.go:394-399): InputTokens,
	// OutputTokens, TotalTokens, CostUSD. There is no Model field.
	return &RunAgentResult{
		Reply:     res.FinalText,
		SessionID: sess.ID,
		Agent:     agentName,
		Usage: RunAgentUsage{
			InputTokens:  res.Usage.InputTokens,
			OutputTokens: res.Usage.OutputTokens,
			TotalTokens:  res.Usage.TotalTokens,
			CostUSD:      res.Usage.CostUSD,
		},
	}, nil
}

func closeRouteDone(done chan struct{}) {
	if done == nil {
		return
	}
	defer func() {
		if recover() != nil {
			// Best effort cleanup; callers may close defensively in multiple paths.
			// Avoid panic if the channel was already closed externally.
		}
	}()
	close(done)
}

// isSoftRunError reports whether err is a normal termination (cancel, timeout,
// max iterations) rather than a hard failure. Soft errors should persist the
// full conversation from RunMessages(), not just a friendly error stub.
//
// ErrStreamIdleTimeout is soft: the agent loop already captures partial
// streaming text and emits OnRunStatus("stream_idle_timeout") with
// CodeDeadlineExceeded + Partial=true before returning. Treating it as hard
// here would overwrite the partial reply with the friendly-error stub and
// drop the agent_reply event entirely (issue: silent stream drops would lose
// any text received before the drop).
func isSoftRunError(err error) bool {
	return errors.Is(err, agent.ErrMaxIterReached) ||
		errors.Is(err, agent.ErrHardIdleTimeout) ||
		errors.Is(err, client.ErrStreamIdleTimeout) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// turnBaseline captures pre-turn session state so both mid-turn checkpoints
// and the post-turn final save can idempotently rebuild the session from
// (baseline + current loop snapshot) — never append-on-top. This is the
// single persistence invariant for a turn: after applyTurnState runs, the
// session reflects exactly one canonical transcript and one usage total
// for the accumulated turn, no matter how many times the function is
// called.
type turnBaseline struct {
	msgCount    int
	metaCount   int
	usage       session.UsageSummary // pre-turn cumulative usage; zero if sess.Usage was nil
	hadUsage    bool                 // true if sess.Usage was non-nil at baseline
	source      string
	preLoopUser bool
}

// captureTurnBaseline snapshots sess state at turn start so subsequent
// applyTurnState calls can rebuild idempotently.
func captureTurnBaseline(sess *session.Session, source string, preLoopUserAppended bool) turnBaseline {
	b := turnBaseline{
		msgCount:    len(sess.Messages),
		metaCount:   len(sess.MessageMeta),
		source:      source,
		preLoopUser: preLoopUserAppended,
	}
	if sess.Usage != nil {
		b.usage = *sess.Usage
		b.hadUsage = true
	}
	return b
}

// applyTurnMessages rebuilds sess.Messages/MessageMeta from baseline +
// loop.RunMessages(). Idempotent — safe to call any number of times with
// changing loop state (compaction shrinks etc.).
//
// Baseline messages (anything saved by prior turns) are NOT re-sanitized here —
// only the new portion from the current turn passes through
// loop.SanitizedRunMessages(). This is intentional: even if Layer 1 (source-
// time compression) fails open and an oversize image lands in a baseline
// session message, the wire-time sanitizer (filterOversizeImages inside
// messagesForLLM) catches it on every API call, so the persisted-but-oversize
// state has no API-failure impact. Re-sanitizing the baseline on every save
// would double the work for no observable benefit. Trade-off: session.json
// on disk may carry residual oversize bytes after a fail-open event until
// the message ages out of context via the time-based image-strip pass.
func applyTurnMessages(sess *session.Session, loop *agent.AgentLoop, b turnBaseline) {
	if len(sess.Messages) > b.msgCount {
		sess.Messages = sess.Messages[:b.msgCount]
	}
	if len(sess.MessageMeta) > b.metaCount {
		sess.MessageMeta = sess.MessageMeta[:b.metaCount]
	}
	runMsgs := loop.SanitizedRunMessages()
	if len(runMsgs) == 0 {
		return
	}
	runInjected := loop.RunMessageInjected()
	runTimestamps := loop.RunMessageTimestamps()
	startIdx := 0
	if b.preLoopUser && runMsgs[0].Role == "user" {
		startIdx = 1
	}
	fallbackTime := time.Now()
	for i := startIdx; i < len(runMsgs); i++ {
		ts := fallbackTime
		if i < len(runTimestamps) && !runTimestamps[i].IsZero() {
			ts = runTimestamps[i]
		}
		sess.Messages = append(sess.Messages, runMsgs[i])
		meta := session.MessageMeta{Source: b.source, Timestamp: session.TimePtr(ts)}
		if i < len(runInjected) && runInjected[i] {
			meta.SystemInjected = true
		}
		sess.MessageMeta = append(sess.MessageMeta, meta)
	}
}

// usageProvider is the local interface applyTurnUsage needs. Defined here
// (rather than accepting agent.UsageProvider directly) so the caller type
// is restricted at compile time — a future refactor that dropped the
// interface on the daemon handler would fail to compile instead of
// silently no-op'ing the usage folding at runtime.
type usageProvider interface {
	Usage() agent.AccumulatedUsage
}

// applyTurnUsage sets sess.Usage to (baseline + current accumulator).
// Idempotent — no double-counting across checkpoint + final-save calls.
// A nil provider is a no-op (used by unit tests that exercise only the
// message path).
func applyTurnUsage(sess *session.Session, up usageProvider, b turnBaseline) {
	if up == nil {
		return
	}
	acc := up.Usage()
	llm := acc.LLM
	hasTurnUsage := llm.LLMCalls > 0 || acc.ToolCalls > 0 || llm.InputTokens > 0 ||
		llm.CostUSD > 0 || acc.ToolCostUSD > 0
	if !b.hadUsage && !hasTurnUsage {
		return
	}
	total := b.usage
	if hasTurnUsage {
		total.Add(session.UsageFromAccumulated(
			llm.LLMCalls, llm.InputTokens, llm.OutputTokens, llm.TotalTokens,
			llm.CostUSD, llm.CacheReadTokens, llm.CacheCreationTokens, llm.CacheCreation5mTokens, llm.CacheCreation1hTokens, llm.Model,
			acc.ToolCalls, acc.ToolCostUSD,
		))
	}
	sess.Usage = &total
	if sess.SchemaVersion < 2 {
		sess.SchemaVersion = 2
	}
}

// applyTurnState is the combined rebuild — messages + usage — used by
// both mid-turn checkpoints and the post-turn final save so a turn is
// never persisted twice via different paths. up may be nil (usage skipped).
func applyTurnState(sess *session.Session, loop *agent.AgentLoop,
	up usageProvider, b turnBaseline) {
	applyTurnMessages(sess, loop, b)
	applyTurnUsage(sess, up, b)
	sess.ToolResultReplacements = loop.ToolResultReplacements()
	sess.ToolResultSeen = loop.ToolResultSeen()
}

// FriendlyAgentError maps raw agent errors to user-facing messages.
// Full error detail is logged separately; this keeps session/UI clean.
// Uses FriendlyMessageFromError so 429 sub-codes (quota / credits /
// throttle / upstream) yield the right user message instead of the
// generic "try again in a moment" — quota holders get told when the
// quota resets, credits-exhausted users get told to top up.
func FriendlyAgentError(err error) string {
	return runstatus.FriendlyMessageFromError(err)
}
