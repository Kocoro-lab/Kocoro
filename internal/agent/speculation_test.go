package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestBuildSpeculationRequest_ByteEqualPrefix(t *testing.T) {
	main := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("fix the bug")},
			{Role: "assistant", Content: client.NewTextContent("I fixed it.")},
		},
		ModelTier:   "medium",
		CacheSource: "channel",
		Tools:       []client.Tool{{Name: "file_read"}},
	}
	spec := BuildSpeculationRequest(main, "run the test now")

	// Exactly one more message at end
	if len(spec.Messages) != len(main.Messages)+1 {
		t.Fatalf("messages len = %d, want %d", len(spec.Messages), len(main.Messages)+1)
	}
	last := spec.Messages[len(spec.Messages)-1]
	if last.Role != "user" {
		t.Errorf("last role = %q, want user", last.Role)
	}
	// Verify last message contains the suggestion text. Use the MessageContent
	// type's Text() / String() helper or compare via JSON marshal — whichever
	// the existing client.NewTextContent contract guarantees.
	{
		b, _ := json.Marshal(last.Content)
		if !contains(string(b), "run the test now") {
			t.Errorf("last content JSON does not contain suggestion text: %s", string(b))
		}
	}

	// Prefix byte-equal
	for i := range main.Messages {
		if !reflect.DeepEqual(main.Messages[i], spec.Messages[i]) {
			t.Errorf("Messages[%d] differs", i)
		}
	}

	// SkipCacheWrite set
	if !spec.SkipCacheWrite {
		t.Error("speculation must have SkipCacheWrite=true")
	}
	if spec.ForkedKind != "speculation" {
		t.Errorf("ForkedKind = %q, want speculation", spec.ForkedKind)
	}

	// All other fields byte-equal (excluding Messages/SkipCacheWrite/ForkedKind)
	mainCopy := main
	specCopy := spec
	mainCopy.Messages = nil
	specCopy.Messages = nil
	specCopy.SkipCacheWrite = false
	specCopy.ForkedKind = ""
	mb, _ := json.Marshal(mainCopy)
	sb, _ := json.Marshal(specCopy)
	if string(mb) != string(sb) {
		t.Errorf("non-prefix fields differ:\n  main: %s\n  spec: %s", mb, sb)
	}
}

// small inline contains helper to avoid importing strings just for this
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRunSpeculation_HappyPath(t *testing.T) {
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}
	llm := &fakeLLM{resp: "Here is what I would do: step 1, step 2."}

	got, err := RunSpeculation(context.Background(), llm, main, "do the next step")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "Here is what I would do: step 1, step 2." {
		t.Errorf("got %q", got)
	}
	if !llm.gotReq.SkipCacheWrite {
		t.Error("speculation request did not have SkipCacheWrite=true")
	}
	if llm.gotReq.ForkedKind != "speculation" {
		t.Errorf("ForkedKind = %q, want speculation", llm.gotReq.ForkedKind)
	}
}

func TestRunSpeculation_GatewayError(t *testing.T) {
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}
	llm := &fakeLLM{completeErr: errors.New("boom")}

	got, err := RunSpeculation(context.Background(), llm, main, "next")
	if err == nil {
		t.Error("expected error from gateway")
	}
	if got != "" {
		t.Errorf("expected empty result on error, got %q", got)
	}
}

func TestRunSpeculation_EmptyResponse(t *testing.T) {
	// Defensive: nil/empty OutputText returns ("", nil), not an error.
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}
	llm := &fakeLLM{resp: ""}

	got, err := RunSpeculation(context.Background(), llm, main, "next")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty on empty OutputText", got)
	}
}

// TestRunSpeculationWithUsage_PopulatesUsage pins the cache-audit contract for
// the Usage-returning speculation variant — the daemon's post-Run hook needs
// model + cache_read_tokens to write audit rows.
func TestRunSpeculationWithUsage_PopulatesUsage(t *testing.T) {
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}
	// fakeLLM hardcodes Usage{InputTokens:100, CacheReadTokens:95}.
	llm := &fakeLLM{resp: "Here is the pre-run response."}

	res, err := RunSpeculationWithUsage(context.Background(), llm, main, "do the next step")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Text != "Here is the pre-run response." {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", res.Usage.InputTokens)
	}
	if res.Usage.CacheReadTokens != 95 {
		t.Errorf("Usage.CacheReadTokens = %d, want 95", res.Usage.CacheReadTokens)
	}
	if res.Model == "" {
		t.Error("Model should be populated for audit correlation")
	}
}
