package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func makeDir(t *testing.T, p string) error {
	t.Helper()
	return os.MkdirAll(p, 0o755)
}

func writeFile(t *testing.T, p, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
