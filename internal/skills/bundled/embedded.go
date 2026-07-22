package bundled

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

//go:embed skills
var FS embed.FS

const (
	bundledDirName = "bundled-skills"
	lockFileName   = "bundled-skills.lock"
	hashFileName   = ".content-hash"
)

// ExtractBundledSkills materializes the embedded skills tree into
// <shannonDir>/bundled-skills and returns that directory.
//
// It is content-addressed: a sidecar records the sha256 of the embedded tree,
// and re-extraction is skipped only when the sidecar matches the running
// binary's embedded content. This fixes two problems with the previous
// version-string sidecar:
//
//   - Staleness. bundledVersion() derived from debug.ReadBuildInfo().Main.Version,
//     which is "(devel)" (→ the constant "dev") for every `go build` binary this
//     project ships. So the sidecar never changed and bundled skills were
//     extracted exactly once per ~/.shannon and never refreshed across app
//     upgrades — installFromBundled/PreviewSkill kept serving stale content.
//     A content hash changes whenever the embedded skills change, so upgrades
//     self-heal on the next call.
//   - Windows activation. The old path did RemoveAll(bundledDir) then
//     Rename(tmp, bundledDir); on Windows os.Rename cannot replace an existing
//     directory (MoveFileEx has no REPLACE_EXISTING for dirs), and the ignored
//     RemoveAll error could leave the target in place, failing the rename with
//     "Access is denied". Activation now moves the current dir aside first, so
//     the rename target never pre-exists.
//
// The fast path costs one small sidecar read (no disk walk), and the embedded
// hash is memoized per process, so the common "already current" case is cheap
// even though this runs on a hot path (per-message skill injection).
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

	wantHash, err := embedSkillsHash()
	if err != nil {
		return "", fmt.Errorf("hash embedded skills: %w", err)
	}

	// Fast path: sidecar already matches this binary's embedded content.
	if cur, err := os.ReadFile(filepath.Join(bundledDir, hashFileName)); err == nil &&
		strings.TrimSpace(string(cur)) == wantHash {
		return bundledDir, nil
	}

	// Best-effort cleanup of the legacy version sidecar from the previous design.
	_ = os.Remove(filepath.Join(bundledDir, ".version"))

	tmpDir := bundledDir + ".tmp"
	oldDir := bundledDir + ".old"
	// Remove leftovers from any prior interrupted activation before staging.
	_ = os.RemoveAll(tmpDir)
	_ = os.RemoveAll(oldDir)

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

	if err := os.WriteFile(filepath.Join(tmpDir, hashFileName), []byte(wantHash), 0600); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("write bundle content hash: %w", err)
	}

	// Windows-safe activation: rename the current directory aside first (moving
	// a directory to a fresh, non-existent name works even where MoveFile
	// refuses to overwrite an existing target), then move the new one into
	// place, then drop the old copy.
	if _, statErr := os.Stat(bundledDir); statErr == nil {
		if err := os.Rename(bundledDir, oldDir); err != nil {
			os.RemoveAll(tmpDir)
			return "", fmt.Errorf("archive previous bundled skills: %w", err)
		}
	}
	if err := os.Rename(tmpDir, bundledDir); err != nil {
		// Restore the previous dir so a failed activation never leaves nothing.
		_ = os.Rename(oldDir, bundledDir)
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("activate bundled skills: %w", err)
	}
	_ = os.RemoveAll(oldDir)

	return bundledDir, nil
}

// embedSkillsHashOnce memoizes the sha256 of the embedded skills tree. The
// embedded content is baked into the binary, so the hash is invariant for the
// process lifetime — recomputing it on every hot-path call would be wasted work.
var embedSkillsHashOnce = sync.OnceValues(computeEmbedSkillsHash)

func embedSkillsHash() (string, error) { return embedSkillsHashOnce() }

// computeEmbedSkillsHash walks the embedded skills tree and hashes every file's
// (path, length, content). Path + length framing prevents rename/prefix
// collisions. embed.FS paths are always forward-slash, so the hash is
// OS-independent.
func computeEmbedSkillsHash() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(FS, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(p), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
