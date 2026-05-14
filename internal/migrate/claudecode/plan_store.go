package claudecode

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrPlanNotFound        = errors.New("plan_not_found")
	ErrPlanExpired         = errors.New("plan_expired")
	ErrPlanStale           = errors.New("plan_stale")
	ErrMigrationInProgress = errors.New("migration_in_progress")
)

// PlanStore keeps preview plans in memory until apply or expiry.
type PlanStore struct {
	mu    sync.Mutex
	now   func() time.Time
	plans map[string]*Plan
}

func NewPlanStore() *PlanStore {
	return &PlanStore{
		now:   time.Now,
		plans: map[string]*Plan{},
	}
}

func (s *PlanStore) Put(p *Plan) {
	if s == nil || p == nil || p.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans[p.ID] = p
}

func (s *PlanStore) Get(id string) (*Plan, error) {
	if s == nil || id == "" {
		return nil, ErrPlanNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return nil, ErrPlanNotFound
	}
	if s.now().After(p.ExpiresAt) {
		delete(s.plans, id)
		return nil, ErrPlanExpired
	}
	return p, nil
}

func (s *PlanStore) Delete(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.plans, id)
}

func (s *PlanStore) SweepExpired() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for id, p := range s.plans {
		if now.After(p.ExpiresAt) {
			delete(s.plans, id)
			n++
		}
	}
	return n
}
