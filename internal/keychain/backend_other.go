//go:build !darwin && !windows

package keychain

import "log"

// NewOSStore is not supported on platforms without an OS credential store
// backend (i.e. Linux and others; go-keyring's Secret Service path is not
// enabled — see backend_keyring.go). Callers detect ErrUnsupportedPlatform
// and fall back to the legacy cfg.APIKey path.
func NewOSStore(_ *log.Logger) (*Store, error) {
	return nil, ErrUnsupportedPlatform
}
