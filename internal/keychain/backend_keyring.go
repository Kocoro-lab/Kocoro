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
// always-available OS service and never prompts. Linux does NOT use
// go-keyring at all (its Secret Service / dbus backend fails on headless
// hosts); Linux gets a file-backed store instead — see backend_linux.go /
// backend_file.go.
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

// NewOSStoreAt returns a Store backed by the OS credential store (macOS
// Keychain or Windows Credential Manager). dir is ignored on these platforms
// — the OS store is keyed by service/account, not a filesystem path — but is
// part of the signature so cross-platform callers share one entry point with
// the Linux file backend (backend_linux.go), which does use dir.
func NewOSStoreAt(_ string, logger *log.Logger) (*Store, error) {
	return NewStore(newOSBackend(), logger), nil
}
