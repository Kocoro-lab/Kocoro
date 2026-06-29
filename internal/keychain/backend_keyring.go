//go:build darwin || windows

package keychain

import (
	"errors"
	"log"

	"github.com/zalando/go-keyring"
)

// osBackend talks to the OS credential store via zalando/go-keyring. On
// macOS it invokes /usr/bin/security (the Keychain) — reads/writes can
// prompt the user the first time an unsigned daemon binary touches a
// service, and "Always Allow" persists the trust per service. On Windows
// it uses the Windows Credential Manager (danieljoos/wincred), which is an
// always-available OS service and never prompts. Linux (go-keyring's
// Secret Service / dbus backend) is intentionally NOT enabled here — see
// backend_other.go — because headless servers usually have no Secret
// Service daemon and every read/write would fail at runtime.
type osBackend struct{}

func newOSBackend() Backend { return osBackend{} }

func (osBackend) Read(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	return v, nil
}

func (osBackend) Write(service, account, value string) error {
	return keyring.Set(service, account, value)
}

func (osBackend) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if err == nil {
		return nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// NewOSStore returns a Store backed by the OS credential store (macOS
// Keychain or Windows Credential Manager).
func NewOSStore(logger *log.Logger) (*Store, error) {
	return NewStore(newOSBackend(), logger), nil
}
