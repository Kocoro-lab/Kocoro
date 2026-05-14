package claudecode

import (
	"fmt"
	"path/filepath"
)

// ComputeSkillFingerprint returns the fingerprint that detects any change to
// the skill's contributing files. For flat layout: sha256 of the .md file.
// For dir layout: tree-hash over every non-symlink file in the skill dir
// (same algorithm scanner_skills uses to produce ScannedSkill.ContentHash).
func ComputeSkillFingerprint(s ScannedSkill) (SourceFingerprint, error) {
	switch s.Layout {
	case "flat":
		h, err := fileSHA256(s.SrcAbsPath)
		if err != nil {
			return SourceFingerprint{}, err
		}
		return SourceFingerprint{Kind: "file", Hash: h}, nil
	case "dir":
		h, _, ok, _ := hashSkillTree(s.SrcAbsPath, s.Name)
		if !ok {
			return SourceFingerprint{}, fmt.Errorf("skill_tree hash failed for %q", s.Name)
		}
		return SourceFingerprint{Kind: "skill_tree", Hash: h}, nil
	default:
		return SourceFingerprint{}, fmt.Errorf("unknown skill layout %q", s.Layout)
	}
}

// ComputeFileFingerprint hashes a single file. Use for agents, commands,
// global rules, and the ~/.claude.json source of MCP servers.
func ComputeFileFingerprint(path string) (SourceFingerprint, error) {
	h, err := fileSHA256(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{Kind: "file", Hash: h}, nil
}

// ValidateSourceFingerprint re-computes the fingerprint for path using the
// algorithm recorded in the original fingerprint, and returns (true, nil) iff
// the new value matches. The applier calls this for every entry in
// Plan.SourceHashes during Phase A; any false result causes a 409 plan_stale
// response and aborts the apply with no target changes.
func ValidateSourceFingerprint(path string, recorded SourceFingerprint) (bool, error) {
	switch recorded.Kind {
	case "file":
		h, err := fileSHA256(path)
		if err != nil {
			return false, err
		}
		return h == recorded.Hash, nil
	case "skill_tree":
		// Slug here is purely informational for warnings — pass the dir basename
		// so any symlink_escape warning we surface points at the right name.
		h, _, ok, _ := hashSkillTree(path, filepath.Base(path))
		if !ok {
			return false, fmt.Errorf("re-hash failed for %s", path)
		}
		return h == recorded.Hash, nil
	default:
		return false, fmt.Errorf("unknown fingerprint kind: %q", recorded.Kind)
	}
}
