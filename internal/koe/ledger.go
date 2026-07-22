//go:build darwin && cgo

package koe

import (
	"fmt"
	"sort"
)

type VoiceTaskState string

const (
	TaskRunning   VoiceTaskState = "running"
	TaskCompleted VoiceTaskState = "completed"
	TaskFailed    VoiceTaskState = "failed"
	TaskCancelled VoiceTaskState = "cancelled"
)

// VoiceTask is one call-scoped task lineage. Immutable identity/routing fields
// are safe for the detached do_task goroutine to retain; mutable lifecycle fields
// are guarded by CallState.mu.
type VoiceTask struct {
	ID       string
	Label    string
	Agent    string
	ThreadID string
	State    VoiceTaskState

	Revision          int
	DeliveredRevision int
	Reply             string
	Deliverables      []Deliverable
	FailReason        string
}

// TaskLedgerEnabled keeps the ledger and multi-lane tool schema independently
// rollbackable while the native S2S control loop is fielded.
func TaskLedgerEnabled() bool { return koeEnvBool("KOE_TASK_LEDGER", true) }

// BeginTask allocates a stable task id and daemon lane. Sequential work reuses
// the main burst lane for context continuity; a truly concurrent task for the
// same agent receives its own sub-lane.
func (s *CallState) BeginTask(label, agent string) *VoiceTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taskSeq++
	task := &VoiceTask{
		ID:       fmt.Sprintf("t%02d", s.taskSeq),
		Label:    label,
		Agent:    agent,
		ThreadID: s.burstID,
		State:    TaskRunning,
		Revision: 1,
	}
	for _, other := range s.tasks {
		if other.State == TaskRunning && other.Agent == agent && other.ThreadID == s.burstID {
			task.ThreadID = s.burstID + "." + task.ID
			break
		}
	}
	if s.tasks == nil {
		s.tasks = make(map[string]*VoiceTask)
	}
	s.tasks[task.ID] = task
	return task
}

func (s *CallState) BeginFollowUp(taskID, label string) (*VoiceTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return nil, false
	}
	task.Revision++
	task.Label = label
	task.State = TaskRunning
	return task, true
}

func (s *CallState) LandResult(taskID string, result SayResult) (VoiceTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return VoiceTask{}, false
	}
	switch result.Status {
	case "injected":
		return *task, false
	case "ok":
		task.State = TaskCompleted
	case "cancelled":
		task.State = TaskCancelled
	default:
		task.State = TaskFailed
	}
	supersedes := task.DeliveredRevision > 0 && task.Revision > task.DeliveredRevision
	task.DeliveredRevision = task.Revision
	if result.Reply != "" {
		task.Reply = result.Reply
	}
	task.Deliverables = append([]Deliverable(nil), result.Deliverables...)
	task.FailReason = result.FailReason
	copy := *task
	copy.Deliverables = append([]Deliverable(nil), task.Deliverables...)
	return copy, supersedes
}

func (s *CallState) MarkCancelled(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task, ok := s.tasks[taskID]; ok && task.State == TaskRunning {
		task.State = TaskCancelled
	}
}

func (s *CallState) RunningMainLaneTask(agent string) *VoiceTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.tasks {
		if task.State == TaskRunning && task.Agent == agent && task.ThreadID == s.burstID {
			return task
		}
	}
	return nil
}

func (s *CallState) TaskByID(id string) (VoiceTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task, ok := s.tasks[id]; ok {
		return *task, true
	}
	return VoiceTask{}, false
}

func (s *CallState) RunningTasks() []VoiceTask {
	return s.tasksWhere(func(task *VoiceTask) bool { return task.State == TaskRunning })
}

func (s *CallState) RunningTasksForAgent(agent string) []VoiceTask {
	return s.tasksWhere(func(task *VoiceTask) bool {
		return task.State == TaskRunning && task.Agent == agent
	})
}

func (s *CallState) AnyRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.tasks {
		if task.State == TaskRunning {
			return true
		}
	}
	return false
}

func (s *CallState) HasTasks() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks) > 0
}

func (s *CallState) AllTasks() []VoiceTask {
	return s.tasksWhere(func(*VoiceTask) bool { return true })
}

func (s *CallState) tasksWhere(keep func(*VoiceTask) bool) []VoiceTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]VoiceTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		if keep(task) {
			out = append(out, *task)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].ID) != len(out[j].ID) {
			return len(out[i].ID) < len(out[j].ID)
		}
		return out[i].ID < out[j].ID
	})
	return out
}
