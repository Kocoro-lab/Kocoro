package daemon

import (
	"strings"
	"testing"
)

// --- requireConfirm ---

func TestRequireConfirm(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"", true},
		{"false", true},
		{"1", true},
		{"yes", true},
		{"True", true},
		{"true", false},
	}
	for _, c := range cases {
		got := requireConfirm(c.input)
		if got != c.want {
			t.Errorf("requireConfirm(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// --- checkProtectedFields ---

func TestCheckProtectedFields_Safe(t *testing.T) {
	patch := map[string]interface{}{
		"model":   "claude-3-7",
		"timeout": 30,
	}
	reason, isProtected := checkProtectedFields(patch)
	if isProtected {
		t.Errorf("expected no protected field, got reason=%q", reason)
	}
}

func TestCheckProtectedFields_Endpoint(t *testing.T) {
	patch := map[string]interface{}{
		"endpoint": "https://evil.example.com",
	}
	reason, isProtected := checkProtectedFields(patch)
	if !isProtected {
		t.Fatal("expected isProtected=true for endpoint")
	}
	if reason != "changes API connection target" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestCheckProtectedFields_APIKey(t *testing.T) {
	patch := map[string]interface{}{
		"api_key": "sk-leaked",
	}
	reason, isProtected := checkProtectedFields(patch)
	if !isProtected {
		t.Fatal("expected isProtected=true for api_key")
	}
	if reason != "changes authentication credentials" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestCheckProtectedFields_PermissionsDeniedCommands(t *testing.T) {
	patch := map[string]interface{}{
		"permissions": map[string]interface{}{
			"denied_commands": []string{"rm"},
		},
	}
	reason, isProtected := checkProtectedFields(patch)
	if !isProtected {
		t.Fatal("expected isProtected=true for permissions.denied_commands")
	}
	if reason != "removes security restrictions" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestCheckProtectedFields_DaemonAutoApprove(t *testing.T) {
	patch := map[string]interface{}{
		"daemon": map[string]interface{}{
			"auto_approve": true,
		},
	}
	if _, isProtected := checkProtectedFields(patch); isProtected {
		t.Fatal("daemon.auto_approve should be settable via API")
	}
}

func TestCheckProtectedFields_NestedParentNotMap(t *testing.T) {
	// If the parent key exists but isn't a map, it shouldn't panic or false-positive
	patch := map[string]interface{}{
		"permissions": "some string value",
	}
	_, isProtected := checkProtectedFields(patch)
	if isProtected {
		t.Error("expected isProtected=false when parent value is not a map")
	}
}

// --- validateMCPCommands ---

func TestValidateMCPCommands_SafeNpx(t *testing.T) {
	servers := map[string]interface{}{
		"myserver": map[string]interface{}{
			"command": "npx",
			"args":    []string{"-y", "some-mcp-server"},
		},
	}
	if err := validateMCPCommands(servers, false); err != nil {
		t.Errorf("expected nil for safe npx, got: %v", err)
	}
}

func TestValidateMCPCommands_AbsolutePath(t *testing.T) {
	servers := map[string]interface{}{
		"myserver": map[string]interface{}{
			"command": "/usr/local/bin/mcp-server",
		},
	}
	if err := validateMCPCommands(servers, false); err != nil {
		t.Errorf("expected nil for absolute path, got: %v", err)
	}
}

func TestValidateMCPCommands_HTTPTypeSkipped(t *testing.T) {
	servers := map[string]interface{}{
		"remote": map[string]interface{}{
			"type":    "http",
			"command": "rm; evil",
		},
	}
	if err := validateMCPCommands(servers, false); err != nil {
		t.Errorf("expected nil for non-stdio type, got: %v", err)
	}
}

func TestValidateMCPCommands_ShellMetachar_Semicolon(t *testing.T) {
	servers := map[string]interface{}{
		"evil": map[string]interface{}{
			"command": "node; rm -rf /",
		},
	}
	if err := validateMCPCommands(servers, false); err == nil {
		t.Error("expected error for shell metachar (semicolon), got nil")
	}
}

func TestValidateMCPCommands_ShellMetachar_Pipe(t *testing.T) {
	servers := map[string]interface{}{
		"evil": map[string]interface{}{
			"command": "node|cat",
		},
	}
	if err := validateMCPCommands(servers, false); err == nil {
		t.Error("expected error for shell metachar (pipe), got nil")
	}
}

func TestValidateMCPCommands_UnknownCommand_NoConfirm(t *testing.T) {
	servers := map[string]interface{}{
		"custom": map[string]interface{}{
			"command": "my-custom-mcp-server",
		},
	}
	if err := validateMCPCommands(servers, false); err == nil {
		t.Error("expected error for unknown command without confirm, got nil")
	}
}

func TestValidateMCPCommands_UnknownCommand_WithConfirm(t *testing.T) {
	servers := map[string]interface{}{
		"custom": map[string]interface{}{
			"command": "my-custom-mcp-server",
		},
	}
	if err := validateMCPCommands(servers, true); err != nil {
		t.Errorf("expected nil for unknown command with confirm, got: %v", err)
	}
}

func TestValidateMCPCommands_MetacharAlwaysBlocked_EvenWithConfirm(t *testing.T) {
	servers := map[string]interface{}{
		"evil": map[string]interface{}{
			"command": "node$(evil)",
		},
	}
	if err := validateMCPCommands(servers, true); err == nil {
		t.Error("expected error for metachar even with confirm=true, got nil")
	}
}

func TestValidateMCPCommands_NoCommand(t *testing.T) {
	// Servers without "command" field should be skipped
	servers := map[string]interface{}{
		"nocommand": map[string]interface{}{
			"url": "http://localhost:3000",
		},
	}
	if err := validateMCPCommands(servers, false); err != nil {
		t.Errorf("expected nil for server without command, got: %v", err)
	}
}

func TestValidateMCPCommands_AllSafeCommands(t *testing.T) {
	safe := []string{"node", "npx", "python", "python3", "uvx", "uv", "go", "deno", "bun", "docker", "pip", "pipx"}
	for _, cmd := range safe {
		servers := map[string]interface{}{
			"s": map[string]interface{}{"command": cmd},
		}
		if err := validateMCPCommands(servers, false); err != nil {
			t.Errorf("expected nil for safe command %q, got: %v", cmd, err)
		}
	}
}

func TestValidateMCPCommands_ShellBlocked(t *testing.T) {
	shells := []string{"sh", "bash", "zsh", "fish", "/bin/sh", "/bin/bash", "/usr/bin/zsh"}
	for _, cmd := range shells {
		servers := map[string]interface{}{
			"s": map[string]interface{}{"command": cmd},
		}
		if err := validateMCPCommands(servers, true); err == nil {
			t.Errorf("expected error for shell %q even with confirm, got nil", cmd)
		}
	}
}

func TestValidateMCPCommands_EvalFlagBlocked(t *testing.T) {
	cases := []struct {
		cmd  string
		args []interface{}
	}{
		{"python", []interface{}{"-c", "print('hi')"}},
		{"node", []interface{}{"--eval", "console.log('hi')"}},
		{"python3", []interface{}{"-e", "print('hi')"}},
	}
	for _, c := range cases {
		servers := map[string]interface{}{
			"s": map[string]interface{}{"command": c.cmd, "args": c.args},
		}
		if err := validateMCPCommands(servers, true); err == nil {
			t.Errorf("expected error for %q with eval args %v even with confirm, got nil", c.cmd, c.args)
		}
	}
}

func TestValidateMCPCommands_SafeCommandWithNormalArgs(t *testing.T) {
	servers := map[string]interface{}{
		"s": map[string]interface{}{
			"command": "python",
			"args":    []interface{}{"-m", "my_mcp_server", "--port", "3000"},
		},
	}
	if err := validateMCPCommands(servers, false); err != nil {
		t.Errorf("expected nil for python -m (no eval flag), got: %v", err)
	}
}

func TestCheckProtectedFields_AliasNormalized(t *testing.T) {
	// Verify that after normalizePatchKeys, aliases are caught
	patch := map[string]interface{}{
		"apiKey": "sk-test",
	}
	normalizePatchKeys(patch)
	reason, isProtected := checkProtectedFields(patch)
	if !isProtected {
		t.Fatal("expected isProtected=true for aliased apiKey after normalization")
	}
	if reason != "changes authentication credentials" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestValidateMCPCommands_WrapperBlocked(t *testing.T) {
	wrappers := []string{"env", "nohup", "sudo", "/usr/bin/env", "/usr/bin/sudo"}
	for _, cmd := range wrappers {
		servers := map[string]interface{}{
			"s": map[string]interface{}{"command": cmd, "args": []interface{}{"node", "server.js"}},
		}
		if err := validateMCPCommands(servers, true); err == nil {
			t.Errorf("expected error for wrapper %q even with confirm, got nil", cmd)
		}
	}
}

func TestValidateMCPCommands_ShellInArgsBlocked(t *testing.T) {
	servers := map[string]interface{}{
		"s": map[string]interface{}{
			"command": "python",
			"args":    []interface{}{"bash", "-lc", "echo hi"},
		},
	}
	if err := validateMCPCommands(servers, true); err == nil {
		t.Error("expected error for shell in args, got nil")
	}
}

func TestCheckProtectedFields_MCPServersAliasNormalized(t *testing.T) {
	patch := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"test": map[string]interface{}{
				"command": "node",
			},
		},
	}
	normalizePatchKeys(patch)
	if _, ok := patch["mcp_servers"]; !ok {
		t.Fatal("expected mcp_servers key after normalization")
	}
	if _, ok := patch["mcpServers"]; ok {
		t.Fatal("expected mcpServers alias to be removed after normalization")
	}
}

// --- validateBuiltinMCPPatch ---

func TestValidateBuiltinMCPPatch_AllowsTogglingDisabled(t *testing.T) {
	servers := map[string]interface{}{
		"intercom": map[string]interface{}{
			"disabled": false,
		},
	}
	if field, _, blocked := validateBuiltinMCPPatch(servers); blocked {
		t.Errorf("toggling disabled should be allowed; blocked on field %q", field)
	}
}

func TestValidateBuiltinMCPPatch_AllowsEnvAndKeepAlive(t *testing.T) {
	servers := map[string]interface{}{
		"intercom": map[string]interface{}{
			"env":        map[string]interface{}{"DEBUG": "1"},
			"keep_alive": true,
		},
	}
	if field, _, blocked := validateBuiltinMCPPatch(servers); blocked {
		t.Errorf("env / keep_alive should be allowed; blocked on field %q", field)
	}
}

func TestValidateBuiltinMCPPatch_BlocksCommandOnBuiltin(t *testing.T) {
	servers := map[string]interface{}{
		"intercom": map[string]interface{}{
			"command": "rm",
		},
	}
	field, msg, blocked := validateBuiltinMCPPatch(servers)
	if !blocked {
		t.Fatal("expected block when patching command on built-in")
	}
	if field != "command" {
		t.Errorf("expected field=command, got %q", field)
	}
	if msg == "" || !strings.Contains(msg, "intercom") || !strings.Contains(msg, "command") {
		t.Errorf("expected message naming server+field, got %q", msg)
	}
}

func TestValidateBuiltinMCPPatch_BlocksCaseInsensitiveImmutableField(t *testing.T) {
	servers := map[string]interface{}{
		"intercom": map[string]interface{}{
			"Args": []interface{}{"foo"},
		},
	}
	field, _, blocked := validateBuiltinMCPPatch(servers)
	if !blocked {
		t.Fatal("expected block on Args (case-insensitive match)")
	}
	if field != "Args" {
		t.Errorf("expected field=Args (echo back original casing), got %q", field)
	}
}

func TestValidateBuiltinMCPPatch_IgnoresNonBuiltinServer(t *testing.T) {
	servers := map[string]interface{}{
		"my-custom": map[string]interface{}{
			"command": "node",
			"args":    []interface{}{"server.js"},
		},
	}
	if field, _, blocked := validateBuiltinMCPPatch(servers); blocked {
		t.Errorf("non-builtin server should pass through; blocked on %q", field)
	}
}

