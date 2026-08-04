package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Three complete loads cover the short rename/write bursts produced by common
// atomic-save editors without accepting a mixed snapshot. If all three collide,
// startup or POST /config/reload returns an explicit retry error; there is no
// unsafe override, and the operator retries after the editor finishes writing.
const stableLoadAttempts = 3

// BytesRevision returns the content identity used by the daemon to distinguish
// the exact config.yaml bytes reflected in memory from a later disk revision.
func BytesRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}

// FileRevision returns the current config.yaml content identity.
func FileRevision(shannonDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing", nil
		}
		return "", err
	}
	return BytesRevision(data), nil
}

// LoadWithRevision loads config and returns the revision of the exact global
// config.yaml snapshot it observed. External editors do not participate in the
// daemon's advisory lock, so stable before/after reads close the window where a
// later file revision could otherwise be marked as already applied.
func LoadWithRevision() (*Config, string, error) {
	shannonDir := ShannonDir()
	if shannonDir == "" {
		return nil, "", fmt.Errorf("failed to resolve home directory")
	}
	for attempt := 0; attempt < stableLoadAttempts; attempt++ {
		before, err := FileRevision(shannonDir)
		if err != nil {
			return nil, "", fmt.Errorf("read config revision before load: %w", err)
		}
		cfg, err := Load()
		if err != nil {
			return nil, "", err
		}
		after, err := FileRevision(shannonDir)
		if err != nil {
			return nil, "", fmt.Errorf("read config revision after load: %w", err)
		}
		if before == after {
			return cfg, after, nil
		}
	}
	return nil, "", fmt.Errorf("config.yaml changed while it was being loaded; retry")
}
