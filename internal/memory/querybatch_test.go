package memory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueryBatch_Parallel(t *testing.T) {
	const queryDelay = 80 * time.Millisecond
	fakeQuery := func(ctx context.Context, _ QueryIntent) (*ResponseEnvelope, ErrorClass, error) {
		select {
		case <-time.After(queryDelay):
			return &ResponseEnvelope{}, ClassOK, nil
		case <-ctx.Done():
			return nil, ClassUnavailable, ctx.Err()
		}
	}

	intents := []QueryIntent{
		{Mode: ModeDirectRelation, AnchorMentions: []string{"a"}},
		{Mode: ModeDirectRelation, AnchorMentions: []string{"b"}},
		{Mode: ModeDirectRelation, AnchorMentions: []string{"c"}},
	}

	start := time.Now()
	results := runQueryBatch(context.Background(), intents, fakeQuery)
	elapsed := time.Since(start)

	if len(results) != len(intents) {
		t.Fatalf("expected %d results, got %d", len(intents), len(results))
	}
	if elapsed > 2*queryDelay {
		t.Errorf("expected parallel (~%v), got %v (looks serial)", queryDelay, elapsed)
	}
	for i, r := range results {
		if r.Class != ClassOK {
			t.Errorf("result[%d] class=%v want ClassOK", i, r.Class)
		}
	}
}

func TestQueryBatch_PreservesOrder(t *testing.T) {
	// Sleep inversely proportional to anchor name so first finishes last,
	// proving the result slice is ordered by input position, not completion.
	delays := map[string]time.Duration{
		"first":  60 * time.Millisecond,
		"second": 30 * time.Millisecond,
		"third":  5 * time.Millisecond,
	}
	fakeQuery := func(_ context.Context, q QueryIntent) (*ResponseEnvelope, ErrorClass, error) {
		time.Sleep(delays[q.AnchorMentions[0]])
		return &ResponseEnvelope{BundleVersion: q.AnchorMentions[0]}, ClassOK, nil
	}

	intents := []QueryIntent{
		{AnchorMentions: []string{"first"}},
		{AnchorMentions: []string{"second"}},
		{AnchorMentions: []string{"third"}},
	}
	results := runQueryBatch(context.Background(), intents, fakeQuery)

	expected := []string{"first", "second", "third"}
	for i, r := range results {
		if r.Envelope == nil {
			t.Fatalf("result[%d] envelope nil", i)
		}
		if r.Envelope.BundleVersion != expected[i] {
			t.Errorf("result[%d] = %q, want %q", i, r.Envelope.BundleVersion, expected[i])
		}
	}
}

func TestQueryBatch_TimeoutDoesNotBlockOthers(t *testing.T) {
	var slowEntered atomic.Bool
	fakeQuery := func(ctx context.Context, q QueryIntent) (*ResponseEnvelope, ErrorClass, error) {
		if q.AnchorMentions[0] == "slow" {
			slowEntered.Store(true)
			select {
			case <-time.After(2 * time.Second):
				return &ResponseEnvelope{}, ClassOK, nil
			case <-ctx.Done():
				return nil, ClassUnavailable, ctx.Err()
			}
		}
		return &ResponseEnvelope{BundleVersion: q.AnchorMentions[0]}, ClassOK, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	intents := []QueryIntent{
		{AnchorMentions: []string{"fast1"}},
		{AnchorMentions: []string{"slow"}},
		{AnchorMentions: []string{"fast2"}},
	}

	start := time.Now()
	results := runQueryBatch(ctx, intents, fakeQuery)
	elapsed := time.Since(start)

	if !slowEntered.Load() {
		t.Fatal("slow query never entered fakeQuery")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("batch took %v, expected ~100ms (ctx timeout cut slow query short)", elapsed)
	}
	if results[0].Class != ClassOK {
		t.Errorf("fast1 class=%v want ClassOK", results[0].Class)
	}
	if results[1].Class != ClassUnavailable {
		t.Errorf("slow class=%v want ClassUnavailable", results[1].Class)
	}
	if !errors.Is(results[1].Err, context.DeadlineExceeded) {
		t.Errorf("slow err=%v want context.DeadlineExceeded", results[1].Err)
	}
	if results[2].Class != ClassOK {
		t.Errorf("fast2 class=%v want ClassOK", results[2].Class)
	}
}

func TestQueryBatch_Empty(t *testing.T) {
	fakeQuery := func(_ context.Context, _ QueryIntent) (*ResponseEnvelope, ErrorClass, error) {
		return &ResponseEnvelope{}, ClassOK, nil
	}
	results := runQueryBatch(context.Background(), nil, fakeQuery)
	if results != nil {
		t.Errorf("expected nil for empty intents, got %v", results)
	}
}

func TestQueryResult_ZeroValueClass(t *testing.T) {
	// Sanity: zero QueryResult should look like ClassOK envelope=nil err=nil.
	// Callers must check r.Envelope == nil before reading it.
	var r QueryResult
	if r.Envelope != nil {
		t.Errorf("zero envelope should be nil")
	}
	if r.Class != ClassOK {
		t.Errorf("zero class=%v want ClassOK", r.Class)
	}
	if r.Err != nil {
		t.Errorf("zero err=%v want nil", r.Err)
	}
}
