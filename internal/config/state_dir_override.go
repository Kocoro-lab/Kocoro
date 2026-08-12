package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

var processShannonDir struct {
	sync.RWMutex
	dir string
}

var processCredentialStoreDisabled struct {
	sync.RWMutex
	disabled bool
}

// SetShannonDirOverrideForProcess redirects state for the current process only.
// It is a startup-only hook for the hidden live-E2E daemon mode; ordinary CLI
// commands cannot inherit it from a shell environment.
func SetShannonDirOverrideForProcess(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("state directory must be an absolute path")
	}
	dir = filepath.Clean(dir)
	processShannonDir.Lock()
	defer processShannonDir.Unlock()
	if processShannonDir.dir != "" && processShannonDir.dir != dir {
		return fmt.Errorf("state directory override is already set")
	}
	processShannonDir.dir = dir
	return nil
}

func shannonDirProcessOverride() string {
	processShannonDir.RLock()
	defer processShannonDir.RUnlock()
	return processShannonDir.dir
}

// DisableCredentialStoreForProcess prevents config loading from reading or
// migrating credentials through the OS credential store. It is a startup-only
// hook for the hidden isolated-daemon E2E mode, where the API key is supplied
// through an inherited pipe and must remain process-memory-only.
func DisableCredentialStoreForProcess() {
	processCredentialStoreDisabled.Lock()
	processCredentialStoreDisabled.disabled = true
	processCredentialStoreDisabled.Unlock()
}

func credentialStoreDisabledForProcess() bool {
	processCredentialStoreDisabled.RLock()
	defer processCredentialStoreDisabled.RUnlock()
	return processCredentialStoreDisabled.disabled
}
