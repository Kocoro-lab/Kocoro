package claudecode

import (
	"os"
	"path/filepath"
)

func scanRules(claudeHome string) (*ScannedRules, []Warning, error) {
	path := filepath.Join(claudeHome, "CLAUDE.md")
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, []Warning{{Kind: "symlink_escape", Path: "~/.claude/CLAUDE.md"}}, nil
	}
	if info.IsDir() {
		return nil, nil, nil
	}
	if info.Size() > MaxFileBytes {
		return nil, []Warning{{Kind: "size_limit", Path: "~/.claude/CLAUDE.md"}}, nil
	}
	hash, err := fileSHA256(path)
	if err != nil {
		return &ScannedRules{SrcAbsPath: path, Status: "error", ErrorReason: "hash_failed"}, nil, nil
	}
	return &ScannedRules{
		SrcAbsPath:  path,
		SizeBytes:   info.Size(),
		ContentHash: hash,
		Status:      "ok",
	}, nil, nil
}
