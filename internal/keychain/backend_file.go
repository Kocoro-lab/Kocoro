package keychain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

// fileBackend persists credentials as JSON at <dir>/credentials.json (mode
// 0600). It backs the Linux Store (see backend_linux.go) and is exercised
// directly by tests on every platform (this file carries no build tag).
//
// Security note: this is plaintext at rest, mode 0600 — deliberately no
// worse than the legacy ~/.shannon/config.yaml api_key path it supersedes on
// Linux. go-keyring's Linux Secret Service backend is intentionally NOT used:
// it returns success at construction but fails every read/write on headless
// hosts with no D-Bus (Docker / SSH / servers), which would let the
// yaml→store migration strip the key from yaml and then fail to persist it —
// stranding the user. A durable file keeps the Supported()==(NewOSStore
// err==nil) invariant honest and works everywhere. See the cross-platform
// credential-store notes in CLAUDE.md.
type fileBackend struct {
	mu       sync.Mutex
	dir      string
	path     string // <dir>/credentials.json
	lockPath string // <dir>/credentials.lock (persistent; never deleted)
}

// credStore is the on-disk shape: service -> account -> secret. The nested
// form namespaces the two daemon services (api_key, state) cleanly, mirroring
// MemBackend's memKey separation.
type credStore map[string]map[string]string

func newFileBackend(dir string) (Backend, error) {
	if dir == "" {
		return nil, errors.New("keychain: file backend requires a non-empty dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &fileBackend{
		dir:      dir,
		path:     filepath.Join(dir, "credentials.json"),
		lockPath: filepath.Join(dir, "credentials.lock"),
	}, nil
}

// withLock opens the persistent lock file, acquires the advisory lock
// (exclusive when write, shared otherwise) via internal/fslock, runs fn, then
// releases. The lock file is a separate file that is NEVER deleted — deleting
// it would race holders onto different inodes (same rule as schedules.json /
// secrets-index.json). Cross-process safety comes from fslock; intra-process
// from b.mu in the callers.
func (b *fileBackend) withLock(write bool, fn func() error) error {
	lf, err := os.OpenFile(b.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if write {
		err = fslock.Lock(lf.Fd())
	} else {
		err = fslock.RLock(lf.Fd())
	}
	if err != nil {
		return err
	}
	defer fslock.Unlock(lf.Fd())
	return fn()
}

// load reads and parses credentials.json. A missing, empty, or corrupt file
// is treated as an empty store (callers then see ErrNotFound, never a parse
// error) — a fresh credential store simply has no entries. A subsequent Write
// rewrites a corrupt file cleanly.
func (b *fileBackend) load() (credStore, error) {
	raw, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credStore{}, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return credStore{}, nil
	}
	var m credStore
	if err := json.Unmarshal(raw, &m); err != nil {
		return credStore{}, nil
	}
	if m == nil {
		m = credStore{}
	}
	return m, nil
}

// save atomically persists the store: write a 0600 temp file in the same dir,
// fsync, then rename over credentials.json. Matches the temp+rename pattern in
// migrate.go / schedules.json. A crash mid-write leaves the prior file intact.
func (b *fileBackend) save(m credStore) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(b.dir, "credentials-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// CreateTemp already makes the file 0600, but set it explicitly so a
	// future switch to os.OpenFile can't widen it via umask.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, b.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func (b *fileBackend) Read(service, account string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out string
	found := false
	err := b.withLock(false, func() error {
		m, err := b.load()
		if err != nil {
			return err
		}
		if accs, ok := m[service]; ok {
			if v, ok2 := accs[account]; ok2 {
				out, found = v, true
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	return out, nil
}

func (b *fileBackend) Write(service, account, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.withLock(true, func() error {
		m, err := b.load()
		if err != nil {
			return err
		}
		if m[service] == nil {
			m[service] = map[string]string{}
		}
		m[service][account] = value
		return b.save(m)
	})
}

func (b *fileBackend) Delete(service, account string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.withLock(true, func() error {
		m, err := b.load()
		if err != nil {
			return err
		}
		accs, ok := m[service]
		if !ok {
			return ErrNotFound
		}
		if _, ok := accs[account]; !ok {
			return ErrNotFound
		}
		delete(accs, account)
		if len(accs) == 0 {
			delete(m, service)
		}
		return b.save(m)
	})
}
