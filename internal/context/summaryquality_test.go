package context

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// e2eJunkSummary is the live regression fixture from the 2026-08-04 e2e run:
// the first compaction's small-tier summary was this single line — no
// five-section structure, no identifiers — and it was accepted with zero
// validation, permanently losing the decoy identifiers.
const e2eJunkSummary = "I saw the function buildForceStopReason. Continuing with Step 8..."

// decoyText mirrors the e2e decoy file: identifiers buried in filler prose.
const decoyText = `The deploy pipeline notes follow.
The release commit is 9f3c2a71d4b85e06 and it must be cited verbatim.
Health endpoint listens on internal-host.example.com:7443 for probes.
Tracking issue https://github.com/Kocoro-lab/ShanClaw/issues/12345 covers the rollout.
Incident ticket number 88220145 was filed for the earlier outage.
`

func toolResultMsg(text string) client.Message {
	return client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
		{Type: "tool_result", ToolUseID: "toolu_x", ToolContent: text},
	})}
}

// buildAuditableHistory returns a history long enough that ShapeHistory could
// drop its middle: system + first user + a droppable middle carrying the
// decoy identifiers + a tail of minKeepLast pairs.
func buildAuditableHistory() []client.Message {
	msgs := []client.Message{
		{Role: "system", Content: client.NewTextContent("system prompt")},
		{Role: "user", Content: client.NewTextContent("read the decoy file and remember the identifiers")},
		{Role: "assistant", Content: client.NewTextContent("reading now")},
		toolResultMsg(decoyText),
	}
	for i := 0; i < minKeepLast+2; i++ {
		msgs = append(msgs,
			client.Message{Role: "assistant", Content: client.NewTextContent("working on the next step")},
			client.Message{Role: "user", Content: client.NewTextContent("continue")},
		)
	}
	return msgs
}

func TestExtractOpaqueIdentifiers(t *testing.T) {
	got := extractOpaqueIdentifiers(decoyText)
	want := []string{
		"9F3C2A71D4B85E06", // pure hex normalizes to upper case
		"https://github.com/Kocoro-lab/ShanClaw/issues/12345",
		"internal-host.example.com:7443",
		"88220145",
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("identifier %q not extracted; got %v", w, got)
		}
	}
}

func TestIdentifiersAtRisk_ScansOnlyDroppableMiddle(t *testing.T) {
	msgs := buildAuditableHistory()
	ids := identifiersAtRisk(msgs)
	if len(ids) == 0 {
		t.Fatal("identifiers in the droppable middle must be collected")
	}

	// The same decoy moved into the protected tail is NOT at risk.
	safe := []client.Message{
		{Role: "system", Content: client.NewTextContent("system prompt")},
		{Role: "user", Content: client.NewTextContent("start")},
	}
	for i := 0; i < minKeepLast-1; i++ {
		safe = append(safe,
			client.Message{Role: "assistant", Content: client.NewTextContent("step")},
			client.Message{Role: "user", Content: client.NewTextContent("continue")},
		)
	}
	safe = append(safe, client.Message{Role: "assistant", Content: client.NewTextContent("reading")}, toolResultMsg(decoyText))
	if ids := identifiersAtRisk(safe); len(ids) != 0 {
		t.Errorf("identifiers only in the kept tail must not be at risk, got %v", ids)
	}
}

func TestAuditSummary_JunkFixtureFails(t *testing.T) {
	ids := extractOpaqueIdentifiers(decoyText)
	reasons := auditSummary(e2eJunkSummary, ids)
	if len(reasons) == 0 {
		t.Fatal("the e2e junk one-liner must fail the quality audit")
	}
	joined := strings.Join(reasons, ";")
	if !strings.Contains(joined, "missing_sections") {
		t.Errorf("junk summary should be flagged for missing sections: %v", reasons)
	}
	if !strings.Contains(joined, "missing_identifiers") {
		t.Errorf("junk summary should be flagged for missing identifiers: %v", reasons)
	}
}

func TestAuditSummary_GoodSummaryPasses(t *testing.T) {
	ids := extractOpaqueIdentifiers(decoyText)
	good := `## Current task & next steps
Finish the rollout verification; next read the health probe output.

## User corrections & decisions
User demanded verbatim identifier recall: commit 9f3C2A71D4B85E06, endpoint internal-host.example.com:7443, issue https://github.com/Kocoro-lab/ShanClaw/issues/12345, ticket 88220145.

## Open files / important reads
decoy.md — deploy pipeline notes with identifiers`
	if reasons := auditSummary(good, ids); len(reasons) != 0 {
		t.Errorf("structured summary carrying every identifier must pass, got %v", reasons)
	}
}

// scriptedCompleter returns canned OutputText values in order and records
// every request it saw.
type scriptedCompleter struct {
	outputs []string
	reqs    []client.CompletionRequest
}

func (s *scriptedCompleter) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	s.reqs = append(s.reqs, req)
	out := s.outputs[0]
	if len(s.outputs) > 1 {
		s.outputs = s.outputs[1:]
	}
	return &client.CompletionResponse{
		OutputText: out,
		Usage:      client.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CostUSD: 0.001},
	}, nil
}

func TestGenerateSummary_RetriesJunkWithFailureReason(t *testing.T) {
	good := `<summary>## Current task & next steps
Continue step 8 of the workload.

## User corrections & decisions
Recall verbatim: 9F3C2A71D4B85E06, internal-host.example.com:7443, https://github.com/Kocoro-lab/ShanClaw/issues/12345, 88220145.</summary>`
	c := &scriptedCompleter{outputs: []string{"<summary>" + e2eJunkSummary + "</summary>", good}}

	summary, usage, err := GenerateSummary(context.Background(), c, buildAuditableHistory())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.reqs) != 2 {
		t.Fatalf("junk first summary must trigger exactly one retry, got %d calls", len(c.reqs))
	}
	// The retry request must carry the failure reason and the identifiers.
	retryBody := ""
	for _, m := range c.reqs[1].Messages {
		retryBody += m.Content.Text() + "\n"
	}
	if !strings.Contains(retryBody, "missing_sections") || !strings.Contains(retryBody, "9F3C2A71D4B85E06") {
		t.Errorf("retry request must state what failed and which identifiers to preserve; got: %.400s", retryBody)
	}
	if !strings.Contains(summary, "## Current task & next steps") {
		t.Errorf("retry's structured summary should be accepted, got %.200q", summary)
	}
	if usage.TotalTokens != 300 || usage.CostUSD < 0.0019 {
		t.Errorf("usage must sum both calls, got %+v", usage)
	}
}

func TestGenerateSummary_MechanicalIdentifierBackstop(t *testing.T) {
	// Both attempts return the junk line: identifiers are then appended
	// mechanically so compaction can never lose them silently again.
	c := &scriptedCompleter{outputs: []string{"<summary>" + e2eJunkSummary + "</summary>"}}

	summary, _, err := GenerateSummary(context.Background(), c, buildAuditableHistory())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.reqs) != 2 {
		t.Fatalf("expected one retry, got %d calls", len(c.reqs))
	}
	for _, id := range []string{"9F3C2A71D4B85E06", "internal-host.example.com:7443", "88220145"} {
		if !strings.Contains(summary, id) {
			t.Errorf("identifier %q must survive via the mechanical backstop; summary: %q", id, summary)
		}
	}
}

func TestGenerateSummary_CleanFirstPassMakesOneCall(t *testing.T) {
	good := `<summary>## Current task & next steps
Continue the work.

## User corrections & decisions
Keep 9F3C2A71D4B85E06, internal-host.example.com:7443, https://github.com/Kocoro-lab/ShanClaw/issues/12345, 88220145 handy.</summary>`
	c := &scriptedCompleter{outputs: []string{good}}

	summary, _, err := GenerateSummary(context.Background(), c, buildAuditableHistory())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.reqs) != 1 {
		t.Fatalf("a clean first summary must not pay a retry, got %d calls", len(c.reqs))
	}
	if strings.Contains(summary, "auto-preserved") {
		t.Errorf("no mechanical section expected on a clean pass: %q", summary)
	}
}

func TestGenerateSummary_ShortHistorySkipsAudit(t *testing.T) {
	// Histories too short for ShapeHistory to drop anything have no
	// droppable middle — prose summaries stay acceptable and cost one call.
	c := &scriptedCompleter{outputs: []string{"<summary>short prose recap</summary>"}}
	msgs := []client.Message{
		{Role: "system", Content: client.NewTextContent("sys")},
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi")},
	}
	summary, _, err := GenerateSummary(context.Background(), c, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.reqs) != 1 || summary != "short prose recap" {
		t.Fatalf("short history must keep single-call prose behavior: calls=%d summary=%q", len(c.reqs), summary)
	}
}
