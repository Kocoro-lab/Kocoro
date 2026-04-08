package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type TaskUpdates struct {
	Subject     *string
	Description *string
	Status      *string
	Owner       *string
}

type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) Create(t Task) (string, error) {
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	highest := s.highestID()
	id := strconv.Itoa(highest + 1)
	t.ID = id

	if err := s.writeTask(t); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) Get(id string) (*Task, error) {
	if _, err := strconv.Atoi(id); err != nil {
		return nil, fmt.Errorf("invalid task id %q", id)
	}
	data, err := os.ReadFile(s.taskPath(id))
	if err != nil {
		return nil, fmt.Errorf("task %q not found", id)
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("task %q: corrupt JSON: %w", id, err)
	}
	return &t, nil
}

func (s *Store) Update(id string, u TaskUpdates) (*Task, error) {
	if u.Status != nil && !ValidStatus(*u.Status) {
		return nil, fmt.Errorf("invalid status: %q", *u.Status)
	}

	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if u.Subject != nil {
		t.Subject = *u.Subject
	}
	if u.Description != nil {
		t.Description = *u.Description
	}
	if u.Status != nil {
		t.Status = *u.Status
	}
	if u.Owner != nil {
		t.Owner = *u.Owner
	}
	if err := s.writeTask(*t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) List() ([]Task, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var tasks []Task
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		t, err := s.Get(id)
		if err != nil {
			continue
		}
		tasks = append(tasks, *t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		a, _ := strconv.Atoi(tasks[i].ID)
		b, _ := strconv.Atoi(tasks[j].ID)
		return a < b
	})
	return tasks, nil
}

func (s *Store) Delete(id string) error {
	return os.Remove(s.taskPath(id))
}

// Clear removes all task JSON files under flock protection.
func (s *Store) Clear() error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
	return nil
}

func (s *Store) taskPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *Store) ensureDir() error {
	return os.MkdirAll(s.dir, 0700)
}

func (s *Store) writeTask(t Task) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".task-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	if err := os.Rename(tmpPath, s.taskPath(t.ID)); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(s.dir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (s *Store) highestID() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		if id > highest {
			highest = id
		}
	}
	return highest
}
