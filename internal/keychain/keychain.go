// Package keychain is the Kocoro daemon's wrapper over the OS credential
// store. The daemon uses it as the source of truth for the long-lived
// Cloud API key (sk_…) that Bootstrap reads on startup and Login writes
// after Cloud /auth/api-keys.
//
// Supported on macOS (zalando/go-keyring → Keychain) and Windows
// (→ Credential Manager). NewOSStore returns ErrUnsupportedPlatform on
// other GOOS values (Linux and friends); callers fall back to the legacy
// ~/.shannon/config.yaml api_key path.
package keychain

import (
	"errors"
	"fmt"
	"log"
	"runtime"
)

// Supported reports whether this platform has an OS credential store
// backing NewOSStore. It MUST stay in sync with the build tags on
// backend_keyring.go (darwin || windows) / backend_other.go. Runtime
// callers (config hydrate/save/migrate/setup) gate on this instead of
// checking runtime.GOOS directly, so the supported-platform set lives in
// one place.
func Supported() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

// Service / account identifiers. Constants — never assemble these inline.
const (
	// ServiceDaemonAPIKey holds the per-user api_key entries. Account is
	// the Cloud user_id (UUID). Multi-account support stores one entry per
	// user_id; the active one is named by ServiceDaemonState/current_user_id.
	ServiceDaemonAPIKey = "ai.kocoro.daemon.api_key"

	// ServiceDaemonState holds daemon state pointers (which user_id is active).
	ServiceDaemonState = "ai.kocoro.daemon.state"

	// AccountCurrentUser is the well-known account name under
	// ServiceDaemonState whose value is the active user_id.
	AccountCurrentUser = "current_user_id"

	// AccountLegacy is the placeholder account used when the yaml→Keychain
	// migration runs without a known user_id. AuthManager.Bootstrap calls
	// /auth/me with the legacy key, learns the real user_id, then renames
	// the entry under (ServiceDaemonAPIKey, <real_user_id>).
	AccountLegacy = "legacy"
)

// ErrUnsupportedPlatform is returned by NewOSStore on platforms without an
// OS credential store backend (currently anything other than macOS / Windows).
var ErrUnsupportedPlatform = errors.New("keychain: no OS credential store on this platform")

// ErrNotFound is returned when an entry does not exist. Callers normally
// treat this as "empty string" rather than an error condition.
var ErrNotFound = errors.New("keychain: not found")

// Backend is the low-level credential store. Production uses osBackend
// (darwin Keychain via zalando/go-keyring); tests use NewMemBackend.
type Backend interface {
	Read(service, account string) (string, error)
	Write(service, account, value string) error
	Delete(service, account string) error
}

// Store is the high-level api the AuthManager calls. It composes a Backend
// with the daemon's domain concepts (active user, legacy migration entry).
type Store struct {
	backend Backend
	log     *log.Logger
}

// NewStore builds a Store from a Backend. Tests inject NewMemBackend();
// production calls NewOSStore which delegates here with osBackend.
func NewStore(backend Backend, logger *log.Logger) *Store {
	if logger == nil {
		logger = log.Default()
	}
	return &Store{backend: backend, log: logger}
}

// Read returns the raw value at (service, account). Returns "" with no
// error if the entry is absent (callers rarely care about distinguishing).
func (s *Store) Read(service, account string) (string, error) {
	if s == nil || s.backend == nil {
		return "", ErrUnsupportedPlatform
	}
	v, err := s.backend.Read(service, account)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// Write stores value at (service, account), overwriting any prior value.
func (s *Store) Write(service, account, value string) error {
	if s == nil || s.backend == nil {
		return ErrUnsupportedPlatform
	}
	return s.backend.Write(service, account, value)
}

// Delete removes (service, account). Idempotent: absent → nil.
func (s *Store) Delete(service, account string) error {
	if s == nil || s.backend == nil {
		return ErrUnsupportedPlatform
	}
	err := s.backend.Delete(service, account)
	if err == nil || errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// CurrentUserID returns the active user_id, or "" if none is set.
func (s *Store) CurrentUserID() (string, error) {
	return s.Read(ServiceDaemonState, AccountCurrentUser)
}

// GetAPIKey returns the api_key for the currently-active user_id. If the
// active user is AccountLegacy (yaml migration just ran), returns the
// legacy entry — callers (Bootstrap) are expected to rename it after
// resolving the real user_id via /auth/me.
//
// Returns "" with no error when there is no active user or no key stored.
func (s *Store) GetAPIKey() (string, error) {
	userID, err := s.CurrentUserID()
	if err != nil {
		return "", err
	}
	if userID == "" {
		return "", nil
	}
	return s.Read(ServiceDaemonAPIKey, userID)
}

// GetActiveUserAndKey returns (user_id, api_key, err). Convenient for
// Bootstrap which needs both to decide whether the active entry is the
// legacy placeholder (needs rename) or a real user (just load).
func (s *Store) GetActiveUserAndKey() (string, string, error) {
	userID, err := s.CurrentUserID()
	if err != nil {
		return "", "", err
	}
	if userID == "" {
		return "", "", nil
	}
	key, err := s.Read(ServiceDaemonAPIKey, userID)
	if err != nil {
		return "", "", err
	}
	return userID, key, nil
}

// SetAPIKey stores (userID, key) AND updates current_user_id atomically
// from the caller's perspective. On any partial failure, the function
// returns the original error without attempting rollback — Keychain
// operations are not transactional and a best-effort cleanup risks
// destroying a valid entry that another caller just wrote.
func (s *Store) SetAPIKey(userID, key string) error {
	if userID == "" {
		return fmt.Errorf("keychain: SetAPIKey requires non-empty userID")
	}
	if key == "" {
		return fmt.Errorf("keychain: SetAPIKey requires non-empty key")
	}
	if err := s.Write(ServiceDaemonAPIKey, userID, key); err != nil {
		return fmt.Errorf("keychain: write api_key: %w", err)
	}
	if err := s.Write(ServiceDaemonState, AccountCurrentUser, userID); err != nil {
		return fmt.Errorf("keychain: write current_user_id: %w", err)
	}
	return nil
}

// RenameLegacy moves the api_key entry from AccountLegacy to (ServiceDaemonAPIKey,
// realUserID) and updates current_user_id. Called by AuthManager.Bootstrap
// after a yaml→Keychain migration when /auth/me resolves the real user.
func (s *Store) RenameLegacy(realUserID string) error {
	if realUserID == "" {
		return fmt.Errorf("keychain: RenameLegacy requires non-empty realUserID")
	}
	legacy, err := s.Read(ServiceDaemonAPIKey, AccountLegacy)
	if err != nil {
		return err
	}
	if legacy == "" {
		return nil
	}
	if err := s.SetAPIKey(realUserID, legacy); err != nil {
		return err
	}
	_ = s.Delete(ServiceDaemonAPIKey, AccountLegacy)
	return nil
}

// DeleteAPIKey removes the api_key for the current active user but preserves
// current_user_id. Callers that want a full sign-out should call
// ClearActiveUser after this, because clearing first loses the account name
// needed to delete the key entry. Idempotent.
func (s *Store) DeleteAPIKey() error {
	userID, err := s.CurrentUserID()
	if err != nil {
		return err
	}
	if userID == "" {
		return nil
	}
	return s.Delete(ServiceDaemonAPIKey, userID)
}

// ClearActiveUser removes current_user_id. Used by sign-out-full to
// fully forget the active session.
func (s *Store) ClearActiveUser() error {
	return s.Delete(ServiceDaemonState, AccountCurrentUser)
}
