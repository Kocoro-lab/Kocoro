package tools

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

// TestSkillExemptInventory pins down which production tools opt into framework
// skill-exemption. The whole-list table is the test surface so a future
// developer who copy-pastes `SkillExempt() bool { return true }` onto a
// side-effecting tool gets caught here, not at runtime in someone's
// confidential-context skill that thought it was locking the tool out.
//
// Allowlist (must be true): pure-infrastructure tools, no I/O.
// Denylist (must be false): everything with filesystem / network / publish /
// shell side effects.
func TestSkillExemptInventory(t *testing.T) {
	// Construct each tool type the same way RegisterLocalTools does. We don't
	// register them — we just want to ask "does this Go type opt into
	// SkillExempt?".
	skillsPtr := &[]*skills.Skill{}

	cases := []struct {
		name       string
		tool       agent.Tool
		wantExempt bool
	}{
		// Allowlist: pure infrastructure.
		{"think", &ThinkTool{}, true},
		{"use_skill", newUseSkillTool(skillsPtr), true},

		// Denylist: anything with I/O. Adding SkillExempt to one of these
		// would silently bypass an active skill's allowed-tools restriction.
		{"file_read", &FileReadTool{}, false},
		{"file_write", &FileWriteTool{}, false},
		{"file_edit", &FileEditTool{}, false},
		{"glob", &GlobTool{}, false},
		{"grep", &GrepTool{}, false},
		{"bash", &BashTool{}, false},
		{"http", &HTTPTool{}, false},
		{"directory_list", &DirectoryListTool{}, false},
		{"system_info", &SystemInfoTool{}, false},
		{"clipboard", &ClipboardTool{}, false},
		{"notify", &NotifyTool{}, false},
		{"process", &ProcessTool{}, false},
		{"applescript", &AppleScriptTool{}, false},
		{"accessibility", &AccessibilityTool{}, false},
		{"ghostty", &GhosttyTool{}, false},
		{"browser", &BrowserTool{}, false},
		{"screenshot", &ScreenshotTool{}, false},
		{"computer", &ComputerTool{}, false},
		{"wait_for", &WaitTool{}, false},
		{"memory_append", &MemoryAppendTool{}, false},
		{"memory_recall", &MemoryTool{}, false},
		{"session_search", &SessionSearchTool{}, false},
		{"schedule_create", &ScheduleTool{action: "create"}, false},
		{"schedule_list", &ScheduleTool{action: "list"}, false},
		{"schedule_update", &ScheduleTool{action: "update"}, false},
		{"schedule_remove", &ScheduleTool{action: "remove"}, false},
		{"cloud_delegate", NewCloudDelegateTool(nil, "", time.Second, nil, "", ""), false},
		{"server_tool", NewServerTool(client.ServerToolSchema{Name: "web_search"}, nil), false},
		{"mcp_tool", NewMCPTool("playwright", mcpproto.Tool{Name: "browser_navigate"}, nil), false},
		{"publish_to_web", &PublishToWebTool{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agent.IsSkillExempt(tc.tool)
			if got != tc.wantExempt {
				if tc.wantExempt {
					t.Errorf("%s: expected SkillExempt=true (pure infrastructure), got false. "+
						"Add `func (...) SkillExempt() bool { return true }` if this tool is "+
						"genuinely side-effect-free reasoning/loading.", tc.name)
				} else {
					t.Errorf("%s: SkillExempt=true on a tool with side effects! "+
						"This silently bypasses every active skill's allowed-tools list. "+
						"Remove the SkillExempt method from this type.", tc.name)
				}
			}
		})
	}
}
