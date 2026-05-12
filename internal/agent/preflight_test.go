package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestAgentLoop_MemoryPreflightInjectedButNotPersisted(t *testing.T) {
	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
		OutputText:   "ok",
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
	}}}
	loop := NewAgentLoop(llm, NewToolRegistry(), "medium", t.TempDir(), 3, 2000, 200, nil, nil, nil)
	loop.SetSkillDiscovery(false)
	loop.SetMemoryPreflight(func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult {
		return &MemoryPreflightResult{Context: "<private_memory>\n- Example Contact via collaborated_with\n</private_memory>"}
	})

	query := "who is Example Contact to me?"
	if _, _, err := loop.Run(context.Background(), query, nil, nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("requests=%d want 1", len(llm.requests))
	}
	prompt := llm.requests[0].Messages[len(llm.requests[0].Messages)-1].Content.Text()
	if !strings.Contains(prompt, "<private_memory>") {
		t.Fatalf("first payload missing private memory: %q", prompt)
	}
	if strings.Index(prompt, "<private_memory>") > strings.LastIndex(prompt, query) {
		t.Fatalf("private memory was not inserted before user query: %q", prompt)
	}

	runMessages := loop.RunMessages()
	if len(runMessages) == 0 {
		t.Fatal("RunMessages empty")
	}
	if got := runMessages[0].Content.Text(); got != query {
		t.Fatalf("persisted user message=%q want %q", got, query)
	}
	if strings.Contains(runMessages[0].Content.Text(), "<private_memory>") {
		t.Fatalf("private memory leaked into run messages: %q", runMessages[0].Content.Text())
	}
}

func TestAgentLoop_MemoryPreflightInjectionLogsCacheEvent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
		OutputText:   "ok",
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
	}}}
	loop := NewAgentLoop(llm, NewToolRegistry(), "medium", t.TempDir(), 3, 2000, 200, nil, nil, nil)
	loop.SetSkillDiscovery(false)
	loop.SetMemoryPreflight(func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult {
		return &MemoryPreflightResult{Context: "<private_memory>\n- Example Contact via collaborated_with\n</private_memory>"}
	})

	if _, _, err := loop.Run(context.Background(), "who is Example Contact to me?", nil, nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	if err != nil {
		t.Fatalf("read cache log: %v", err)
	}
	if !strings.Contains(string(data), `"action":"preflight_inject"`) {
		t.Fatalf("missing preflight_inject cache event:\n%s", data)
	}
}

func TestAgentLoop_MemoryPreflightStrippedFromMultimodalRunMessages(t *testing.T) {
	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
		OutputText:   "ok",
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
	}}}
	loop := NewAgentLoop(llm, NewToolRegistry(), "medium", t.TempDir(), 3, 2000, 200, nil, nil, nil)
	loop.SetSkillDiscovery(false)
	loop.SetMemoryPreflight(func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult {
		return &MemoryPreflightResult{Context: "<private_memory>\n- Example Contact via collaborated_with\n</private_memory>"}
	})

	query := "who is Example Contact to me?"
	userContent := []client.ContentBlock{{Type: "image"}}
	if _, _, err := loop.Run(context.Background(), query, userContent, nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	prompt := llm.requests[0].Messages[len(llm.requests[0].Messages)-1].Content.Text()
	if !strings.Contains(prompt, "<private_memory>") {
		t.Fatalf("first payload missing private memory: %q", prompt)
	}

	runMessages := loop.RunMessages()
	if len(runMessages) == 0 {
		t.Fatal("RunMessages empty")
	}
	first := runMessages[0]
	if !first.Content.HasBlocks() {
		t.Fatal("persisted multimodal message lost block content")
	}
	if got := first.Content.Text(); got != query {
		t.Fatalf("persisted user text=%q want %q", got, query)
	}
	if strings.Contains(first.Content.Text(), "<private_memory>") {
		t.Fatalf("private memory leaked into multimodal run messages: %q", first.Content.Text())
	}
	if len(first.Content.Blocks()) != 2 || first.Content.Blocks()[1].Type != "image" {
		t.Fatalf("image block not preserved: %+v", first.Content.Blocks())
	}
}

func TestAgentLoop_MemoryPreflightForceHelperOnlyOnFirstConversationTurn(t *testing.T) {
	run := func(t *testing.T, history []client.Message) bool {
		t.Helper()
		llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
			OutputText:   "ok",
			FinishReason: "end_turn",
			Usage:        client.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
		}}}
		loop := NewAgentLoop(llm, NewToolRegistry(), "medium", t.TempDir(), 3, 2000, 200, nil, nil, nil)
		loop.SetSkillDiscovery(false)
		var got bool
		loop.SetMemoryPreflight(func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult {
			got = opts.ForceHelper
			return nil
		})
		if _, _, err := loop.Run(context.Background(), "hello", nil, history); err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		return got
	}

	if !run(t, nil) {
		t.Fatal("first conversation user message should force helper preflight")
	}
	history := []client.Message{{Role: "user", Content: client.NewTextContent("earlier")}}
	if run(t, history) {
		t.Fatal("non-first conversation turn should not force helper preflight")
	}
}

func TestAgentLoop_MemoryPreflightAuditTraceOmitsContent(t *testing.T) {
	logDir := t.TempDir()
	auditor, err := audit.NewAuditLogger(logDir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer auditor.Close()

	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
		OutputText:   "ok",
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
	}}}
	loop := NewAgentLoop(llm, NewToolRegistry(), "medium", t.TempDir(), 3, 2000, 200, nil, auditor, nil)
	loop.SetSessionID("test-session")
	loop.SetSkillDiscovery(false)
	loop.SetMemoryPreflight(func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult {
		if opts.Trace != nil {
			opts.Trace.HelperUsed = true
			opts.Trace.IntentSource = "helper"
			opts.Trace.IntentsCount = 1
			opts.Trace.Queried = true
			opts.Trace.ResultsCount = 1
			opts.Trace.ContextReturned = true
			opts.Trace.Outcome = "context_returned"
		}
		return &MemoryPreflightResult{Context: "<private_memory>\n- Example Organization via collaborated_with\n</private_memory>"}
	})

	query := "who is Example Contact to me?"
	if _, _, err := loop.Run(context.Background(), query, nil, nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	var entry map[string]any
	for _, e := range readAuditLines(t, logDir) {
		if e["event"] == "memory_preflight" {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatal("missing memory_preflight audit entry")
	}
	summary, _ := entry["input_summary"].(string)
	if summary == "" {
		t.Fatal("missing memory_preflight summary")
	}
	for _, forbidden := range []string{"Example Contact", "Example Organization", "collaborated_with", query} {
		if strings.Contains(summary, forbidden) {
			t.Fatalf("memory_preflight audit leaked %q in %q", forbidden, summary)
		}
	}
	var decoded struct {
		Attempted       bool   `json:"attempted"`
		ForceHelper     bool   `json:"force_helper"`
		HelperUsed      bool   `json:"helper_used"`
		IntentSource    string `json:"intent_source"`
		IntentsCount    int    `json:"intents_count"`
		Queried         bool   `json:"queried"`
		ResultsCount    int    `json:"results_count"`
		ContextReturned bool   `json:"context_returned"`
		ContextInjected bool   `json:"context_injected"`
		Outcome         string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(summary), &decoded); err != nil {
		t.Fatalf("decode memory_preflight summary: %v", err)
	}
	if !decoded.Attempted || !decoded.ForceHelper || !decoded.HelperUsed || decoded.IntentSource != "helper" {
		t.Fatalf("unexpected trace flags: %+v", decoded)
	}
	if decoded.IntentsCount != 1 || !decoded.Queried || decoded.ResultsCount != 1 || !decoded.ContextReturned || !decoded.ContextInjected {
		t.Fatalf("unexpected trace counts: %+v", decoded)
	}
	if decoded.Outcome != "context_returned" {
		t.Fatalf("outcome=%q want context_returned", decoded.Outcome)
	}
}

func TestStripPrivateMemoryForSummary(t *testing.T) {
	// Defence-in-depth: the compaction summary must never see recalled private
	// memory facts. The agent loop injects <private_memory> into the user
	// message during a turn; this helper strips it before GenerateSummary.
	private := "<private_memory>\nLine A via employed_at\nLine B via has_email\n</private_memory>"
	user := "system header\n\n" + private + "\n\nWho is Example Contact to me?"
	assistant := "Example Contact " + private + " works at..."
	in := []client.Message{
		{Role: "system", Content: client.NewTextContent("system")},
		{Role: "user", Content: client.NewTextContent(user)},
		{Role: "assistant", Content: client.NewTextContent(assistant)},
	}
	got := stripPrivateMemoryForSummary(in)
	if len(got) != len(in) {
		t.Fatalf("len mismatch: got %d want %d", len(got), len(in))
	}
	if strings.Contains(got[1].Content.Text(), "<private_memory>") {
		t.Fatalf("user message still has private memory: %q", got[1].Content.Text())
	}
	if !strings.Contains(got[1].Content.Text(), "Who is Example Contact to me?") {
		t.Fatalf("user message lost the actual question: %q", got[1].Content.Text())
	}
	// Assistant text must be untouched — private memory in the assistant
	// reply is the model's own output and outside this helper's scope.
	if got[2].Content.Text() != assistant {
		t.Fatalf("assistant message changed: %q want %q", got[2].Content.Text(), assistant)
	}
	// Source slice must not be mutated.
	if !strings.Contains(in[1].Content.Text(), "<private_memory>") {
		t.Fatalf("source user message was mutated: %q", in[1].Content.Text())
	}
}

func TestStripPrivateMemoryForSummary_NoOpWhenAbsent(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi")},
	}
	got := stripPrivateMemoryForSummary(in)
	// No private memory present → expect zero-allocation passthrough.
	if &got[0] != &in[0] {
		t.Fatal("strip allocated despite no private_memory blocks present")
	}
}

func TestStripPrivateMemoryForSummary_PreservesImageBlocks(t *testing.T) {
	user := client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: "<private_memory>\nfoo\n</private_memory>\n\nLook at this image"},
			{Type: "image"},
		}),
	}
	out := stripPrivateMemoryForSummary([]client.Message{user})
	stripped := out[0]
	if !stripped.Content.HasBlocks() {
		t.Fatal("stripped message lost block structure")
	}
	blocks := stripped.Content.Blocks()
	if len(blocks) != 2 || blocks[1].Type != "image" {
		t.Fatalf("image block dropped: %+v", blocks)
	}
	if strings.Contains(blocks[0].Text, "<private_memory>") {
		t.Fatalf("text block still has private memory: %q", blocks[0].Text)
	}
	if !strings.Contains(blocks[0].Text, "Look at this image") {
		t.Fatalf("text block lost real content: %q", blocks[0].Text)
	}
}

func TestAgentLoop_MemoryPreflightAuditTraceIncludesErrorClass(t *testing.T) {
	logDir := t.TempDir()
	auditor, err := audit.NewAuditLogger(logDir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer auditor.Close()

	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
		OutputText:   "ok",
		FinishReason: "end_turn",
	}}}
	loop := NewAgentLoop(llm, NewToolRegistry(), "medium", t.TempDir(), 3, 2000, 200, nil, auditor, nil)
	loop.SetSkillDiscovery(false)
	loop.SetMemoryPreflight(func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult {
		if opts.Trace != nil {
			opts.Trace.HelperUsed = true
			opts.Trace.IntentSource = "helper"
			opts.Trace.Outcome = "helper_error"
			opts.Trace.ErrorClass = "auth"
			opts.Trace.HTTPStatus = 401
		}
		return nil
	})

	if _, _, err := loop.Run(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	var summary string
	for _, e := range readAuditLines(t, logDir) {
		if e["event"] == "memory_preflight" {
			summary, _ = e["input_summary"].(string)
			break
		}
	}
	if summary == "" {
		t.Fatal("missing memory_preflight summary")
	}
	var decoded struct {
		Outcome    string `json:"outcome"`
		ErrorClass string `json:"error_class"`
		HTTPStatus int    `json:"http_status"`
	}
	if err := json.Unmarshal([]byte(summary), &decoded); err != nil {
		t.Fatalf("decode memory_preflight summary: %v", err)
	}
	if decoded.Outcome != "helper_error" || decoded.ErrorClass != "auth" || decoded.HTTPStatus != 401 {
		t.Fatalf("unexpected trace: %+v", decoded)
	}
}
