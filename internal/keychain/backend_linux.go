//go:build linux

package keychain

import "log"

// NewOSStoreAt returns a file-backed Store on Linux, persisting credentials at
// <dir>/credentials.json (mode 0600). dir is the daemon's shannon dir
// (config.ShannonDir()), passed in by the caller so the credential file and
// the yaml→store migration always agree on a location. keychain must NOT
// import config (that is an import cycle — config already imports keychain),
// so the dir is plumbed in rather than re-derived here; this also lets tests
// point it at a temp dir.
//
// It ALWAYS succeeds unless dir cannot be created, which is already fatal for
// the daemon (the rest of ~/.shannon is unusable too). That keeps the
// Supported()==(NewOSStore err==nil) invariant honest: unlike go-keyring's
// Secret Service path there is no lazy runtime failure mode. Secret Service /
// D-Bus is deliberately not used — see backend_file.go for the rationale.
func NewOSStoreAt(dir string, logger *log.Logger) (*Store, error) {
	be, err := newFileBackend(dir)
	if err != nil {
		return nil, err
	}
	return NewStore(be, logger), nil
}
