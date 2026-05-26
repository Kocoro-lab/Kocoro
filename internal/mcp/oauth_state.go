package mcp

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
)

// MCPRemoteHasToken reports whether mcp-remote has a cached OAuth token
// for serverURL. Used by Desktop UX to skip the "authorize browser will
// open" confirm modal when activating a built-in OAuth-bridged MCP server
// the user has already authorized in a previous session — without this
// check the user clicks "Authorize", no browser opens (mcp-remote silently
// reuses the cached token), and the UX looks broken.
//
// mcp-remote namespaces its cache as
//
//	~/.mcp-auth/mcp-remote-<version>/<md5(serverURL)>_tokens.json
//
// The hash algorithm is verified at HEAD of mcp-remote (chunk-65X3S4HB.js
// getServerUrlHash: md5 of serverURL joined with optional authorize-
// resource and headers by "|"). We hash only the URL because the built-in
// catalog never sets authorize-resource or headers for these servers.
//
// homeDir is normally os.UserHomeDir(); accepting it as an argument lets
// the test suite point at a fixture without env hacks.
func MCPRemoteHasToken(homeDir, serverURL string) bool {
	if homeDir == "" || serverURL == "" {
		return false
	}
	sum := md5.Sum([]byte(serverURL))
	hash := hex.EncodeToString(sum[:])

	// mcp-remote keys cache dirs by package version (mcp-remote-0.1.37,
	// mcp-remote-0.2.0, ...). Glob across versions so we don't need to
	// track the user's installed release.
	pattern := filepath.Join(homeDir, ".mcp-auth", "mcp-remote-*", hash+"_tokens.json")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return false
	}
	// Token file existing is necessary but not sufficient — mcp-remote also
	// writes a sibling client_info.json once registration completes. Treat
	// both being present as "authorized"; lone tokens.json (interrupted
	// registration) means OAuth would still pop on next activation.
	for _, tok := range matches {
		ci := tok[:len(tok)-len("_tokens.json")] + "_client_info.json"
		if _, err := os.Stat(ci); err == nil {
			return true
		}
	}
	return false
}

// IntercomMCPRemoteURL is the URL hardcoded into BuiltinMCPServers["intercom"]
// args — kept in this package so the authorized-state check can hash the
// exact same string that mcp-remote sees on the wire.
const IntercomMCPRemoteURL = "https://mcp.intercom.com/mcp"
