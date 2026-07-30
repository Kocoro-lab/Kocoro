package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestOpenAIComputerCompletionFixtureReachesStrictBatchAdapterEntry(t *testing.T) {
	path := filepath.Join(
		"..",
		"..",
		"docs",
		"desktop-wire-fixtures",
		"execution-profiles-v1",
		"completion.openai-native-computer-call.json",
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OpenAI completion fixture: %v", err)
	}
	var response client.CompletionResponse
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("decode OpenAI completion fixture: %v", err)
	}
	if len(response.ContentBlocks) != 1 {
		t.Fatalf("completion content blocks = %#v", response.ContentBlocks)
	}
	normalized, err := json.Marshal(response.ContentBlocks[0])
	if err != nil {
		t.Fatalf("marshal normalized computer_call: %v", err)
	}
	call, err := DecodeOpenAIComputerCallV1(normalized)
	if err != nil {
		t.Fatalf("strict batch adapter decode: %v", err)
	}
	if call.CallID != "call_001" ||
		call.ToolContract != client.ToolContractOpenAIComputerV1 ||
		len(call.Actions) != 2 {
		t.Fatalf("strict batch adapter call = %#v", call)
	}
	if !OpenAINativeComputerBatchExecutionAvailable() {
		t.Fatal("canonical fixture did not reach the enabled production batch executor")
	}
}
