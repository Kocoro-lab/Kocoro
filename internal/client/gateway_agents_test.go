package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSyncAndPullAgents(t *testing.T) {
	var gotMethod, gotPath, gotKey string
	var gotBody syncAgentsBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("X-API-Key")
		if r.Method == http.MethodPut {
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.Write([]byte(`{"synced":1,"soft_deleted":0}`))
			return
		}
		w.Write([]byte(`{"agents":[{"agent_key":"demo","updated_at":"2026-06-16T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	c := NewGatewayClient(srv.URL, "k-123")
	item := SyncAgentItem{AgentKey: "demo", UpdatedAt: time.Now().UTC()}
	if err := c.SyncAgents(context.Background(), []SyncAgentItem{item}, true); err != nil {
		t.Fatalf("SyncAgents: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/agents/sync" || gotKey != "k-123" {
		t.Fatalf("bad request: %s %s key=%s", gotMethod, gotPath, gotKey)
	}
	if !gotBody.FullSync || len(gotBody.Agents) != 1 || gotBody.Agents[0].AgentKey != "demo" {
		t.Fatalf("bad body: %+v", gotBody)
	}
	got, err := c.PullAgents(context.Background())
	if err != nil {
		t.Fatalf("PullAgents: %v", err)
	}
	if len(got) != 1 || got[0].AgentKey != "demo" {
		t.Fatalf("bad pull: %+v", got)
	}
}
