package claudecode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ConvertSkill stages a fully-formed skill directory at stagingDir, ready
// for atomic rename to ~/.shannon/skills/<name>. Flat .md sources get wrapped
// into a <name>/SKILL.md; dir sources mirror the entire tree (SKILL.md +
// scripts/ + any sibling files). Symlinks anywhere inside the tree are
// silently skipped (privacy §7.4); the scanner already rejected dir skills
// containing symlinks before they reach this converter, so this is a
// defense-in-depth check.
func ConvertSkill(s ScannedSkill, stagingDir string) error {
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}
	switch s.Layout {
	case "flat":
		return copyFile(s.SrcAbsPath, filepath.Join(stagingDir, "SKILL.md"))
	case "dir":
		// SrcAbsPath is the skill directory itself.
		return copySkillTree(s.SrcAbsPath, stagingDir)
	default:
		return fmt.Errorf("unknown skill layout %q for %q", s.Layout, s.Name)
	}
}

func copySkillTree(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(dstDir, e.Name())
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		// Defense in depth — scanner rejected dir skills containing symlinks,
		// but if we ever reach here with one, refuse to copy.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			if err := copySkillTree(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
