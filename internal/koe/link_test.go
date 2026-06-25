package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoTaskCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/message" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var got DoTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.Source != "koe" {
			t.Errorf("source = %q, want \"koe\"", got.Source)
		}
		if got.Agent != "finance" || got.ThreadID != "burst-1" || got.Text != "check NVDA" {
			t.Errorf("unexpected req: %+v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"reply": "NVDA is up two percent today.", "session_id": "s1", "agent": "finance",
		})
	}))
	defer srv.Close()

	c := NewDaemonClient(srv.URL)
	out, err := c.DoTask(context.Background(), DoTaskRequest{Text: "check NVDA", Agent: "finance", ThreadID: "burst-1"})
	if err != nil {
		t.Fatalf("DoTask: %v", err)
	}
	if out.Kind != OutcomeCompleted {
		t.Fatalf("Kind = %v, want OutcomeCompleted", out.Kind)
	}
	if out.Reply != "NVDA is up two percent today." {
		t.Errorf("Reply = %q", out.Reply)
	}
}

func TestDoTaskInjected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "injected", "route": "agent:finance:koe:burst-1"})
	}))
	defer srv.Close()

	c := NewDaemonClient(srv.URL)
	out, err := c.DoTask(context.Background(), DoTaskRequest{Text: "make it 6pm", Agent: "finance", ThreadID: "burst-1"})
	if err != nil {
		t.Fatalf("DoTask: %v", err)
	}
	if out.Kind != OutcomeInjected {
		t.Fatalf("Kind = %v, want OutcomeInjected", out.Kind)
	}
}

func TestDoTaskRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"status": "rejected", "reason": "cwd_conflict", "route": "r"})
	}))
	defer srv.Close()

	c := NewDaemonClient(srv.URL)
	out, err := c.DoTask(context.Background(), DoTaskRequest{Text: "x", Agent: "finance", ThreadID: "burst-1"})
	if err != nil {
		t.Fatalf("DoTask should not error on a structured rejection: %v", err)
	}
	if out.Kind != OutcomeRejected || out.Reason != "cwd_conflict" {
		t.Errorf("Kind=%v Reason=%q, want OutcomeRejected/cwd_conflict", out.Kind, out.Reason)
	}
}
