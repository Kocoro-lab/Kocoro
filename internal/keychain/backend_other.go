//go:build !darwin && !windows && !linux

package keychain

import "log"

// NewOSStoreAt is not supported on platforms without a credential-store
// backend — i.e. everything except darwin (Keychain), windows (Credential
// Manager), and linux (file store, see backend_linux.go). Callers detect
// ErrUnsupportedPlatform and fall back to the legacy cfg.APIKey path.
func NewOSStoreAt(_ string, _ *log.Logger) (*Store, error) {
	return nil, ErrUnsupportedPlatform
}
