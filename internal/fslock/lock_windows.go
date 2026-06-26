//go:build windows

package fslock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// fullRange locks the entire file: offset 0, length 0xFFFFFFFFFFFFFFFF
// (low+high = ^uint32(0)). LockFileEx is mandatory and byte-range based, but
// since every caller locks the same full range it behaves as a whole-file
// advisory lock among our own processes — matching POSIX flock semantics.
func lockEx(fd uintptr, flags uint32) error {
	return windows.LockFileEx(
		windows.Handle(fd),
		flags,
		0,          // reserved
		^uint32(0), // nNumberOfBytesToLockLow
		^uint32(0), // nNumberOfBytesToLockHigh
		new(windows.Overlapped),
	)
}

// Lock acquires a blocking exclusive (write) lock over the whole file.
func Lock(fd uintptr) error { return lockEx(fd, windows.LOCKFILE_EXCLUSIVE_LOCK) }

// RLock acquires a blocking shared (read) lock over the whole file.
func RLock(fd uintptr) error { return lockEx(fd, 0) }

// TryLock attempts a non-blocking exclusive lock; it returns a non-nil error
// (IsWouldBlock reports true) when the lock is already held by someone else.
func TryLock(fd uintptr) error {
	return lockEx(fd, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY)
}

// Unlock releases a previously acquired lock.
func Unlock(fd uintptr) error {
	return windows.UnlockFileEx(windows.Handle(fd), 0, ^uint32(0), ^uint32(0), new(windows.Overlapped))
}

// IsWouldBlock reports whether err means "lock is currently held by another
// holder" — LockFileEx with LOCKFILE_FAIL_IMMEDIATELY fails with
// ERROR_LOCK_VIOLATION under contention. (We open lock files with synchronous
// handles, so ERROR_IO_PENDING — the async-overlapped "operation queued"
// status — never arises and is deliberately NOT treated as contention.)
func IsWouldBlock(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
