package daemon

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

// PIDFile manages a flock-guarded PID file for daemon single-instance enforcement.
// The flock is held for the entire daemon lifetime. On crash, the OS releases
// the lock automatically — no stale PID file problem.
type PIDFile struct {
	path string
	file *os.File
}

// AcquirePIDFile attempts to acquire an exclusive flock on the PID file at path.
// If another daemon holds the lock, it returns an error with the existing PID.
// On success, the current process PID is written to the file and the lock is held
// until Close() is called (or the process exits).
func AcquirePIDFile(path string) (*PIDFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open pid file: %w", err)
	}

	// Non-blocking exclusive lock.
	if err := fslock.TryLock(f.Fd()); err != nil {
		// Lock held by another process — read existing PID for the error message.
		existingPID := readPIDFromFile(f)
		f.Close()
		if existingPID > 0 {
			return nil, fmt.Errorf("daemon already running (PID %d)", existingPID)
		}
		return nil, fmt.Errorf("daemon already running (could not read PID)")
	}

	// Lock acquired — write our PID.
	if err := f.Truncate(0); err != nil {
		fslock.Unlock(f.Fd())
		f.Close()
		return nil, fmt.Errorf("truncate pid file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		fslock.Unlock(f.Fd())
		f.Close()
		return nil, fmt.Errorf("seek pid file: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		fslock.Unlock(f.Fd())
		f.Close()
		return nil, fmt.Errorf("write pid: %w", err)
	}
	if err := f.Sync(); err != nil {
		fslock.Unlock(f.Fd())
		f.Close()
		return nil, fmt.Errorf("sync pid file: %w", err)
	}

	return &PIDFile{path: path, file: f}, nil
}

// Close removes the PID file, releases the flock, and closes the file descriptor.
// File is removed while lock is still held to prevent a window where the file
// exists but is unlocked.
func (p *PIDFile) Close() {
	if p.file == nil {
		return
	}
	if runtime.GOOS == "windows" {
		// Windows can't delete a file that still has an open handle. Release
		// the lock and close first, then remove. If another instance grabbed
		// the file in the window after Close, os.Remove fails harmlessly with a
		// sharing violation and leaves that instance's pidfile intact.
		fslock.Unlock(p.file.Fd())
		p.file.Close()
		os.Remove(p.path)
	} else {
		// POSIX: remove while the lock is still held so a newly-starting
		// instance can't acquire the lock on a file we're about to unlink
		// (single-instance handoff). unlink-while-open is allowed here.
		os.Remove(p.path)
		fslock.Unlock(p.file.Fd())
		p.file.Close()
	}
	p.file = nil
}

// ReadPID reads the PID from a PID file without acquiring a lock.
// Returns 0 and an error if the file doesn't exist or contains invalid data.
func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file content: %w", err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid: %d", pid)
	}
	return pid, nil
}

// IsLocked checks whether the PID file at path is currently locked by another process.
// Returns the PID if locked, 0 if not locked or file doesn't exist.
//
// The handle is opened O_RDWR (not O_RDONLY): on Windows fslock.TryLock maps to
// LockFileEx(LOCKFILE_EXCLUSIVE_LOCK), which requires a writable handle — a
// read-only handle fails with ERROR_ACCESS_DENIED even when no one holds the
// lock, which would make this probe always report "locked". POSIX flock works
// on any fd, so this is harmless there.
func IsLocked(path string) (int, bool) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	// Try non-blocking lock — if we get it, no one else holds it.
	if err := fslock.TryLock(f.Fd()); err == nil {
		fslock.Unlock(f.Fd())
		return 0, false
	}

	pid := readPIDFromFile(f)
	return pid, true
}

func readPIDFromFile(f *os.File) int {
	if _, err := f.Seek(0, 0); err != nil {
		return 0
	}
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
