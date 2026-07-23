package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

type HTTPTool struct{}

type httpArgs struct {
	URL          string            `json:"url"`
	Description  string            `json:"description,omitempty"`
	Method       string            `json:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	BodyFromFile string            `json:"body_from_file,omitempty"`
	Timeout      int               `json:"timeout,omitempty"`
}

func (t *HTTPTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "http",
		Description: "Make an HTTP request. Returns status code, response headers, and body (truncated to 10KB). Use body_from_file to send a file's raw bytes as the request body without inlining/escaping (good for PUT/POST of large structured payloads)." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":            map[string]any{"type": "string", "description": "Request URL"},
				"description":    agent.DescriptionFieldSpec,
				"method":         map[string]any{"type": "string", "description": "HTTP method (default: GET)"},
				"headers":        map[string]any{"type": "object", "description": "Request headers as key-value pairs"},
				"body":           map[string]any{"type": "string", "description": "Request body (mutually exclusive with body_from_file)"},
				"body_from_file": map[string]any{"type": "string", "description": "Read request body from this file path (raw bytes, no escaping). Mutually exclusive with body. Useful for PUT/POST of file contents without JSON-string escaping. Note: 307/308 redirects on POST/PUT may not preserve the body — use a direct URL when possible."},
				"timeout":        map[string]any{"type": "integer", "description": "Timeout in seconds (default: 30)"},
			},
		},
		Required: []string{"url", "description"},
	}
}

func (t *HTTPTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args httpArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.URL) == "" {
		return agent.ValidationError("http: missing required `url` parameter"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("http: missing required `description` parameter"), nil
	}

	method := args.Method
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(method)

	timeout := 30 * time.Second
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if args.Body != "" && args.BodyFromFile != "" {
		return agent.ValidationError("body and body_from_file are mutually exclusive — use one or the other"), nil
	}

	var bodyReader io.Reader
	// fileBodySize is the explicit Content-Length for body_from_file requests.
	// stdlib auto-detects length for *strings.Reader/*bytes.Reader/*bytes.Buffer
	// but not for *os.File — without this, file bodies fall back to chunked
	// transfer encoding, which some strict HTTP/1.0 proxies reject.
	fileBodySize := int64(-1)
	if args.Body != "" {
		bodyReader = strings.NewReader(args.Body)
	} else if args.BodyFromFile != "" {
		resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.BodyFromFile)
		if resolveErr != nil {
			if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
				return agent.ValidationError(
					"http: body_from_file requires an absolute path when no session working directory is set",
				), nil
			}
			return agent.ValidationError(fmt.Sprintf("http: %v", resolveErr)), nil
		}
		f, openErr := os.Open(resolved)
		if openErr != nil {
			if os.IsPermission(openErr) {
				return agent.PermissionError(fmt.Sprintf("cannot read %s: permission denied", resolved)), nil
			}
			if os.IsNotExist(openErr) {
				return agent.ValidationError(fmt.Sprintf("body_from_file %q does not exist", resolved)), nil
			}
			return agent.ToolResult{Content: fmt.Sprintf("error opening body_from_file: %v", openErr), IsError: true}, nil
		}
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return agent.ToolResult{Content: fmt.Sprintf("error stating body_from_file: %v", statErr), IsError: true}, nil
		}
		if info.IsDir() {
			return agent.ValidationError(fmt.Sprintf("body_from_file %q is a directory", resolved)), nil
		}
		bodyReader = f
		fileBodySize = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, method, args.URL, bodyReader)
	if err != nil {
		return agent.ValidationError(fmt.Sprintf("error creating request: %v", err)), nil
	}
	if fileBodySize >= 0 {
		req.ContentLength = fileBodySize
	}

	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return agent.TransientError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10240))
	if err != nil {
		return agent.TransientError(fmt.Sprintf("error reading response body: %v", err)), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Status: %d %s\n\nHeaders:\n", resp.StatusCode, resp.Status)
	for k, vals := range resp.Header {
		for _, v := range vals {
			fmt.Fprintf(&sb, "  %s: %s\n", k, v)
		}
	}
	fmt.Fprintf(&sb, "\nBody:\n%s", string(body))

	// Method-aware IsError. Status is always visible in the "Status: ..." line
	// of the body, so the model can branch on any status regardless of IsError.
	//   5xx                      → always IsError (server failure)
	//   4xx on mutations
	//   (POST/PUT/PATCH/DELETE)  → IsError (validation / auth / routing bug)
	//   4xx on reads
	//   (GET/HEAD/OPTIONS/other) → IsError EXCEPT 404 and 410, which are
	//                              polling-friendly ("not yet" / "gone").
	//                              401/403/429 and other 4xx stay IsError:
	//                              auth failures and rate-limits are real
	//                              operational errors that the SameToolError
	//                              loop detector should see.
	isError := false
	switch {
	case resp.StatusCode >= 500:
		isError = true
	case resp.StatusCode >= 400:
		switch method {
		case "POST", "PUT", "PATCH", "DELETE":
			isError = true
		default:
			// Reads: only 404/410 are polling-exempt.
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusGone {
				isError = true
			}
		}
	}
	return agent.ToolResult{Content: sb.String(), IsError: isError}, nil
}

func (t *HTTPTool) RequiresApproval() bool { return true }

func (t *HTTPTool) IsReadOnlyCall(string) bool { return false }

func (t *HTTPTool) IsSafeArgs(argsJSON string) bool {
	var args httpArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}

	method := strings.ToUpper(args.Method)
	if method == "" {
		method = "GET"
	}
	if method != "GET" {
		return false
	}

	// A GET that carries a body (inline or from file) is not a plain read —
	// it can exfiltrate data even to localhost. Require explicit approval.
	if args.Body != "" || args.BodyFromFile != "" {
		return false
	}

	parsed, err := url.Parse(args.URL)
	if err != nil {
		return false
	}

	host := parsed.Hostname()
	return host == "localhost" || host == "127.0.0.1"
}
