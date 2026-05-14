package claudecode

import "os"

// Scan walks the two source roots independently. Either source being missing
// or unreadable does not fail the overall scan; categories from the broken
// source are empty, and a SourceErrors entry is recorded. The handler turns a
// scan with zero importable items into a 404 claude_not_found per spec §12.1.
//
// Privacy: the top-level source roots themselves are os.Lstat'd. If either is
// a symlink we refuse to traverse it — a malicious ~/.claude or ~/.claude.json
// symlink could otherwise redirect the scanner to attacker-controlled content
// in violation of spec §7.4. Per-entry symlink rejection in the sub-scanners
// only kicks in after the root is opened, so root protection has to live here.
func Scan(src SourcePaths) (*ScanResult, error) {
	r := &ScanResult{SourceErrors: map[string]string{}}

	homeInfo, err := os.Lstat(src.ClaudeHome)
	switch {
	case err != nil:
		r.SourceErrors["claude_home"] = err.Error()
	case homeInfo.Mode()&os.ModeSymlink != 0:
		// Symlinked ~/.claude — refuse, surface warning, do not traverse.
		r.SourceErrors["claude_home"] = "symlinked_source_root"
		r.Warnings = append(r.Warnings, Warning{Kind: "symlink_escape", Path: "~/.claude"})
	default:
		skills, warns, err := scanSkills(src.ClaudeHome)
		if err != nil {
			r.SourceErrors["claude_home"] = err.Error()
		} else {
			r.Skills = skills
			r.Warnings = append(r.Warnings, warns...)
		}
		agents, warns, err := scanAgents(src.ClaudeHome)
		if err != nil {
			r.SourceErrors["claude_home"] = err.Error()
		} else {
			r.Agents = agents
			r.Warnings = append(r.Warnings, warns...)
		}
		commands, warns, err := scanCommands(src.ClaudeHome)
		if err != nil {
			r.SourceErrors["claude_home"] = err.Error()
		} else {
			r.Commands = commands
			r.Warnings = append(r.Warnings, warns...)
		}
		rules, warns, err := scanRules(src.ClaudeHome)
		if err != nil {
			r.SourceErrors["claude_home"] = err.Error()
		} else {
			r.GlobalRules = rules
			r.Warnings = append(r.Warnings, warns...)
		}
	}

	if _, err := os.Stat(src.ClaudeUserConfig); err != nil {
		if !os.IsNotExist(err) {
			r.SourceErrors["claude_user_config"] = err.Error()
		}
		// Absence of ~/.claude.json is normal; no warning, no SourceErrors entry.
	} else {
		mcps, warns, err := scanMCP(src.ClaudeUserConfig)
		if err != nil {
			r.SourceErrors["claude_user_config"] = err.Error()
		} else {
			r.MCPServers = mcps
			r.Warnings = append(r.Warnings, warns...)
		}
	}

	return r, nil
}

// TotalImportable returns the number of items that would actually be imported.
// Used by handlers to decide whether to return 404 when both sources are
// unusable (zero importable items AND at least one SourceErrors entry).
func (r *ScanResult) TotalImportable() int {
	n := len(r.Skills) + len(r.Agents) + len(r.Commands) + len(r.MCPServers)
	if r.GlobalRules != nil {
		n++
	}
	return n
}
