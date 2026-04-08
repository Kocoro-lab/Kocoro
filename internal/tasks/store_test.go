package tasks

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

// --- Status constants ---

func TestStatusConstants(t *testing.T) {
	if StatusPending != "pending" {
		t.Errorf("StatusPending = %q, want %q", StatusPending, "pending")
	}
	if StatusInProgress != "in_progress" {
		t.Errorf("StatusInProgress = %q, want %q", StatusInProgress, "in_progress")
	}
	if StatusCompleted != "completed" {
		t.Errorf("StatusCompleted = %q, want %q", StatusCompleted, "completed")
	}
}

func TestValidStatus(t *testing.T) {
	valid := []string{StatusPending, StatusInProgress, StatusCompleted}
	for _, s := range valid {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "done", "PENDING", "in progress", "cancelled"}
	for _, s := range invalid {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true, want false", s)
		}
	}
}

// --- Task struct ---

func TestTaskStruct(t *testing.T) {
	task := Task{
		ID:          "1",
		Subject:     "Test subject",
		Description: "Test description",
		Status:      StatusPending,
		Owner:       "alice",
		Metadata:    map[string]any{"priority": "high"},
	}
	if task.ID != "1" {
		t.Errorf("ID = %q, want %q", task.ID, "1")
	}
	if task.Subject != "Test subject" {
		t.Errorf("Subject = %q", task.Subject)
	}
	if task.Description != "Test description" {
		t.Errorf("Description = %q, want %q", task.Description, "Test description")
	}
	if task.Status != StatusPending {
		t.Errorf("Status = %q", task.Status)
	}
	if task.Owner != "alice" {
		t.Errorf("Owner = %q", task.Owner)
	}
	if task.Metadata["priority"] != "high" {
		t.Errorf("Metadata[priority] = %v", task.Metadata["priority"])
	}
}

// --- Store Create + Get ---

func TestCreateAndGet(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id, err := store.Create(Task{
		Subject:     "Fix login bug",
		Description: "Users cannot log in with SSO",
		Status:      StatusPending,
		Owner:       "bob",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty ID")
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id {
		t.Errorf("Got ID = %q, want %q", got.ID, id)
	}
	if got.Subject != "Fix login bug" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.Status != StatusPending {
		t.Errorf("Status = %q", got.Status)
	}
	if got.Owner != "bob" {
		t.Errorf("Owner = %q", got.Owner)
	}
}

// --- Auto-incrementing IDs ---

func TestAutoIncrementingIDs(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	var ids []string
	for i := range 5 {
		id, err := store.Create(Task{
			Subject: fmt.Sprintf("Task %d", i+1),
			Status:  StatusPending,
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	// IDs should be "1", "2", "3", "4", "5"
	expected := []string{"1", "2", "3", "4", "5"}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ID[%d] = %q, want %q", i, id, expected[i])
		}
	}
}

// --- Update (status, owner) ---

func TestUpdateStatus(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id, err := store.Create(Task{
		Subject: "Refactor auth",
		Status:  StatusPending,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newStatus := StatusInProgress
	updated, err := store.Update(id, TaskUpdates{Status: &newStatus})
	if err != nil {
		t.Fatalf("Update status: %v", err)
	}
	if updated.Status != StatusInProgress {
		t.Errorf("Status = %q, want %q", updated.Status, StatusInProgress)
	}

	// Verify persisted
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Status != StatusInProgress {
		t.Errorf("Persisted status = %q, want %q", got.Status, StatusInProgress)
	}
}

func TestUpdateOwner(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id, err := store.Create(Task{
		Subject: "Deploy v2",
		Status:  StatusPending,
		Owner:   "alice",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newOwner := "carol"
	updated, err := store.Update(id, TaskUpdates{Owner: &newOwner})
	if err != nil {
		t.Fatalf("Update owner: %v", err)
	}
	if updated.Owner != "carol" {
		t.Errorf("Owner = %q, want %q", updated.Owner, "carol")
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Owner != "carol" {
		t.Errorf("Persisted owner = %q, want %q", got.Owner, "carol")
	}
}

func TestUpdateMultipleFields(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id, err := store.Create(Task{
		Subject:     "Old subject",
		Description: "Old description",
		Status:      StatusPending,
		Owner:       "old-owner",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newSubject := "New subject"
	newDesc := "New description"
	newStatus := StatusCompleted
	newOwner := "new-owner"

	updated, err := store.Update(id, TaskUpdates{
		Subject:     &newSubject,
		Description: &newDesc,
		Status:      &newStatus,
		Owner:       &newOwner,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Subject != "New subject" {
		t.Errorf("Subject = %q", updated.Subject)
	}
	if updated.Description != "New description" {
		t.Errorf("Description = %q", updated.Description)
	}
	if updated.Status != StatusCompleted {
		t.Errorf("Status = %q", updated.Status)
	}
	if updated.Owner != "new-owner" {
		t.Errorf("Owner = %q", updated.Owner)
	}
}

// --- List ---

func TestList(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Empty list
	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List (empty): %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("List (empty) len = %d, want 0", len(tasks))
	}

	// Create a few tasks
	subjects := []string{"Task A", "Task B", "Task C"}
	for _, s := range subjects {
		if _, err := store.Create(Task{Subject: s, Status: StatusPending}); err != nil {
			t.Fatalf("Create %q: %v", s, err)
		}
	}

	tasks, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 3 {
		t.Errorf("List len = %d, want 3", len(tasks))
	}

	// Collect subjects from listed tasks
	var got []string
	for _, tk := range tasks {
		got = append(got, tk.Subject)
	}
	sort.Strings(got)
	sort.Strings(subjects)
	for i := range subjects {
		if got[i] != subjects[i] {
			t.Errorf("List[%d] subject = %q, want %q", i, got[i], subjects[i])
		}
	}
}

func TestListOnNonExistentDir(t *testing.T) {
	store := NewStore("/tmp/shanclaw-tasks-nonexistent-dir-that-should-not-exist-xyz123")
	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if tasks != nil {
		t.Errorf("expected nil slice, got %v", tasks)
	}
}

// --- Get not found ---

func TestGetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	_, err := store.Get("999")
	if err == nil {
		t.Fatal("expected error for missing task, got nil")
	}
}

// --- Update invalid status ---

func TestUpdateInvalidStatus(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id, err := store.Create(Task{Subject: "Test", Status: StatusPending})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	bad := "invalid-status"
	_, err = store.Update(id, TaskUpdates{Status: &bad})
	if err == nil {
		t.Fatal("expected error for invalid status, got nil")
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id, err := store.Create(Task{Subject: "To delete", Status: StatusPending})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = store.Get(id)
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("List after delete len = %d, want 0", len(tasks))
	}
}

// --- Concurrent creates (flock safety) ---

func TestConcurrentCreates(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	const goroutines = 20
	var wg sync.WaitGroup
	idCh := make(chan string, goroutines)
	errCh := make(chan error, goroutines)

	wg.Add(goroutines)
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			id, err := store.Create(Task{
				Subject: fmt.Sprintf("Concurrent task %d", n),
				Status:  StatusPending,
			})
			if err != nil {
				errCh <- err
				return
			}
			idCh <- id
		}(i)
	}
	wg.Wait()
	close(idCh)
	close(errCh)

	for err := range errCh {
		t.Errorf("goroutine error: %v", err)
	}

	// Collect all IDs and verify uniqueness
	seen := make(map[string]bool)
	var ids []string
	for id := range idCh {
		if seen[id] {
			t.Errorf("duplicate ID: %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	if len(ids) != goroutines {
		t.Errorf("got %d unique IDs, want %d", len(ids), goroutines)
	}

	// Verify all tasks are readable and listed
	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List after concurrent creates: %v", err)
	}
	if len(tasks) != goroutines {
		t.Errorf("List len = %d, want %d", len(tasks), goroutines)
	}
}
