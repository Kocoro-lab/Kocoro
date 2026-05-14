package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func scanSkills(claudeHome string) ([]ScannedSkill, []Warning, error) {
	skillsDir := filepath.Join(claudeHome, "skills")
	info, err := os.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []Warning{{Kind: "source_unavailable", Path: "~/.claude/skills"}}, nil
		}
		return nil, nil, err
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
		if e.IsDir() {
			skillFile := filepath.Join(skillsDir, name, "SKILL.md")
			if _, err := os.Stat(skillFile); err != nil {
				continue
			}
			s, w := scanOneSkillFile(skillFile, name, "dir", claudeHome)
			if s != nil {
				out = append(out, *s)
			}
			warns = append(warns, w...)
		} else if strings.HasSuffix(name, ".md") {
			flatPath := filepath.Join(skillsDir, name)
			slug := strings.TrimSuffix(name, ".md")
			s, w := scanOneSkillFile(flatPath, slug, "flat", claudeHome)
			if s != nil {
				out = append(out, *s)
			}
			warns = append(warns, w...)
		}
	}
	return out, warns, nil
}

func scanOneSkillFile(path, slug, layout, claudeHome string) (*ScannedSkill, []Warning) {
	info, err := os.Stat(path)
	if err != nil {
		return &ScannedSkill{Name: slug, Status: "error", ErrorReason: "stat_failed", SrcAbsPath: path}, nil
	}
	if info.Size() > MaxFileBytes {
		return nil, []Warning{{Kind: "size_limit", Path: SymbolicForm(path, filepath.Dir(claudeHome))}}
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
		Layout:      layout,
		SizeBytes:   info.Size(),
		ContentHash: hash,
		Status:      "ok",
	}, nil
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
