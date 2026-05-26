package daemon

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// requireConfirm returns true if the ?confirm=true query parameter is missing.
func requireConfirm(confirmParam string) bool {
	return confirmParam != "true"
}

// protectedFields maps a top-level config key to a human-readable reason.
var protectedFields = map[string]string{
	"endpoint": "changes API connection target",
	"api_key":  "changes authentication credentials",
}

// protectedNestedFields maps [parent, child] to a reason.
var protectedNestedFields = map[[2]string]string{
	{"permissions", "denied_commands"}: "removes security restrictions",
}

// checkProtectedFields inspects a config patch for protected fields.
// Returns (reason, true) if a protected field is being modified.
func checkProtectedFields(patch map[string]interface{}) (string, bool) {
	for key, reason := range protectedFields {
		if _, ok := patch[key]; ok {
			return reason, true
		}
	}
	for pair, reason := range protectedNestedFields {
		parent, child := pair[0], pair[1]
		parentVal, ok := patch[parent]
		if !ok {
			continue
		}
		parentMap, ok := parentVal.(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := parentMap[child]; ok {
			return reason, true
		}
	}
	return "", false
}

// safeCommands is the allowlist of known-safe MCP server commands.
var safeCommands = map[string]bool{
	"node": true, "npx": true, "python": true, "python3": true,
	"uvx": true, "uv": true, "go": true, "deno": true, "bun": true,
	"docker": true, "pip": true, "pipx": true,
}

// interpreterEvalFlags lists flags that cause interpreters to evaluate
// inline code from the next argument, enabling arbitrary code execution.
var interpreterEvalFlags = map[string]bool{
	"-c": true, "-e": true, "--eval": true, "--exec": true,
}

// shellBases identifies shells that can execute arbitrary commands via args.
var shellBases = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "csh": true, "tcsh": true,
}

// wrapperCommands are programs that delegate execution to an arbitrary child
// command, making them equivalent to running that child directly.
var wrapperCommands = map[string]bool{
	"env": true, "nohup": true, "sudo": true, "su": true, "xargs": true,
}

// shellMetachars matches characters that indicate shell injection.
var shellMetachars = regexp.MustCompile(`[;|&><$()\x60]`) // backtick = \x60

// validateMCPCommands checks that MCP server commands are safe.
// confirmed=true relaxes the unknown-command check but NOT the metachar check
// or the interpreter eval-flag check.
func validateMCPCommands(servers map[string]interface{}, confirmed bool) error {
	for name, srvRaw := range servers {
		srvMap, ok := srvRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// Skip non-stdio servers
		if t, ok := srvMap["type"].(string); ok && t != "stdio" && t != "" {
			continue
		}
		cmdRaw, ok := srvMap["command"]
		if !ok {
			continue
		}
		cmd, ok := cmdRaw.(string)
		if !ok {
			continue
		}

		// Always block shell metacharacters in command
		if shellMetachars.MatchString(cmd) {
			return fmt.Errorf("mcp_servers.%s: command %q contains shell metacharacters", name, cmd)
		}

		// Resolve base name for both relative and absolute paths
		base := filepath.Base(cmd)

		// Always block shells — they execute arbitrary commands via args
		if shellBases[base] {
			return fmt.Errorf("mcp_servers.%s: shell %q cannot be used as MCP command — use the actual server binary", name, cmd)
		}

		// Block wrapper commands (env, nohup, sudo) that delegate to arbitrary children
		if wrapperCommands[base] {
			return fmt.Errorf("mcp_servers.%s: wrapper command %q cannot be used as MCP command — use the actual server binary", name, cmd)
		}

		// Check args for shells and interpreter eval flags
		if args := extractArgs(srvMap); len(args) > 0 {
			for _, arg := range args {
				// Block shell names appearing anywhere in args (e.g. python bash -c ...)
				argBase := filepath.Base(arg)
				if shellBases[argBase] {
					return fmt.Errorf("mcp_servers.%s: shell %q in args is not allowed", name, arg)
				}
				// Block eval flags (e.g. python -c "...")
				if interpreterEvalFlags[arg] {
					return fmt.Errorf("mcp_servers.%s: eval flag %q with command %q enables arbitrary code execution — use a script file instead", name, arg, cmd)
				}
			}
		}

		// Allow known-safe commands and absolute paths
		if filepath.IsAbs(cmd) || safeCommands[base] {
			continue
		}

		// Unknown command — block unless confirmed
		if !confirmed {
			return fmt.Errorf("mcp_servers.%s: unknown command %q — add X-Confirm header to proceed", name, cmd)
		}
	}
	return nil
}

// validateBuiltinMCPPatch refuses PATCH /config attempts that try to mutate
// daemon-owned fields on a built-in MCP server. Users may still toggle
// disabled, set env, or flip keep_alive — those are persisted to yaml and
// round-tripped on each Load. Returns (field, message, true) when blocked.
func validateBuiltinMCPPatch(servers map[string]interface{}) (string, string, bool) {
	if len(servers) == 0 {
		return "", "", false
	}
	immutable := make(map[string]struct{}, len(mcp.BuiltinImmutableFields))
	for _, f := range mcp.BuiltinImmutableFields {
		immutable[f] = struct{}{}
	}
	for name, srvRaw := range servers {
		if !mcp.IsBuiltin(name) {
			continue
		}
		srvMap, ok := srvRaw.(map[string]interface{})
		if !ok {
			continue
		}
		for key := range srvMap {
			if _, hit := immutable[strings.ToLower(key)]; hit {
				return key, fmt.Sprintf(
					"mcp_servers.%s.%s is owned by the built-in catalog and cannot be edited — toggle disabled / env / keep_alive instead",
					name, key,
				), true
			}
		}
	}
	return "", "", false
}

// extractArgs reads the "args" field from an MCP server config map.
func extractArgs(srvMap map[string]interface{}) []string {
	argsRaw, ok := srvMap["args"]
	if !ok {
		return nil
	}
	switch v := argsRaw.(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, a := range v {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}
