package daemon

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	// File caps aligned with claude.ai (500 MB / 20 files) per plan §2 P0.
	maxFileSize     = 500 * 1024 * 1024 // 500 MB per file
	maxFiles        = 20                // max attachments per message
	downloadTimeout = 2 * time.Minute

	// maxInlineImageBase64Bytes caps the pre-decode size of inline image
	// blocks so a hostile or buggy caller cannot force a multi-hundred-MB
	// base64 allocation before the downstream 20 MB decoded cap in
	// resolveFileRef fires. Uses the worst-case 4/3 base64 inflation ratio
	// plus a few bytes of padding slack.
	//
	// NOTE: intentionally NOT raised alongside maxFileSize. Inline images
	// arrive base64-decoded into one contiguous allocation, so they own a
	// much tighter budget than streamed file downloads. Anthropic's vision
	// payload sweet spot is well under 20 MB; raising this cap rewards
	// pathological inputs (multi-hundred-MB screenshots) without unlocking
	// new use cases.
	maxInlineImageDecodedBytes = 20 * 1024 * 1024
	maxInlineImageBase64Bytes  = maxInlineImageDecodedBytes*4/3 + 4

	// MaxExtractedTextChars enforces plan §4.5.1 at the daemon edge: even if
	// cloud sends an oversized ExtractedText (old build / bug), daemon
	// truncates before writing to session JSON. Rune-counted to avoid
	// splitting multi-byte UTF-8 characters mid-symbol.
	MaxExtractedTextChars = 500_000

	// MaxInlineDocumentB64Bytes caps base64 size of inline `document` blocks
	// so we reject before allocating decoded bytes. 32 MB raw × 4/3 base64
	// inflation = ~42.7 MB, but Anthropic's per-request hard cap is 32 MB
	// (encoded). The 25 MB raw threshold (≈33 MB base64) keeps a small buffer
	// for the rest of the request body. Cloud's §4.5 size gate is the primary
	// guard; this is defense in depth.
	maxInlineDocumentDecodedBytes = 25 * 1024 * 1024
	maxInlineDocumentB64Bytes  = maxInlineDocumentDecodedBytes*4/3 + 4
)

// urlValidator is the URL validation function used before each download.
// Tests may replace this to allow httptest (loopback) URLs.
var urlValidator = validateDownloadURL

func createAttachmentDir(shannonDir string) (string, func(), error) {
	// Generate a random nonce for the attachment directory (session ID is
	// not yet available at this point in RunAgent).
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, fmt.Errorf("generate attachment nonce: %w", err)
	}
	dir := filepath.Join(shannonDir, "tmp", "attachments", hex.EncodeToString(nonce[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create attachment dir %s: %w", dir, err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("daemon: failed to cleanup attachment dir %s: %v", dir, err)
		}
	}
	return dir, cleanup, nil
}

// combineCleanup composes two cleanup closures into one, running them in
// LIFO order: the most-recently-registered (next) runs first. Callers can
// chain new cleanups onto an accumulator and rely on the later cleanup
// completing before earlier ones (e.g. remove remote-file dir first, then
// inline-image dir).
func combineCleanup(existing, next func()) func() {
	if existing == nil {
		return next
	}
	if next == nil {
		return existing
	}
	return func() {
		next()
		existing()
	}
}

// downloadRemoteFiles materializes remote file attachments into
// RequestContentBlocks suitable for the LLM. Per plan §4.3 priority order:
//
//  1. DocumentB64 non-empty → decode + write to tmp dir, emit a `document`
//     block (base64 source) + companion `text` hint pointing at the local path.
//  2. ExtractedText non-empty → single `text` block prefixed with the filename
//     and mimetype. Daemon-side enforces MaxExtractedTextChars truncation as a
//     defense-in-depth guard (plan §4.5.1).
//  3. URL non-empty → legacy HTTP download path (file_ref block).
//  4. None of the above → text warning block (don't silently drop, §4.8).
//
// Cleanup removes the per-message attachment directory. The directory is
// created lazily so requests carrying only ExtractedText (no files written
// to disk) don't leave empty directories behind.
func downloadRemoteFiles(shannonDir string, files []RemoteFile) ([]RequestContentBlock, func()) {
	if len(files) == 0 {
		return nil, func() {}
	}

	// Cap the number of files to prevent excessive downloads.
	capped := files
	var capBlocks []RequestContentBlock
	if len(files) > maxFiles {
		capped = files[:maxFiles]
		capBlocks = append(capBlocks, RequestContentBlock{
			Type: "text",
			Text: fmt.Sprintf("[Warning: only the first %d of %d attachments were downloaded]", maxFiles, len(files)),
		})
	}

	// Lazy attachment directory: only created when the first file actually
	// needs disk storage (DocumentB64 decode or URL download). Pure
	// ExtractedText requests never allocate a directory.
	var (
		dir     string
		cleanup func()
		dirErr  error
	)
	ensureDir := func() (string, error) {
		if dir != "" || dirErr != nil {
			return dir, dirErr
		}
		dir, cleanup, dirErr = createAttachmentDir(shannonDir)
		if dirErr != nil {
			log.Printf("daemon: failed to create attachment dir: %v", dirErr)
		}
		return dir, dirErr
	}

	// Custom client that preserves Authorization header across redirects.
	// Slack may redirect file URLs to CDN; Go's default policy strips
	// the header on cross-domain redirects.
	httpClient := &http.Client{
		Timeout: downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			// Validate redirect target to prevent SSRF via open redirect.
			if err := urlValidator(req.URL.String()); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			// Carry Authorization from the original request.
			if auth := via[0].Header.Get("Authorization"); auth != "" {
				req.Header.Set("Authorization", auth)
			}
			return nil
		},
	}
	blocks := make([]RequestContentBlock, 0, len(capped))

	for i, f := range capped {
		displayName := f.Name
		if displayName == "" || displayName == "." || displayName == ".." {
			displayName = sanitizeFilename(i, f.Name)
		}

		switch {
		case f.DocumentB64 != "":
			docBlocks, err := materializeInlineDocument(ensureDir, i, f, displayName)
			if err != nil {
				log.Printf("daemon: inline document %s failed: %v", f.Name, err)
				// Fall back to URL download if available — never silently drop.
				// Re-call ensureDir() so we use the lazily-created dir; the outer
				// `dir` closure variable is still empty here if materializeInlineDocument
				// returned before it allocated storage.
				if f.URL != "" {
					if localDir, derr := ensureDir(); derr == nil {
						block, derr := downloadOneFile(httpClient, f, filepath.Join(localDir, sanitizeFilename(i, f.Name)), displayName)
						if derr == nil {
							blocks = append(blocks, block)
							continue
						}
						log.Printf("daemon: fallback download %s failed: %v", f.Name, sanitizeError(derr))
					}
				}
				blocks = append(blocks, RequestContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[Error: unable to process inline document %s]", displayName),
				})
				continue
			}
			blocks = append(blocks, docBlocks...)

		case f.ExtractedText != "":
			blocks = append(blocks, buildExtractedTextBlock(f, displayName))

		case f.URL != "":
			localDir, err := ensureDir()
			if err != nil {
				blocks = append(blocks, RequestContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[Error: unable to prepare storage for %s]", displayName),
				})
				continue
			}
			diskName := sanitizeFilename(i, f.Name)
			localPath := filepath.Join(localDir, diskName)
			block, err := downloadOneFile(httpClient, f, localPath, displayName)
			if err != nil {
				log.Printf("daemon: download %s failed: %v", f.Name, sanitizeError(err))
				blocks = append(blocks, RequestContentBlock{
					Type: "text",
					Text: fmt.Sprintf("[Error: unable to download file %s]", displayName),
				})
				continue
			}
			blocks = append(blocks, block)

		default:
			// No payload of any kind — surface a warning rather than silently
			// dropping the attachment (plan §4.8 "never silently drop").
			blocks = append(blocks, RequestContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[Warning: attachment %s arrived with no URL, extracted text, or inline base64]", displayName),
			})
		}
	}
	blocks = append(blocks, capBlocks...)
	if cleanup == nil {
		cleanup = func() {}
	}
	return blocks, cleanup
}

// materializeInlineDocument decodes RemoteFile.DocumentB64 and writes it to
// the per-message attachment directory, returning a `document` block (base64
// source) followed by a `text` hint pointing at the saved local path so the
// LLM can fall through to file_read / bash for selective access.
//
// Only application/pdf is treated as a native document MIME; any other type is
// rejected and the caller falls back to URL download. Anthropic's document
// content block accepts PDF natively; other formats must arrive as
// extracted_text.
func materializeInlineDocument(ensureDir func() (string, error), index int, f RemoteFile, displayName string) ([]RequestContentBlock, error) {
	mime := strings.ToLower(strings.TrimSpace(f.MimeType))
	// Empty MIME is treated as an error rather than silently defaulting to
	// PDF: a future cloud build that ships DocumentB64 with non-PDF bytes
	// and an unset MimeType would otherwise be forwarded to Anthropic as
	// application/pdf and 400 with a confusing message. The caller falls
	// back to URL download in this case, so the message still reaches the
	// model.
	if mime == "" {
		return nil, fmt.Errorf("inline document missing MIME type")
	}
	if mime != "application/pdf" {
		return nil, fmt.Errorf("unsupported inline document media type %q", mime)
	}

	if len(f.DocumentB64) > maxInlineDocumentB64Bytes {
		return nil, fmt.Errorf("inline document exceeds size guard (%d base64 bytes > %d)", len(f.DocumentB64), maxInlineDocumentB64Bytes)
	}
	data, err := base64.StdEncoding.DecodeString(f.DocumentB64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(data) > maxInlineDocumentDecodedBytes {
		return nil, fmt.Errorf("inline document exceeds decoded size guard (%d bytes > %d)", len(data), maxInlineDocumentDecodedBytes)
	}

	dir, err := ensureDir()
	if err != nil {
		return nil, fmt.Errorf("create attachment dir: %w", err)
	}
	diskName := sanitizeFilename(index, f.Name)
	if filepath.Ext(diskName) == "" {
		diskName += ".pdf"
	}
	localPath := filepath.Join(dir, diskName)
	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	encoded := f.DocumentB64
	// Strip any whitespace from the cloud-provided base64 so the marshaled
	// content block is canonical bytes (Anthropic accepts whitespace, but
	// stripping keeps the prompt-cache prefix byte-stable for re-sends).
	if strings.ContainsAny(encoded, "\r\n\t ") {
		var b strings.Builder
		b.Grow(len(encoded))
		for _, r := range encoded {
			if r != '\r' && r != '\n' && r != '\t' && r != ' ' {
				b.WriteRune(r)
			}
		}
		encoded = b.String()
	}

	return []RequestContentBlock{
		{
			Type: "document",
			Source: &client.ImageSource{
				Type:      "base64",
				MediaType: mime,
				Data:      encoded,
			},
			Filename: displayName,
			ByteSize: int64(len(data)),
		},
		{
			Type: "text",
			Text: fmt.Sprintf("[Attached PDF: %s — also saved locally at %s (use file_read for selective access)]",
				displayName, localPath),
		},
	}, nil
}

// buildExtractedTextBlock formats Cloud's pre-extracted text into a single
// `text` content block. Daemon enforces MaxExtractedTextChars truncation here
// (plan §4.5.1 defense in depth) — even if Cloud sends an oversized payload,
// daemon trims before the bytes hit session JSON / agent loop.
func buildExtractedTextBlock(f RemoteFile, displayName string) RequestContentBlock {
	mime := f.MimeType
	if mime == "" {
		mime = "unknown"
	}
	text := f.ExtractedText
	if runeCount := utf8RuneCount(text); runeCount > MaxExtractedTextChars {
		text = truncateToRunes(text, MaxExtractedTextChars)
		text += fmt.Sprintf("\n\n[Daemon truncated extracted text: %d → %d chars (cap=%d). See original file via URL if available.]",
			runeCount, MaxExtractedTextChars, MaxExtractedTextChars)
	}
	header := fmt.Sprintf("[Attached: %s (%s)]\n", displayName, mime)
	return RequestContentBlock{
		Type: "text",
		Text: header + text,
	}
}

// utf8RuneCount counts UTF-8 runes without importing unicode/utf8 in the call
// path — slightly cheaper than utf8.RuneCountInString for the short-string
// case that's the common path here.
func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// truncateToRunes returns at most n runes of s. Safe for multi-byte UTF-8.
func truncateToRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// materializeInlineImageBlocks rewrites inline base64 image blocks into
// file_ref blocks backed by temp files so the model can keep both vision
// access and a stable tool-usable file handle.
func materializeInlineImageBlocks(shannonDir string, blocks []RequestContentBlock) ([]RequestContentBlock, func()) {
	if len(blocks) == 0 {
		return blocks, nil
	}

	out := make([]RequestContentBlock, 0, len(blocks))
	var dir string
	var cleanup func()
	materializedAny := false
	nextIndex := 0

	for i, b := range blocks {
		if b.Type != "image" || b.Source == nil || b.Source.Data == "" {
			out = append(out, b)
			continue
		}
		if t := strings.TrimSpace(b.Source.Type); t != "" && t != "base64" {
			out = append(out, b)
			continue
		}

		ext := inlineImageExtension(b.Source.MediaType)
		if ext == "" {
			log.Printf("daemon: unsupported inline image media type %q for block %d", b.Source.MediaType, i)
			out = append(out, b)
			continue
		}

		// Pre-decode size guard: reject before base64 decoding allocates
		// memory proportional to the encoded length. Oversized blocks are
		// replaced with a text error rather than passed through — at this
		// size the downstream Anthropic API would refuse the request and
		// the user would see a generic 400 instead of a clear reason.
		if len(b.Source.Data) > maxInlineImageBase64Bytes {
			log.Printf("daemon: inline image block %d exceeds size guard (%d base64 bytes > %d)", i, len(b.Source.Data), maxInlineImageBase64Bytes)
			out = append(out, RequestContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[Inline image rejected: %d base64 bytes exceeds the %d-byte cap (≈%d MB raw). Re-upload as a smaller image or via file_ref.]",
					len(b.Source.Data), maxInlineImageBase64Bytes, maxInlineImageDecodedBytes/(1024*1024)),
			})
			continue
		}

		data, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil {
			log.Printf("daemon: failed to decode inline image block %d: %v", i, err)
			out = append(out, b)
			continue
		}

		if dir == "" {
			dir, cleanup, err = createAttachmentDir(shannonDir)
			if err != nil {
				log.Printf("daemon: failed to create attachment dir for inline images: %v", err)
				return blocks, nil
			}
		}

		// Normalize the caller-supplied filename through filepath.Base before
		// using it anywhere user-visible: the local path goes through
		// sanitizeFilename, but the Filename field is also echoed back in
		// the "[User attached image: <name> ...]" hint text the model sees.
		// Without normalization, a name like "../etc/passwd" would leak into
		// that hint and look alarming even though the actual disk path is
		// safe.
		displayName := filepath.Base(strings.TrimSpace(b.Filename))
		if displayName == "" || displayName == "." || displayName == ".." {
			displayName = fmt.Sprintf("attachment_%d%s", nextIndex, ext)
		} else if filepath.Ext(displayName) == "" {
			displayName += ext
		}
		localPath := filepath.Join(dir, sanitizeFilename(nextIndex, displayName))
		if err := os.WriteFile(localPath, data, 0o600); err != nil {
			log.Printf("daemon: failed to write inline image block %d: %v", i, err)
			out = append(out, b)
			continue
		}

		out = append(out, RequestContentBlock{
			Type:     "file_ref",
			FilePath: localPath,
			Filename: displayName,
			ByteSize: int64(len(data)),
		})
		materializedAny = true
		nextIndex++
	}

	if !materializedAny && cleanup != nil {
		cleanup()
		cleanup = nil
	}
	return out, cleanup
}

func inlineImageExtension(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func downloadOneFile(httpClient *http.Client, f RemoteFile, localPath, displayName string) (RequestContentBlock, error) {
	dlURL := slackDownloadURL(f.URL)

	if err := urlValidator(dlURL); err != nil {
		return RequestContentBlock{}, err
	}

	req, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		return RequestContentBlock{}, fmt.Errorf("bad URL: %w", err)
	}
	if f.AuthHeader != "" {
		req.Header.Set("Authorization", f.AuthHeader)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return RequestContentBlock{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return RequestContentBlock{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Detect HTML login pages returned by Slack/Feishu when auth fails.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		return RequestContentBlock{}, fmt.Errorf("got text/html response (auth may have failed)")
	}

	// Stream to disk with a size limit to avoid buffering large files in memory.
	out, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return RequestContentBlock{}, fmt.Errorf("create file: %w", err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxFileSize+1))
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(localPath)
		return RequestContentBlock{}, fmt.Errorf("write file: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(localPath)
		return RequestContentBlock{}, fmt.Errorf("close file: %w", closeErr)
	}
	if n > maxFileSize {
		os.Remove(localPath)
		return RequestContentBlock{}, fmt.Errorf("exceeds %d MB size limit", maxFileSize/(1024*1024))
	}

	return RequestContentBlock{
		Type:     "file_ref",
		FilePath: localPath,
		Filename: displayName,
		ByteSize: n,
	}, nil
}

// slackDownloadURL converts a Slack url_private to url_private_download format.
// Slack's url_private (files-pri/TEAM-FILE/name) requires browser cookies;
// url_private_download (files-pri/TEAM-FILE/download/name) accepts bot tokens
// via Authorization header. Non-Slack URLs are returned unchanged.
func slackDownloadURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	// Only rewrite Slack file URLs.
	if !strings.HasSuffix(u.Hostname(), "slack.com") {
		return rawURL
	}
	// Pattern: /files-pri/TEAM-FILE/filename
	// Target:  /files-pri/TEAM-FILE/download/filename
	const prefix = "/files-pri/"
	if !strings.HasPrefix(u.Path, prefix) {
		return rawURL
	}
	rest := u.Path[len(prefix):]
	slashPos := strings.Index(rest, "/")
	if slashPos < 0 {
		return rawURL
	}
	afterTeamFile := rest[slashPos+1:]
	if strings.HasPrefix(afterTeamFile, "download/") {
		return rawURL
	}
	u.Path = prefix + rest[:slashPos+1] + "download/" + afterTeamFile
	return u.String()
}

// validateDownloadURL rejects URLs that could cause SSRF — only HTTPS (and
// HTTP for local dev) to non-loopback, non-link-local hosts are permitted.
func validateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	host := u.Hostname()
	// Block literal IPs in private/loopback/link-local ranges.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("download from private/loopback IP is not allowed")
		}
	}
	// Block well-known loopback/metadata hostnames that bypass the IP check.
	lowHost := strings.ToLower(host)
	blocked := []string{"localhost", "metadata.google.internal", "metadata.google.com"}
	for _, b := range blocked {
		if lowHost == b {
			return fmt.Errorf("download from %s is not allowed", b)
		}
	}
	// Also resolve the hostname and check if it points to a blocked IP.
	if net.ParseIP(host) == nil && host != "" {
		addrs, err := net.LookupHost(host)
		if err == nil {
			for _, addr := range addrs {
				if ip := net.ParseIP(addr); ip != nil {
					if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
						return fmt.Errorf("download from %s (%s) is not allowed", host, addr)
					}
				}
			}
		}
	}
	return nil
}

// sanitizeFilename returns a safe filename with an index prefix to prevent collisions.
func sanitizeFilename(index int, name string) string {
	base := filepath.Base(name)
	if base == "" || base == "." || base == ".." {
		base = "file"
	}
	return fmt.Sprintf("%d_%s", index, base)
}

// sanitizeError strips URLs from error messages to prevent leaking auth
// tokens that may be embedded in query parameters (e.g. Slack CDN URLs).
func sanitizeError(err error) string {
	s := err.Error()
	// Go's http.Client errors typically include the full URL in quotes.
	// Replace any "https://..." or "http://..." substring to avoid token leaks.
	for _, scheme := range []string{"https://", "http://"} {
		for {
			idx := strings.Index(s, scheme)
			if idx < 0 {
				break
			}
			end := idx + len(scheme)
			for end < len(s) && s[end] != ' ' && s[end] != '"' && s[end] != '\'' {
				end++
			}
			s = s[:idx] + "<redacted-url>" + s[end:]
		}
	}
	return s
}
