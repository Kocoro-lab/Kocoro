// Package sync — batcher: marshal-once + cap arithmetic for grouping candidate
// sessions into upload batches. Single-session size violations and loader
// errors are recorded directly on the marker as permanent failures; transient
// failure recording (transport errors and per-id rejections) lives in the
// uploader/run orchestration where the response is observed.
package sync

import (
	"context"
	"encoding/json"
	"time"
)

type MarshaledSession struct {
	Candidate Candidate
	AgentName string
	SessionID string
	JSON      json.RawMessage
	SizeBytes uint64
}

type Batch struct {
	Sessions  []MarshaledSession
	SizeBytes uint64
}

// SessionLoader returns the marshaled session JSON bytes for (dir, id).
// Returning an error puts the session into marker.Failed with reason load_error.
type SessionLoader func(dir, id string) ([]byte, error)

// BuildBatches packs candidates into batches under cfg.BatchMaxSessions and
// cfg.BatchMaxBytes caps. Mutates marker in place to record load errors and
// single-session size rejections.
func BuildBatches(ctx context.Context, cands []Candidate, loader SessionLoader, cfg Config, marker *Marker, now time.Time) ([]Batch, error) {
	if marker.Failed == nil {
		marker.Failed = map[string]FailedEntry{}
	}

	var batches []Batch
	cur := Batch{}

	flush := func() {
		if len(cur.Sessions) > 0 {
			batches = append(batches, cur)
			cur = Batch{}
		}
	}

	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return batches, err
		}

		body, err := loader(c.Dir, c.SessionID)
		if err != nil {
			recordFailed(marker, c.SessionID, "load_error", 0, c.UpdatedAt, now)
			continue
		}
		// Strip assistant `thinking` / `redacted_thinking` content blocks BEFORE
		// the size limit check. Privacy: thinking content can carry sensitive
		// intermediate reasoning the user never sees; the sync endpoint uses
		// sessions for cross-device resume and doesn't need them. Putting the
		// strip before the size check also means a session whose ON-DISK size
		// would exceed SingleSessionMaxBytes only because of thinking will now
		// fit the cap after strip, instead of being silently rejected.
		// Parse-failure path: helper returns the original body + err; we log
		// and continue with the unstripped body so a single bad file doesn't
		// block the whole sync run (Cloud will surface its own error if the
		// JSON is genuinely malformed).
		if stripped, stripErr := stripThinkingFromSessionJSON(body); stripErr == nil {
			body = stripped
		}
		size := uint64(len(body))

		if cfg.SingleSessionMaxBytes > 0 && size > uint64(cfg.SingleSessionMaxBytes) {
			recordFailed(marker, c.SessionID, "size_limit_exceeded", size, c.UpdatedAt, now)
			continue
		}

		ms := MarshaledSession{
			Candidate: c,
			AgentName: c.AgentName,
			SessionID: c.SessionID,
			JSON:      body,
			SizeBytes: size,
		}

		// Will adding this exceed either cap?
		if len(cur.Sessions) >= cfg.BatchMaxSessions {
			flush()
		}
		if cur.SizeBytes+ms.SizeBytes > uint64(cfg.BatchMaxBytes) && len(cur.Sessions) > 0 {
			flush()
		}

		cur.Sessions = append(cur.Sessions, ms)
		cur.SizeBytes += ms.SizeBytes
	}
	flush()
	return batches, nil
}

// recordFailed merges or creates a marker.Failed entry. observedUpdatedAt is
// the candidate's Session.UpdatedAt at the time of this attempt — required so
// the scanner's no-churn rule can detect whether the session has been edited
// since this failure was recorded.
//
// Permanent reasons get NextAttemptAt = nil; transient reasons get backoff
// (handled by caller in sync.Run when applying upload responses; here we only
// handle the local-permanent reasons load_error and size_limit_exceeded).
func recordFailed(m *Marker, id, reason string, sizeBytes uint64, observedUpdatedAt, now time.Time) {
	prev, existed := m.Failed[id]
	if !existed {
		prev = FailedEntry{
			Reason:         reason,
			Category:       ClassifyReason(reason),
			Attempts:       0,
			FirstAttemptAt: now,
		}
	} else {
		prev.Reason = reason
		prev.Category = ClassifyReason(reason)
	}
	prev.Attempts++
	prev.LastAttemptAt = now
	prev.LastObservedUpdatedAt = observedUpdatedAt
	prev.SizeBytes = sizeBytes
	if prev.Category == CategoryPermanent {
		prev.NextAttemptAt = nil
	}
	m.Failed[id] = prev
}
