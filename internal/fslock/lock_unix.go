//go:build !windows

// Package fslock provides a tiny cross-platform advisory file-lock helper so
// callers don't reach for syscall.Flock directly (POSIX-only, breaks the
// Windows build). The POSIX implementation is a thin wrapper over flock(2);
// the Windows implementation (lock_windows.go) uses LockFileEx. Locks are
// released on Unlock or automatically when the underlying fd/handle closes
// (process exit), so callers that hold a lock for their whole lifetime —
// e.g. the daemon single-instance pidfile — behave identically on both.
package fslock

import (
	"errors"
	"syscall"
)

// Lock acquires a blocking exclusive (write) lock over the whole file.
func Lock(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_EX) }

// RLock acquires a blocking shared (read) lock over the whole file.
func RLock(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_SH) }

// TryLock attempts a non-blocking exclusive lock; it returns a non-nil error
// (IsWouldBlock reports true) when the lock is already held by someone else.
func TryLock(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB) }

// Unlock releases a previously acquired lock.
func Unlock(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_UN) }

// IsWouldBlock reports whether err means "lock is currently held by another
// holder" — the contention signal from a non-blocking TryLock.
func IsWouldBlock(err error) bool { return errors.Is(err, syscall.EWOULDBLOCK) }
