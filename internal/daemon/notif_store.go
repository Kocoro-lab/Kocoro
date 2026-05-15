package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// notifFileName holds the on-disk notification history. Lives under ShannonDir
// so it follows the same backup / cleanup semantics as agents/ and sessions/.
const notifFileName = "notifications.jsonl"

// notifStore is the single-writer file appender for notification-class events.
// The daemon is single-instance (pidfile flock in cmd/daemon.go), so we only
// need an in-process mutex to serialize writes against compaction.
type notifStore struct {
	path    string
	mu      sync.Mutex
	errOnce sync.Once // fires the first I/O failure to logs, then stays silent
}

// newNotifStore opens (or initialises) the on-disk history file and returns
// the trimmed-to-capacity events that should rehydrate the in-memory ring.
// If the file does not yet exist the returned slice is nil; a write on the
// first emit will create it.
func newNotifStore(shannonDir string) (*notifStore, []Event, error) {
	if shannonDir == "" {
		return nil, nil, nil
	}
	path := filepath.Join(shannonDir, notifFileName)
	events, err := loadAndCompactNotifications(path, notifRingSize)
	// Always return a usable store even on partial-failure paths (e.g. read
	// succeeded but compaction rewrite failed). Append has its own one-shot
	// error log via errOnce, so subsequent writes can still try to recover —
	// versus silently disabling persistence for the rest of the daemon's
	// lifetime, which would lose every new notification.
	return &notifStore{path: path}, events, err
}

// loadAndCompactNotifications reads the JSONL log, keeps the most recent
// `keep` entries, and rewrites the file atomically if any were trimmed.
// Corrupt lines are skipped silently — we never want a partial-write line
// from a previous crash to wedge daemon startup. Lines exceeding the buffer
// growth limit are also skipped (read-until-newline-then-drop), so a single
// oversize approval_request payload cannot lose the surrounding history.
func loadAndCompactNotifications(path string, keep int) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)
	var all []Event
	for {
		line, err := readJSONLLine(br)
		if line != nil {
			var evt Event
			if jerr := json.Unmarshal(line, &evt); jerr == nil {
				all = append(all, evt)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Hard read error (not just an oversize-line skip, which
			// readJSONLLine swallows internally). Return what we have so the
			// caller can still surface history accumulated so far.
			return all, err
		}
	}

	trimmed := false
	if keep > 0 && len(all) > keep {
		all = all[len(all)-keep:]
		trimmed = true
	}
	if trimmed {
		if err := rewriteNotifications(path, all); err != nil {
			return all, err
		}
	}
	return all, nil
}

// readJSONLLine returns the next newline-delimited record from br. Lines up
// to maxLineSize are returned as-is; oversize lines are consumed up to the
// next '\n' (or EOF) and reported as nil + nil error so the caller skips
// them without losing position in the stream. EOF is returned with whatever
// trailing data was read.
//
// maxLineSize bounds the per-line memory footprint we'll commit to a single
// payload. 4 MB matches publish_to_web's practical args ceiling; raising it
// is cheap if some future tool needs more.
const maxLineSize = 4 * 1024 * 1024

func readJSONLLine(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	dropping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if !dropping {
				if buf.Len()+len(chunk) > maxLineSize {
					// Crossed the cap mid-line: discard what we accumulated
					// and any further fragments until we hit '\n'.
					dropping = true
					buf.Reset()
				} else {
					buf.Write(chunk)
				}
			}
		}
		if err == nil {
			// Found terminator. Strip trailing '\n' (and optional '\r').
			out := buf.Bytes()
			if n := len(out); n > 0 && out[n-1] == '\n' {
				out = out[:n-1]
				if n2 := len(out); n2 > 0 && out[n2-1] == '\r' {
					out = out[:n2-1]
				}
			}
			if dropping || len(out) == 0 {
				return nil, nil
			}
			return out, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Line longer than the bufio reader buffer — keep reading more
			// fragments. The maxLineSize check above caps total accumulated
			// growth so we can't OOM on a pathological input.
			continue
		}
		// Final read (typically io.EOF). Return any trailing data without
		// a newline, then signal EOF on the next call via empty buffer.
		out := buf.Bytes()
		if dropping || len(out) == 0 {
			return nil, err
		}
		return out, err
	}
}

func rewriteNotifications(path string, events []Event) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Append serialises `evt` and writes a single line to the log. Best-effort:
// I/O failures don't block event delivery, but the FIRST failure of each
// daemon lifetime is logged so a systemic problem (read-only mount, ENOSPC,
// chmod gone wrong) doesn't go entirely silent. Subsequent failures stay
// quiet to avoid log spam.
func (s *notifStore) Append(evt Event) {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.reportErr("open", err)
		return
	}
	defer f.Close()
	b, err := json.Marshal(evt)
	if err != nil {
		s.reportErr("marshal", err)
		return
	}
	if _, err := f.Write(b); err != nil {
		s.reportErr("write", err)
		return
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		s.reportErr("write", err)
	}
}

func (s *notifStore) reportErr(op string, err error) {
	s.errOnce.Do(func() {
		log.Printf("daemon: notification history %s failed (further errors silenced): %v", op, err)
	})
}
