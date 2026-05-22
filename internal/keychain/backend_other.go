//go:build !darwin

package keychain

import "log"

// NewOSStore is not supported on non-darwin platforms. Callers detect
// ErrUnsupportedPlatform and fall back to the legacy cfg.APIKey path.
func NewOSStore(_ *log.Logger) (*Store, error) {
	return nil, ErrUnsupportedPlatform
}
