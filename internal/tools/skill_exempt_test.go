package tools

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
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
		name      string
		tool      agent.Tool
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
		{"memory_append", &MemoryAppendTool{}, false},
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
