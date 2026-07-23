package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

// defaultMaxConcurrentRequests caps how many tool calls execute concurrently
// within a single MCP session.
//
//   - Workload: a client pipelining tool calls (e.g. an agent fanning out a
//     batch of reads) — 64 covers normal fan-out with headroom.
//   - Symptom when it binds: excess calls queue on a semaphore acquired inside
//     the request goroutine (the scanner keeps reading frames, so cancellations
//     and elicitation responses still land) instead of spawning unbounded
//     goroutines that a misbehaving or malicious client could use to exhaust
//     memory.
//   - Override: SHANNON_MCP_MAX_CONCURRENT_REQUESTS accepts a positive integer;
//     a non-positive or unparseable value keeps the default.
const defaultMaxConcurrentRequests = 64

func maxConcurrentRequests() int {
	if v := os.Getenv("SHANNON_MCP_MAX_CONCURRENT_REQUESTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxConcurrentRequests
}

// defaultEOFDrainGrace bounds how long Serve waits for in-flight requests to
// finish after the input stream ends (clean EOF) before force-cancelling them.
//
//   - Workload: a client that closed stdin while a tool call is still running —
//     2 seconds lets nearly-done in-process work flush its response (single-shot
//     callers rely on this to receive their reply after closing the pipe).
//   - Symptom when it binds: a tool blocked on ctx.Done() would otherwise pin
//     wg.Wait() forever; once the grace elapses the request is cancelled and
//     Serve returns.
//   - Override: SHANNON_MCP_EOF_DRAIN_GRACE accepts a Go duration string (e.g.
//     "100ms"); a non-positive or unparseable value keeps the default.
const defaultEOFDrainGrace = 2 * time.Second

func eofDrainGrace() time.Duration {
	if v := os.Getenv("SHANNON_MCP_EOF_DRAIN_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultEOFDrainGrace
}

// JSON-RPC 2.0 types.

type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type inboundMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP protocol types.

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
}

type ClientCapabilities struct {
	Elicitation *ElicitationCapability `json:"elicitation,omitempty"`
}

type ElicitationCapability struct {
	Form map[string]any `json:"form,omitempty"`
	URL  map[string]any `json:"url,omitempty"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolsListResult struct {
	Tools []ToolDef `json:"tools"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      RequestMeta     `json:"_meta,omitempty"`
}

type RequestMeta struct {
	ProgressToken json.RawMessage `json:"progressToken,omitempty"`
}

type ToolCallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ProgressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         float64         `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

type CancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

type ElicitationParams struct {
	Mode            string         `json:"mode,omitempty"`
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema"`
}

type ElicitationResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// Server is a lightweight MCP server that exposes a ToolRegistry over
// JSON-RPC 2.0 via stdio.
type Server struct {
	tools       *agent.ToolRegistry
	name        string
	version     string
	permissions *permissions.PermissionsConfig
	auditor     *audit.AuditLogger
	hookRunner  *hooks.HookRunner
}

// NewServer creates a new MCP server backed by the given tool registry.
func NewServer(tools *agent.ToolRegistry, name, version string, perms *permissions.PermissionsConfig, auditor *audit.AuditLogger, hookRunner *hooks.HookRunner) *Server {
	return &Server{
		tools:       tools,
		name:        name,
		version:     version,
		permissions: perms,
		auditor:     auditor,
		hookRunner:  hookRunner,
	}
}

type serverSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	enc    *json.Encoder

	writeMu sync.Mutex
	wg      sync.WaitGroup

	activeMu sync.Mutex
	active   map[string]context.CancelFunc

	pendingMu sync.Mutex
	pending   map[string]chan inboundMessage
	nextID    atomic.Uint64

	stateMu             sync.RWMutex
	initialized         bool
	supportsElicitation bool
	writeErr            error
}

func newServerSession(ctx context.Context, writer io.Writer) *serverSession {
	sessionCtx, cancel := context.WithCancel(ctx)
	return &serverSession{
		ctx:     sessionCtx,
		cancel:  cancel,
		enc:     json.NewEncoder(writer),
		active:  make(map[string]context.CancelFunc),
		pending: make(map[string]chan inboundMessage),
	}
}

func rawIDKey(id *json.RawMessage) string {
	if id == nil {
		return ""
	}
	return string(*id)
}

func (ss *serverSession) write(value any) error {
	ss.writeMu.Lock()
	defer ss.writeMu.Unlock()
	if ss.writeErr != nil {
		return ss.writeErr
	}
	if err := ss.enc.Encode(value); err != nil {
		ss.writeErr = err
		ss.cancel()
		return err
	}
	return nil
}

func (ss *serverSession) setInitialized(params json.RawMessage) {
	var init InitializeParams
	_ = json.Unmarshal(params, &init)
	protocolVersion := negotiatedProtocolVersion(params)
	ss.stateMu.Lock()
	ss.supportsElicitation =
		(protocolVersion == "2025-06-18" || protocolVersion == "2025-11-25") &&
			init.Capabilities.Elicitation != nil &&
			init.Capabilities.Elicitation.Form != nil
	ss.stateMu.Unlock()
}

func (ss *serverSession) markReady() {
	ss.stateMu.Lock()
	ss.initialized = true
	ss.stateMu.Unlock()
}

func (ss *serverSession) ready() bool {
	ss.stateMu.RLock()
	defer ss.stateMu.RUnlock()
	return ss.initialized
}

func (ss *serverSession) canElicit() bool {
	ss.stateMu.RLock()
	defer ss.stateMu.RUnlock()
	return ss.supportsElicitation
}

// tryRegisterActive records a cancel func for an in-flight request id. It
// returns false when that id is already in flight, leaving the existing entry
// intact so a duplicate request cannot clobber the original's cancel func.
func (ss *serverSession) tryRegisterActive(id *json.RawMessage, cancel context.CancelFunc) bool {
	key := rawIDKey(id)
	ss.activeMu.Lock()
	defer ss.activeMu.Unlock()
	if _, exists := ss.active[key]; exists {
		return false
	}
	ss.active[key] = cancel
	return true
}

func (ss *serverSession) finishActive(id *json.RawMessage) {
	ss.activeMu.Lock()
	delete(ss.active, rawIDKey(id))
	ss.activeMu.Unlock()
}

func (ss *serverSession) cancelActive(id json.RawMessage) {
	ss.activeMu.Lock()
	cancel := ss.active[string(id)]
	ss.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (ss *serverSession) cancelAllActive() {
	ss.activeMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(ss.active))
	for _, cancel := range ss.active {
		cancels = append(cancels, cancel)
	}
	ss.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (ss *serverSession) deliverResponse(msg inboundMessage) {
	key := rawIDKey(msg.ID)
	ss.pendingMu.Lock()
	ch := ss.pending[key]
	ss.pendingMu.Unlock()
	if ch != nil {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Serve reads newline-delimited JSON-RPC messages. Requests execute
// concurrently so cancellation notifications and responses to server-initiated
// elicitation remain processable while a tool is running.
func (s *Server) Serve(ctx context.Context, reader io.Reader, writer io.Writer) error {
	ss := newServerSession(ctx, writer)
	defer ss.cancel()

	changes, unsubscribe := s.tools.SubscribeChanges()
	defer unsubscribe()
	listChangedDone := make(chan struct{})
	go func() {
		defer close(listChangedDone)
		for {
			select {
			case <-ss.ctx.Done():
				return
			case <-changes:
				if ss.ready() {
					_ = ss.write(map[string]any{
						"jsonrpc": "2.0",
						"method":  "notifications/tools/list_changed",
					})
				}
			}
		}
	}()

	// Bounds concurrent tool EXECUTION, not frame reading. Acquired inside the
	// request goroutine so the scanner stays responsive while requests queue.
	sem := make(chan struct{}, maxConcurrentRequests())

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		if ss.ctx.Err() != nil {
			break
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg inboundMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			_ = ss.write(errorResponse(nil, -32700, "parse error"))
			continue
		}
		if msg.Method == "" && msg.ID != nil {
			ss.deliverResponse(msg)
			continue
		}
		if msg.ID == nil {
			switch msg.Method {
			case "notifications/initialized":
				ss.markReady()
			case "notifications/cancelled":
				var params CancelledParams
				if json.Unmarshal(msg.Params, &params) == nil && len(params.RequestID) > 0 {
					ss.cancelActive(params.RequestID)
				}
			}
			continue
		}

		if msg.Method == "initialize" {
			ss.setInitialized(msg.Params)
			_ = ss.write(s.handleInitialize(msg.ID, msg.Params))
			continue
		}

		requestCtx, requestCancel := context.WithCancel(ss.ctx)
		if !ss.tryRegisterActive(msg.ID, requestCancel) {
			requestCancel()
			_ = ss.write(errorResponse(msg.ID, -32600, "request id already in flight"))
			continue
		}
		ss.wg.Add(1)
		go func(msg inboundMessage, requestCtx context.Context, requestCancel context.CancelFunc) {
			defer ss.wg.Done()
			defer requestCancel()
			defer ss.finishActive(msg.ID)

			// Bound concurrent execution. Acquiring here (not before spawn) keeps
			// the scanner reading further frames (cancellations, elicitation
			// responses) while this request queues. The acquire is blocking, not
			// cancel-aborted: on session teardown running requests are cancelled
			// and release the semaphore, so a queued request drains rather than
			// hanging, and an EOF-cancelled request still flushes its response.
			sem <- struct{}{}
			defer func() { <-sem }()

			requestCtx = withLifecycleSession(requestCtx, ss)

			var resp Response
			switch msg.Method {
			case "tools/list":
				resp = s.handleToolsList(msg.ID)
			case "tools/call":
				resp = s.handleToolCall(requestCtx, msg.ID, msg.Params)
			default:
				resp = errorResponse(msg.ID, -32601, "method not found: "+msg.Method)
			}
			// Cancellation is fire-and-forget: once cancelled, the receiver
			// stops work and does not send a late response for that request.
			if requestCtx.Err() == nil {
				_ = ss.write(resp)
			}
		}(msg, requestCtx, requestCancel)
	}

	scanErr := scanner.Err()
	if scanErr != nil || ctx.Err() != nil {
		// The read failed or the parent context is done: no client remains, so
		// cancel in-flight work immediately.
		ss.cancelAllActive()
	}
	// Drain in-flight requests, then guarantee termination. On a clean EOF the
	// parent ctx is still live and nothing has been cancelled yet — running
	// requests get a bounded grace to finish and flush their responses (this is
	// how single-shot callers that close stdin still receive their reply), then
	// any straggler is cancelled so a tool blocked on ctx.Done() cannot pin
	// wg.Wait() forever. When cancelAllActive already ran above, the drain
	// completes immediately.
	drained := make(chan struct{})
	go func() {
		ss.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(eofDrainGrace()):
		ss.cancelAllActive()
		<-drained
	}
	ss.cancel()
	<-listChangedDone
	if scanErr != nil {
		return scanErr
	}
	if ss.writeErr != nil {
		return ss.writeErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func negotiatedProtocolVersion(params json.RawMessage) string {
	var init InitializeParams
	if json.Unmarshal(params, &init) != nil || init.ProtocolVersion == "" {
		return "2024-11-05"
	}
	switch init.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return init.ProtocolVersion
	default:
		return "2025-11-25"
	}
}

func (s *Server) handleInitialize(id *json.RawMessage, params json.RawMessage) Response {
	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Result: InitializeResult{
			ProtocolVersion: negotiatedProtocolVersion(params),
			Capabilities: Capabilities{
				Tools: &ToolsCapability{ListChanged: true},
			},
			ServerInfo: ServerInfo{Name: s.name, Version: s.version},
		},
	}
}

func (s *Server) handleToolsList(id *json.RawMessage) Response {
	var tools []ToolDef
	for _, t := range s.tools.All() {
		info := t.Info()
		schema := cloneSchema(info.Parameters)
		// Current builtin tools expose a complete JSON Schema in Parameters.
		// Keep compatibility with older/custom tools that expose only the flat
		// properties map, but never nest a complete schema under properties.
		if _, hasType := schema["type"]; !hasType {
			schema = map[string]any{"type": "object", "properties": schema}
		} else if _, hasProperties := schema["properties"]; !hasProperties {
			schema["properties"] = map[string]any{}
		}
		if len(info.Required) > 0 {
			schema["required"] = info.Required
		}
		schemaJSON, _ := json.Marshal(schema)
		tools = append(tools, ToolDef{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: schemaJSON,
		})
	}
	if tools == nil {
		tools = []ToolDef{}
	}
	return Response{JSONRPC: "2.0", ID: id, Result: ToolsListResult{Tools: tools}}
}

func (s *Server) handleToolCall(ctx context.Context, id *json.RawMessage, params json.RawMessage) Response {
	var p ToolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	tool, ok := s.tools.Get(p.Name)
	if !ok {
		return errorResponse(id, -32602, "unknown tool: "+p.Name)
	}

	argsStr := string(p.Arguments)

	// Validate before permission evaluation or hooks so malformed calls cannot
	// prompt for approval, trigger automation, or reach Tool.Run.
	if validationResult, valid := agent.ValidateToolArgumentPresence(tool.Info(), argsStr); !valid {
		s.logAudit(p.Name, argsStr, validationResult.Content, "validation", 0)
		return Response{
			JSONRPC: "2.0",
			ID:      id,
			Result: ToolCallResult{
				Content: []ContentBlock{{Type: "text", Text: validationResult.Content}},
				IsError: true,
			},
		}
	}

	requiresInteractiveApproval := tool.RequiresApproval()
	if checker, ok := tool.(agent.SafeCheckerWithContext); ok && checker.IsSafeArgsWithContext(ctx, argsStr) {
		requiresInteractiveApproval = false
	} else if checker, ok := tool.(agent.SafeChecker); ok && checker.IsSafeArgs(argsStr) {
		requiresInteractiveApproval = false
	}
	approvalReason := ""
	if s.permissions != nil {
		decision, reason := permissions.CheckToolCall(p.Name, argsStr, s.permissions)
		if decision == "deny" {
			s.logAudit(p.Name, argsStr, "denied by permission policy: "+reason, "deny", 0)
			return errorResponse(id, -32603, "tool call denied by permission policy")
		}
		if decision == "ask" {
			requiresInteractiveApproval = true
			approvalReason = reason
		} else if decision == "allow" {
			requiresInteractiveApproval = false
		}
	}
	if requiresInteractiveApproval {
		approved, approvalErr := requestToolApproval(ctx, p.Name)
		if approvalErr != nil || !approved {
			detail := approvalReason
			if approvalErr != nil {
				detail = approvalErr.Error()
			}
			s.logAudit(p.Name, argsStr, "approval unavailable or declined: "+detail, "deny", 0)
			return errorResponse(id, -32603, "tool call requires user approval")
		}
	}

	// Pre-tool-use hook
	if s.hookRunner != nil {
		hookDecision, hookReason, hookErr := s.hookRunner.RunPreToolUse(ctx, p.Name, argsStr, "mcp")
		if hookErr != nil {
			fmt.Fprintf(io.Discard, "[hooks] pre-tool-use error: %v\n", hookErr)
		}
		if hookDecision == "deny" {
			s.logAudit(p.Name, argsStr, "denied by hook: "+hookReason, "deny", 0)
			return errorResponse(id, -32603, "tool call denied by hook: "+hookReason)
		}
	}

	ctx = withProgressToken(ctx, p.Meta.ProgressToken)
	ReportProgress(ctx, 0, 1, "Tool execution started")
	startTime := time.Now()
	result, err := tool.Run(ctx, argsStr)
	elapsed := time.Since(startTime)
	if err != nil {
		return errorResponse(id, -32603, err.Error())
	}
	ReportProgress(ctx, 1, 1, "Tool execution completed")

	// Post-tool-use hook
	if s.hookRunner != nil {
		_ = s.hookRunner.RunPostToolUse(ctx, p.Name, argsStr, result.Content, "mcp")
	}

	s.logAudit(p.Name, argsStr, result.Content, "allow", elapsed.Milliseconds())

	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Result: ToolCallResult{
			Content: []ContentBlock{{Type: "text", Text: result.Content}},
			IsError: result.IsError,
		},
	}
}

func requestToolApproval(ctx context.Context, toolName string) (bool, error) {
	result, err := RequestElicitation(
		ctx,
		"Allow the requested "+toolName+" tool call?",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirmed": map[string]any{
					"type":        "boolean",
					"title":       "Allow tool call",
					"description": "Confirm this single tool execution.",
				},
			},
			"required": []string{"confirmed"},
		},
	)
	if err != nil {
		return false, err
	}
	confirmed, _ := result.Content["confirmed"].(bool)
	return result.Action == "accept" && confirmed, nil
}

func cloneSchema(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *Server) logAudit(toolName, argsStr, outputSummary, decision string, durationMs int64) {
	if s.auditor == nil {
		return
	}
	s.auditor.Log(audit.AuditEntry{
		Timestamp:     time.Now(),
		SessionID:     "mcp",
		ToolName:      toolName,
		InputSummary:  argsStr,
		OutputSummary: outputSummary,
		Decision:      decision,
		Approved:      decision == "allow",
		DurationMs:    durationMs,
	})
}

func errorResponse(id *json.RawMessage, code int, message string) Response {
	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message},
	}
}
