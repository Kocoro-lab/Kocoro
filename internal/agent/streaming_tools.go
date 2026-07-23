package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

type speculativeToolRun struct {
	call   client.FunctionCall
	cancel context.CancelFunc
	done   chan struct{}
	result toolExecResult
}

// streamToolStarter starts only side-effect-free calls whose complete name and
// arguments arrived before the completion stream's final response. The final
// response remains authoritative: results are consumed only when it contains
// the exact same call; unmatched work is cancelled and never enters history.
type streamToolStarter struct {
	mu      sync.Mutex
	ctx     context.Context
	loop    *AgentLoop
	tools   *ToolRegistry
	handler EventHandler
	runs    map[string]*speculativeToolRun
}

func newStreamToolStarter(ctx context.Context, loop *AgentLoop, tools *ToolRegistry, handler EventHandler) *streamToolStarter {
	return &streamToolStarter{
		ctx:     ctx,
		loop:    loop,
		tools:   tools,
		handler: handler,
		runs:    make(map[string]*speculativeToolRun),
	}
}

func streamedToolCallKey(fc client.FunctionCall) string {
	return fc.ID + "\x00" + fc.Name + "\x00" + normalizeJSON(fc.Arguments)
}

func (s *streamToolStarter) eligible(fc client.FunctionCall, activeSkillFilter map[string]bool) (Tool, string, bool) {
	if s == nil || s.loop == nil || s.tools == nil || fc.Name == "" || fc.Name == "tool_search" {
		return nil, "", false
	}
	tool, ok := s.tools.Get(fc.Name)
	if !ok {
		return nil, "", false
	}
	if activeSkillFilter != nil && !IsSkillExempt(tool) && !activeSkillFilter[fc.Name] {
		return nil, "", false
	}
	argsStr := fc.ArgumentsString()
	if _, valid := ValidateToolArgumentPresence(tool.Info(), argsStr); !valid {
		return nil, "", false
	}
	readOnly, ok := tool.(ReadOnlyChecker)
	if !ok || !readOnly.IsReadOnlyCall(argsStr) || tool.RequiresApproval() {
		return nil, "", false
	}
	// Hooks are allowed to deny or transform the execution environment. Keep
	// their ordering exact by leaving those calls on the normal post-stream
	// path rather than firing hooks from the network callback.
	if s.loop.hookRunner != nil {
		return nil, "", false
	}
	if s.loop.permissions != nil {
		decision, _ := permissions.CheckToolCall(fc.Name, argsStr, s.loop.permissions)
		if decision == "deny" || decision == "ask" {
			return nil, "", false
		}
	}
	return tool, argsStr, true
}

func (s *streamToolStarter) Start(fc client.FunctionCall, activeSkillFilter map[string]bool) {
	tool, argsStr, ok := s.eligible(fc, activeSkillFilter)
	if !ok {
		return
	}
	key := streamedToolCallKey(fc)
	s.mu.Lock()
	if _, exists := s.runs[key]; exists {
		s.mu.Unlock()
		return
	}
	toolCtx, cancel := context.WithCancel(s.ctx)
	run := &speculativeToolRun{call: fc, cancel: cancel, done: make(chan struct{})}
	s.runs[key] = run
	s.mu.Unlock()

	if s.loop.tracker != nil {
		s.loop.tracker.Enter(PhaseExecutingTools)
	}
	if s.handler != nil {
		// Usage belongs to the real provider/tool work and is emitted even if
		// the final response later omits this speculative call. Such a result
		// stays out of transcript/UI, but hiding its incurred usage would
		// under-report billing.
		toolCtx = WithUsageEmit(toolCtx, s.handler.OnUsage)
	}
	go func() {
		defer close(run.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				run.result = toolExecResult{
					result: ToolResult{Content: fmt.Sprintf("tool panicked: %v", recovered), IsError: true},
					name:   fc.Name,
				}
			}
		}()
		dispatchToolCtx, dispatchCancel := dispatchCtx(toolCtx, tool)
		if dispatchCancel != nil {
			defer dispatchCancel()
		}
		start := time.Now()
		result, err := tool.Run(dispatchToolCtx, argsStr)
		run.result = toolExecResult{
			result:  result,
			elapsed: time.Since(start),
			err:     err,
			name:    fc.Name,
		}
	}()
}

func (s *streamToolStarter) Claim(ctx context.Context, fc client.FunctionCall) (toolExecResult, bool) {
	if s == nil {
		return toolExecResult{}, false
	}
	key := streamedToolCallKey(fc)
	s.mu.Lock()
	run, ok := s.runs[key]
	if ok {
		delete(s.runs, key)
	}
	s.mu.Unlock()
	if !ok {
		return toolExecResult{}, false
	}
	if s.handler != nil {
		// Keep speculative work invisible until the final response commits the
		// exact call. This avoids orphaned or failed-looking tool cards when an
		// upstream stream reports a call that its final response does not keep.
		s.handler.OnToolCall(fc.Name, fc.ArgumentsString(), fc.ID)
	}
	select {
	case <-run.done:
		run.cancel()
		return run.result, true
	case <-ctx.Done():
		run.cancel()
		return toolExecResult{
			result: ToolResult{Content: "tool startup cancelled with the turn", IsError: true},
			err:    ctx.Err(),
			name:   fc.Name,
		}, true
	}
}

func (s *streamToolStarter) CancelUnmatched(finalCalls []client.FunctionCall) {
	if s == nil {
		return
	}
	keep := make(map[string]struct{}, len(finalCalls))
	for _, call := range finalCalls {
		keep[streamedToolCallKey(call)] = struct{}{}
	}
	s.mu.Lock()
	for key, run := range s.runs {
		if _, ok := keep[key]; ok {
			continue
		}
		delete(s.runs, key)
		run.cancel()
	}
	s.mu.Unlock()
	// No tool card was emitted before final commitment, so cancellation is
	// intentionally silent. The speculative result never enters history.
}

func (s *streamToolStarter) CancelAll() {
	s.CancelUnmatched(nil)
}
