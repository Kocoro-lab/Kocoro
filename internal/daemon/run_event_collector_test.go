package daemon

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

type recordingRunEventAppender struct {
	mu       sync.Mutex
	failNext bool
	batches  [][]session.RunEventRecord
}

func (a *recordingRunEventAppender) Append(batch []session.RunEventRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNext {
		a.failNext = false
		return errors.New("injected append failure")
	}
	cloned := append([]session.RunEventRecord(nil), batch...)
	a.batches = append(a.batches, cloned)
	return nil
}

func TestRunEventCollectorBuffersWithoutIOAndDeepCopies(t *testing.T) {
	appender := &recordingRunEventAppender{}
	collector := newRunEventCollector(&multiHandler{}, appender, "session-1", "run-1", "attempt-1")
	batchIndex := 2
	event := agent.RunTraceEvent{
		Seq:       1,
		Iteration: 1,
		Type:      agent.RunTraceEventToolOutcome,
		Tool: &agent.RunTraceToolOutcome{
			Name: "bash", Outcome: "succeeded", ExecutionBatchIndex: &batchIndex,
		},
	}

	collector.OnRunTrace(event)
	event.Tool.Name = "mutated"
	*event.Tool.ExecutionBatchIndex = 9
	if len(appender.batches) != 0 {
		t.Fatal("OnRunTrace performed I/O")
	}
	if err := collector.Flush(); err != nil {
		t.Fatal(err)
	}
	got := appender.batches[0][0].Event.Tool
	if got == nil || got.Name != "bash" || got.ExecutionBatchIndex == nil || *got.ExecutionBatchIndex != 2 {
		t.Fatalf("collector retained caller-owned trace pointers: %+v", got)
	}
}

func TestRunEventCollectorFlushFailureRequeuesBeforeLaterEvents(t *testing.T) {
	appender := &recordingRunEventAppender{failNext: true}
	collector := newRunEventCollector(&multiHandler{}, appender, "session-1", "run-1", "attempt-1")
	collector.OnRunTrace(agent.RunTraceEvent{Seq: 1, Type: agent.RunTraceEventModelResponse})
	if err := collector.Flush(); err == nil {
		t.Fatal("injected append failure was ignored")
	}
	collector.OnRunTrace(agent.RunTraceEvent{Seq: 2, Type: agent.RunTraceEventTerminal})
	if err := collector.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(appender.batches) != 1 {
		t.Fatalf("batches = %d", len(appender.batches))
	}
	var seqs []int64
	for _, record := range appender.batches[0] {
		seqs = append(seqs, record.Event.Seq)
	}
	if !reflect.DeepEqual(seqs, []int64{1, 2}) {
		t.Fatalf("sequences = %v", seqs)
	}
}

func TestRunEventCollectorSurvivesFullDaemonWrapperChain(t *testing.T) {
	trace := &runTraceSpy{}
	base := &multiHandler{handlers: []agent.EventHandler{trace}}
	appender := &recordingRunEventAppender{}
	collector := newRunEventCollector(base, appender, "session-1", "run-1", "attempt-1")
	outer := &skillRecommendationEffectHandler{EventHandler: collector}
	event := agent.RunTraceEvent{Seq: 1, Type: agent.RunTraceEventTerminal}

	outer.OnRunTrace(event)
	if len(trace.events) != 1 || trace.events[0].Seq != 1 {
		t.Fatalf("wrapped trace events = %+v", trace.events)
	}
	if err := collector.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(appender.batches) != 1 || len(appender.batches[0]) != 1 {
		t.Fatalf("persisted batches = %+v", appender.batches)
	}
}

func TestAgentRunAndAttemptIDsAreDistinctAndCanonical(t *testing.T) {
	runID, err := newAgentRunID()
	if err != nil {
		t.Fatal(err)
	}
	attemptA, err := newAgentAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	attemptB, err := newAgentAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	if len(runID) != len("run1_")+32 || len(attemptA) != len("att1_")+32 || attemptA == attemptB {
		t.Fatalf("run=%q attemptA=%q attemptB=%q", runID, attemptA, attemptB)
	}
}
