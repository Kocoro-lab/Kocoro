package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxFileSize     = 100 * 1024 * 1024 // 100 MB per file
	downloadTimeout = 2 * time.Minute
)

// downloadRemoteFiles downloads remote file attachments to a local temp
// directory and returns file_ref RequestContentBlocks. Download failures
// produce text error blocks rather than failing the entire message.
func downloadRemoteFiles(shannonDir string, files []RemoteFile) []RequestContentBlock {
	if len(files) == 0 {
		return nil
	}

	// Generate a random nonce for the download directory (session ID is
	// not yet available at this point in RunAgent).
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		log.Printf("daemon: failed to generate attachment nonce: %v", err)
		return nil
	}
	dir := filepath.Join(shannonDir, "tmp", "attachments", hex.EncodeToString(nonce[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("daemon: failed to create attachment dir %s: %v", dir, err)
		return nil
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
			// Carry Authorization from the original request.
			if auth := via[0].Header.Get("Authorization"); auth != "" {
				req.Header.Set("Authorization", auth)
			}
			return nil
		},
	}
	blocks := make([]RequestContentBlock, 0, len(files))

	for i, f := range files {
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
			log.Printf("daemon: download %s failed: %v", f.Name, err)
			blocks = append(blocks, RequestContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to download file %s: %v]", f.Name, err),
			})
			continue
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func downloadOneFile(httpClient *http.Client, f RemoteFile, localPath, displayName string) (RequestContentBlock, error) {
	dlURL := slackDownloadURL(f.URL)
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

	// Limit read to maxFileSize+1 to detect oversized files.
	lr := io.LimitReader(resp.Body, maxFileSize+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return RequestContentBlock{}, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > maxFileSize {
		return RequestContentBlock{}, fmt.Errorf("exceeds %d MB size limit", maxFileSize/(1024*1024))
	}

	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		return RequestContentBlock{}, fmt.Errorf("write file: %w", err)
	}

	return RequestContentBlock{
		Type:     "file_ref",
		FilePath: localPath,
		Filename: displayName,
		ByteSize: int64(len(data)),
	}, nil
}

// slackDownloadURL converts a Slack url_private to url_private_download format.
// Slack's url_private (files-pri/TEAM-FILE/name) requires browser cookies;
// url_private_download (files-pri/TEAM-FILE/download/name) accepts bot tokens
// via Authorization header. Non-Slack URLs are returned unchanged.
func slackDownloadURL(rawURL string) string {
	// Pattern: https://files.slack.com/files-pri/TEAM-FILE/filename
	// Target:  https://files.slack.com/files-pri/TEAM-FILE/download/filename
	const marker = "/files-pri/"
	idx := strings.Index(rawURL, marker)
	if idx < 0 {
		return rawURL
	}
	// Find the last slash after /files-pri/ — that's where filename starts.
	rest := rawURL[idx+len(marker):]
	slashPos := strings.Index(rest, "/")
	if slashPos < 0 {
		return rawURL
	}
	// Check if /download/ is already present.
	afterTeamFile := rest[slashPos+1:]
	if strings.HasPrefix(afterTeamFile, "download/") {
		return rawURL
	}
	// Insert /download/ before the filename.
	insertAt := idx + len(marker) + slashPos + 1
	return rawURL[:insertAt] + "download/" + rawURL[insertAt:]
}

// sanitizeFilename returns a safe filename with an index prefix to prevent collisions.
func sanitizeFilename(index int, name string) string {
	// Strip directory components and handle edge cases.
	base := filepath.Base(name)
	base = strings.ReplaceAll(base, string(filepath.Separator), "_")
	if base == "" || base == "." || base == ".." {
		base = "file"
	}
	return fmt.Sprintf("%d_%s", index, base)
}
