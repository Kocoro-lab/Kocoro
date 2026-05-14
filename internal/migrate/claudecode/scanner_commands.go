package claudecode

import (
	"os"
	"path/filepath"
	"strings"
)

func scanCommands(claudeHome string) ([]ScannedCommand, []Warning, error) {
	dir := filepath.Join(claudeHome, "commands")
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // commands dir is rare; absence is not a warning
		}
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, []Warning{{Kind: "symlink_escape", Path: "~/.claude/commands"}}, nil
	}
	if !info.IsDir() {
		return nil, nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var out []ScannedCommand
	var warns []Warning
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			warns = append(warns, Warning{Kind: "symlink_escape", Path: "~/.claude/commands/" + name})
			continue
		}
		if info.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		if info.Size() > MaxFileBytes {
			warns = append(warns, Warning{Kind: "size_limit", Path: "~/.claude/commands/" + name})
			continue
		}
		hash, err := fileSHA256(path)
		if err != nil {
			out = append(out, ScannedCommand{Name: slug, Status: "error", ErrorReason: "hash_failed"})
			continue
		}
		rel, _ := filepath.Rel(claudeHome, path)
		out = append(out, ScannedCommand{
			Name: slug, SrcRelPath: rel, SrcAbsPath: path,
			SizeBytes: info.Size(), ContentHash: hash, Status: "ok",
		})
	}
	return out, warns, nil
}
