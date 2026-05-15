package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Round-trip: emit notifications under a persister, then load + restore in a
// fresh EventBus and verify history is intact with monotonic IDs.
func TestNotifStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, restored, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("newNotifStore: %v", err)
	}
	if restored != nil {
		t.Fatalf("expected no restored events on first open, got %d", len(restored))
	}

	bus := NewEventBus()
	bus.SetNotifPersister(store.Append)
	// Drain subscribers so notification-class events count as delivered and
	// also exercise the in-memory ring path. Persister fires regardless of
	// delivery — that's the whole point of the new store.
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)
	go func() {
		for range ch {
		}
	}()

	for i := 0; i < 5; i++ {
		bus.EmitTo(Event{
			Type:    EventNotification,
			Payload: json.RawMessage(`{"title":"hi"}`),
		})
		bus.EmitTo(Event{
			Type:    EventAgentReply, // non-notification, must NOT land on disk
			Payload: json.RawMessage(`{}`),
		})
	}

	// Simulate a daemon restart: drop bus, reload from disk.
	store2, restored2, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(restored2); got != 5 {
		t.Fatalf("expected 5 restored notifications, got %d", got)
	}

	bus2 := NewEventBus()
	bus2.RestoreNotifications(restored2)
	bus2.SetNotifPersister(store2.Append)
	// Subscribe before emitting so the EventNotification below counts as
	// delivered. Undelivered notifications are intentionally skipped (their
	// banners already fired via osascript fallback in the notify tool path);
	// see EmitTo's notification-history retention rule.
	ch2 := bus2.Subscribe()
	defer bus2.Unsubscribe(ch2)
	go func() {
		for range ch2 {
		}
	}()

	got := bus2.Notifications(0, nil, 0)
	if len(got) != 5 {
		t.Fatalf("expected 5 in-memory notifications after restore, got %d", len(got))
	}
	// IDs must remain monotonic — the last restored ID was 9 (we emitted 10
	// events, notifications were at IDs 1,3,5,7,9). A new emit should pick
	// up at 10 or higher.
	maxBefore := got[len(got)-1].ID
	bus2.EmitTo(Event{Type: EventNotification, Payload: json.RawMessage(`{}`)})
	got2 := bus2.Notifications(maxBefore, nil, 0)
	if len(got2) != 1 {
		t.Fatalf("expected 1 new notification after restore, got %d", len(got2))
	}
	if got2[0].ID <= maxBefore {
		t.Fatalf("expected new event ID > %d, got %d", maxBefore, got2[0].ID)
	}
}

// A corrupt mid-line (e.g. crash during write) must not stop load — the
// scanner should skip the bad line and return the surrounding events.
func TestNotifStoreCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, notifFileName)

	good := `{"id":1,"type":"notification","payload":{"title":"ok"}}`
	corrupt := `{"id":2,"type":"notif`
	good2 := `{"id":3,"type":"notification","payload":{"title":"ok2"}}`
	content := good + "\n" + corrupt + "\n" + good2 + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, restored, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 valid events (corrupt skipped), got %d", len(restored))
	}
	if restored[0].ID != 1 || restored[1].ID != 3 {
		t.Fatalf("unexpected ID order: %d, %d", restored[0].ID, restored[1].ID)
	}
}

// When the on-disk log exceeds capacity, load must trim to the most recent
// notifRingSize entries AND atomically rewrite the file so it stops growing.
func TestNotifStoreCompactionOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, notifFileName)

	var sb strings.Builder
	total := notifRingSize + 100
	for i := 1; i <= total; i++ {
		// json.Marshal preserves field order from struct definition; build by
		// hand to keep the test independent of encoding/json output shape.
		sb.WriteString(`{"id":`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(`,"type":"notification","payload":{}}`)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, restored, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(restored) != notifRingSize {
		t.Fatalf("expected trim to %d entries, got %d", notifRingSize, len(restored))
	}
	// Most-recent kept: first restored should be id=101 (total-notifRingSize+1).
	wantFirst := uint64(total - notifRingSize + 1)
	if restored[0].ID != wantFirst {
		t.Fatalf("expected first id=%d after trim, got %d", wantFirst, restored[0].ID)
	}
	if restored[len(restored)-1].ID != uint64(total) {
		t.Fatalf("expected last id=%d, got %d", total, restored[len(restored)-1].ID)
	}

	// File on disk must have been rewritten to the trimmed set.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after compact: %v", err)
	}
	lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1
	if lines != notifRingSize {
		t.Fatalf("expected file rewritten to %d lines, got %d", notifRingSize, lines)
	}
}

// A line that exceeds maxLineSize must be skipped without losing the
// surrounding good events. This guards against an unusually large
// approval_request payload wedging history load.
func TestNotifStoreOversizeLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, notifFileName)

	good := `{"id":1,"type":"notification","payload":{"title":"ok"}}`
	// Build an oversize line: > maxLineSize bytes of valid JSON-ish junk.
	huge := strings.Repeat("x", maxLineSize+1024)
	oversize := `{"id":2,"type":"notification","payload":"` + huge + `"}`
	good2 := `{"id":3,"type":"notification","payload":{"title":"ok2"}}`
	content := good + "\n" + oversize + "\n" + good2 + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, restored, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 events (oversize skipped), got %d", len(restored))
	}
	if restored[0].ID != 1 || restored[1].ID != 3 {
		t.Fatalf("unexpected IDs after oversize skip: %d, %d", restored[0].ID, restored[1].ID)
	}
}

// Counter-based assertion: the persister hook itself must not fire for an
// undelivered EventNotification. Earlier tests check the on-disk file
// content, but a future refactor could conceivably break the in-memory
// branch while accidentally keeping the disk side passing via some other
// path — this test pins the contract at the hook boundary.
func TestEventBusUndeliveredNotificationDoesNotCallPersister(t *testing.T) {
	bus := NewEventBus()
	var persistCalls int
	var persistedTypes []string
	bus.SetNotifPersister(func(evt Event) {
		persistCalls++
		persistedTypes = append(persistedTypes, evt.Type)
	})
	// No subscribers attached.

	bus.EmitTo(Event{Type: EventNotification, Payload: json.RawMessage(`{}`)})
	bus.EmitTo(Event{Type: EventApprovalRequest, Payload: json.RawMessage(`{}`)})
	bus.EmitTo(Event{Type: EventHeartbeatAlert, Payload: json.RawMessage(`{}`)})
	bus.EmitTo(Event{Type: EventAgentError, Payload: json.RawMessage(`{}`)})

	if persistCalls != 3 {
		t.Fatalf("expected 3 persister calls (EventNotification skipped), got %d (types=%v)",
			persistCalls, persistedTypes)
	}
	for _, tt := range persistedTypes {
		if tt == EventNotification {
			t.Fatalf("undelivered EventNotification reached persister hook: types=%v", persistedTypes)
		}
	}
}

// EventNotification with zero subscribers fires the osascript fallback in
// the notify tool, so persisting it would let Desktop re-banner the same
// notification on next launch. The notifRing must stay empty in this case
// while other notification-class events (which have no osascript fallback)
// continue to be retained.
func TestEventBusUndeliveredNotificationNotPersisted(t *testing.T) {
	dir := t.TempDir()
	store, _, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("newNotifStore: %v", err)
	}

	bus := NewEventBus()
	bus.SetNotifPersister(store.Append)
	// No subscribers: delivered == 0 for both emits below.

	bus.EmitTo(Event{Type: EventNotification, Payload: json.RawMessage(`{}`)})
	bus.EmitTo(Event{Type: EventApprovalRequest, Payload: json.RawMessage(`{}`)})
	bus.EmitTo(Event{Type: EventHeartbeatAlert, Payload: json.RawMessage(`{}`)})
	bus.EmitTo(Event{Type: EventAgentError, Payload: json.RawMessage(`{}`)})

	got := bus.Notifications(0, nil, 0)
	if len(got) != 3 {
		t.Fatalf("expected 3 retained (notification dropped, others kept), got %d", len(got))
	}
	for _, e := range got {
		if e.Type == EventNotification {
			t.Fatalf("undelivered EventNotification should not be in notifRing: %+v", e)
		}
	}

	// Disk must also reflect this — only 3 lines written.
	_, restored, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(restored) != 3 {
		t.Fatalf("expected 3 persisted (notification dropped on disk too), got %d", len(restored))
	}
}

// After notifCompactEvery successful appends, the on-disk log must be
// compacted in place so a long-lived daemon can't accrue unbounded growth.
func TestNotifStoreOpportunisticCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, notifFileName)

	store, _, err := newNotifStore(dir)
	if err != nil {
		t.Fatalf("newNotifStore: %v", err)
	}

	// Drive exactly notifCompactEvery appends; the final Append should fire
	// compaction, trimming the file back to notifRingSize lines.
	for i := 1; i <= notifCompactEvery; i++ {
		store.Append(Event{
			ID:      uint64(i),
			Type:    EventNotification,
			Payload: json.RawMessage(`{}`),
		})
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1
	if lines != notifRingSize {
		t.Fatalf("expected file compacted to %d lines, got %d", notifRingSize, lines)
	}
}

// Even when load returns an error (e.g. compaction rewrite failed because the
// directory became read-only mid-trim), NewServer must still restore the
// events that were successfully parsed AND keep writing new ones. The
// previous behaviour wiped /notifications for the entire daemon lifetime
// whenever load returned err != nil.
func TestNotifStoreReturnsPartialEventsOnRewriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, notifFileName)

	// Seed with enough events to trigger compaction trim.
	var sb strings.Builder
	total := notifRingSize + 10
	for i := 1; i <= total; i++ {
		sb.WriteString(`{"id":` + strconv.Itoa(i) + `,"type":"notification","payload":{}}` + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Make the directory read-only so the temp+rename compaction fails. The
	// read itself succeeds because we already have the file handle.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore so TempDir cleanup works

	store, restored, err := newNotifStore(dir)
	if err == nil {
		// Some platforms (e.g. running as root) ignore the chmod. Skip
		// instead of fabricating a false pass.
		t.Skip("rewrite did not fail on this platform; test cannot exercise the partial-success path")
	}
	if store == nil {
		t.Fatalf("expected a usable store even on partial-failure, got nil")
	}
	if len(restored) != notifRingSize {
		t.Fatalf("expected partial restored events == %d, got %d", notifRingSize, len(restored))
	}
}

