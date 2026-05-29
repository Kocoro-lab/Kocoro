package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSEClient_ReadEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "id: 1\nevent: AGENT_STARTED\ndata: {\"agent_id\":\"shibuya\",\"message\":\"planning\"}\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "id: 2\nevent: done\ndata: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make([]SSEEvent, 0)
	err := StreamSSE(ctx, server.URL, "", func(ev SSEEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) < 1 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Event != "AGENT_STARTED" {
		t.Errorf("expected AGENT_STARTED, got %s", events[0].Event)
	}
}

func TestSSEClient_MultiLineData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		fmt.Fprintf(w, "event: test\ndata: line one\ndata: line two\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "event: done\ndata: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make([]SSEEvent, 0)
	err := StreamSSE(ctx, server.URL, "", func(ev SSEEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) < 1 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Data != "line one\nline two" {
		t.Errorf("expected multi-line data %q, got %q", "line one\nline two", events[0].Data)
	}
}

func TestStreamSSEWithOptions_ReconnectsWithLastEventID(t *testing.T) {
	var conns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&conns, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if n == 1 {
			// First connection: emit one event with an id, then drop (close
			// without `done`) by simply returning from the handler.
			fmt.Fprintf(w, "id: 5\nevent: AGENT_STARTED\ndata: {\"agent_id\":\"a\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// Reconnect: the client must have sent Last-Event-ID: 5.
		if got := r.Header.Get("Last-Event-ID"); got != "5" {
			t.Errorf("reconnect Last-Event-ID = %q, want \"5\"", got)
		}
		fmt.Fprintf(w, "id: 6\nevent: WORKFLOW_COMPLETED\ndata: {\"message\":\"done\"}\n\n")
		fmt.Fprintf(w, "event: done\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []string
	err := StreamSSEWithOptions(context.Background(), srv.URL, "", StreamSSEOptions{
		MaxReconnects:        3,
		ReconnectBackoffBase: time.Millisecond,
	}, func(ev SSEEvent) {
		events = append(events, ev.Event)
	})
	if err != nil {
		t.Fatalf("StreamSSEWithOptions err: %v", err)
	}
	if atomic.LoadInt32(&conns) != 2 {
		t.Fatalf("expected 2 connections (1 drop + 1 resume), got %d", conns)
	}
	want := []string{"AGENT_STARTED", "WORKFLOW_COMPLETED"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestStreamSSEWithOptions_IdleTimeoutThenReconnect(t *testing.T) {
	var conns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&conns, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if n == 1 {
			// Flush SSE headers FIRST so the client's Do() returns and the
			// read loop (and its idle watchdog) starts; THEN go silent so the
			// watchdog fires.
			w.WriteHeader(http.StatusOK)
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
			return
		}
		fmt.Fprintf(w, "event: WORKFLOW_COMPLETED\ndata: {}\n\nevent: done\ndata: [DONE]\n\n")
		_ = flusher
	}))
	defer srv.Close()

	err := StreamSSEWithOptions(context.Background(), srv.URL, "", StreamSSEOptions{
		IdleTimeout:          50 * time.Millisecond,
		MaxReconnects:        2,
		ReconnectBackoffBase: time.Millisecond,
	}, func(ev SSEEvent) {})
	if err != nil {
		t.Fatalf("expected recovery after idle timeout, got err: %v", err)
	}
	if atomic.LoadInt32(&conns) < 2 {
		t.Fatalf("expected reconnect after idle timeout, conns=%d", conns)
	}
}
