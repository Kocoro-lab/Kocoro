package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// Live compaction verification.
//
// Unit tests prove the compaction machinery is structurally correct; they
// cannot prove a real summary is USEFUL. This test drives the production
// daemon runner path (RunAgent → AgentLoop → ShapeHistory → checkpoint) against
// the real gateway with a deliberately small per-agent context window, then
// asserts the four things that fail silently in production:
//
//  1. a compaction actually fired and persisted a live checkpoint
//  2. the pre-compaction snapshot landed on disk and carries no image payload
//  3. recently read files were restored after a proactive/preflight compaction
//  4. the next turn reuses the checkpoint instead of summarizing the archive again
//  5. a fact planted BEFORE the compaction survives it — asked after the fact,
//     the model must still answer with the exact identifier
//
// (4) is the one that matters: a well-formed summary that loses the user's
// data still passes every unit test in the repo.
//
// Isolation: sessions/snapshots are written to a t.TempDir() shannon dir, not
// ~/.shannon. No WebSocket client is constructed, so this never becomes a live
// consumer of the user's Slack/LINE channels the way `shan daemon start` does.
//
// Cost: real LLM calls at ~60K context each. Budget a few dollars per run.

// compactionProbeIdentifier is shaped to match the summary audit's opaque
// identifier pattern (\b[A-Fa-f0-9]{8,}\b) so this test also exercises the
// identifier-preservation path, not just prose recall.
const compactionProbeIdentifier = "a3f9c21b4e7d8055"

// compactionProbeWindow is small enough that two or three file reads cross the
// 90% trigger, and large enough that the system prompt + tool schemas do not
// themselves exceed it (which would make every turn pathological rather than
// representative).
const compactionProbeWindow = 60000

type compactionProbeHandler struct {
	mu       sync.Mutex
	statuses []string
	tools    []string
	texts    []string
}

func (h *compactionProbeHandler) OnToolCall(name, args, toolUseID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools = append(h.tools, name)
}

func (h *compactionProbeHandler) OnToolResult(name, args, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
}

func (h *compactionProbeHandler) OnText(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.texts = append(h.texts, text)
}

func (h *compactionProbeHandler) OnPreamble(string)                    {}
func (h *compactionProbeHandler) OnStreamDelta(string)                 {}
func (h *compactionProbeHandler) OnApprovalNeeded(string, string) bool { return true }
func (h *compactionProbeHandler) OnUsage(agent.TurnUsage)              {}
func (h *compactionProbeHandler) OnCloudAgent(string, string, string)  {}
func (h *compactionProbeHandler) OnCloudProgress(int, int)             {}
func (h *compactionProbeHandler) OnCloudPlan(string, string, bool)     {}

func (h *compactionProbeHandler) OnRunStatus(code, detail string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses = append(h.statuses, code)
}

func (h *compactionProbeHandler) sawStatus(want string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.statuses {
		if s == want {
			return true
		}
	}
	return false
}

func (h *compactionProbeHandler) snapshotStatuses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.statuses...)
}

func (h *compactionProbeHandler) compactionStatusCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, status := range h.statuses {
		if status == "proactive_compaction" || status == "preflight_compaction" {
			count++
		}
	}
	return count
}

// liveGatewayClient builds a real gateway client from the operator's own
// credentials: env first (CI / non-macOS), then the daemon's credential store.
func liveGatewayClient(t *testing.T) *client.GatewayClient {
	t.Helper()

	endpoint := os.Getenv("KOCORO_ENDPOINT")
	if endpoint == "" {
		endpoint = endpointFromUserConfig()
	}
	key := os.Getenv("KOCORO_API_KEY")
	if key == "" {
		key = apiKeyFromCredentialStore()
	}
	if endpoint == "" || key == "" {
		t.Skip("no live credentials: set KOCORO_ENDPOINT + KOCORO_API_KEY, or sign in so the daemon credential store holds an api_key")
	}
	return client.NewGatewayClient(endpoint, key)
}

// endpointFromUserConfig reads only the top-level `endpoint:` scalar out of the
// operator's global config. A full config.Load() would mutate the real
// ~/.shannon (bundled-skill sync, migrations), which this isolated test must not do.
func endpointFromUserConfig() string {
	data, err := os.ReadFile(filepath.Join(config.ShannonDir(), "config.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "endpoint:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "endpoint:"))
		}
	}
	return ""
}

func apiKeyFromCredentialStore() string {
	if !keychain.Supported() {
		return ""
	}
	store, err := keychain.NewOSStoreAt(config.ShannonDir(), nil)
	if err != nil {
		return ""
	}
	if uid, err := store.Read(keychain.ServiceDaemonState, keychain.AccountCurrentUser); err == nil && uid != "" {
		if key, err := store.Read(keychain.ServiceDaemonAPIKey, uid); err == nil && key != "" {
			return key
		}
	}
	key, _ := store.Read(keychain.ServiceDaemonAPIKey, keychain.AccountLegacy)
	return key
}

func TestLive_Compaction_SurvivesAcrossTheBoundary(t *testing.T) {
	skipUnlessLive(t)
	gw := liveGatewayClient(t)

	const agentName = "compaction-probe"

	shanDir := t.TempDir()
	agentsDir := filepath.Join(shanDir, "agents")
	agentDir := filepath.Join(agentsDir, agentName)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"),
		[]byte("# compaction-probe\n\nTerse verification assistant. Answer in one short sentence.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The window MUST come from the per-agent overlay: that path calls
	// SetContextWindowExplicit, which locks out maybeAutoAdjustContextWindow.
	// A global agent.context_window would be reset back to the model's real
	// 1M window on the first response and no compaction would ever fire.
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"),
		[]byte(fmt.Sprintf("agent:\n    context_window: %d\n", compactionProbeWindow)), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Agent.CompactionSnapshotRetention = 1
	cfg.Agent.CompactionSnapshotMaxAgeDays = 14
	cfg.Daemon.AutoApprove = true

	reg, _, cleanup := tools.RegisterLocalTools(cfg, nil)
	if cleanup != nil {
		defer cleanup()
	}

	deps := &daemon.ServerDeps{
		Config:           cfg,
		GW:               gw,
		Registry:         reg,
		BaselineReg:      reg,
		SessionCache:     daemon.NewSessionCache(shanDir),
		ShannonDir:       shanDir,
		AgentsDir:        agentsDir,
		ReadTrackerCache: daemon.NewReadTrackerCache(),
	}
	defer deps.SessionCache.CloseAll()

	handler := &compactionProbeHandler{}
	sessionID := ""

	run := func(prompt string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		req := daemon.RunAgentRequest{
			Text:      prompt,
			Agent:     agentName,
			SessionID: sessionID,
			Source:    "cli",
		}
		res, err := daemon.RunAgent(ctx, deps, req, handler)
		if err != nil {
			t.Fatalf("RunAgent(%.60q): %v", prompt, err)
		}
		if sessionID == "" {
			sessionID = res.SessionID
		}
		return res.Reply
	}

	// 1. Plant the fact that must survive the boundary.
	run(fmt.Sprintf("Remember this exact build id for later: %s. Reply with just OK.", compactionProbeIdentifier))

	// 2. Inflate the history with real file reads until a compaction fires.
	//    Reads (not pasted text) are deliberate: only a read populates the
	//    session ReadTracker that post-compaction restoration draws from.
	inflaters := []string{
		"internal/daemon/router.go",
		"internal/tools/register.go",
		"internal/session/store.go",
		"internal/mcp/client.go",
		"internal/agent/loopdetect.go",
	}
	root := repoRoot()
	compacted := false
	for _, rel := range inflaters {
		run(fmt.Sprintf("Read the file %s and reply with one sentence about what it does.", filepath.Join(root, rel)))
		if handler.sawStatus("proactive_compaction") || handler.sawStatus("preflight_compaction") {
			compacted = true
			break
		}
	}
	if !compacted {
		t.Fatalf("no compaction fired after %d file reads at a %d-token window; statuses=%v",
			len(inflaters), compactionProbeWindow, handler.snapshotStatuses())
	}

	sessionsDir := filepath.Join(agentsDir, agentName, "sessions")
	store := session.NewStore(sessionsDir)
	defer store.Close()
	beforeFollowup, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load compacted session: %v", err)
	}
	if beforeFollowup.CompactionCheckpoint == nil || len(beforeFollowup.CompactionCheckpoint.Messages) == 0 {
		t.Fatalf("compaction fired but no live checkpoint was persisted: %#v", beforeFollowup.CompactionCheckpoint)
	}
	checkpointBefore, err := json.Marshal(beforeFollowup.CompactionCheckpoint)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	compactionCountBefore := handler.compactionStatusCount()

	// 3. The question that only a useful summary can answer.
	answer := run("What exact build id did I give you at the very start of this conversation? Reply with the id and nothing else.")

	t.Run("identifier survives the compaction boundary", func(t *testing.T) {
		if !strings.Contains(strings.ToLower(answer), compactionProbeIdentifier) {
			t.Fatalf("build id lost across compaction.\nwant %q in the answer\ngot: %s", compactionProbeIdentifier, answer)
		}
	})

	t.Run("next turn reuses the stable checkpoint", func(t *testing.T) {
		if got := handler.compactionStatusCount(); got != compactionCountBefore {
			t.Fatalf("follow-up summarized the archive again: compaction statuses %d -> %d", compactionCountBefore, got)
		}
		afterFollowup, err := store.Load(sessionID)
		if err != nil {
			t.Fatalf("reload session: %v", err)
		}
		checkpointAfter, err := json.Marshal(afterFollowup.CompactionCheckpoint)
		if err != nil {
			t.Fatalf("marshal checkpoint after follow-up: %v", err)
		}
		if string(checkpointAfter) != string(checkpointBefore) {
			t.Fatalf("non-compacting follow-up rewrote the checkpoint\nbefore: %s\nafter:  %s", checkpointBefore, checkpointAfter)
		}
		if len(afterFollowup.Messages) <= len(beforeFollowup.Messages) {
			t.Fatalf("follow-up did not append to lossless archive: before=%d after=%d",
				len(beforeFollowup.Messages), len(afterFollowup.Messages))
		}
	})

	t.Run("snapshot landed and carries no image payload", func(t *testing.T) {
		dir := filepath.Join(sessionsDir, ".compaction-snapshots", sessionID)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("no snapshot directory for the compacted session: %v", err)
		}
		var files []string
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".json" {
				files = append(files, e.Name())
			}
		}
		if len(files) == 0 {
			t.Fatal("snapshot directory exists but holds no snapshot")
		}
		raw, err := os.ReadFile(filepath.Join(dir, files[0]))
		if err != nil {
			t.Fatal(err)
		}
		var snap session.CompactionSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatalf("snapshot is not decodable: %v", err)
		}
		if len(snap.Messages) == 0 {
			t.Fatal("snapshot persisted an empty history")
		}
		for _, m := range snap.Messages {
			if !m.Content.HasBlocks() {
				continue
			}
			for _, b := range m.Content.Blocks() {
				if b.Type == "image" {
					t.Fatal("snapshot retained an inline image block")
				}
			}
		}
		t.Logf("snapshot ok: %d files, newest holds %d messages, %d KB on disk",
			len(files), len(snap.Messages), len(raw)/1024)
	})

	// Compaction now separates the lossless archive from durable model-live
	// state. Session.Messages stays complete for search/share/sync while
	// HistoryForLoop resolves checkpoint + raw tail on every later turn.
	t.Run("archive and live checkpoint remain separate", func(t *testing.T) {
		sess, err := store.Load(sessionID)
		if err != nil {
			t.Fatalf("load session: %v", err)
		}

		plantedSurvives, restored, summaries := false, 0, 0
		for _, m := range sess.Messages {
			text := m.Content.Text()
			if strings.Contains(strings.ToLower(text), compactionProbeIdentifier) {
				plantedSurvives = true
			}
			if strings.Contains(text, "## [restored] ") {
				restored++
			}
			if strings.Contains(text, "Previous context summary:") {
				summaries++
			}
		}

		// The turn that planted the id predates the compaction. If compaction
		// had rewritten the transcript, this message would be gone.
		if !plantedSurvives {
			t.Error("pre-compaction turn is missing from the persisted transcript")
		}
		if summaries != 0 {
			t.Errorf("compaction summary leaked into the lossless transcript (%d occurrences)", summaries)
		}
		if restored == 0 {
			t.Error("no restored file block after a proactive/preflight compaction")
		}
		live := sess.HistoryForLoop()
		if sess.CompactionCheckpoint == nil || len(live) >= len(sess.Messages) {
			t.Fatalf("live checkpoint did not reduce context: checkpoint=%#v live=%d archive=%d",
				sess.CompactionCheckpoint, len(live), len(sess.Messages))
		}
		t.Logf("persisted transcript: %d messages; live context: %d messages; %d restored block(s)",
			len(sess.Messages), len(live), restored)
	})
}
