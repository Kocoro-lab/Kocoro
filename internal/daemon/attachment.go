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
)

const (
	maxFileSize     = 100 * 1024 * 1024 // 100 MB per file
	maxFiles        = 10                // max attachments per message
	downloadTimeout = 2 * time.Minute

	// maxInlineImageBase64Bytes caps the pre-decode size of inline image
	// blocks so a hostile or buggy caller cannot force a multi-hundred-MB
	// base64 allocation before the downstream 20 MB decoded cap in
	// resolveFileRef fires. Uses the worst-case 4/3 base64 inflation ratio
	// plus a few bytes of padding slack.
	maxInlineImageDecodedBytes   = 20 * 1024 * 1024
	maxInlineImageBase64Bytes    = maxInlineImageDecodedBytes*4/3 + 4
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

// downloadRemoteFiles downloads remote file attachments to a local temp
// directory and returns file_ref RequestContentBlocks plus a cleanup function
// that removes the attachment directory. The caller must register the cleanup
// (e.g. via OnSessionClose). Download failures produce text error blocks
// rather than failing the entire message.
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

	dir, cleanup, err := createAttachmentDir(shannonDir)
	if err != nil {
		log.Printf("daemon: failed to create attachment dir: %v", err)
		return nil, func() {}
	}

	// Custom client that preserves Authorization header across redirects.
	// Slack may redirect file URLs to CDN; Go's default policy strips
	// the header on cross-domain redirects.
	client := &http.Client{
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
		diskName := sanitizeFilename(i, f.Name)
		localPath := filepath.Join(dir, diskName)
		// Use the original filename for display (what the LLM sees in hints).
		// Fall back to the sanitized disk name for empty/degenerate inputs.
		displayName := f.Name
		if displayName == "" || displayName == "." || displayName == ".." {
			displayName = diskName
		}

		block, err := downloadOneFile(client, f, localPath, displayName)
		if err != nil {
			log.Printf("daemon: download %s failed: %v", f.Name, sanitizeError(err))
			blocks = append(blocks, RequestContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to download file %s]", f.Name),
			})
			continue
		}
		blocks = append(blocks, block)
	}
	blocks = append(blocks, capBlocks...)
	return blocks, cleanup
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
		// memory proportional to the encoded length. The decoded block
		// still gets the stricter 20 MB cap in resolveFileRef; this guard
		// just protects the decode step itself.
		if len(b.Source.Data) > maxInlineImageBase64Bytes {
			log.Printf("daemon: inline image block %d exceeds size guard (%d base64 bytes > %d)", i, len(b.Source.Data), maxInlineImageBase64Bytes)
			out = append(out, b)
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
