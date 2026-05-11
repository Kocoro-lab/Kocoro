package agent

import (
	"math"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestCacheTracker_EmptyReturnsZeroSummary(t *testing.T) {
	tr := &CacheTracker{}
	s := tr.Summary()
	if s.Calls != 0 || s.CER != 0 || s.WarmStart {
		t.Errorf("empty tracker: expected zero summary, got %+v", s)
	}
}

func TestCacheTracker_NilSafeRecord(t *testing.T) {
	var tr *CacheTracker
	tr.Record(client.Usage{CacheCreationTokens: 100}) // must not panic
	if got := tr.Summary(); got.Calls != 0 {
		t.Errorf("nil tracker should yield empty summary, got %+v", got)
	}
}

func TestCacheTracker_AccumulatesAcrossCalls(t *testing.T) {
	tr := &CacheTracker{}
	// InputTokens populated so the audit cache_summary's input_tokens field
	// reflects the uncached portion of the prompt (without this, large
	// prompts that Anthropic chooses not to cache showed TOTAL=0 in audit
	// — the 2026-05-11 stress test cache_summary reporting bug P2-#6).
	tr.Record(client.Usage{InputTokens: 100, CacheCreationTokens: 1000, CacheReadTokens: 0})
	tr.Record(client.Usage{InputTokens: 50, CacheCreationTokens: 200, CacheReadTokens: 800})
	tr.Record(client.Usage{InputTokens: 30, CacheCreationTokens: 50, CacheReadTokens: 1500})

	s := tr.Summary()
	if s.Calls != 3 {
		t.Errorf("Calls = %d, want 3", s.Calls)
	}
	if s.InputTotal != 180 {
		t.Errorf("InputTotal = %d, want 180", s.InputTotal)
	}
	if s.CCTotal != 1250 {
		t.Errorf("CCTotal = %d, want 1250", s.CCTotal)
	}
	if s.CRTotal != 2300 {
		t.Errorf("CRTotal = %d, want 2300", s.CRTotal)
	}
	want := 2300.0 / 1250.0
	if math.Abs(s.CER-want) > 0.001 {
		t.Errorf("CER = %f, want %f", s.CER, want)
	}
}

func TestCacheTracker_TailCERWindowsLastN(t *testing.T) {
	tr := &CacheTracker{}
	// Cold start with 10K creation, read 0 — bad CER
	tr.Record(client.Usage{CacheCreationTokens: 10000, CacheReadTokens: 0})
	// Then 3 healthy calls with cc small, cr large
	tr.Record(client.Usage{CacheCreationTokens: 100, CacheReadTokens: 5000})
	tr.Record(client.Usage{CacheCreationTokens: 100, CacheReadTokens: 5000})
	tr.Record(client.Usage{CacheCreationTokens: 100, CacheReadTokens: 5000})

	s := tr.Summary()
	// Total CER = 15000 / 10300 ≈ 1.46
	if s.CER < 1.4 || s.CER > 1.5 {
		t.Errorf("total CER = %f, want ≈1.46", s.CER)
	}
	// Tail CER (last 3 healthy calls) = 15000 / 300 = 50.0
	if math.Abs(s.TailCERLast3-50.0) > 0.1 {
		t.Errorf("tail CER = %f, want 50.0 — cold call should be excluded from tail window", s.TailCERLast3)
	}
}

func TestCacheTracker_WarmStartDetection(t *testing.T) {
	cases := []struct {
		name        string
		firstUsage  client.Usage
		wantWarm    bool
	}{
		{"warm: cc=0 cr>0", client.Usage{CacheCreationTokens: 0, CacheReadTokens: 5000}, true},
		{"cold: cc>0 cr=0", client.Usage{CacheCreationTokens: 5000, CacheReadTokens: 0}, false},
		{"empty: cc=0 cr=0", client.Usage{CacheCreationTokens: 0, CacheReadTokens: 0}, false},
		{"both: cc>0 cr>0", client.Usage{CacheCreationTokens: 100, CacheReadTokens: 5000}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &CacheTracker{}
			tr.Record(tc.firstUsage)
			if got := tr.Summary().WarmStart; got != tc.wantWarm {
				t.Errorf("WarmStart = %v, want %v", got, tc.wantWarm)
			}
		})
	}
}

func TestCacheTracker_FirstCallStateImmutable(t *testing.T) {
	// Subsequent calls must not flip WarmStart — the field is "did this Run
	// start warm". A run that started warm and then took a cold-rebuild hit
	// in the middle is still considered warm-start at the boundary.
	tr := &CacheTracker{}
	tr.Record(client.Usage{CacheCreationTokens: 0, CacheReadTokens: 5000}) // warm first
	tr.Record(client.Usage{CacheCreationTokens: 10000, CacheReadTokens: 0}) // mid-run cold
	if !tr.Summary().WarmStart {
		t.Error("WarmStart must remain true after first call established it")
	}
}

func TestCacheTracker_TailCERZeroDivisionGuard(t *testing.T) {
	tr := &CacheTracker{}
	// Three calls with no creation at all — should not divide by zero
	tr.Record(client.Usage{CacheCreationTokens: 0, CacheReadTokens: 0})
	tr.Record(client.Usage{CacheCreationTokens: 0, CacheReadTokens: 0})
	tr.Record(client.Usage{CacheCreationTokens: 0, CacheReadTokens: 0})
	s := tr.Summary()
	if s.CER != 0 || s.TailCERLast3 != 0 {
		t.Errorf("zero-division guard failed: CER=%f tail=%f", s.CER, s.TailCERLast3)
	}
}
