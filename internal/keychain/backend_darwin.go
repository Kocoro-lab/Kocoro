//go:build darwin

package keychain

import (
	"errors"
	"log"

	"github.com/zalando/go-keyring"
)

// osBackend talks to the macOS Keychain via zalando/go-keyring, which
// invokes /usr/bin/security under the hood. Reads/writes can prompt the
// user the first time an unsigned daemon binary touches a service —
// "Always Allow" persists the trust per service.
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

// NewOSStore returns a Store backed by the macOS Keychain.
func NewOSStore(logger *log.Logger) (*Store, error) {
	return NewStore(newOSBackend(), logger), nil
}
