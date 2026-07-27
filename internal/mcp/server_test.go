package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

// mockTool implements agent.Tool for testing.
type mockTool struct {
	name        string
	description string
	params      map[string]any
	required    []string
	result      string
	isError     bool
	runErr      error
	runCalls    int
}

func (m *mockTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        m.name,
		Description: m.description,
		Parameters:  m.params,
		Required:    m.required,
	}
}

func (m *mockTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	m.runCalls++
	if m.runErr != nil {
		return agent.ToolResult{}, m.runErr
	}
	return agent.ToolResult{Content: m.result, IsError: m.isError}, nil
}

func (m *mockTool) RequiresApproval() bool { return false }

func newTestRegistry(tools ...agent.Tool) *agent.ToolRegistry {
	reg := agent.NewToolRegistry()
	for _, t := range tools {
		reg.Register(t)
	}
	return reg
}

func rawID(v int) *json.RawMessage {
	b, _ := json.Marshal(v)
	raw := json.RawMessage(b)
	return &raw
}

// sendRequest encodes a request as a JSON line and returns the response parsed
// from the server's output.
func sendRequest(t *testing.T, srv *Server, req Request) *Response {
	t.Helper()
	reqLine, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reqLine = append(reqLine, '\n')

	var out bytes.Buffer
	err = srv.Serve(context.Background(), bytes.NewReader(reqLine), &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	output := strings.TrimSpace(out.String())
	if output == "" {
		return nil // no response (notification)
	}

	var resp Response
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		t.Fatalf("unmarshal response %q: %v", output, err)
	}
	return &resp
}

func TestHandleInitialize(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test-server", "0.1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "initialize",
	})

	if resp == nil {
		t.Fatal("expected response, got nil")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	resultJSON, _ := json.Marshal(resp.Result)
	var result InitializeResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version = %q, want %q", result.ProtocolVersion, "2024-11-05")
	}
	if result.ServerInfo.Name != "test-server" {
		t.Errorf("server name = %q, want %q", result.ServerInfo.Name, "test-server")
	}
	if result.ServerInfo.Version != "0.1.0" {
		t.Errorf("server version = %q, want %q", result.ServerInfo.Version, "0.1.0")
	}
	if result.Capabilities.Tools == nil {
		t.Fatal("expected tools capability, got nil")
	}
	if !result.Capabilities.Tools.ListChanged {
		t.Error("expected listChanged=true")
	}
}

func TestHandleToolsList(t *testing.T) {
	tool := &mockTool{
		name:        "echo",
		description: "echoes input",
		params: map[string]any{
			"message": map[string]any{"type": "string", "description": "message to echo"},
		},
		required: []string{"message"},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/list",
	})

	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	resultJSON, _ := json.Marshal(resp.Result)
	var result ToolsListResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want %q", result.Tools[0].Name, "echo")
	}
	if result.Tools[0].Description != "echoes input" {
		t.Errorf("tool description = %q, want %q", result.Tools[0].Description, "echoes input")
	}

	var schema map[string]any
	if err := json.Unmarshal(result.Tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	req, ok := schema["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "message" {
		t.Errorf("schema required = %v, want [message]", schema["required"])
	}
}

func TestHandleToolsList_PreservesCompleteJSONSchema(t *testing.T) {
	tool := &mockTool{
		name:        "echo",
		description: "echoes input",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
		},
		required: []string{"message"},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(3),
		Method:  "tools/list",
	})

	resultJSON, _ := json.Marshal(resp.Result)
	var result ToolsListResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(result.Tools))
	}
	var schema map[string]any
	if err := json.Unmarshal(result.Tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties = %#v", schema["properties"])
	}
	if _, nested := properties["properties"]; nested {
		t.Fatalf("complete schema was nested under properties: %#v", schema)
	}
	if _, ok := properties["message"]; !ok {
		t.Fatalf("message property missing: %#v", schema)
	}
}

func TestHandleToolCall_ValidatesBeforeExecution(t *testing.T) {
	tool := &mockTool{
		name:     "echo",
		params:   map[string]any{"message": map[string]any{"type": "string"}},
		required: []string{"message"},
		result:   "should not run",
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(4),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"echo","arguments":{}}`),
	})

	if resp.Error != nil {
		t.Fatalf("validation should be a tool result, got JSON-RPC error: %+v", resp.Error)
	}
	resultJSON, _ := json.Marshal(resp.Result)
	var result ToolCallResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatal(err)
	}
	if !result.IsError || len(result.Content) != 1 ||
		!strings.HasPrefix(result.Content[0].Text, "[validation error]") {
		t.Fatalf("unexpected validation result: %#v", result)
	}
	if tool.runCalls != 0 {
		t.Fatalf("Tool.Run called %d time(s) for invalid input", tool.runCalls)
	}
}

func TestHandleToolsListEmpty(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "tools/list",
	})

	resultJSON, _ := json.Marshal(resp.Result)
	var result ToolsListResult
	json.Unmarshal(resultJSON, &result)

	if result.Tools == nil {
		t.Fatal("expected non-nil tools slice")
	}
	if len(result.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(result.Tools))
	}
}

func TestHandleToolCall(t *testing.T) {
	tool := &mockTool{
		name:   "greet",
		result: "hello world",
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	params, _ := json.Marshal(ToolCallParams{
		Name:      "greet",
		Arguments: json.RawMessage(`{"name":"world"}`),
	})
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(3),
		Method:  "tools/call",
		Params:  params,
	})

	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	resultJSON, _ := json.Marshal(resp.Result)
	var result ToolCallResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(result.Content))
	}
	if result.Content[0].Type != "text" {
		t.Errorf("content type = %q, want text", result.Content[0].Type)
	}
	if result.Content[0].Text != "hello world" {
		t.Errorf("content text = %q, want %q", result.Content[0].Text, "hello world")
	}
	if result.IsError {
		t.Error("expected isError=false")
	}
}

func TestHandleToolCallError(t *testing.T) {
	tool := &mockTool{
		name:    "fail",
		result:  "error output",
		isError: true,
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	params, _ := json.Marshal(ToolCallParams{
		Name:      "fail",
		Arguments: json.RawMessage(`{}`),
	})
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(4),
		Method:  "tools/call",
		Params:  params,
	})

	resultJSON, _ := json.Marshal(resp.Result)
	var result ToolCallResult
	json.Unmarshal(resultJSON, &result)

	if !result.IsError {
		t.Error("expected isError=true")
	}
	if result.Content[0].Text != "error output" {
		t.Errorf("content = %q, want %q", result.Content[0].Text, "error output")
	}
}

func TestHandleToolCallRunError(t *testing.T) {
	tool := &mockTool{
		name:   "broken",
		runErr: errors.New("something went wrong"),
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	params, _ := json.Marshal(ToolCallParams{
		Name:      "broken",
		Arguments: json.RawMessage(`{}`),
	})
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(5),
		Method:  "tools/call",
		Params:  params,
	})

	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("error code = %d, want -32603", resp.Error.Code)
	}
	if resp.Error.Message != "something went wrong" {
		t.Errorf("error message = %q, want %q", resp.Error.Message, "something went wrong")
	}
}

func TestHandleToolCallUnknownTool(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	params, _ := json.Marshal(ToolCallParams{
		Name:      "nonexistent",
		Arguments: json.RawMessage(`{}`),
	})
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(6),
		Method:  "tools/call",
		Params:  params,
	})

	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "nonexistent") {
		t.Errorf("error message = %q, want it to contain 'nonexistent'", resp.Error.Message)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})

	if resp != nil {
		t.Fatalf("expected no response for notification, got %+v", resp)
	}
}

func TestInvalidJSON(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	var out bytes.Buffer
	input := "this is not json\n"
	err := srv.Serve(context.Background(), strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	var resp Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("error code = %d, want -32700", resp.Error.Code)
	}
}

func TestUnknownMethod(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(7),
		Method:  "bogus/method",
	})

	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "bogus/method") {
		t.Errorf("error message = %q, want it to contain method name", resp.Error.Message)
	}
}

func TestServeMultipleRequests(t *testing.T) {
	tool := &mockTool{
		name:   "ping",
		result: "pong",
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)

	// Build a stream of multiple requests.
	var input bytes.Buffer
	requests := []Request{
		{JSONRPC: "2.0", ID: rawID(1), Method: "initialize"},
		{JSONRPC: "2.0", Method: "notifications/initialized"}, // notification, no response
		{JSONRPC: "2.0", ID: rawID(2), Method: "tools/list"},
	}
	callParams, _ := json.Marshal(ToolCallParams{Name: "ping", Arguments: json.RawMessage(`{}`)})
	requests = append(requests, Request{JSONRPC: "2.0", ID: rawID(3), Method: "tools/call", Params: callParams})

	for _, req := range requests {
		line, _ := json.Marshal(req)
		input.Write(line)
		input.WriteByte('\n')
	}

	var out bytes.Buffer
	err := srv.Serve(context.Background(), &input, &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	// We expect 3 responses (notification produces none).
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 response lines, got %d: %v", len(lines), lines)
	}

	// Concurrent request execution permits responses after initialize to
	// arrive in either order.
	seenIDs := make(map[int]bool)
	for i, line := range lines {
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
		if resp.ID == nil {
			t.Fatalf("response %d has nil ID", i)
		}
		var id int
		json.Unmarshal(*resp.ID, &id)
		seenIDs[id] = true
		if resp.Error != nil {
			t.Errorf("response %d unexpected error: %+v", i, resp.Error)
		}
	}
	for _, id := range []int{1, 2, 3} {
		if !seenIDs[id] {
			t.Errorf("missing response ID %d", id)
		}
	}
}

func TestServeContextCancellation(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())

	// Use a pipe so the reader blocks until we close it.
	pr, pw := io.Pipe()
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, pr, &out)
	}()

	cancel()
	pw.Close()

	err := <-done
	// After cancellation and pipe close, Serve should return.
	// It may return ctx.Err() or nil (scanner sees EOF first).
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEmptyLinesSkipped(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	req, _ := json.Marshal(Request{JSONRPC: "2.0", ID: rawID(1), Method: "initialize"})
	input := "\n\n" + string(req) + "\n\n"

	var out bytes.Buffer
	err := srv.Serve(context.Background(), strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response, got %d", len(lines))
	}
}

type lifecycleTool struct {
	name             string
	requiresApproval bool
	started          chan struct{}
	cancelled        chan struct{}
	runCalls         atomic.Int32
	run              func(context.Context) (agent.ToolResult, error)
}

func (t *lifecycleTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:       t.name,
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (t *lifecycleTool) Run(ctx context.Context, _ string) (agent.ToolResult, error) {
	t.runCalls.Add(1)
	if t.started != nil {
		close(t.started)
	}
	if t.run != nil {
		return t.run(ctx)
	}
	return agent.ToolResult{Content: "ok"}, nil
}

func (t *lifecycleTool) RequiresApproval() bool { return t.requiresApproval }

func writeJSONLine(t *testing.T, writer io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
}

func readJSONLine(t *testing.T, scanner *bufio.Scanner) map[string]json.RawMessage {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("expected JSON-RPC message: %v", scanner.Err())
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
		t.Fatalf("decode message %q: %v", scanner.Text(), err)
	}
	return msg
}

func TestInitializeNegotiatesSupportedProtocolVersion(t *testing.T) {
	srv := NewServer(newTestRegistry(), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{}}`),
	})
	resultJSON, _ := json.Marshal(resp.Result)
	var result InitializeResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("protocol version = %q", result.ProtocolVersion)
	}
}

func TestToolCallProgressNotifications(t *testing.T) {
	tool := &lifecycleTool{
		name: "progress_probe",
		run: func(ctx context.Context) (agent.ToolResult, error) {
			if !ReportProgress(ctx, 0.5, 1, "halfway") {
				t.Error("ReportProgress returned false")
			}
			return agent.ToolResult{Content: "done"}, nil
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	token := json.RawMessage(`"turn-7"`)
	params, _ := json.Marshal(ToolCallParams{
		Name:      tool.name,
		Arguments: json.RawMessage(`{}`),
		Meta:      RequestMeta{ProgressToken: token},
	})
	request, _ := json.Marshal(Request{
		JSONRPC: "2.0",
		ID:      rawID(7),
		Method:  "tools/call",
		Params:  params,
	})

	var output bytes.Buffer
	if err := srv.Serve(context.Background(), bytes.NewReader(append(request, '\n')), &output); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("messages = %d, want 4: %s", len(lines), output.String())
	}
	var progresses []ProgressParams
	var gotResponse bool
	for _, line := range lines {
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method == "notifications/progress" {
			var progress ProgressParams
			if err := json.Unmarshal(msg.Params, &progress); err != nil {
				t.Fatal(err)
			}
			progresses = append(progresses, progress)
		} else if string(msg.ID) == "7" {
			gotResponse = true
		}
	}
	if !gotResponse || len(progresses) != 3 {
		t.Fatalf("response=%v progress=%d", gotResponse, len(progresses))
	}
	for _, progress := range progresses {
		if string(progress.ProgressToken) != `"turn-7"` {
			t.Fatalf("progress token = %s", progress.ProgressToken)
		}
	}
	if progresses[0].Progress != 0 || progresses[1].Progress != 0.5 || progresses[2].Progress != 1 {
		t.Fatalf("progress sequence = %#v", progresses)
	}
}

func TestToolCallCancellationStopsWorkWithoutLateResponse(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	tool := &lifecycleTool{
		name:      "cancel_probe",
		started:   started,
		cancelled: cancelled,
		run: func(ctx context.Context) (agent.ToolResult, error) {
			<-ctx.Done()
			close(cancelled)
			return agent.ToolResult{}, ctx.Err()
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), inputReader, &output)
	}()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(9),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"cancel_probe","arguments":{}}`),
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	writeJSONLine(t, inputWriter, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 9, "reason": "client stopped"},
	})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("tool context was not cancelled")
	}
	_ = inputWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "" {
		t.Fatalf("unexpected late response: %s", output.String())
	}
}

func TestToolsListChangedNotification(t *testing.T) {
	registry := newTestRegistry()
	srv := NewServer(registry, "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), inputReader, outputWriter)
	}()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "initialize",
	})
	readJSONLine(t, scanner)
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/list",
	})
	readJSONLine(t, scanner)

	registry.Register(&lifecycleTool{name: "dynamic_probe"})
	msg := readJSONLine(t, scanner)
	if string(msg["method"]) != `"notifications/tools/list_changed"` {
		t.Fatalf("method = %s", msg["method"])
	}

	_ = inputWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = outputWriter.Close()
}

func TestToolApprovalViaFormElicitation(t *testing.T) {
	tool := &lifecycleTool{name: "approval_probe", requiresApproval: true}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), inputReader, outputWriter)
	}()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "initialize",
		Params: json.RawMessage(
			`{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{"form":{}}}}`,
		),
	})
	readJSONLine(t, scanner)
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"approval_probe","arguments":{}}`),
	})

	elicit := readJSONLine(t, scanner)
	if string(elicit["method"]) != `"elicitation/create"` {
		t.Fatalf("method = %s", elicit["method"])
	}
	writeJSONLine(t, inputWriter, map[string]any{
		"jsonrpc": "2.0",
		"id":      elicit["id"],
		"result": map[string]any{
			"action":  "accept",
			"content": map[string]any{"confirmed": true},
		},
	})
	response := readJSONLine(t, scanner)
	if string(response["id"]) != "2" {
		t.Fatalf("tool response id = %s", response["id"])
	}
	if tool.runCalls.Load() != 1 {
		t.Fatalf("run calls = %d", tool.runCalls.Load())
	}

	_ = inputWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = outputWriter.Close()
}

func TestToolApprovalWithoutElicitationFailsClosed(t *testing.T) {
	tool := &lifecycleTool{name: "approval_probe", requiresApproval: true}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"approval_probe","arguments":{}}`),
	})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "requires user approval") {
		t.Fatalf("response = %#v", resp)
	}
	if tool.runCalls.Load() != 0 {
		t.Fatalf("run calls = %d", tool.runCalls.Load())
	}
}

func TestToolPermissionDenylistOverridesExecution(t *testing.T) {
	tool := &lifecycleTool{name: "bash"}
	srv := NewServer(
		newTestRegistry(tool),
		"test",
		"1.0",
		&permissions.PermissionsConfig{DeniedCommands: []string{"customctl status alpha"}},
		nil,
		nil,
	)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params: json.RawMessage(
			`{"name":"bash","arguments":{"command":"customctl status alpha"}}`,
		),
	})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "denied") {
		t.Fatalf("response = %#v", resp)
	}
	if tool.runCalls.Load() != 0 {
		t.Fatalf("run calls = %d", tool.runCalls.Load())
	}
}

// readJSONLineTimeout reads one JSON-RPC line but fails the test instead of
// blocking forever, so a hung server surfaces as a bounded failure.
func readJSONLineTimeout(t *testing.T, scanner *bufio.Scanner, d time.Duration) map[string]json.RawMessage {
	t.Helper()
	type result struct {
		msg map[string]json.RawMessage
		ok  bool
	}
	ch := make(chan result, 1)
	go func() {
		if scanner.Scan() {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(scanner.Bytes(), &m)
			ch <- result{m, true}
			return
		}
		ch <- result{nil, false}
	}()
	select {
	case r := <-ch:
		if !r.ok {
			t.Fatalf("scanner closed without a message: %v", scanner.Err())
		}
		return r.msg
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for JSON-RPC message", d)
		return nil
	}
}

// Fix 1: clean EOF (client closes stdin) must cancel in-flight requests so
// Serve does not hang on wg.Wait().
func TestServeCancelsInFlightRequestsOnCleanEOF(t *testing.T) {
	t.Setenv("SHANNON_MCP_EOF_DRAIN_GRACE", "100ms")
	started := make(chan struct{})
	tool := &lifecycleTool{
		name:    "block_probe",
		started: started,
		run: func(ctx context.Context) (agent.ToolResult, error) {
			<-ctx.Done()
			return agent.ToolResult{}, ctx.Err()
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), inputReader, &output)
	}()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(9),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"block_probe","arguments":{}}`),
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}

	// Client closes stdin with no prior cancellation notification: clean EOF.
	_ = inputWriter.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve hung on clean EOF with an in-flight blocking request")
	}
}

func TestServeReturnsWhenToolIgnoresEOFCancellation(t *testing.T) {
	t.Setenv("SHANNON_MCP_EOF_DRAIN_GRACE", "50ms")
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	tool := &lifecycleTool{
		name: "ignore_cancel_probe",
		run: func(context.Context) (agent.ToolResult, error) {
			close(started)
			<-release
			close(finished)
			return agent.ToolResult{Content: "late result"}, nil
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), inputReader, io.Discard)
	}()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(10),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"ignore_cancel_probe","arguments":{}}`),
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	_ = inputWriter.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Serve remained pinned by a tool that ignored cancellation")
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("ignored-cancellation probe did not clean up")
	}
}

// Fix 2: a client that receives elicitation/create but never answers must not
// pin the tool goroutine forever; the wait times out and fails closed.
func TestElicitationTimeoutFailsClosed(t *testing.T) {
	t.Setenv("SHANNON_MCP_ELICITATION_TIMEOUT", "100ms")
	tool := &lifecycleTool{name: "approval_probe", requiresApproval: true}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inputReader, outputWriter) }()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "initialize",
		Params: json.RawMessage(
			`{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{"form":{}}}}`,
		),
	})
	readJSONLineTimeout(t, scanner, 2*time.Second)
	writeJSONLine(t, inputWriter, Request{JSONRPC: "2.0", Method: "notifications/initialized"})
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"approval_probe","arguments":{}}`),
	})

	elicit := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(elicit["method"]) != `"elicitation/create"` {
		t.Fatalf("method = %s", elicit["method"])
	}

	// Never answer the elicitation. The bounded timeout must produce a denial.
	resp := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(resp["id"]) != "2" {
		t.Fatalf("response id = %s, want 2", resp["id"])
	}
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("expected JSON-RPC error (fail closed) on elicitation timeout, got %v", resp)
	}
	if tool.runCalls.Load() != 0 {
		t.Fatalf("run calls = %d, tool must not run when approval times out", tool.runCalls.Load())
	}

	_ = inputWriter.Close()
	<-done
	_ = outputWriter.Close()
}

// Fix 3: a second request re-using an in-flight id must be rejected without
// corrupting the original request's active-map entry.
func TestDuplicateInFlightRequestIDRejected(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tool := &lifecycleTool{
		name: "dup_probe",
		run: func(ctx context.Context) (agent.ToolResult, error) {
			once.Do(func() { close(started) })
			select {
			case <-release:
				return agent.ToolResult{Content: "done"}, nil
			case <-ctx.Done():
				return agent.ToolResult{}, ctx.Err()
			}
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inputReader, outputWriter) }()

	call := Request{
		JSONRPC: "2.0",
		ID:      rawID(9),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"dup_probe","arguments":{}}`),
	}
	writeJSONLine(t, inputWriter, call)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first call did not start")
	}

	// Second call re-uses the in-flight id 9.
	writeJSONLine(t, inputWriter, call)
	dup := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(dup["id"]) != "9" {
		t.Fatalf("duplicate response id = %s, want 9", dup["id"])
	}
	raw, ok := dup["error"]
	if !ok {
		t.Fatalf("expected JSON-RPC error for duplicate id, got %v", dup)
	}
	var rpcErr RPCError
	if err := json.Unmarshal(raw, &rpcErr); err != nil {
		t.Fatal(err)
	}
	if rpcErr.Code != -32600 {
		t.Fatalf("error code = %d, want -32600", rpcErr.Code)
	}

	// The original request's entry survives: it still completes normally.
	close(release)
	orig := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(orig["id"]) != "9" {
		t.Fatalf("original response id = %s, want 9", orig["id"])
	}
	if _, hasErr := orig["error"]; hasErr {
		t.Fatalf("original call should succeed, got error: %v", orig["error"])
	}

	_ = inputWriter.Close()
	<-done
	_ = outputWriter.Close()
}

// Fix 4: concurrent tool EXECUTION is bounded by a semaphore while the scanner
// stays responsive (queued requests do not block frame reading).
func TestRequestConcurrencyCapBoundsExecution(t *testing.T) {
	t.Setenv("SHANNON_MCP_MAX_CONCURRENT_REQUESTS", "1")
	release := make(chan struct{})
	var running, maxRunning atomic.Int32
	firstStarted := make(chan struct{})
	var once sync.Once
	tool := &lifecycleTool{
		name: "cap_probe",
		run: func(ctx context.Context) (agent.ToolResult, error) {
			n := running.Add(1)
			for {
				m := maxRunning.Load()
				if n <= m || maxRunning.CompareAndSwap(m, n) {
					break
				}
			}
			once.Do(func() { close(firstStarted) })
			select {
			case <-release:
			case <-ctx.Done():
				running.Add(-1)
				return agent.ToolResult{}, ctx.Err()
			}
			running.Add(-1)
			return agent.ToolResult{Content: "done"}, nil
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inputReader, outputWriter) }()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"cap_probe","arguments":{}}`),
	})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first call did not start")
	}

	// Second call must queue on the semaphore, not run concurrently.
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"cap_probe","arguments":{}}`),
	})
	time.Sleep(200 * time.Millisecond)
	if got := running.Load(); got != 1 {
		t.Fatalf("concurrent executions = %d, want 1 (cap not enforced)", got)
	}

	close(release)
	got1 := readJSONLineTimeout(t, scanner, 2*time.Second)
	got2 := readJSONLineTimeout(t, scanner, 2*time.Second)
	ids := map[string]bool{string(got1["id"]): true, string(got2["id"]): true}
	if !ids["1"] || !ids["2"] {
		t.Fatalf("expected responses for ids 1 and 2, got %v", ids)
	}
	if m := maxRunning.Load(); m != 1 {
		t.Fatalf("max concurrent executions = %d, want 1", m)
	}

	_ = inputWriter.Close()
	<-done
	_ = outputWriter.Close()
}

func TestCancelledQueuedRequestDoesNotExecute(t *testing.T) {
	t.Setenv("SHANNON_MCP_MAX_CONCURRENT_REQUESTS", "1")
	t.Setenv("SHANNON_MCP_MAX_QUEUED_REQUESTS", "1")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	var tool *lifecycleTool
	tool = &lifecycleTool{
		name: "queued_cancel_probe",
		run: func(ctx context.Context) (agent.ToolResult, error) {
			if tool.runCalls.Load() == 1 {
				firstOnce.Do(func() { close(firstStarted) })
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return agent.ToolResult{}, ctx.Err()
				}
			}
			return agent.ToolResult{Content: "done"}, nil
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inputReader, outputWriter) }()

	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(1),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"queued_cancel_probe","arguments":{}}`),
	})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"queued_cancel_probe","arguments":{}}`),
	})
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  json.RawMessage(`{"requestId":2,"reason":"no longer needed"}`),
	})
	// initialize is handled synchronously by the scanner loop. Receiving its
	// response proves the preceding cancellation notification was processed
	// before the running request releases the only execution slot.
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(99),
		Method:  "initialize",
	})
	initialized := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(initialized["id"]) != "99" {
		t.Fatalf("initialize response id = %s, want 99", initialized["id"])
	}

	close(releaseFirst)
	completed := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(completed["id"]) != "1" {
		t.Fatalf("completed response id = %s, want 1", completed["id"])
	}
	_ = inputWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit")
	}
	if got := tool.runCalls.Load(); got != 1 {
		t.Fatalf("tool executed %d times; cancelled queued request executed", got)
	}
	_ = outputWriter.Close()
}

func TestRequestQueueCapRejectsExcessWithoutSpawningExecution(t *testing.T) {
	t.Setenv("SHANNON_MCP_MAX_CONCURRENT_REQUESTS", "1")
	t.Setenv("SHANNON_MCP_MAX_QUEUED_REQUESTS", "1")

	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tool := &lifecycleTool{
		name: "queue_cap_probe",
		run: func(ctx context.Context) (agent.ToolResult, error) {
			once.Do(func() { close(firstStarted) })
			select {
			case <-release:
				return agent.ToolResult{Content: "done"}, nil
			case <-ctx.Done():
				return agent.ToolResult{}, ctx.Err()
			}
		},
	}
	srv := NewServer(newTestRegistry(tool), "test", "1.0", nil, nil, nil)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	scanner := bufio.NewScanner(outputReader)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inputReader, outputWriter) }()

	for id := 1; id <= 2; id++ {
		writeJSONLine(t, inputWriter, Request{
			JSONRPC: "2.0",
			ID:      rawID(id),
			Method:  "tools/call",
			Params:  json.RawMessage(`{"name":"queue_cap_probe","arguments":{}}`),
		})
		if id == 1 {
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("first request did not start")
			}
		}
	}
	writeJSONLine(t, inputWriter, Request{
		JSONRPC: "2.0",
		ID:      rawID(3),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"queue_cap_probe","arguments":{}}`),
	})

	rejected := readJSONLineTimeout(t, scanner, 2*time.Second)
	if string(rejected["id"]) != "3" {
		t.Fatalf("rejected response id = %s, want 3", rejected["id"])
	}
	var rpcErr RPCError
	if err := json.Unmarshal(rejected["error"], &rpcErr); err != nil {
		t.Fatal(err)
	}
	if rpcErr.Code != -32000 {
		t.Fatalf("queue overflow error = %d, want -32000", rpcErr.Code)
	}

	close(release)
	readJSONLineTimeout(t, scanner, 2*time.Second)
	readJSONLineTimeout(t, scanner, 2*time.Second)
	_ = inputWriter.Close()
	<-done
	_ = outputWriter.Close()
	if got := tool.runCalls.Load(); got != 2 {
		t.Fatalf("tool executions = %d, want only accepted requests 1 and 2", got)
	}
}

func TestToolPermissionAllowlistBypassesGenericApproval(t *testing.T) {
	tool := &lifecycleTool{name: "bash", requiresApproval: true}
	srv := NewServer(
		newTestRegistry(tool),
		"test",
		"1.0",
		&permissions.PermissionsConfig{AllowedCommands: []string{"customctl status alpha"}},
		nil,
		nil,
	)
	resp := sendRequest(t, srv, Request{
		JSONRPC: "2.0",
		ID:      rawID(2),
		Method:  "tools/call",
		Params: json.RawMessage(
			`{"name":"bash","arguments":{"command":"customctl status alpha"}}`,
		),
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %#v", resp.Error)
	}
	if tool.runCalls.Load() != 1 {
		t.Fatalf("run calls = %d", tool.runCalls.Load())
	}
}
