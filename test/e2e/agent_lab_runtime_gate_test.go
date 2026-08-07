package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// TestOffline_AgentLabGeneralPurposePromptContract deliberately stops at the
// provider boundary. Offline fixtures cannot establish writing or research
// quality, but they can fail the release if the production AgentLoop stops
// presenting Kocoro as a general-purpose assistant for common task domains.
func TestOffline_AgentLabGeneralPurposePromptContract(t *testing.T) {
	cases := []struct {
		name          string
		prompt        string
		source        string
		registerThink bool
		required      []string
	}{
		{
			name:   "everyday writing",
			prompt: "帮我写一封简短的会议改期邮件。",
			required: []string{
				"for writing, write",
				"A request for an email, meeting agenda, research summary, or plan is not a coding task.",
			},
		},
		{
			name:   "bounded research",
			prompt: "研究两种通勤方案并给出有来源的比较。",
			required: []string{
				"for research, research",
				"Never invent tool output, state changes, sources, identifiers, URLs, restrictions, or completion.",
			},
		},
		{
			name:          "everyday planning",
			prompt:        "根据周五截止日期安排三天准备计划。",
			registerThink: true,
			required: []string{
				"### Planning",
				"Complete the outcome the user requested in the domain they chose.",
			},
		},
		{
			name:   "voice entry remains general purpose",
			prompt: "用一句话告诉我今天先处理哪件事。",
			source: "kocoro",
			required: []string{
				"Kocoro is not limited to everyday work or coding",
				"Reply in the language of the user's latest substantive message",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &agentLabCaptureClient{}
			registry := agent.NewToolRegistry()
			if tc.registerThink {
				registry.Register(&agentLabThinkTool{})
			}
			loop := agent.NewAgentLoop(provider, registry, "medium", t.TempDir(), 3, 4_000, 200, nil, nil, nil)
			loop.SetSkillDiscovery(false)
			if tc.source != "" {
				loop.SetSource(tc.source)
			}

			reply, _, err := loop.Run(context.Background(), tc.prompt, nil, nil)
			if err != nil {
				t.Fatalf("AgentLoop.Run: %v", err)
			}
			if reply != "offline contract probe accepted" {
				t.Fatalf("reply = %q", reply)
			}
			req := provider.singleRequest(t)
			if !requestContainsRoleText(req, "user", tc.prompt) {
				t.Fatalf("provider request lost user outcome: %q", tc.prompt)
			}
			system := requestRoleText(req, "system")
			for _, phrase := range tc.required {
				if !strings.Contains(system, phrase) {
					t.Errorf("system prompt missing %q", phrase)
				}
			}
		})
	}
}

func TestOffline_AgentLabLongReadTrajectoryReachesOutcome(t *testing.T) {
	const steps = 14
	provider := &agentLabTrajectoryClient{totalSteps: steps}
	tool := newAgentLabReadTool(192)
	registry := agent.NewToolRegistry()
	registry.Register(tool)
	loop := agent.NewAgentLoop(provider, registry, "medium", t.TempDir(), 24, 4_000, 200, nil, nil, nil)
	loop.SetSkillDiscovery(false)

	reply, usage, err := loop.Run(context.Background(), "Inspect every bounded source, then report the final evidence marker.", nil, nil)
	if err != nil {
		t.Fatalf("long AgentLoop.Run: %v", err)
	}
	if want := fmt.Sprintf("completed through STEP-%02d", steps); !strings.Contains(reply, want) {
		t.Fatalf("reply = %q, want %q", reply, want)
	}
	if usage == nil || usage.LLMCalls < steps+1 {
		t.Fatalf("usage = %+v, want at least %d calls including the final synthesis", usage, steps+1)
	}
	// The production loop may spend one bounded provider turn on its long-run
	// progress/nudge seam. The release contract is completion without skipped
	// or replayed work, while still bounding that overhead to one call.
	if got := provider.mainCallCount(); got < steps+1 || got > steps+2 {
		t.Fatalf("main provider calls = %d, want %d..%d", got, steps+1, steps+2)
	}
	assertEachTrajectoryStepRanOnce(t, tool.snapshot(), steps)
	if got := countTrajectoryToolResults(loop.RunMessages()); got != steps {
		t.Fatalf("lossless run transcript has %d file_read results, want %d", got, steps)
	}
}

func TestOffline_AgentLabCompactionPersistsAcrossRestart(t *testing.T) {
	const steps = 14
	provider := &agentLabTrajectoryClient{totalSteps: steps, reportGrowingUsage: true}
	tool := newAgentLabReadTool(2_048)
	registry := agent.NewToolRegistry()
	registry.Register(tool)
	loop := agent.NewAgentLoop(provider, registry, "medium", t.TempDir(), 30, 4_000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(20_000)
	loop.SetSkillDiscovery(false)

	reply, _, err := loop.Run(context.Background(), "Complete the long evidence review without losing the latest marker.", nil, nil)
	if err != nil {
		t.Fatalf("compacting AgentLoop.Run: %v", err)
	}
	if !strings.Contains(reply, "STEP-14") {
		t.Fatalf("final reply lost the last marker: %q", reply)
	}
	archive := loop.RunMessages()
	checkpoint := loop.CompactionCheckpointMessages()
	if len(checkpoint) == 0 {
		t.Fatal("long trajectory did not produce a compaction checkpoint")
	}
	if len(checkpoint) >= len(archive) {
		t.Fatalf("checkpoint did not reduce live context: checkpoint=%d archive=%d", len(checkpoint), len(archive))
	}
	if !messagesContain(checkpoint, "Previous context summary:") {
		t.Fatal("checkpoint is missing the production compaction marker")
	}

	store := session.NewStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	sess := &session.Session{
		ID:        "agent-lab-compaction-restart",
		Title:     "agent lab compaction restart",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  archive,
		CompactionCheckpoint: &session.CompactionCheckpoint{
			SchemaVersion:       session.CompactionCheckpointSchemaVersion,
			ArchiveThroughIndex: len(archive),
			Messages:            checkpoint,
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("save compacted session: %v", err)
	}
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("load compacted session: %v", err)
	}
	liveHistory := loaded.HistoryForLoop()
	if len(liveHistory) == 0 || len(liveHistory) >= len(loaded.Messages) {
		t.Fatalf("restart did not select compacted live history: live=%d archive=%d", len(liveHistory), len(loaded.Messages))
	}
	if !messagesContain(liveHistory, "STEP-14") {
		t.Fatal("compacted live history lost the final trajectory marker")
	}

	restartProvider := &agentLabRecallClient{marker: "STEP-14"}
	restarted := agent.NewAgentLoop(restartProvider, registry, "medium", t.TempDir(), 3, 4_000, 200, nil, nil, nil)
	restarted.SetSkillDiscovery(false)
	restartReply, _, err := restarted.Run(context.Background(), "Which evidence step did the previous run finish?", nil, liveHistory)
	if err != nil {
		t.Fatalf("restarted AgentLoop.Run: %v", err)
	}
	if restartReply != "restart retained STEP-14" {
		t.Fatalf("restart reply = %q", restartReply)
	}
	assertEachTrajectoryStepRanOnce(t, tool.snapshot(), steps)
}

func TestOffline_AgentLabInterruptedTrajectoryResumesWithoutReplay(t *testing.T) {
	const (
		totalSteps = 10
		stopAfter  = 6
	)
	provider := &agentLabTrajectoryClient{totalSteps: totalSteps}
	tool := newAgentLabReadTool(128)
	registry := agent.NewToolRegistry()
	registry.Register(tool)
	loop := agent.NewAgentLoop(provider, registry, "medium", t.TempDir(), 20, 4_000, 200, nil, nil, nil)
	loop.SetSkillDiscovery(false)

	ctx, cancel := context.WithCancel(context.Background())
	loop.SetCheckpointFunc(func(context.Context) error {
		if len(tool.snapshot()) == stopAfter {
			cancel()
		}
		return nil
	})
	_, _, err := loop.Run(ctx, "Process the bounded trajectory and checkpoint after each source.", nil, nil)
	if err == nil {
		t.Fatal("interrupted trajectory unexpectedly completed")
	}
	checkpoint := loop.RunMessages()
	if got := len(tool.snapshot()); got != stopAfter {
		t.Fatalf("executed %d steps before interruption, want %d", got, stopAfter)
	}
	if got := countTrajectoryToolResults(checkpoint); got != stopAfter {
		t.Fatalf("checkpoint has %d completed tool results, want %d", got, stopAfter)
	}

	resumedProvider := &agentLabTrajectoryClient{totalSteps: totalSteps}
	resumed := agent.NewAgentLoop(resumedProvider, registry, "medium", t.TempDir(), 20, 4_000, 200, nil, nil, nil)
	resumed.SetSkillDiscovery(false)
	reply, _, err := resumed.ResumeInterrupted(context.Background(), "Continue from the durable checkpoint.", checkpoint)
	if err != nil {
		t.Fatalf("ResumeInterrupted: %v", err)
	}
	if !strings.Contains(reply, "STEP-10") {
		t.Fatalf("resumed reply = %q", reply)
	}
	assertEachTrajectoryStepRanOnce(t, tool.snapshot(), totalSteps)
}

type agentLabCaptureClient struct {
	mu       sync.Mutex
	requests []client.CompletionRequest
}

func (c *agentLabCaptureClient) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return &client.CompletionResponse{
		OutputText:   "offline contract probe accepted",
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105},
	}, nil
}

func (c *agentLabCaptureClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func (c *agentLabCaptureClient) singleRequest(t *testing.T) client.CompletionRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(c.requests))
	}
	return c.requests[0]
}

type agentLabThinkTool struct{}

func (*agentLabThinkTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "think",
		Description: "Record a structured thought for a genuinely complex plan.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thought": map[string]any{"type": "string"},
			},
			"required": []string{"thought"},
		},
		Required: []string{"thought"},
	}
}

func (*agentLabThinkTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "thought recorded"}, nil
}

func (*agentLabThinkTool) RequiresApproval() bool { return false }

type agentLabReadTool struct {
	mu          sync.Mutex
	runs        map[int]int
	payloadSize int
}

func newAgentLabReadTool(payloadSize int) *agentLabReadTool {
	return &agentLabReadTool{runs: make(map[int]int), payloadSize: payloadSize}
}

func (*agentLabReadTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "file_read",
		Description: "Read one bounded release-gate evidence source.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step": map[string]any{"type": "integer"},
			},
			"required": []string{"step"},
		},
		Required: []string{"step"},
	}
}

func (t *agentLabReadTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args struct {
		Step int `json:"step"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid input: %v", err)), nil
	}
	if args.Step <= 0 {
		return agent.ValidationError("step must be positive"), nil
	}
	t.mu.Lock()
	t.runs[args.Step]++
	t.mu.Unlock()
	content := fmt.Sprintf("trajectory evidence STEP-%02d ", args.Step) + strings.Repeat("e", t.payloadSize)
	return agent.ToolResult{Content: content}, nil
}

func (*agentLabReadTool) RequiresApproval() bool            { return false }
func (*agentLabReadTool) IsReadOnlyCall(string) bool        { return true }
func (*agentLabReadTool) IsConcurrencySafeCall(string) bool { return true }

func (t *agentLabReadTool) snapshot() map[int]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[int]int, len(t.runs))
	for step, count := range t.runs {
		out[step] = count
	}
	return out
}

type agentLabTrajectoryClient struct {
	mu                 sync.Mutex
	totalSteps         int
	reportGrowingUsage bool
	mainCalls          int
	summaryCalls       int
}

var agentLabStepPattern = regexp.MustCompile(`STEP-(\d{2})`)

func (c *agentLabTrajectoryClient) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	completed := highestTrajectoryStep(req.Messages)
	if req.ModelTier == "small" {
		c.mu.Lock()
		c.summaryCalls++
		c.mu.Unlock()
		summary := fmt.Sprintf(`<analysis>Completed bounded source reads through STEP-%02d.</analysis>
<summary>
## Current task & next steps
Continue the bounded evidence review after STEP-%02d and finish all %d steps.
</summary>`, completed, completed, c.totalSteps)
		return &client.CompletionResponse{
			OutputText:   summary,
			FinishReason: "end_turn",
			Model:        "offline-small",
			Usage:        client.Usage{InputTokens: 200, OutputTokens: 50, TotalTokens: 250},
		}, nil
	}

	c.mu.Lock()
	c.mainCalls++
	mainCall := c.mainCalls
	c.mu.Unlock()
	usage := client.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}
	if c.reportGrowingUsage {
		usage.InputTokens = len(req.Messages) * 3_000
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if completed >= c.totalSteps {
		return &client.CompletionResponse{
			OutputText:   fmt.Sprintf("completed through STEP-%02d", completed),
			FinishReason: "end_turn",
			Model:        "offline-main",
			Usage:        usage,
		}, nil
	}
	next := completed + 1
	return &client.CompletionResponse{
		FinishReason: "tool_use",
		FunctionCall: &client.FunctionCall{
			ID:        fmt.Sprintf("agent-lab-read-%02d-call-%02d", next, mainCall),
			Name:      "file_read",
			Arguments: json.RawMessage(fmt.Sprintf(`{"step":%d}`, next)),
		},
		Model: "offline-main",
		Usage: usage,
	}, nil
}

func (c *agentLabTrajectoryClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func (c *agentLabTrajectoryClient) mainCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mainCalls
}

type agentLabRecallClient struct{ marker string }

func (c *agentLabRecallClient) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	if !messagesContain(req.Messages, c.marker) {
		return nil, fmt.Errorf("restart request missing %s", c.marker)
	}
	return &client.CompletionResponse{
		OutputText:   "restart retained " + c.marker,
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105},
	}, nil
}

func (c *agentLabRecallClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func highestTrajectoryStep(messages []client.Message) int {
	highest := 0
	for _, message := range messages {
		for _, match := range agentLabStepPattern.FindAllStringSubmatch(message.Content.Text(), -1) {
			step, err := strconv.Atoi(match[1])
			if err == nil && step > highest {
				highest = step
			}
		}
	}
	return highest
}

func requestRoleText(req client.CompletionRequest, role string) string {
	var texts []string
	for _, message := range req.Messages {
		if message.Role == role {
			texts = append(texts, message.Content.Text())
		}
	}
	return strings.Join(texts, "\n")
}

func requestContainsRoleText(req client.CompletionRequest, role, want string) bool {
	return strings.Contains(requestRoleText(req, role), want)
}

func messagesContain(messages []client.Message, want string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content.Text(), want) {
			return true
		}
	}
	return false
}

func countTrajectoryToolResults(messages []client.Message) int {
	count := 0
	for _, message := range messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_result" && strings.Contains(client.ToolResultText(block), "STEP-") {
				count++
			}
		}
	}
	return count
}

func assertEachTrajectoryStepRanOnce(t *testing.T, runs map[int]int, total int) {
	t.Helper()
	if len(runs) != total {
		t.Fatalf("executed step cardinality = %d, want %d: %+v", len(runs), total, runs)
	}
	for step := 1; step <= total; step++ {
		if runs[step] != 1 {
			t.Fatalf("step %d executions = %d, want 1: %+v", step, runs[step], runs)
		}
	}
}
