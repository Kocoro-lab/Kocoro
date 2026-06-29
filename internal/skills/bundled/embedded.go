package bundled

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

//go:embed skills
var FS embed.FS

const bundledDirName = "bundled-skills"
const lockFileName = "bundled-skills.lock"
const versionFileName = ".version"

func ExtractBundledSkills(shannonDir string) (string, error) {
	if shannonDir == "" {
		return "", fmt.Errorf("shannonDir is required")
	}
	bundledDir := filepath.Join(shannonDir, bundledDirName)
	lockPath := filepath.Join(shannonDir, lockFileName)

	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return "", fmt.Errorf("create bundle lock dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", fmt.Errorf("open bundle lock: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return "", fmt.Errorf("lock bundle: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	currentVersion, err := os.ReadFile(filepath.Join(bundledDir, versionFileName))
	if err == nil && strings.TrimSpace(string(currentVersion)) == bundledVersion() {
		return bundledDir, nil
	}

	tmpDir := filepath.Join(filepath.Dir(bundledDir), bundledDirName+".tmp")
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", fmt.Errorf("clean tmp bundle dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return "", fmt.Errorf("create tmp bundle dir: %w", err)
	}

	if err := fs.WalkDir(FS, "skills", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel("skills", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dest := filepath.Join(tmpDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		content, err := FS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, content, 0644)
	}); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}

	if err := os.WriteFile(filepath.Join(tmpDir, versionFileName), []byte(bundledVersion()), 0600); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("write bundle version: %w", err)
	}

	_ = os.RemoveAll(bundledDir)
	if err := os.Rename(tmpDir, bundledDir); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("activate bundled skills: %w", err)
	}

	return bundledDir, nil
}

func bundledVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		version := strings.TrimSpace(info.Main.Version)
		if version != "" && version != "(devel)" {
			return version
		}
	}
	return "dev"
}
