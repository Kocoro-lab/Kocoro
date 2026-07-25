package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

const (
	computerUseAppPolicySchemaVersion = 1
	computerUseAppPolicyFilename      = "computer-use-app-policy.json"

	ComputerUseAppPolicyAsk     ComputerUseAppPolicyDecision = "ask"
	ComputerUseAppPolicyBlocked ComputerUseAppPolicyDecision = "blocked"

	ComputerUseAppPolicySourceDefault       ComputerUseAppPolicySource = "default"
	ComputerUseAppPolicySourceUser          ComputerUseAppPolicySource = "user"
	ComputerUseAppPolicySourceBuiltIn       ComputerUseAppPolicySource = "built_in"
	ComputerUseAppPolicySourceInvalidStore  ComputerUseAppPolicySource = "invalid_store"
	ComputerUseAppPolicySourceInvalidTarget ComputerUseAppPolicySource = "invalid_target"
)

var ErrComputerUseAppPolicyBuiltIn = errors.New("computer-use built-in app policy is immutable")

type ComputerUseAppPolicyDecision string
type ComputerUseAppPolicySource string

type ComputerUseAppPolicyEntry struct {
	BundleID string                       `json:"bundle_id"`
	Decision ComputerUseAppPolicyDecision `json:"decision"`
	Source   ComputerUseAppPolicySource   `json:"source"`
}

type ComputerUseAppPolicySnapshot struct {
	SchemaVersion int                         `json:"schema_version"`
	Revision      uint64                      `json:"revision"`
	Entries       []ComputerUseAppPolicyEntry `json:"entries"`
}

type persistedComputerUseAppPolicy struct {
	SchemaVersion int                          `json:"schema_version"`
	Revision      *uint64                      `json:"revision"`
	Entries       *[]ComputerUseAppPolicyEntry `json:"entries"`
}

// Keep this list narrow and exact. Blocking a general-purpose app because it
// happens to contain a terminal or settings view would make ordinary desktop
// work unusable; V1 blocks only Kocoro itself, macOS security surfaces, and
// dedicated shell applications.
var builtInComputerUseBlockedBundleIDs = []string{
	"com.apple.authorizationhost",
	"com.apple.coreauthui",
	"com.apple.keychainaccess",
	"com.apple.loginwindow",
	"com.apple.passwords",
	"com.apple.securityagent",
	"com.apple.systempreferences",
	"com.apple.terminal",
	"com.github.wez.wezterm",
	"com.googlecode.iterm2",
	"com.mitchellh.ghostty",
	"dev.warp.warp-stable",
	"net.kovidgoyal.kitty",
	"org.alacritty",
	"run.shannon.shanclaw",
	"run.shannon.shanclaw.ax-server",
	"run.shannon.shanclaw.dev",
	"run.shannon.shanclaw.dev.ax-server",
}

var builtInComputerUseBlockedSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(builtInComputerUseBlockedBundleIDs))
	for _, value := range builtInComputerUseBlockedBundleIDs {
		set[value] = struct{}{}
	}
	return set
}()

type ComputerUseAppPolicyStore struct {
	mu       sync.RWMutex
	path     string
	lockPath string
	revision uint64
	entries  map[string]ComputerUseAppPolicyDecision
	loadErr  error
}

func NewComputerUseAppPolicyStore(shannonDir string) *ComputerUseAppPolicyStore {
	store := &ComputerUseAppPolicyStore{
		entries: make(map[string]ComputerUseAppPolicyDecision),
	}
	if strings.TrimSpace(shannonDir) == "" {
		store.loadErr = fmt.Errorf("computer-use app policy directory is required")
		return store
	}
	store.path = filepath.Join(shannonDir, computerUseAppPolicyFilename)
	store.lockPath = store.path + ".lock"
	store.load()
	return store
}

func normalizeComputerUseBundleID(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 255 {
		return "", fmt.Errorf("invalid bundle_id")
	}
	normalized := strings.ToLower(value)
	parts := strings.Split(normalized, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid bundle_id")
	}
	for _, part := range parts {
		if part == "" || part[0] == '-' || part[len(part)-1] == '-' {
			return "", fmt.Errorf("invalid bundle_id")
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("invalid bundle_id")
			}
		}
	}
	return normalized, nil
}

func (s *ComputerUseAppPolicyStore) LoadError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

func (s *ComputerUseAppPolicyStore) DecisionFor(bundleID string) ComputerUseAppPolicyEntry {
	normalized, err := normalizeComputerUseBundleID(bundleID)
	if err != nil {
		if bundleID == "" {
			return ComputerUseAppPolicyEntry{Decision: ComputerUseAppPolicyAsk, Source: ComputerUseAppPolicySourceDefault}
		}
		return ComputerUseAppPolicyEntry{Decision: ComputerUseAppPolicyBlocked, Source: ComputerUseAppPolicySourceInvalidTarget}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.loadErr != nil {
		return ComputerUseAppPolicyEntry{BundleID: normalized, Decision: ComputerUseAppPolicyBlocked, Source: ComputerUseAppPolicySourceInvalidStore}
	}
	if _, ok := builtInComputerUseBlockedSet[normalized]; ok {
		return ComputerUseAppPolicyEntry{BundleID: normalized, Decision: ComputerUseAppPolicyBlocked, Source: ComputerUseAppPolicySourceBuiltIn}
	}
	if decision, ok := s.entries[normalized]; ok {
		return ComputerUseAppPolicyEntry{BundleID: normalized, Decision: decision, Source: ComputerUseAppPolicySourceUser}
	}
	return ComputerUseAppPolicyEntry{BundleID: normalized, Decision: ComputerUseAppPolicyAsk, Source: ComputerUseAppPolicySourceDefault}
}

func (s *ComputerUseAppPolicyStore) Snapshot() (ComputerUseAppPolicySnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.loadErr != nil {
		return ComputerUseAppPolicySnapshot{}, s.loadErr
	}
	entries := make([]ComputerUseAppPolicyEntry, 0, len(builtInComputerUseBlockedBundleIDs)+len(s.entries))
	for _, bundleID := range builtInComputerUseBlockedBundleIDs {
		entries = append(entries, ComputerUseAppPolicyEntry{BundleID: bundleID, Decision: ComputerUseAppPolicyBlocked, Source: ComputerUseAppPolicySourceBuiltIn})
	}
	for bundleID, decision := range s.entries {
		entries = append(entries, ComputerUseAppPolicyEntry{BundleID: bundleID, Decision: decision, Source: ComputerUseAppPolicySourceUser})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].BundleID < entries[j].BundleID })
	return ComputerUseAppPolicySnapshot{SchemaVersion: computerUseAppPolicySchemaVersion, Revision: s.revision, Entries: entries}, nil
}

func (s *ComputerUseAppPolicyStore) Update(bundleID string, decision ComputerUseAppPolicyDecision) (ComputerUseAppPolicySnapshot, error) {
	normalized, err := normalizeComputerUseBundleID(bundleID)
	if err != nil {
		return ComputerUseAppPolicySnapshot{}, err
	}
	if decision != ComputerUseAppPolicyAsk && decision != ComputerUseAppPolicyBlocked {
		return ComputerUseAppPolicySnapshot{}, fmt.Errorf("invalid decision")
	}
	if _, ok := builtInComputerUseBlockedSet[normalized]; ok {
		return ComputerUseAppPolicySnapshot{}, ErrComputerUseAppPolicyBuiltIn
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ComputerUseAppPolicySnapshot{}, s.loadErr
	}
	var restoreEntries map[string]ComputerUseAppPolicyDecision
	var restoreRevision uint64
	mutated := false
	err = s.withFileLock(true, func() error {
		if err := s.reloadFromDiskLocked(); err != nil {
			return err
		}
		restoreEntries = cloneComputerUseAppPolicyEntries(s.entries)
		restoreRevision = s.revision
		s.entries[normalized] = decision
		s.revision++
		mutated = true
		return s.persistLocked()
	})
	if err != nil {
		if mutated {
			s.entries = restoreEntries
			s.revision = restoreRevision
		} else {
			s.loadErr = err
		}
		return ComputerUseAppPolicySnapshot{}, err
	}
	return s.snapshotLocked(), nil
}

func (s *ComputerUseAppPolicyStore) Revoke(bundleID string) (ComputerUseAppPolicySnapshot, error) {
	normalized, err := normalizeComputerUseBundleID(bundleID)
	if err != nil {
		return ComputerUseAppPolicySnapshot{}, err
	}
	if _, ok := builtInComputerUseBlockedSet[normalized]; ok {
		return ComputerUseAppPolicySnapshot{}, ErrComputerUseAppPolicyBuiltIn
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ComputerUseAppPolicySnapshot{}, s.loadErr
	}
	var restoreEntries map[string]ComputerUseAppPolicyDecision
	var restoreRevision uint64
	mutated := false
	err = s.withFileLock(true, func() error {
		if err := s.reloadFromDiskLocked(); err != nil {
			return err
		}
		if _, existed := s.entries[normalized]; !existed {
			return nil
		}
		restoreEntries = cloneComputerUseAppPolicyEntries(s.entries)
		restoreRevision = s.revision
		delete(s.entries, normalized)
		s.revision++
		mutated = true
		return s.persistLocked()
	})
	if err != nil {
		if mutated {
			s.entries = restoreEntries
			s.revision = restoreRevision
		} else {
			s.loadErr = err
		}
		return ComputerUseAppPolicySnapshot{}, err
	}
	return s.snapshotLocked(), nil
}

func cloneComputerUseAppPolicyEntries(entries map[string]ComputerUseAppPolicyDecision) map[string]ComputerUseAppPolicyDecision {
	clone := make(map[string]ComputerUseAppPolicyDecision, len(entries))
	for bundleID, decision := range entries {
		clone[bundleID] = decision
	}
	return clone
}

func (s *ComputerUseAppPolicyStore) snapshotLocked() ComputerUseAppPolicySnapshot {
	entries := make([]ComputerUseAppPolicyEntry, 0, len(builtInComputerUseBlockedBundleIDs)+len(s.entries))
	for _, bundleID := range builtInComputerUseBlockedBundleIDs {
		entries = append(entries, ComputerUseAppPolicyEntry{BundleID: bundleID, Decision: ComputerUseAppPolicyBlocked, Source: ComputerUseAppPolicySourceBuiltIn})
	}
	for bundleID, decision := range s.entries {
		entries = append(entries, ComputerUseAppPolicyEntry{BundleID: bundleID, Decision: decision, Source: ComputerUseAppPolicySourceUser})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].BundleID < entries[j].BundleID })
	return ComputerUseAppPolicySnapshot{SchemaVersion: computerUseAppPolicySchemaVersion, Revision: s.revision, Entries: entries}
}

func (s *ComputerUseAppPolicyStore) load() {
	if s.loadErr != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		s.loadErr = fmt.Errorf("create computer-use app policy directory: %w", err)
		return
	}
	if err := s.withFileLock(false, s.reloadFromDiskLocked); err != nil {
		s.loadErr = err
	}
}

func (s *ComputerUseAppPolicyStore) reloadFromDiskLocked() error {
	s.entries = make(map[string]ComputerUseAppPolicyDecision)
	s.revision = 0
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read computer-use app policy: %w", err)
	}
	var persisted persistedComputerUseAppPolicy
	if err := decodeStrictComputerUseAppPolicyJSON(data, &persisted); err != nil {
		return fmt.Errorf("decode computer-use app policy: %w", err)
	}
	if persisted.SchemaVersion != computerUseAppPolicySchemaVersion || persisted.Revision == nil || persisted.Entries == nil {
		return fmt.Errorf("unsupported computer-use app policy schema")
	}
	for _, entry := range *persisted.Entries {
		normalized, err := normalizeComputerUseBundleID(entry.BundleID)
		if err != nil || normalized != entry.BundleID || entry.Source != ComputerUseAppPolicySourceUser ||
			(entry.Decision != ComputerUseAppPolicyAsk && entry.Decision != ComputerUseAppPolicyBlocked) {
			return fmt.Errorf("invalid computer-use app policy entry")
		}
		if _, builtIn := builtInComputerUseBlockedSet[normalized]; builtIn {
			return fmt.Errorf("built-in computer-use app policy cannot be persisted")
		}
		if _, duplicate := s.entries[normalized]; duplicate {
			return fmt.Errorf("duplicate computer-use app policy entry")
		}
		s.entries[normalized] = entry.Decision
	}
	s.revision = *persisted.Revision
	return nil
}

func (s *ComputerUseAppPolicyStore) withFileLock(exclusive bool, fn func() error) error {
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open computer-use app policy lock: %w", err)
	}
	defer lockFile.Close()
	if exclusive {
		err = fslock.Lock(lockFile.Fd())
	} else {
		err = fslock.RLock(lockFile.Fd())
	}
	if err != nil {
		return fmt.Errorf("lock computer-use app policy: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())
	return fn()
}

func (s *ComputerUseAppPolicyStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create computer-use app policy directory: %w", err)
	}
	revision := s.revision
	entries := make([]ComputerUseAppPolicyEntry, 0, len(s.entries))
	persisted := persistedComputerUseAppPolicy{
		SchemaVersion: computerUseAppPolicySchemaVersion,
		Revision:      &revision,
		Entries:       &entries,
	}
	for bundleID, decision := range s.entries {
		entries = append(entries, ComputerUseAppPolicyEntry{BundleID: bundleID, Decision: decision, Source: ComputerUseAppPolicySourceUser})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].BundleID < entries[j].BundleID })
	data, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("encode computer-use app policy: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".computer-use-app-policy-*")
	if err != nil {
		return fmt.Errorf("create computer-use app policy temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace computer-use app policy: %w", err)
	}
	return nil
}

func decodeStrictComputerUseAppPolicyJSON(payload []byte, target any) error {
	if err := rejectDuplicateComputerUseAppPolicyMembers(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func rejectDuplicateComputerUseAppPolicyMembers(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := scanComputerUseAppPolicyJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func scanComputerUseAppPolicyJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object member %q", key)
			}
			seen[key] = struct{}{}
			if err := scanComputerUseAppPolicyJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid JSON object terminator")
		}
	case '[':
		for decoder.More() {
			if err := scanComputerUseAppPolicyJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid JSON array terminator")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
