package claudecode

import (
	"os"
	"path/filepath"
	"strings"
)

func scanAgents(claudeHome string) ([]ScannedAgent, []Warning, error) {
	dir := filepath.Join(claudeHome, "agents")
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []Warning{{Kind: "source_unavailable", Path: "~/.claude/agents"}}, nil
		}
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, []Warning{{Kind: "symlink_escape", Path: "~/.claude/agents"}}, nil
	}
	if !info.IsDir() {
		return nil, []Warning{{Kind: "source_unavailable", Path: "~/.claude/agents"}}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var out []ScannedAgent
	var warns []Warning
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			warns = append(warns, Warning{Kind: "symlink_escape", Path: "~/.claude/agents/" + name})
			continue
		}
		if info.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		if info.Size() > MaxFileBytes {
			warns = append(warns, Warning{Kind: "size_limit", Path: "~/.claude/agents/" + name})
			continue
		}
		hash, err := fileSHA256(path)
		if err != nil {
			out = append(out, ScannedAgent{Name: slug, Status: "error", ErrorReason: "hash_failed"})
			continue
		}
		rel, _ := filepath.Rel(claudeHome, path)
		out = append(out, ScannedAgent{
			Name: slug, SrcRelPath: rel, SrcAbsPath: path,
			SizeBytes: info.Size(), ContentHash: hash, Status: "ok",
		})
	}
	return out, warns, nil
}
