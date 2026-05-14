package claudecode

import (
	"path/filepath"
	"strings"
)

// SymbolicForm returns the ~/-prefixed form of abs if abs is under home,
// otherwise returns abs unchanged.
func SymbolicForm(abs, home string) string {
	if home == "" {
		return abs
	}
	if abs == home {
		return "~"
	}
	if strings.HasPrefix(abs, home+string(filepath.Separator)) {
		return "~/" + strings.TrimPrefix(abs, home+string(filepath.Separator))
	}
	return abs
}

// DefaultSources returns the canonical Claude Code source locations for a given home dir.
func DefaultSources(home string) SourcePaths {
	return SourcePaths{
		ClaudeHome:       filepath.Join(home, ".claude"),
		ClaudeUserConfig: filepath.Join(home, ".claude.json"),
	}
}

// SymbolicForPlan builds SymbolicPaths from SourcePaths + target + home.
func SymbolicForPlan(src SourcePaths, target, home string) SymbolicPaths {
	return SymbolicPaths{
		ClaudeHome:       SymbolicForm(src.ClaudeHome, home),
		ClaudeUserConfig: SymbolicForm(src.ClaudeUserConfig, home),
		Target:           SymbolicForm(target, home),
	}
}
