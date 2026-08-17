package tools

import (
	"net/url"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func xURL(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host != "x.com" && host != "www.x.com" &&
		host != "mobile.x.com" && host != "twitter.com" &&
		host != "www.twitter.com" && host != "mobile.twitter.com" {
		return nil, false
	}
	return parsed, true
}

func isXComposerURL(raw string) bool {
	parsed, ok := xURL(raw)
	if !ok {
		return false
	}
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	switch path {
	case "/intent/tweet", "/compose/post", "/compose/tweet", "/i/flow/compose":
		return true
	default:
		return strings.HasPrefix(path, "/intent/tweet/") ||
			strings.HasPrefix(path, "/compose/post/") ||
			strings.HasPrefix(path, "/compose/tweet/") ||
			strings.HasPrefix(path, "/i/flow/compose/")
	}
}

func isXURL(raw string) bool {
	_, ok := xURL(raw)
	return ok
}

func xAutomationBlockedResult() agent.ToolResult {
	return agent.BusinessError(
		"X composer automation is disabled. Return the review link and let the user review the draft and click Post on X manually",
	)
}

func xPageMutationBlockedResult() agent.ToolResult {
	return agent.BusinessError(
		"Playwright mutation on X is disabled because X home and timeline pages contain an inline composer. Keep X automation read-only and let the user click Post manually",
	)
}

func xUnrestrictedPlaywrightCodeBlockedResult() agent.ToolResult {
	return agent.BusinessError(
		"browser_run_code and browser_evaluate are disabled on the canonical Playwright adapter because unrestricted page code can navigate to X and publish in one uninspectable call",
	)
}

func looksLikeXComposerControl(values ...string) bool {
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if looksLikeExplicitXComposerControl(normalized) {
			return true
		}
		for _, token := range []string{
			"post button",
		} {
			if strings.Contains(normalized, token) {
				return true
			}
		}
		if normalized == "post" || normalized == "tweet" ||
			normalized == "投稿" || normalized == "ポストする" ||
			normalized == "发布" || normalized == "發佈" {
			return true
		}
	}
	return false
}

func looksLikeExplicitXComposerControl(values ...string) bool {
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		for _, token := range []string{
			"tweetbutton", "tweettextarea", "sidenav_newtweet_button",
			"compose/post", "intent/tweet", "/i/flow/compose",
			"post tweet", "publish tweet", "send tweet",
		} {
			if strings.Contains(normalized, token) {
				return true
			}
		}
	}
	return false
}

func isBrowserBundleID(bundleID string) bool {
	switch strings.ToLower(strings.TrimSpace(bundleID)) {
	case "com.apple.safari", "com.google.chrome", "com.google.chrome.beta",
		"com.google.chrome.canary", "com.microsoft.edgemac", "com.brave.browser",
		"org.mozilla.firefox", "org.mozilla.firefoxdeveloperedition",
		"company.thebrowser.browser", "com.operasoftware.opera":
		return true
	default:
		return false
	}
}

func looksLikeXWindow(title string) bool {
	normalized := strings.ToLower(strings.TrimSpace(title))
	return normalized == "x" || strings.HasSuffix(normalized, " / x") ||
		strings.HasSuffix(normalized, " on x") || strings.Contains(normalized, "x.com") ||
		strings.Contains(normalized, "twitter.com")
}
