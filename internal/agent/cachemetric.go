package agent

import "github.com/Kocoro-lab/ShanClaw/internal/client"

// CacheTracker accumulates per-LLM-call cache statistics across a single
// agent Run so the result can be emitted to audit.log on Run exit.
//
// Why per-Run: a Run is the natural boundary for measuring cache effectiveness
// (one user message → one Run → one summary line). Sessions are too coarse
// (TUI sessions live for hours) and per-call would flood the audit log.
//
// Only main-tier calls are recorded — helper calls (skill_discovery,
// summarize, persist, microcompact) are tagged cache_source="helper" and
// share Anthropic's cache namespace; including them would pollute the
// session-level cache health signal.
//
// See docs/issues/cache-action-plan.md §1.3.
type CacheTracker struct {
	calls      int
	inputTotal int64 // uncached input tokens (the portion Anthropic charged at base rate)
	ccTotal    int64
	crTotal    int64
	tail       []callPair // last tailCERWindow (cc, cr) pairs
	firstCC    int
	firstCR    int
	firstSeen  bool
}

type callPair struct {
	cc int
	cr int
}

// tailCERWindow is the number of trailing calls used to compute tail_cer.
// 3 is small enough to react to a fresh cliff but large enough to absorb
// transient single-call dips (e.g. one cold write inside a long warm session).
const tailCERWindow = 3

// Record absorbs the cache fields from one main-tier LLM response.
// Helper-tier calls should not be recorded here.
func (t *CacheTracker) Record(u client.Usage) {
	if t == nil {
		return
	}
	t.calls++
	t.inputTotal += int64(u.InputTokens)
	t.ccTotal += int64(u.CacheCreationTokens)
	t.crTotal += int64(u.CacheReadTokens)
	if !t.firstSeen {
		t.firstCC = u.CacheCreationTokens
		t.firstCR = u.CacheReadTokens
		t.firstSeen = true
	}
	t.tail = append(t.tail, callPair{cc: u.CacheCreationTokens, cr: u.CacheReadTokens})
	if len(t.tail) > tailCERWindow {
		t.tail = t.tail[len(t.tail)-tailCERWindow:]
	}
}

// CacheSummary is the aggregate metric emitted to audit.log.
//
// CER = cache_read / cache_creation. A healthy long session ramps from
// ~0 (cold) to 30×+ (rolling reads dominating). TailCERLast3 isolates the
// recent window so a long-tail healthy run with one cold write at the start
// still reports a high tail.
//
// WarmStart=true means the first call read prior cache without paying any
// creation cost — diagnostic of cross-session cache reuse.
type CacheSummary struct {
	Calls        int
	InputTotal   int64 // uncached input tokens — fills the audit cache_summary's input_tokens field so TOTAL = input + cache_read + cache_creation reflects the real prompt size, including the portion Anthropic decided not to cache (large prompts often fall here)
	CCTotal      int64
	CRTotal      int64
	CER          float64
	TailCERLast3 float64
	WarmStart    bool
}

// Summary returns the aggregate metrics. Empty (Calls=0) when no main-tier
// calls were recorded — caller should skip emitting an audit entry in that case.
func (t *CacheTracker) Summary() CacheSummary {
	if t == nil || t.calls == 0 {
		return CacheSummary{}
	}
	cer := 0.0
	if t.ccTotal > 0 {
		cer = float64(t.crTotal) / float64(t.ccTotal)
	}
	var tailCC, tailCR int64
	for _, p := range t.tail {
		tailCC += int64(p.cc)
		tailCR += int64(p.cr)
	}
	tailCER := 0.0
	if tailCC > 0 {
		tailCER = float64(tailCR) / float64(tailCC)
	}
	return CacheSummary{
		Calls:        t.calls,
		InputTotal:   t.inputTotal,
		CCTotal:      t.ccTotal,
		CRTotal:      t.crTotal,
		CER:          cer,
		TailCERLast3: tailCER,
		WarmStart:    t.firstSeen && t.firstCC == 0 && t.firstCR > 0,
	}
}
