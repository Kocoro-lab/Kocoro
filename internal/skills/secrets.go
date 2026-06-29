package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
	"github.com/zalando/go-keyring"
)

var validEnvKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// IsValidEnvKey checks if a key name is a valid environment variable name.
func IsValidEnvKey(key string) bool {
	return validEnvKey.MatchString(key)
}

// SecretsStore manages per-skill secret values (API keys).
// Values are stored in the macOS Keychain (encrypted); a plaintext index
// file (<shannonDir>/secrets-index.json) records which key names are
// configured per skill so ConfiguredKeys() can answer without triggering
// Keychain access prompts.
type SecretsStore struct {
	indexPath string
	lockPath  string
}

type secretsIndex struct {
	Skills map[string][]string `json:"skills"`
}

// keychainServiceName returns the macOS Keychain service identifier for a skill.
func keychainServiceName(skillName string) string {
	return "com.shannon.skill." + skillName
}

// NewSecretsStore returns a store rooted at <shannonDir>. Returns nil for
// empty shannonDir so callers can safely use it in test contexts.
func NewSecretsStore(shannonDir string) *SecretsStore {
	if shannonDir == "" {
		return nil
	}
	return &SecretsStore{
		indexPath: filepath.Join(shannonDir, "secrets-index.json"),
		lockPath:  filepath.Join(shannonDir, "secrets-index.json.lock"),
	}
}

// Get returns the secrets for a skill by reading values from the Keychain
// for each key listed in the index. Returns nil if no secrets are stored.
func (s *SecretsStore) Get(skillName string) map[string]string {
	if s == nil {
		return nil
	}
	keys := s.ConfiguredKeys(skillName)
	if len(keys) == 0 {
		return nil
	}
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		val, err := keychainRead(keychainServiceName(skillName), k)
		if err != nil {
			// Skip keys we can't read (e.g. user denied Keychain access
			// for a single item). The index lists the key but the value
			// is unavailable — treat as absent.
			continue
		}
		result[k] = val
	}
	return result
}

// Set writes secrets to the Keychain and updates the index.
// Existing keys are overwritten, new keys are added, unmentioned keys are preserved.
func (s *SecretsStore) Set(skillName string, secrets map[string]string) error {
	if s == nil {
		return nil
	}
	if len(secrets) == 0 {
		return nil
	}
	service := keychainServiceName(skillName)
	for k, v := range secrets {
		if err := keychainWrite(service, k, v); err != nil {
			return fmt.Errorf("keychain write %s/%s: %w", skillName, k, err)
		}
	}
	// Merge key names into the index.
	return s.modifyIndex(func(data *secretsIndex) {
		if data.Skills == nil {
			data.Skills = make(map[string][]string)
		}
		seen := map[string]bool{}
		for _, k := range data.Skills[skillName] {
			seen[k] = true
		}
		merged := append([]string(nil), data.Skills[skillName]...)
		for k := range secrets {
			if !seen[k] {
				merged = append(merged, k)
				seen[k] = true
			}
		}
		sort.Strings(merged)
		data.Skills[skillName] = merged
	})
}

// Delete removes all secrets for a skill.
func (s *SecretsStore) Delete(skillName string) error {
	if s == nil {
		return nil
	}
	service := keychainServiceName(skillName)
	keys := s.ConfiguredKeys(skillName)
	for _, k := range keys {
		// Best-effort delete: ignore "not found" errors.
		_ = keychainDelete(service, k)
	}
	return s.modifyIndex(func(data *secretsIndex) {
		delete(data.Skills, skillName)
	})
}

// DeleteKey removes a single secret key for a skill.
func (s *SecretsStore) DeleteKey(skillName, key string) error {
	if s == nil {
		return nil
	}
	_ = keychainDelete(keychainServiceName(skillName), key)
	return s.modifyIndex(func(data *secretsIndex) {
		if keys, ok := data.Skills[skillName]; ok {
			filtered := keys[:0]
			for _, k := range keys {
				if k != key {
					filtered = append(filtered, k)
				}
			}
			if len(filtered) == 0 {
				delete(data.Skills, skillName)
			} else {
				data.Skills[skillName] = filtered
			}
		}
	})
}

// ConfiguredKeys returns the sorted list of key names configured for a skill.
// Reads only the index file — does not access the Keychain.
func (s *SecretsStore) ConfiguredKeys(skillName string) []string {
	if s == nil {
		return nil
	}
	data := s.loadIndex()
	keys, ok := data.Skills[skillName]
	if !ok || len(keys) == 0 {
		return nil
	}
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}

// loadIndex reads the index file. Returns an empty struct if file doesn't exist.
func (s *SecretsStore) loadIndex() secretsIndex {
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		return secretsIndex{}
	}
	var idx secretsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return secretsIndex{}
	}
	return idx
}

// modifyIndex performs a read-modify-write cycle under an exclusive flock.
func (s *SecretsStore) modifyIndex(fn func(*secretsIndex)) error {
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return err
	}
	defer fslock.Unlock(lockFile.Fd())
	// Do NOT os.Remove the lock file — concurrent goroutines may flock
	// on different inodes if the file is deleted and recreated.

	data := s.loadIndex()
	fn(&data)

	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.indexPath + ".tmp"
	if err := os.WriteFile(tmp, jsonBytes, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath)
}

// keychainWrite stores a secret in the OS keychain. On macOS this uses
// `security` interactive mode internally (via zalando/go-keyring) so the
// password travels over stdin, never argv — process listings (`ps`,
// ProcessInformation) cannot observe the value.
func keychainWrite(service, account, password string) error {
	if err := keyring.Set(service, account, password); err != nil {
		return fmt.Errorf("keyring set %s/%s: %w", service, account, err)
	}
	return nil
}

// keychainRead retrieves a secret. Returns an error if the item is not found.
func keychainRead(service, account string) (string, error) {
	val, err := keyring.Get(service, account)
	if err != nil {
		return "", fmt.Errorf("keyring get %s/%s: %w", service, account, err)
	}
	return val, nil
}

// keychainDelete removes a secret. Returns nil if the item does not exist
// (idempotent delete).
func keychainDelete(service, account string) error {
	err := keyring.Delete(service, account)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keyring delete %s/%s: %w", service, account, err)
}
