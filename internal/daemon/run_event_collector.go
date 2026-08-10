package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

type runEventAppender interface {
	Append([]session.RunEventRecord) error
}

// runEventCollector keeps the AgentLoop observation callback free of disk I/O.
// The loop goroutine only appends to pending; existing checkpoint/final owners
// synchronously flush the batch at their durability boundaries.
type runEventCollector struct {
	*multiHandler
	log       runEventAppender
	sessionID string
	runID     string
	attemptID string

	mu      sync.Mutex
	pending []session.RunEventRecord
}

func mintRunEventID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func newAgentRunID() (string, error)     { return mintRunEventID("run1_") }
func newAgentAttemptID() (string, error) { return mintRunEventID("att1_") }

func cloneRunTraceEvent(event agent.RunTraceEvent) agent.RunTraceEvent {
	if event.Model != nil {
		value := *event.Model
		event.Model = &value
	}
	if event.Tool != nil {
		value := *event.Tool
		if value.ExecutionBatchIndex != nil {
			index := *value.ExecutionBatchIndex
			value.ExecutionBatchIndex = &index
		}
		event.Tool = &value
	}
	if event.Retry != nil {
		value := *event.Retry
		event.Retry = &value
	}
	if event.Nudge != nil {
		value := *event.Nudge
		event.Nudge = &value
	}
	if event.Compaction != nil {
		value := *event.Compaction
		event.Compaction = &value
	}
	if event.Terminal != nil {
		value := *event.Terminal
		event.Terminal = &value
	}
	return event
}

func newRunEventCollector(handler *multiHandler, log runEventAppender, sessionID, runID, attemptID string) *runEventCollector {
	return &runEventCollector{
		multiHandler: handler,
		log:          log,
		sessionID:    sessionID,
		runID:        runID,
		attemptID:    attemptID,
	}
}

func (c *runEventCollector) OnRunTrace(event agent.RunTraceEvent) {
	if c == nil {
		return
	}
	record := session.RunEventRecord{
		SchemaVersion: session.RunEventSchemaVersion,
		SessionID:     c.sessionID,
		RunID:         c.runID,
		AttemptID:     c.attemptID,
		RecordedAt:    time.Now().UTC(),
		Event:         cloneRunTraceEvent(event),
	}
	c.mu.Lock()
	c.pending = append(c.pending, record)
	c.mu.Unlock()
	if c.multiHandler != nil {
		c.multiHandler.OnRunTrace(event)
	}
}

func (c *runEventCollector) Flush() error {
	if c == nil || c.log == nil {
		return nil
	}
	c.mu.Lock()
	batch := c.pending
	c.pending = nil
	c.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := c.log.Append(batch); err != nil {
		c.mu.Lock()
		pending := make([]session.RunEventRecord, 0, len(batch)+len(c.pending))
		pending = append(pending, batch...)
		pending = append(pending, c.pending...)
		c.pending = pending
		c.mu.Unlock()
		return err
	}
	return nil
}

func mintFreshAgentRun(req *RunAgentRequest) error {
	if req == nil {
		return errors.New("run request is nil")
	}
	runID, err := newAgentRunID()
	if err != nil {
		return err
	}
	attemptID, err := newAgentAttemptID()
	if err != nil {
		return err
	}
	req.RunID = runID
	req.AttemptID = attemptID
	return nil
}
