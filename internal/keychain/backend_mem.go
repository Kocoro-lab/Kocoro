package keychain

import "sync"

// MemBackend is an in-memory Backend used by tests and (potentially) by
// non-darwin platforms that opt in to a non-persistent fallback. It is
// safe for concurrent use.
type MemBackend struct {
	mu   sync.Mutex
	data map[string]string
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{data: map[string]string{}}
}

func memKey(service, account string) string {
	return service + "\x00" + account
}

func (m *MemBackend) Read(service, account string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[memKey(service, account)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *MemBackend) Write(service, account, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[memKey(service, account)] = value
	return nil
}

func (m *MemBackend) Delete(service, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(service, account)
	if _, ok := m.data[k]; !ok {
		return ErrNotFound
	}
	delete(m.data, k)
	return nil
}

// Snapshot returns a copy of the underlying map for assertion in tests.
func (m *MemBackend) Snapshot() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out
}
