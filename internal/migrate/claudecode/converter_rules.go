package claudecode

import (
	"fmt"
	"os"
)

// ConvertRules writes the imported CLAUDE.md to dstPath (a file path, not a
// directory) with a two-line import banner prepended. The banner makes the
// inheritance behavior explicit: this file applies to the default agent and
// named agents inherit it via Kocoro's 5-level instruction system. The
// converter does not handle merge or overwrite — the planner detects target
// existence and refuses to write in that case (spec §9).
func ConvertRules(r *ScannedRules, dstPath, importedAt string) error {
	body, err := os.ReadFile(r.SrcAbsPath)
	if err != nil {
		return err
	}
	banner := fmt.Sprintf(
		"<!-- imported from ~/.claude/CLAUDE.md on %s -->\n"+
			"<!-- This file applies to the Kocoro default agent and is inherited by named agents. -->\n\n",
		importedAt,
	)
	return os.WriteFile(dstPath, []byte(banner+string(body)), 0o644)
}
