package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scanSkills walks ~/.claude/skills and produces a ScannedSkill per skill.
// Symlinks at the skill root level or anywhere inside a dir-layout skill tree
// are rejected (privacy invariant §7.4). The returned ContentHash and
// SizeBytes cover the FULL skill content (SKILL.md + scripts for dir layout)
// so the applier's TOCTOU re-check catches changes to any contributing file.
func scanSkills(claudeHome string) ([]ScannedSkill, []Warning, error) {
	skillsDir := filepath.Join(claudeHome, "skills")
	info, err := os.Lstat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []Warning{{Kind: "source_unavailable", Path: "~/.claude/skills"}}, nil
		}
		return nil, nil, err
	}
	// Refuse to follow a symlink at the skills root itself.
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, []Warning{{Kind: "symlink_escape", Path: "~/.claude/skills"}}, nil
	}
	if !info.IsDir() {
		return nil, []Warning{{Kind: "source_unavailable", Path: "~/.claude/skills"}}, nil
	}

	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil, nil, err
	}
	var out []ScannedSkill
	var warns []Warning
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		entryPath := filepath.Join(skillsDir, name)
		entryInfo, err := os.Lstat(entryPath)
		if err != nil {
			continue
		}
		// Reject symlinks at the skill root (flat-symlink or dir-symlink).
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			warns = append(warns, Warning{Kind: "symlink_escape", Path: "~/.claude/skills/" + name})
			continue
		}

		if entryInfo.IsDir() {
			skillFile := filepath.Join(entryPath, "SKILL.md")
			if _, err := os.Stat(skillFile); err != nil {
				// Empty / non-skill directory; skip silently.
				continue
			}
			s, ws := scanSkillDir(entryPath, name, claudeHome)
			if s != nil {
				out = append(out, *s)
			}
			warns = append(warns, ws...)
		} else if strings.HasSuffix(name, ".md") {
			slug := strings.TrimSuffix(name, ".md")
			s, ws := scanFlatSkill(entryPath, slug, claudeHome)
			if s != nil {
				out = append(out, *s)
			}
			warns = append(warns, ws...)
		}
	}
	return out, warns, nil
}

// scanFlatSkill processes a single ~/.claude/skills/<name>.md file. The entry
// was already verified non-symlink by the caller.
func scanFlatSkill(path, slug, claudeHome string) (*ScannedSkill, []Warning) {
	info, err := os.Lstat(path)
	if err != nil {
		return &ScannedSkill{Name: slug, Status: "error", ErrorReason: "stat_failed", SrcAbsPath: path}, nil
	}
	if info.Size() > MaxFileBytes {
		return nil, []Warning{{Kind: "size_limit", Path: "~/.claude/skills/" + slug + ".md"}}
	}
	hash, err := fileSHA256(path)
	if err != nil {
		return &ScannedSkill{Name: slug, Status: "error", ErrorReason: "hash_failed", SrcAbsPath: path}, nil
	}
	rel, _ := filepath.Rel(claudeHome, path)
	return &ScannedSkill{
		Name:        slug,
		SrcRelPath:  rel,
		SrcAbsPath:  path,
		Layout:      "flat",
		SizeBytes:   info.Size(),
		ContentHash: hash,
		Status:      "ok",
	}, nil
}

// scanSkillDir walks a directory-layout skill (SKILL.md + scripts/) and
// produces a single ScannedSkill whose ContentHash + SizeBytes cover every
// non-symlink file in the tree. Symlinks anywhere inside the tree cause the
// entire skill to be rejected with a symlink_escape warning (privacy §7.4).
// Total tree size > MaxSkillDirBytes also causes rejection.
func scanSkillDir(skillDir, slug, claudeHome string) (*ScannedSkill, []Warning) {
	rel, _ := filepath.Rel(claudeHome, skillDir)
	hash, total, ok, warns := hashSkillTree(skillDir, slug)
	if !ok {
		return nil, warns
	}
	if total > MaxSkillDirBytes {
		return nil, []Warning{{Kind: "size_limit", Path: "~/.claude/skills/" + slug}}
	}
	return &ScannedSkill{
		Name:        slug,
		SrcRelPath:  rel,
		SrcAbsPath:  skillDir,
		Layout:      "dir",
		SizeBytes:   total,
		ContentHash: hash,
		Status:      "ok",
	}, warns
}

// hashSkillTree walks every regular file under root, computing per-file SHA256
// and summing sizes. Symlinks abort with a symlink_escape warning and ok=false.
// Files larger than MaxFileBytes individually abort with size_limit warning.
// The returned hash is sha256 over sorted (relpath, sha256) lines so any
// content or path change flips the value deterministically.
func hashSkillTree(root, slug string) (hash string, total int64, ok bool, warns []Warning) {
	type entry struct{ rel, sum string }
	var entries []entry

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		// Reject symlinks anywhere in the tree. The entire skill is invalidated.
		if info.Mode()&os.ModeSymlink != 0 {
			warns = append(warns, Warning{Kind: "symlink_escape", Path: "~/.claude/skills/" + slug})
			return fmt.Errorf("symlink_escape")
		}
		if d.IsDir() {
			return nil
		}
		// Reject individual oversize files.
		if info.Size() > MaxFileBytes {
			warns = append(warns, Warning{Kind: "size_limit", Path: "~/.claude/skills/" + slug})
			return fmt.Errorf("size_limit")
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		entries = append(entries, entry{rel: filepath.ToSlash(rel), sum: sum})
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		// Symlink/size warnings already appended; for any other error, surface
		// nothing — the caller treats !ok as "skip this skill".
		return "", 0, false, warns
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), total, true, warns
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
