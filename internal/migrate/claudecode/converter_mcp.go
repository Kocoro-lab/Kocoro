package claudecode

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// MergeMCPIntoConfig reads target/config.yaml, merges in the imported MCP
// servers (with `disabled: true` for any server in the disabled map), and
// atomically rewrites the file. Unrelated config keys are preserved.
//
// Env values are NEVER carried — the scanner already discarded them. Each
// imported env key is written with an empty string value so the user knows
// what to fill in via Settings → MCP. Error-status servers (e.g. unsupported
// transport) are skipped entirely.
//
// Atomicity: build the merged document in memory, marshal, validate parse-back,
// write to a tmp file, then rename(2) into place.
func MergeMCPIntoConfig(target string, servers []ScannedMCPServer, disabled map[string]bool, importedAt string) error {
	cfgPath := filepath.Join(target, "config.yaml")
	var root map[string]any
	if data, err := os.ReadFile(cfgPath); err == nil {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse existing config.yaml: %w", err)
		}
	}
	if root == nil {
		root = map[string]any{}
	}
	mcp, _ := root["mcp_servers"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}

	for _, s := range servers {
		if s.Status != "ok" {
			continue
		}
		if _, exists := mcp[s.Name]; exists {
			continue
		}
		entry := map[string]any{}
		if s.Command != "" {
			entry["command"] = s.Command
		}
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if s.Transport != "" && s.Transport != "stdio" {
			entry["type"] = s.Transport
		}
		if s.URL != "" {
			entry["url"] = s.URL
		}
		if len(s.EnvKeys) > 0 {
			env := map[string]string{}
			for _, k := range s.EnvKeys {
				env[k] = "" // user must re-enter via Settings → MCP
			}
			entry["env"] = env
		}
		if s.Disabled || disabled[s.Name] {
			entry["disabled"] = true
		}
		mcp[s.Name] = entry
	}
	root["mcp_servers"] = mcp

	out, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("# imported mcp servers added on %s\n", importedAt)
	merged := append([]byte(header), out...)

	// Validate parse-back before commit.
	var check map[string]any
	if err := yaml.Unmarshal(merged, &check); err != nil {
		return fmt.Errorf("merged config failed parse-back: %w", err)
	}

	tmp := cfgPath + ".migrate.tmp"
	if err := os.WriteFile(tmp, merged, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}
