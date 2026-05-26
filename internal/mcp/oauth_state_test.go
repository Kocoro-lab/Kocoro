package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCPRemoteHasToken_NoCacheDir(t *testing.T) {
	tmp := t.TempDir()
	if MCPRemoteHasToken(tmp, "https://mcp.intercom.com/mcp") {
		t.Error("expected false when ~/.mcp-auth does not exist")
	}
}

func TestMCPRemoteHasToken_EmptyArgs(t *testing.T) {
	if MCPRemoteHasToken("", "https://mcp.intercom.com/mcp") {
		t.Error("empty home dir must short-circuit to false")
	}
	if MCPRemoteHasToken(t.TempDir(), "") {
		t.Error("empty serverURL must short-circuit to false")
	}
}

func TestMCPRemoteHasToken_TokenAndClientInfoBothExist(t *testing.T) {
	tmp := t.TempDir()
	// Mirror mcp-remote's md5 hash for the known URL, observed empirically
	// at chunk-65X3S4HB.js getServerUrlHash: md5("https://mcp.intercom.com/mcp")
	// → 2b6e236cccc0fbb64ea0556af272963f.
	hash := "2b6e236cccc0fbb64ea0556af272963f"
	cacheDir := filepath.Join(tmp, ".mcp-auth", "mcp-remote-0.1.37")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, hash+"_tokens.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write tokens.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, hash+"_client_info.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write client_info.json: %v", err)
	}

	if !MCPRemoteHasToken(tmp, IntercomMCPRemoteURL) {
		t.Error("expected true when both tokens.json and client_info.json exist for matching URL hash")
	}
}

func TestMCPRemoteHasToken_TokenWithoutClientInfo(t *testing.T) {
	// tokens.json alone is from an interrupted registration. mcp-remote
	// will re-run OAuth in that case, so we must report unauthorized.
	tmp := t.TempDir()
	hash := "2b6e236cccc0fbb64ea0556af272963f"
	cacheDir := filepath.Join(tmp, ".mcp-auth", "mcp-remote-0.1.37")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, hash+"_tokens.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write tokens.json: %v", err)
	}
	if MCPRemoteHasToken(tmp, IntercomMCPRemoteURL) {
		t.Error("expected false when client_info.json is missing")
	}
}

func TestMCPRemoteHasToken_DifferentURLHashMisses(t *testing.T) {
	// A token for a different server URL must not return true.
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, ".mcp-auth", "mcp-remote-0.1.37")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Hash of some other server URL — guaranteed not to collide with Intercom.
	otherHash := "deadbeef0000000000000000deadbeef"
	if err := os.WriteFile(filepath.Join(cacheDir, otherHash+"_tokens.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, otherHash+"_client_info.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if MCPRemoteHasToken(tmp, IntercomMCPRemoteURL) {
		t.Error("expected false when only a different server's token exists")
	}
}

func TestMCPRemoteHasToken_CrossesVersionDirs(t *testing.T) {
	// User upgraded mcp-remote — the cache now lives under a different
	// version-suffixed dir. Glob across mcp-remote-* so the answer doesn't
	// become "unauthorized" simply because the package bumped.
	tmp := t.TempDir()
	hash := "2b6e236cccc0fbb64ea0556af272963f"
	cacheDir := filepath.Join(tmp, ".mcp-auth", "mcp-remote-0.2.99")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, hash+"_tokens.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, hash+"_client_info.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !MCPRemoteHasToken(tmp, IntercomMCPRemoteURL) {
		t.Error("expected true across version-suffixed cache dirs")
	}
}
