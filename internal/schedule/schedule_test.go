package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateAndList(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("ops-bot", "0 9 * * *", "check prod health", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}
	list, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d schedules, want 1", len(list))
	}
	if list[0].Agent != "ops-bot" {
		t.Errorf("agent = %q, want %q", list[0].Agent, "ops-bot")
	}
	if list[0].Cron != "0 9 * * *" {
		t.Errorf("cron = %q, want %q", list[0].Cron, "0 9 * * *")
	}
}

func TestCreateRejectsInvalidCron(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	_, err := mgr.Create("bot", "not-a-cron", "task", false)
	if err == nil {
		t.Fatal("expected error for invalid cron")
	}
}

func TestCreateRejectsInvalidAgentName(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	_, err := mgr.Create("../evil", "0 9 * * *", "task", false)
	if err == nil {
		t.Fatal("expected error for invalid agent name")
	}
}

func TestCreateAcceptsEmptyAgent(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("", "0 9 * * *", "task", false)
	if err != nil {
		t.Fatalf("Create with empty agent: %v", err)
	}
	list, _ := mgr.List()
	if list[0].Agent != "" {
		t.Errorf("agent = %q, want empty", list[0].Agent)
	}
	_ = id
}

func TestCreateSupportsCronSyntax(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	crons := []string{
		"*/5 * * * *",
		"0 9-17 * * 1-5",
		"0 9 * * 1,3,5",
		"30 */2 * * *",
	}
	for _, c := range crons {
		_, err := mgr.Create("", c, "task", false)
		if err != nil {
			t.Errorf("expected valid cron %q, got error: %v", c, err)
		}
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := mgr.Create("bot", "0 9 * * *", "task", false)
	err := mgr.Remove(id)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list, _ := mgr.List()
	if len(list) != 0 {
		t.Fatalf("got %d schedules after remove, want 0", len(list))
	}
}

func TestRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	err := mgr.Remove("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent id")
	}
}

func TestUpdate(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := mgr.Create("bot", "0 9 * * *", "old prompt", false)
	err := mgr.Update(id, &UpdateOpts{Prompt: strPtr("new prompt")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	list, _ := mgr.List()
	if list[0].Prompt != "new prompt" {
		t.Errorf("prompt = %q, want %q", list[0].Prompt, "new prompt")
	}
}

func TestUpdateRejectsInvalidCron(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := mgr.Create("bot", "0 9 * * *", "task", false)
	bad := "not-valid"
	err := mgr.Update(id, &UpdateOpts{Cron: &bad})
	if err == nil {
		t.Fatal("expected error for invalid cron update")
	}
}

func TestEnableDisable(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := mgr.Create("bot", "0 9 * * *", "task", false)
	if err := mgr.Update(id, &UpdateOpts{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	list, _ := mgr.List()
	if list[0].Enabled {
		t.Error("expected disabled")
	}
}

func TestConcurrentCreates(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.Create("bot", "0 9 * * *", "task", false)
		}()
	}
	wg.Wait()
	list, _ := mgr.List()
	if len(list) != 10 {
		t.Errorf("got %d schedules, want 10", len(list))
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestSaveLoadContextRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("bot", "0 9 * * *", "task", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	msgs := []ContextMessage{
		{Role: "user", Content: "why am I creating this?"},
		{Role: "assistant", Content: "so you get reminded each morning"},
	}
	if err := mgr.SaveContext(id, msgs); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}

	if !mgr.HasContext(id) {
		t.Fatal("HasContext = false after SaveContext")
	}

	got, err := mgr.LoadContext(id)
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if len(got) != 2 || got[0].Content != msgs[0].Content || got[1].Role != "assistant" {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func TestSaveContextIsAtomic(t *testing.T) {
	// Atomic writes never leave the final file in a half-written state.
	// We can't reliably inject a crash mid-write without fault injection,
	// so instead we verify the write path uses temp+rename by checking
	// that after a successful write no temp files are left behind and
	// the final file permissions are 0600.
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("bot", "0 9 * * *", "task", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.SaveContext(id, []ContextMessage{{Role: "user", Content: "hello"}}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	entries, err := os.ReadDir(mgr.contextDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// Exactly one file, no leftover .tmp files.
	if len(entries) != 1 {
		t.Fatalf("expected 1 file in context dir, got %d: %v", len(entries), entries)
	}
	name := entries[0].Name()
	if name != id+".json" {
		t.Errorf("unexpected file: %q", name)
	}
	info, err := os.Stat(filepath.Join(mgr.contextDir(), name))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("file perm = %v, want 0600", mode)
	}
}

func TestSaveContextEmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("bot", "0 9 * * *", "task", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.SaveContext(id, nil); err != nil {
		t.Fatalf("SaveContext(nil): %v", err)
	}
	if mgr.HasContext(id) {
		t.Error("HasContext = true after SaveContext(nil)")
	}
}

func TestUpdateClearsContextOnPromptChange(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("bot", "0 9 * * *", "check prod", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.SaveContext(id, []ContextMessage{{Role: "user", Content: "old intent"}}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	if !mgr.HasContext(id) {
		t.Fatal("precondition: expected context to exist")
	}

	// Changing the prompt invalidates the captured "why" — sidecar must go.
	newPrompt := "check staging instead"
	if err := mgr.Update(id, &UpdateOpts{Prompt: &newPrompt}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if mgr.HasContext(id) {
		t.Error("context sidecar should have been removed after prompt change")
	}
}

func TestUpdatePreservesContextWhenPromptUnchanged(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("bot", "0 9 * * *", "check prod", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.SaveContext(id, []ContextMessage{{Role: "user", Content: "why"}}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}

	// Disabling the schedule (or changing cron) should NOT clear context —
	// the "why" is still valid.
	disabled := false
	if err := mgr.Update(id, &UpdateOpts{Enabled: &disabled}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !mgr.HasContext(id) {
		t.Error("context sidecar should survive a non-prompt update")
	}

	newCron := "0 10 * * *"
	if err := mgr.Update(id, &UpdateOpts{Cron: &newCron}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !mgr.HasContext(id) {
		t.Error("context sidecar should survive a cron-only update")
	}
}

func TestUpdatePreservesContextWhenPromptSame(t *testing.T) {
	// Update called with the same prompt is a no-op for intent — don't clear.
	dir := t.TempDir()
	mgr := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := mgr.Create("bot", "0 9 * * *", "check prod", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.SaveContext(id, []ContextMessage{{Role: "user", Content: "why"}}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	samePrompt := "check prod"
	if err := mgr.Update(id, &UpdateOpts{Prompt: &samePrompt}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !mgr.HasContext(id) {
		t.Error("context sidecar should survive an update that sets the same prompt")
	}
}

// --- Task 1: Stateful *bool / IsStateless semantics -------------------------

func TestSchedule_IsStateless_LegacyJSONTreatedAsStateful(t *testing.T) {
	raw := `{"id":"abc","agent":"pr-reviewer","cron":"*/30 * * * *","prompt":"check PRs","enabled":true}`
	var s Schedule
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if s.Stateful != nil {
		t.Errorf("legacy schedule should leave Stateful nil, got *%v", *s.Stateful)
	}
	if s.IsStateless() {
		t.Error("legacy schedule must be treated as stateful (current behaviour), got stateless")
	}
}

func TestSchedule_IsStateless_ExplicitTrue(t *testing.T) {
	b := true
	s := Schedule{Stateful: &b}
	if s.IsStateless() {
		t.Error("Stateful=*true should not be stateless")
	}
}

func TestSchedule_IsStateless_ExplicitFalse(t *testing.T) {
	b := false
	s := Schedule{Stateful: &b}
	if !s.IsStateless() {
		t.Error("Stateful=*false should be stateless")
	}
}

func TestSchedule_JSONRoundTrip_ExplicitFalse(t *testing.T) {
	b := false
	s := Schedule{ID: "x", Stateful: &b}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"stateful":false`) {
		t.Errorf("expected explicit false in JSON, got %s", data)
	}

	var back Schedule
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Stateful == nil || *back.Stateful {
		t.Errorf("round-trip lost explicit false: %+v", back.Stateful)
	}
}

// --- Task 2: Create / Update + Stateful plumbing ---------------------------

func TestManager_Create_DefaultsToStateless(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := m.Create("pr-reviewer", "*/30 * * * *", "check", false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stateful == nil {
		t.Fatal("Create should set Stateful explicitly, got nil")
	}
	if *got.Stateful {
		t.Errorf("Create default should be *false (stateless), got *true")
	}
}

func TestManager_Create_OptInStateful(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := m.Create("pr-reviewer", "*/30 * * * *", "check", true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(id)
	if got.Stateful == nil || !*got.Stateful {
		t.Errorf("Create(stateful=true) should set *true, got %v", got.Stateful)
	}
}

func TestManager_Update_FlipStateful(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := m.Create("pr-reviewer", "*/30 * * * *", "check", false)
	tru := true
	if err := m.Update(id, &UpdateOpts{Stateful: &tru}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(id)
	if got.Stateful == nil || !*got.Stateful {
		t.Errorf("Update should flip to *true, got %v", got.Stateful)
	}
}

// Flipping back to false matters more than it looks — false is the zero value
// and a naive PATCH handler that drops zero values would silently no-op this.
// This test guards against that regression at the manager level; the HTTP-
// level analog lives in Task 6.
func TestManager_Update_FlipStatefulFromTrueToFalse(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := m.Create("pr-reviewer", "*/30 * * * *", "check", true)
	fals := false
	if err := m.Update(id, &UpdateOpts{Stateful: &fals}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(id)
	if got.Stateful == nil || *got.Stateful {
		t.Errorf("Update should flip to *false, got %v", got.Stateful)
	}
}

// Legacy schedule (Stateful nil on disk) survives an unrelated Update
// (e.g. cron change) without being silently promoted to *true or *false.
// The admin must explicitly send stateful in the PATCH to migrate it.
func TestManager_Update_NilStatefulNotImplicitlyMigrated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	raw := `[{"id":"legacy","agent":"x","cron":"0 9 * * *","prompt":"p","enabled":true,"sync_status":"ok","created_at":"2025-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	newCron := "0 10 * * *"
	if err := m.Update("legacy", &UpdateOpts{Cron: &newCron}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get("legacy")
	if got.Stateful != nil {
		t.Errorf("unrelated update must leave Stateful nil, got *%v", *got.Stateful)
	}
}

// Operator path: migrate a legacy schedule to stateless via PATCH.
func TestManager_Update_MigrateLegacyToStateless(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	raw := `[{"id":"legacy","agent":"x","cron":"0 9 * * *","prompt":"p","enabled":true,"sync_status":"ok","created_at":"2025-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	fals := false
	if err := m.Update("legacy", &UpdateOpts{Stateful: &fals}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get("legacy")
	if got.Stateful == nil || *got.Stateful {
		t.Errorf("legacy migrate to stateless: got %v", got.Stateful)
	}
}

// --- Task 1: LastRun fields ------------------------------------------------

func TestSchedule_LegacyJSON_LastRunFieldsAreNil(t *testing.T) {
	raw := `{"id":"abc","agent":"x","cron":"0 9 * * *","prompt":"p","enabled":true,"sync_status":"ok","created_at":"2025-01-01T00:00:00Z"}`
	var s Schedule
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if s.LastRunAt != nil {
		t.Errorf("legacy LastRunAt should be nil, got %v", *s.LastRunAt)
	}
	if s.LastRunSessionID != "" {
		t.Errorf("legacy LastRunSessionID should be empty, got %q", s.LastRunSessionID)
	}
	if s.LastRunMessageStartIndex != 0 {
		t.Errorf("legacy LastRunMessageStartIndex should be 0, got %d", s.LastRunMessageStartIndex)
	}
	if s.LastRunMessageEndIndex != 0 {
		t.Errorf("legacy LastRunMessageEndIndex should be 0, got %d", s.LastRunMessageEndIndex)
	}
}

func TestSchedule_JSONRoundTrip_LastRunFieldsPreserved(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 30, 0, 0, time.UTC)
	s := Schedule{
		ID:                       "x",
		LastRunAt:                &now,
		LastRunSessionID:         "2026-05-26-deadbeef",
		LastRunMessageStartIndex: 12,
		LastRunMessageEndIndex:   18,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"last_run_at"`) {
		t.Errorf("expected last_run_at key in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"last_run_session_id":"2026-05-26-deadbeef"`) {
		t.Errorf("expected last_run_session_id in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"last_run_message_start_index":12`) {
		t.Errorf("expected last_run_message_start_index in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"last_run_message_end_index":18`) {
		t.Errorf("expected last_run_message_end_index in JSON, got %s", data)
	}

	var back Schedule
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.LastRunAt == nil || !back.LastRunAt.Equal(now) {
		t.Errorf("round-trip lost LastRunAt: %v", back.LastRunAt)
	}
	if back.LastRunSessionID != "2026-05-26-deadbeef" {
		t.Errorf("round-trip lost LastRunSessionID: %q", back.LastRunSessionID)
	}
	if back.LastRunMessageStartIndex != 12 || back.LastRunMessageEndIndex != 18 {
		t.Errorf("round-trip lost message range: start=%d end=%d", back.LastRunMessageStartIndex, back.LastRunMessageEndIndex)
	}
}

// --- Task 2: MarkLastRun ----------------------------------------------------

func TestManager_MarkLastRun_NormalUpdate(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, err := m.Create("bot", "0 9 * * *", "p", false)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 5, 26, 12, 30, 0, 0, time.UTC)
	if err := m.MarkLastRun(id, "2026-05-26-deadbeef", when, 5, 9); err != nil {
		t.Fatalf("MarkLastRun: %v", err)
	}
	got, _ := m.Get(id)
	if got.LastRunSessionID != "2026-05-26-deadbeef" {
		t.Errorf("LastRunSessionID = %q, want 2026-05-26-deadbeef", got.LastRunSessionID)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(when) {
		t.Errorf("LastRunAt = %v, want %v", got.LastRunAt, when)
	}
	if got.LastRunMessageStartIndex != 5 || got.LastRunMessageEndIndex != 9 {
		t.Errorf("indices: start=%d end=%d, want 5/9", got.LastRunMessageStartIndex, got.LastRunMessageEndIndex)
	}
}

// Overwrites are the common path — schedule fires repeatedly. Second mark
// must replace, not append.
func TestManager_MarkLastRun_Overwrites(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := m.Create("bot", "0 9 * * *", "p", false)

	t1 := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	_ = m.MarkLastRun(id, "sess-1", t1, 0, 4)
	t2 := time.Date(2026, 5, 26, 13, 0, 0, 0, time.UTC)
	_ = m.MarkLastRun(id, "sess-2", t2, 4, 7)

	got, _ := m.Get(id)
	if got.LastRunSessionID != "sess-2" {
		t.Errorf("overwrite lost: %q", got.LastRunSessionID)
	}
	if !got.LastRunAt.Equal(t2) {
		t.Errorf("LastRunAt overwrite lost: %v", got.LastRunAt)
	}
	if got.LastRunMessageStartIndex != 4 || got.LastRunMessageEndIndex != 7 {
		t.Errorf("indices overwrite lost: start=%d end=%d", got.LastRunMessageStartIndex, got.LastRunMessageEndIndex)
	}
}

// MarkLastRun against an unknown id (e.g. schedule deleted between
// runWithLifecycle dispatch and completion) MUST silently no-op so we
// don't crash the scheduler tick.
func TestManager_MarkLastRun_UnknownIDIsNoop(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	if err := m.MarkLastRun("nonexistent", "sess-x", time.Now(), 0, 0); err != nil {
		t.Errorf("unknown id should not error: %v", err)
	}
}

// Empty sessionID would only happen if the run never reached session
// resolution — we must NOT stamp LastRunAt in that case, because there's
// nothing for the show endpoint to resolve.
func TestManager_MarkLastRun_EmptySessionIDIsNoop(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "schedules.json"))
	id, _ := m.Create("bot", "0 9 * * *", "p", false)
	if err := m.MarkLastRun(id, "", time.Now(), 0, 0); err != nil {
		t.Errorf("empty sessionID should be silent no-op: %v", err)
	}
	got, _ := m.Get(id)
	if got.LastRunAt != nil {
		t.Errorf("empty sessionID must not stamp LastRunAt, got %v", got.LastRunAt)
	}
	if got.LastRunMessageStartIndex != 0 || got.LastRunMessageEndIndex != 0 {
		t.Errorf("empty sessionID must not stamp indices, got %d/%d", got.LastRunMessageStartIndex, got.LastRunMessageEndIndex)
	}
}

// --- Task A1: Broadcast + CreatedFromSource fields -------------------------

func TestSchedule_BroadcastFieldRoundTrip(t *testing.T) {
	bTrue := true
	bFalse := false

	tests := []struct {
		name            string
		schedule        Schedule
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "nil Broadcast + empty CreatedFromSource omits both fields",
			schedule: Schedule{
				ID:     "s1",
				Cron:   "* * * * *",
				Prompt: "hi",
			},
			wantContains:    []string{`"id":"s1"`},
			wantNotContains: []string{`"broadcast"`, `"created_from_source"`},
		},
		{
			name: "Broadcast=true + CreatedFromSource set serialize",
			schedule: Schedule{
				ID:                "s2",
				Cron:              "* * * * *",
				Prompt:            "hi",
				Broadcast:         &bTrue,
				CreatedFromSource: "slack",
			},
			wantContains: []string{`"broadcast":true`, `"created_from_source":"slack"`},
		},
		{
			name: "Broadcast=false serializes as false (not omitted)",
			schedule: Schedule{
				ID:                "s3",
				Cron:              "* * * * *",
				Prompt:            "hi",
				Broadcast:         &bFalse,
				CreatedFromSource: "webview",
			},
			wantContains: []string{`"broadcast":false`, `"created_from_source":"webview"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.schedule)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(b)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q in %s", want, got)
				}
			}
			for _, notWant := range tc.wantNotContains {
				if strings.Contains(got, notWant) {
					t.Errorf("output unexpectedly contains %q in %s", notWant, got)
				}
			}

			var round Schedule
			if err := json.Unmarshal(b, &round); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if (round.Broadcast == nil) != (tc.schedule.Broadcast == nil) {
				t.Errorf("Broadcast nil-ness lost: got %v, want %v", round.Broadcast, tc.schedule.Broadcast)
			}
			if round.Broadcast != nil && *round.Broadcast != *tc.schedule.Broadcast {
				t.Errorf("Broadcast value lost: got %v, want %v", *round.Broadcast, *tc.schedule.Broadcast)
			}
			if round.CreatedFromSource != tc.schedule.CreatedFromSource {
				t.Errorf("CreatedFromSource: got %q, want %q", round.CreatedFromSource, tc.schedule.CreatedFromSource)
			}
		})
	}
}

func TestSchedule_LegacyJSONUnmarshalsToNilBroadcast(t *testing.T) {
	// Pre-PR schedules in ~/.shannon/schedules.json don't carry the new fields.
	// Verify they deserialize cleanly with safe defaults.
	const legacy = `{"id":"old","cron":"0 9 * * *","prompt":"morning report","agent":""}`
	var s Schedule
	if err := json.Unmarshal([]byte(legacy), &s); err != nil {
		t.Fatalf("legacy unmarshal failed: %v", err)
	}
	if s.Broadcast != nil {
		t.Errorf("pre-PR Schedule should deserialize with Broadcast=nil, got %v", s.Broadcast)
	}
	if s.CreatedFromSource != "" {
		t.Errorf("pre-PR Schedule should deserialize with CreatedFromSource=\"\", got %q", s.CreatedFromSource)
	}
}
