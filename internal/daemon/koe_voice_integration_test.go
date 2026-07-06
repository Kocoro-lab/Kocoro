package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// TestRunAgent_KoeVoiceProjection_AllConsumersClean locks the two-site wiring: a
// source=koe reply whose model authored a <spoken_summary> block must end up with
// (1) the spoken line in RunAgentResult.SpokenSummary, (2) a tag-free
// RunAgentResult.Reply (the agent_reply bus event + HTTP copy Koe reads back),
// and (3) a tag-free persisted transcript (Desktop history / FTS / next-turn
// reload). A pure-parser test can't prove all three consumers; this does.
func TestRunAgent_KoeVoiceProjection_AllConsumersClean(t *testing.T) {
	const spoken = "You have three new emails."
	reply := "Here is the detail:\n- Acme receipt\n- GitHub build failed\n<spoken_summary>" + spoken + "</spoken_summary>"

	gw := &fakeGatewayBackend{reply: reply}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()

	// Routed (RouteKey set, not BypassRouting) so the session persists to the
	// durable sessions dir we can read back — BypassRouting uses a temp dir
	// RunAgent removes on return.
	req := RunAgentRequest{
		Text:     "any new mail?",
		Source:   "koe",
		RouteKey: "session:test-koe-voice",
	}
	res, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	if res == nil || res.SessionID == "" {
		t.Fatalf("no persisted session: %+v", res)
	}

	// (1) spoken_summary field is exactly the authored inner line.
	if res.SpokenSummary != spoken {
		t.Errorf("SpokenSummary = %q, want %q", res.SpokenSummary, spoken)
	}
	// (2) Reply (bus event + HTTP copy) is tag-free but keeps the detail.
	if strings.Contains(res.Reply, "spoken_summary") {
		t.Errorf("Reply still carries the tag: %q", res.Reply)
	}
	if !strings.Contains(res.Reply, "Here is the detail") {
		t.Errorf("Reply lost the detail body: %q", res.Reply)
	}

	// (3) Persisted transcript's assistant message is tag-free. Join the async
	// smart-title write first (koe is not autonomous-local, so it fires).
	sessPath := filepath.Join(deps.ShannonDir, "sessions", res.SessionID+".json")
	waitForTitlePersisted(t, sessPath)
	data, readErr := os.ReadFile(sessPath)
	if readErr != nil {
		t.Fatalf("read persisted session: %v", readErr)
	}
	var sess session.Session
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	var sawAssistant bool
	for _, m := range sess.Messages {
		if m.Role != "assistant" {
			continue
		}
		sawAssistant = true
		if strings.Contains(m.Content.Text(), "spoken_summary") {
			t.Errorf("persisted assistant message still carries the tag: %q", m.Content.Text())
		}
		if !strings.Contains(m.Content.Text(), "Here is the detail") {
			t.Errorf("persisted assistant message lost the detail: %q", m.Content.Text())
		}
	}
	if !sawAssistant {
		t.Fatal("no assistant message persisted")
	}
}

// TestRunAgent_KoeVoiceProjection_TagOnlyReplyNotBlank locks gap #3: a trivial
// task where the model writes only the <spoken_summary> block, no detail. The
// Reply (bus event + HTTP copy) must keep the spoken line, not go blank — else
// Desktop shows an empty assistant message.
func TestRunAgent_KoeVoiceProjection_TagOnlyReplyNotBlank(t *testing.T) {
	const line = "Done, your 9am task is set."
	gw := &fakeGatewayBackend{reply: "<spoken_summary>" + line + "</spoken_summary>"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()

	req := RunAgentRequest{Text: "set a task", Source: "koe", BypassRouting: true}
	res, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	if res.SpokenSummary != line {
		t.Errorf("SpokenSummary = %q, want %q", res.SpokenSummary, line)
	}
	if strings.TrimSpace(res.Reply) == "" {
		t.Error("tag-only reply left a blank Reply (Desktop would show an empty message)")
	}
	if strings.Contains(res.Reply, "spoken_summary") {
		t.Errorf("Reply still has tag: %q", res.Reply)
	}
}

// TestRunAgent_NonKoeSkipsVoiceProjection locks the isKoeSource gate: a non-koe
// reply is never scanned for a spoken_summary block, so SpokenSummary stays empty
// and Reply is returned verbatim (even when it happens to contain the tag).
func TestRunAgent_NonKoeSkipsVoiceProjection(t *testing.T) {
	reply := "<spoken_summary>should not be extracted</spoken_summary>\nbody"
	gw := &fakeGatewayBackend{reply: reply}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()

	req := RunAgentRequest{
		Text:          "hi",
		Source:        "heartbeat", // non-koe
		BypassRouting: true,
	}
	res, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	if res.SpokenSummary != "" {
		t.Errorf("SpokenSummary = %q, want empty for non-koe source", res.SpokenSummary)
	}
	if res.Reply != reply {
		t.Errorf("non-koe Reply was modified: got %q, want verbatim %q", res.Reply, reply)
	}
}
