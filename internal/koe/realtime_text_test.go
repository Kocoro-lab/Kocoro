//go:build darwin && cgo

package koe

import (
	"context"
	"errors"
	"testing"
)

// frames returns an ordered copy of the captured client messages so a test can
// assert send ordering (item.create before response.create).
func (c *captureSender) frames() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.sent))
	copy(out, c.sent)
	return out
}

// TestInjectUserTextSendsItemThenResponseCreate asserts injectUserText frames a
// user-message conversation item FIRST, then routes response.create through the
// serialized sender (never a direct send) — the shape proven in
// e2e_correction_test.go.
func TestInjectUserTextSendsItemThenResponseCreate(t *testing.T) {
	cap := &captureSender{}
	h := newEventHandler(nil, nil, nil, nil)
	h.sendFn = func(v any) error {
		_ = cap.send(v)
		// Ack the sender's response.create so runResponseSender returns without
		// waiting out the full responseCreateAckTimeout.
		if m, ok := v.(map[string]any); ok && m["type"] == "response.create" {
			signalNonBlocking(h.respCreated)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	if err := h.injectUserText("hello there"); err != nil {
		t.Fatalf("injectUserText: %v", err)
	}
	waitUntil(t, func() bool { return cap.countType("response.create") == 1 }, "response.create was not queued through the serialized sender")

	frames := cap.frames()
	if len(frames) < 2 {
		t.Fatalf("want at least 2 frames (item.create then response.create), got %d: %v", len(frames), cap.types())
	}
	if frames[0]["type"] != "conversation.item.create" {
		t.Fatalf("frame[0].type = %v, want conversation.item.create (types: %v)", frames[0]["type"], cap.types())
	}
	if frames[1]["type"] != "response.create" {
		t.Fatalf("frame[1].type = %v, want response.create (types: %v)", frames[1]["type"], cap.types())
	}

	item, ok := frames[0]["item"].(map[string]any)
	if !ok {
		t.Fatalf("item.create has no item object: %v", frames[0])
	}
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("item = %v, want type=message role=user", item)
	}
	content, ok := item["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("item.content = %v, want a single content part", item["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "input_text" || part["text"] != "hello there" {
		t.Fatalf("content[0] = %v, want {type:input_text, text:hello there}", content[0])
	}
}

// TestInjectUserTextTrimsAndRejectsEmpty verifies the whitespace-only guard: no
// frame is sent and an error is returned (the control layer also 400s empty text).
func TestInjectUserTextTrimsAndRejectsEmpty(t *testing.T) {
	cap := &captureSender{}
	h := newEventHandler(nil, nil, nil, cap.send)
	if err := h.injectUserText("   \n\t "); err == nil {
		t.Fatal("injectUserText(whitespace) = nil, want error")
	}
	if n := len(cap.frames()); n != 0 {
		t.Fatalf("empty inject sent %d frames, want 0", n)
	}
}

// TestInjectUserTextSendFailurePropagates verifies a sendFn failure (e.g. a closed
// data channel — no active session) surfaces as an error and no response.create is
// queued.
func TestInjectUserTextSendFailurePropagates(t *testing.T) {
	h := newEventHandler(nil, nil, nil, func(any) error { return errors.New("data channel closed") })
	if err := h.injectUserText("hi"); err == nil {
		t.Fatal("injectUserText with failing sendFn = nil, want error")
	}
	if len(h.respReq) != 0 {
		t.Fatalf("response.create was queued despite the item.create send failing (respReq=%d)", len(h.respReq))
	}
}

// TestRealtimeConnInjectUserTextNilSafe verifies the public method fails cleanly
// when there is no active session (nil conn or unwired injector).
func TestRealtimeConnInjectUserTextNilSafe(t *testing.T) {
	var nilConn *RealtimeConn
	if err := nilConn.InjectUserText("x"); err == nil {
		t.Fatal("nil RealtimeConn.InjectUserText = nil, want error")
	}
	if err := (&RealtimeConn{}).InjectUserText("x"); err == nil {
		t.Fatal("unwired RealtimeConn.InjectUserText = nil, want error")
	}
}
