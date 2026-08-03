package daemon

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/spf13/viper"
)

type daemonCatalogArtifactProvider struct {
	catalog skills.CatalogProvider
	archive []byte
}

type daemonStaticCatalogProvider struct {
	mu       sync.RWMutex
	entries  []skills.CatalogEntry
	revision string
	archive  []byte
}

func (p *daemonStaticCatalogProvider) Catalog(context.Context) ([]skills.CatalogEntry, string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]skills.CatalogEntry(nil), p.entries...), p.revision, nil
}

func (p *daemonStaticCatalogProvider) OpenCatalogArtifact(_ context.Context, installation skills.CatalogInstallation) (io.ReadCloser, error) {
	if got := fmt.Sprintf("%x", sha256.Sum256(p.archive)); got != installation.ArchiveSHA256 {
		return nil, fmt.Errorf("fixture archive digest mismatch")
	}
	return io.NopCloser(bytes.NewReader(p.archive)), nil
}

func (p *daemonStaticCatalogProvider) replaceEntry(entry skills.CatalogEntry, revision string) {
	p.mu.Lock()
	p.entries = []skills.CatalogEntry{entry}
	p.revision = revision
	p.mu.Unlock()
}

type failingRecommendationWriter struct {
	header http.Header
}

type continuationApprovalTool struct{ runs atomic.Int32 }

func (*continuationApprovalTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "continuation_approval_probe", Description: "Perform a test action after explicit approval", Parameters: map[string]any{"type": "object"}}
}
func (*continuationApprovalTool) RequiresApproval() bool { return true }
func (t *continuationApprovalTool) Run(context.Context, string) (agent.ToolResult, error) {
	t.runs.Add(1)
	return agent.ToolResult{Content: "approved action completed"}, nil
}

type continuationApprovalGateway struct {
	calls atomic.Int32
	mu    sync.Mutex
	reqs  []client.CompletionRequest
}

type continuationBlockingGateway struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (g *continuationBlockingGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		g.calls.Add(1)
		g.once.Do(func() { close(g.started) })
		select {
		case <-r.Context().Done():
		case <-g.release:
		}
	}
}

func (g *continuationApprovalGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		g.reqs = append(g.reqs, req)
		g.mu.Unlock()
		if r.URL.Path != "/v1/completions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if g.calls.Add(1) == 1 {
			fmt.Fprint(w, `data: {"type":"done","provider":"anthropic","model":"test-model","finish_reason":"tool_use","output_text":"Proceeding with the approved test action.","tool_calls":[{"id":"approval-probe-1","name":"continuation_approval_probe","arguments":{}}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"type":"done","provider":"anthropic","model":"test-model","finish_reason":"end_turn","output_text":"continued after approval"}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (w *failingRecommendationWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*failingRecommendationWriter) WriteHeader(int) {}
func (*failingRecommendationWriter) Flush()          {}
func (*failingRecommendationWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("event: skill.recommendation.v1")) {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (p daemonCatalogArtifactProvider) Catalog(ctx context.Context) ([]skills.CatalogEntry, string, error) {
	return p.catalog.Catalog(ctx)
}

func recommendationCatalogFixture(t *testing.T, slug string) (*daemonStaticCatalogProvider, skills.CatalogEntry) {
	t.Helper()
	skillMD := "---\nname: " + slug + "\ndescription: recommendation fixture\n---\n"
	archive := daemonSkillArchive(t, slug, skillMD)
	entry := skills.CatalogEntry{
		ID: "official:" + slug, Slug: slug, Source: "official", DisplayName: "Fixture Capability",
		Description: "Dynamically published fixture capability", Version: "1.0.0", Installable: true,
		Installation:   skills.CatalogInstallation{Provider: "github_archive", Repository: "https://github.com/example/skills", Ref: strings.Repeat("a", 40), ArtifactPath: "skills/" + slug, ArchiveSHA256: fmt.Sprintf("%x", sha256.Sum256(archive))},
		Recommendation: skills.RecommendationMetadata{Eligible: true, IntentTags: []string{"fixture.create"}, Surfaces: []string{"desktop"}, MaxBundleSize: 3},
	}
	return &daemonStaticCatalogProvider{entries: []skills.CatalogEntry{entry}, revision: "sha256:" + strings.Repeat("b", 64), archive: archive}, entry
}

func (p daemonCatalogArtifactProvider) OpenCatalogArtifact(_ context.Context, installation skills.CatalogInstallation) (io.ReadCloser, error) {
	if got := fmt.Sprintf("%x", sha256.Sum256(p.archive)); got != installation.ArchiveSHA256 {
		return nil, fmt.Errorf("fixture archive digest mismatch")
	}
	return io.NopCloser(bytes.NewReader(p.archive)), nil
}

func daemonSkillArchive(t *testing.T, slug, content string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	path := "skills-fixture/skills/" + slug + "/SKILL.md"
	if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSkillRecommendationDynamicCatalogInstallsThroughHTTP(t *testing.T) {
	dir := t.TempDir()
	slug := "dynamic-fixture"
	skillMD := "---\nname: dynamic-fixture\ndescription: dynamically published fixture\n---\n"
	archive := daemonSkillArchive(t, slug, skillMD)
	entry := skills.CatalogEntry{
		ID: "official:" + slug, Slug: slug, Source: "official", DisplayName: "Dynamic Fixture",
		Description: "Dynamically published fixture capability", Version: "1.0.0", Installable: true,
		Installation: skills.CatalogInstallation{
			Provider: "github_archive", Repository: "https://github.com/example/skills",
			Ref: strings.Repeat("a", 40), ArtifactPath: "skills/" + slug,
			ArchiveSHA256: fmt.Sprintf("%x", sha256.Sum256(archive)),
		},
		Recommendation: skills.RecommendationMetadata{Eligible: true, IntentTags: []string{"fixture.create"}, Surfaces: []string{"desktop"}, MaxBundleSize: 3},
	}
	index, err := json.Marshal(skills.RegistryIndex{Version: 1, InstallableCapabilities: []skills.CatalogEntry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(index) }))
	defer registry.Close()
	provider := daemonCatalogArtifactProvider{
		catalog: skills.NewRegistryCatalogProvider(skills.NewMarketplaceClient(registry.URL, 0), skills.NewEmbeddedCatalogProvider(dir), true),
		archive: archive,
	}
	s := NewServer(0, nil, &ServerDeps{Config: &config.Config{}, ShannonDir: dir, CatalogProvider: provider}, "test")
	req := httptest.NewRequest(http.MethodPost, "/skills/install/"+slug, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", rr.Code, rr.Body.String())
	}
	installed, err := os.ReadFile(filepath.Join(dir, "skills", slug, "SKILL.md"))
	if err != nil || string(installed) != skillMD {
		t.Fatalf("installed SKILL.md=%q err=%v", installed, err)
	}
}

func TestSkillRecommendationCatalogFiltersInstalledAndBuiltin(t *testing.T) {
	dir := t.TempDir()
	entries, err := skills.InstallableCatalog(dir, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("empty catalog")
	}
	found := false
	for _, e := range entries {
		if e.ID == "official:pptx" {
			found = true
		}
		if skills.IsBuiltinSkill(e.Slug) {
			t.Fatalf("builtin leaked: %s", e.Slug)
		}
	}
	if !found {
		t.Fatal("pptx absent")
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "pptx"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "pptx", "SKILL.md"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err = skills.InstallableCatalog(dir, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ID == "official:pptx" {
			t.Fatal("installed pptx leaked")
		}
	}
}

func TestSkillRecommendationOfferFiltersUnknownDuplicateAndTOCTOU(t *testing.T) {
	dir := t.TempDir()
	store := newSkillRecommendationStore(dir)
	emitted := 0
	rc := withSkillRecommendationRun(context.Background(), &skillRecommendationRunContext{accountID: "acct", deviceID: "12345678-1234-1234-1234-123456789abc", sessionID: "session", turnID: "turn", store: store, emit: func(skillRecommendationV1) bool { emitted++; return true }})
	discovery, err := (&discoverInstallableSkillsTool{shannonDir: dir}).Run(rc, `{"intent_tags":["presentation.create"]}`)
	if err != nil || discovery.IsError {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	tool := &offerSkillInstallationTool{shannonDir: dir}
	result, err := tool.Run(rc, `{"catalog_ids":["unknown","official:pptx","official:pptx","official:kocoro"],"reason":"presentations"}`)
	if err != nil || result.IsError {
		t.Fatalf("offer=%+v err=%v", result, err)
	}
	if emitted != 1 {
		t.Fatalf("emitted=%d", emitted)
	}
	if !result.StopAgentLoop {
		t.Fatal("offer did not terminate the agent loop")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.byID) != 1 {
		t.Fatalf("offers=%d", len(store.byID))
	}
	for _, v := range store.byID {
		if len(v.Items) != 1 || v.Items[0].CatalogID != "official:pptx" {
			t.Fatalf("items=%+v", v.Items)
		}
	}
}

func TestSkillRecommendationOfferFailsClosedWithoutActiveSink(t *testing.T) {
	dir := t.TempDir()
	store := newSkillRecommendationStore(dir)
	ctx := withSkillRecommendationRun(context.Background(), &skillRecommendationRunContext{
		accountID: "acct", deviceID: "12345678-1234-1234-1234-123456789abc", sessionID: "s", turnID: "t", store: store,
		emit: func(skillRecommendationV1) bool { return false }, discovered: map[string]bool{"official:pptx": true},
		catalogRevision: "", // offer verifies revision after the discovery helper below.
	})
	if _, err := (&discoverInstallableSkillsTool{shannonDir: dir}).Run(ctx, `{"intent_tags":["presentation.create"]}`); err != nil {
		t.Fatal(err)
	}
	result, err := (&offerSkillInstallationTool{shannonDir: dir}).Run(ctx, `{"catalog_ids":["official:pptx"],"reason":"presentation"}`)
	if err != nil || !result.IsError || !result.StopAgentLoop {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, offer := range store.byID {
		if offer.State != "expired" {
			t.Fatalf("undeliverable offer state=%s", offer.State)
		}
	}
	retry, err := (&offerSkillInstallationTool{shannonDir: dir}).Run(ctx, `{"catalog_ids":["official:pptx"],"reason":"presentation"}`)
	if err != nil || !retry.IsError || !retry.StopAgentLoop {
		t.Fatalf("expired same-turn offer was revived: result=%+v err=%v", retry, err)
	}
}

func TestSkillRecommendationStoreBindsOwnerAndExpiresOnAccountChange(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, created, err := s.offer("acct-a", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("not created")
	}
	if _, _, err := s.beginContinuation("acct-b", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken); err == nil {
		t.Fatal("cross-account consume allowed")
	}
	if err := s.invalidateAccount("acct-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.beginContinuation("acct-a", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken); err == nil {
		t.Fatal("expired consume allowed")
	}
}

func TestSkillRecommendationPersistsNamedAgentForContinuation(t *testing.T) {
	dir := t.TempDir()
	s := newSkillRecommendationStore(dir)
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "researcher", "session", "turn", "revision", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	reloaded := newSkillRecommendationStore(dir)
	got := reloaded.byID[v.RecommendationID]
	if got == nil || got.OwnerAgentName != "researcher" {
		t.Fatalf("reloaded recommendation=%+v", got)
	}
	req := skillRecommendationContinuationRequest(cloneSkillRecommendation(got), "continue")
	if req.Agent != "researcher" || req.SessionID != "session" {
		t.Fatalf("continuation request=%+v", req)
	}
}

// Nothing else ever deletes from the store, so without pruning the file and the
// per-mutation serialization cost grow monotonically: turn keys embed a random
// per-run turnID and every terminal record was retained forever.
func TestSkillRecommendationStorePrunesExpiredTerminalRecords(t *testing.T) {
	dir := t.TempDir()
	s := newSkillRecommendationStore(dir)
	device := "12345678-1234-1234-1234-123456789abc"
	stale, _, err := s.offer("acct", device, "", "session", "turn-stale", "revision", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	live, _, err := s.offer("acct", device, "", "session", "turn-live", "revision", []skillRecommendationItemWireV1{{CatalogID: "official:docx"}})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.byID[stale.RecommendationID].State = "dismissed"
	s.byID[stale.RecommendationID].ExpiresAt = time.Now().Add(-time.Hour)
	saveErr := s.saveLocked()
	s.mu.Unlock()
	if saveErr != nil {
		t.Fatal(saveErr)
	}

	reloaded := newSkillRecommendationStore(dir)
	if reloaded.loadErr != nil {
		t.Fatal(reloaded.loadErr)
	}
	if _, ok := reloaded.byID[stale.RecommendationID]; ok {
		t.Fatal("expired terminal record survived the prune")
	}
	if _, ok := reloaded.byID[live.RecommendationID]; !ok {
		t.Fatal("prune dropped a still-actionable record")
	}
	if len(reloaded.byTurn) != 1 {
		t.Fatalf("byTurn kept a dangling key: %+v", reloaded.byTurn)
	}
}

func TestSkillRecommendationEventsPublicSeamIsAccountDeviceDirected(t *testing.T) {
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "opaque-account"}, "")
	s.auth = auth

	httpServer := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := "12345678-1234-1234-1234-123456789abc"
	request.Header.Set(desktopDeviceHeader, deviceID)
	request.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !s.hasSkillRecommendationSink("opaque-account", deviceID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.hasSkillRecommendationSink("opaque-account", deviceID) {
		t.Fatal("directed SSE sink was not registered")
	}
	v, _, err := s.skillRecommendations.offer("opaque-account", deviceID, "researcher", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx", Source: "official", DisplayName: "Presentation", CapabilitySummary: "Create and edit presentations"}})
	if err != nil {
		t.Fatal(err)
	}
	emitted := make(chan bool, 1)
	go func() { emitted <- s.emitSkillRecommendation(v) }()

	scanner := bufio.NewScanner(response.Body)
	var payload []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			payload = append([]byte(nil), line[len("data: "):]...)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 {
		t.Fatal("recommendation event had no data payload")
	}
	select {
	case ok := <-emitted:
		if !ok {
			t.Fatal("producer did not receive flush acknowledgement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not receive delivery acknowledgement")
	}
	var decoded struct {
		RecommendationID  string `json:"recommendation_id"`
		SessionID         string `json:"session_id"`
		TurnID            string `json:"turn_id"`
		ContinuationToken string `json:"continuation_token"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RecommendationID != v.RecommendationID || decoded.SessionID != "session" || decoded.TurnID != "turn" || decoded.ContinuationToken == "" {
		t.Fatalf("decoded directed event=%+v", decoded)
	}
	if bytes.Contains(payload, []byte("opaque-account")) || bytes.Contains(payload, []byte(deviceID)) || bytes.Contains(payload, []byte("researcher")) {
		t.Fatalf("internal owner identity leaked: %s", payload)
	}
}

func TestSkillRecommendationQueuedDeliveryCannotArriveAfterTimeout(t *testing.T) {
	ch := make(chan *recommendationDelivery, 1)
	done := make(chan struct{})
	if enqueueSkillRecommendation(ch, done, skillRecommendationV1{RecommendationID: "stale"}, 5*time.Millisecond) {
		t.Fatal("unclaimed delivery unexpectedly succeeded")
	}
	delivery := <-ch
	if delivery.state.Load() != -1 {
		t.Fatalf("timed-out delivery state=%d", delivery.state.Load())
	}
	if delivery.state.CompareAndSwap(0, 1) {
		t.Fatal("event loop could claim a cancelled delivery")
	}
}

func TestSkillRecommendationDeliveryQueueFullFailsImmediately(t *testing.T) {
	ch := make(chan *recommendationDelivery, 1)
	ch <- &recommendationDelivery{}
	started := time.Now()
	if enqueueSkillRecommendation(ch, make(chan struct{}), skillRecommendationV1{RecommendationID: "overflow"}, time.Second) {
		t.Fatal("full recommendation queue reported success")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("queue-full admission blocked instead of failing closed")
	}
}

func TestSkillRecommendationDeliveryDisconnectBeforeWriteCancelsQueuedEvent(t *testing.T) {
	ch := make(chan *recommendationDelivery, 1)
	done := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		result <- enqueueSkillRecommendation(ch, done, skillRecommendationV1{RecommendationID: "disconnect"}, time.Second)
	}()
	delivery := <-ch
	close(done)
	if <-result {
		t.Fatal("disconnected recommendation stream reported delivery")
	}
	if delivery.state.Load() != -1 || delivery.state.CompareAndSwap(0, 1) {
		t.Fatalf("disconnected queued delivery remained writable: state=%d", delivery.state.Load())
	}
}

func TestSkillRecommendationDeliveryClaimedACKTimeoutIsBounded(t *testing.T) {
	ch := make(chan *recommendationDelivery, 1)
	result := make(chan bool, 1)
	const timeout = 10 * time.Millisecond
	started := time.Now()
	go func() {
		result <- enqueueSkillRecommendation(ch, make(chan struct{}), skillRecommendationV1{RecommendationID: "no-ack"}, timeout)
	}()
	delivery := <-ch
	if !delivery.state.CompareAndSwap(0, 1) {
		t.Fatal("could not simulate writer claim")
	}
	select {
	case delivered := <-result:
		if delivered {
			t.Fatal("missing flush ACK reported success")
		}
		if elapsed := time.Since(started); elapsed > 10*timeout {
			t.Fatalf("ACK timeout was not bounded: %s", elapsed)
		}
	case <-time.After(20 * timeout):
		t.Fatal("enqueue waited indefinitely after writer claim")
	}
}

func TestSkillRecommendationEventsWriteFailureReturnsNegativeACK(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(0, nil, &ServerDeps{Config: &config.Config{}, ShannonDir: dir}, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	s.auth = auth
	device := "12345678-1234-1234-1234-123456789abc"
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	request.Header.Set(desktopDeviceHeader, device)
	request.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	handlerDone := make(chan struct{})
	go func() {
		s.handleEvents(&failingRecommendationWriter{}, request)
		close(handlerDone)
	}()
	deadline := time.Now().Add(time.Second)
	for !s.hasSkillRecommendationSink("acct", device) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	v, _, err := s.skillRecommendations.offer("acct", device, "", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if s.emitSkillRecommendation(v) {
		cancel()
		t.Fatal("SSE write failure reported delivery success")
	}
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("failed SSE handler did not stop after disconnect")
	}
}

func TestSkillRecommendationMessageBoundEmitterRejectsSinkReplacement(t *testing.T) {
	s := &Server{skillRecommendationSinks: make(map[string]skillRecommendationSink)}
	device := "12345678-1234-1234-1234-123456789abc"
	oldCalls := 0
	unregisterOld := s.registerSkillRecommendationSink("acct", device, func(skillRecommendationV1) bool { oldCalls++; return true }, func() {})
	defer unregisterOld()
	emit, ok := s.skillRecommendationEmitter("acct", device)
	if !ok {
		t.Fatal("live sink did not admit message")
	}
	newCalls := 0
	unregisterNew := s.registerSkillRecommendationSink("acct", device, func(skillRecommendationV1) bool { newCalls++; return true }, func() {})
	defer unregisterNew()
	v := skillRecommendationV1{State: "offered", OwnerAccountID: "acct", OwnerDeviceID: device, ExpiresAt: time.Now().Add(time.Hour)}
	if emit(v) {
		t.Fatal("message-bound emitter delivered through a replacement connection")
	}
	if oldCalls != 0 || newCalls != 0 {
		t.Fatalf("replacement routing old=%d new=%d", oldCalls, newCalls)
	}
}

func TestSkillRecommendationContinuePublicSeamResumesNamedAgentSession(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "continued in researcher"}
	upstream := httptest.NewServer(gw.handler())
	defer upstream.Close()
	deps := runAgentContractTestDeps(t, upstream.URL)
	defer deps.SessionCache.CloseAll()
	provider, entry := recommendationCatalogFixture(t, "dynamic-fixture")
	deps.CatalogProvider = provider
	agentDir := filepath.Join(deps.AgentsDir, "researcher")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("You are the researcher."), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := deps.SessionCache.GetOrCreate("researcher")
	sess := mgr.NewSession()
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("prior turn")},
		{Role: "assistant", Content: client.NewTextContent("prior answer")},
	}
	// Seed meta alongside so the two stay index-aligned, as every production
	// append does. Without this the fixture desyncs them and injected-message
	// filtering silently drops the wrong rows.
	sess.MessageMeta = []session.MessageMeta{{Source: "desktop"}, {Source: "desktop"}}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: deps.ShannonDir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "opaque-account"}, "")
	s.auth = auth
	deviceID := "12345678-1234-1234-1234-123456789abc"
	eventsServer := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer eventsServer.Close()
	eventsRequest, _ := http.NewRequest(http.MethodGet, eventsServer.URL, nil)
	eventsRequest.Header.Set(desktopDeviceHeader, deviceID)
	eventsRequest.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	eventsResponse, err := http.DefaultClient.Do(eventsRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer eventsResponse.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !s.hasSkillRecommendationSink("opaque-account", deviceID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	run := &skillRecommendationRunContext{accountID: "opaque-account", deviceID: deviceID, agentName: "researcher", sessionID: sess.ID, turnID: "turn", store: s.skillRecommendations, emit: s.emitSkillRecommendation, discovered: map[string]bool{}}
	runContext := withSkillRecommendationRun(context.Background(), run)
	if discovery, err := (&discoverInstallableSkillsTool{shannonDir: deps.ShannonDir, catalog: provider}).Run(runContext, `{"intent_tags":["fixture.create"]}`); err != nil || discovery.IsError {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	offered := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := (&offerSkillInstallationTool{shannonDir: deps.ShannonDir, catalog: provider}).Run(runContext, `{"catalog_ids":["official:dynamic-fixture"],"reason":"fixture capability"}`)
		offered <- result
	}()
	var event struct {
		RecommendationID  string `json:"recommendation_id"`
		ContinuationToken string `json:"continuation_token"`
	}
	scanner := bufio.NewScanner(eventsResponse.Body)
	for scanner.Scan() {
		if line := scanner.Bytes(); bytes.HasPrefix(line, []byte("data: ")) {
			if err := json.Unmarshal(line[len("data: "):], &event); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if result := <-offered; result.IsError || !result.StopAgentLoop || event.RecommendationID == "" || event.ContinuationToken == "" {
		t.Fatalf("offer=%+v event=%+v", result, event)
	}
	// Change the live catalog after offer. The single install-and-continue action
	// must still install the immutable descriptor captured in the recommendation.
	newer := entry
	newer.Version = "2.0.0"
	newer.Installation.Ref = strings.Repeat("c", 40)
	provider.replaceEntry(newer, "sha256:"+strings.Repeat("d", 64))

	body, _ := json.Marshal(map[string]string{"session_id": sess.ID, "continuation_token": event.ContinuationToken})
	request := httptest.NewRequest(http.MethodPost, "/skill-recommendations/"+event.RecommendationID+"/continue", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(desktopDeviceHeader, deviceID)
	request.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "event: done") {
		t.Fatalf("continue status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	resumed, err := mgr.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, _ := json.Marshal(resumed.Messages)
	if !bytes.Contains(transcript, []byte("continued in researcher")) {
		t.Fatalf("named session was not resumed: %s", transcript)
	}
	// The install follow-up is daemon-authored control input. It must be flagged
	// SystemInjected so displayed history, share export and the FTS index all
	// drop it — otherwise the user sees a bubble reading "User enabled ..." that
	// they never typed.
	followupIdx := -1
	for i, msg := range resumed.Messages {
		if msg.Role == "user" && strings.Contains(msg.Content.Text(), "User enabled ") {
			followupIdx = i
		}
	}
	if followupIdx < 0 {
		t.Fatalf("continuation follow-up was not persisted: %s", transcript)
	}
	if followupIdx >= len(resumed.MessageMeta) || !resumed.MessageMeta[followupIdx].SystemInjected {
		t.Fatalf("continuation follow-up persisted as ordinary user history: meta=%+v", resumed.MessageMeta)
	}
	visible := session.FilterInjected(resumed.Messages, resumed.MessageMeta)
	for _, msg := range visible {
		if strings.Contains(msg.Content.Text(), "User enabled ") {
			t.Fatal("continuation follow-up survived injected-message filtering")
		}
	}
	if _, err := deps.SessionCache.GetOrCreate("").Load(sess.ID); err == nil {
		t.Fatal("continuation incorrectly created the named session in the default agent directory")
	}
	loaded, err := agents.LoadAgent(deps.AgentsDir, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, attached := range loaded.Skills {
		found = found || attached.Slug == entry.Slug
	}
	if !found {
		t.Fatalf("installed skill %q was not attached to the resumed named agent", entry.Slug)
	}
	requestBytes, _ := json.Marshal(gw.requests())
	if !bytes.Contains(requestBytes, []byte(entry.Slug)) {
		t.Fatalf("resumed LLM request did not expose attached skill %q: %s", entry.Slug, requestBytes)
	}
	diskReceipt, ok, err := skills.ReadCatalogInstallReceipt(filepath.Join(deps.ShannonDir, "skills", entry.Slug))
	if err != nil || !ok || diskReceipt.CatalogID != entry.ID || diskReceipt.Version != entry.Version || diskReceipt.TreeSHA256 == "" {
		t.Fatalf("install receipt=%+v ok=%v err=%v", diskReceipt, ok, err)
	}
}

func TestSkillRecommendationContinueUsesAttendedDesktopApprovalSSE(t *testing.T) {
	gateway := &continuationApprovalGateway{}
	upstream := httptest.NewServer(gateway.handler())
	defer upstream.Close()
	deps := runAgentContractTestDeps(t, upstream.URL)
	defer deps.SessionCache.CloseAll()
	provider, entry := recommendationCatalogFixture(t, "approval-fixture")
	deps.CatalogProvider = provider
	tool := &continuationApprovalTool{}
	deps.Registry.Register(tool)
	deps.BaselineReg.Register(tool)
	mgr := deps.SessionCache.GetOrCreate("")
	sess := mgr.NewSession()
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("prior task")},
		{Role: "assistant", Content: client.NewTextContent("waiting for capability")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: deps.ShannonDir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "opaque-account"}, "")
	s.auth = auth
	device := "12345678-1234-1234-1234-123456789abc"
	v, _, err := s.skillRecommendations.offer("opaque-account", device, "", sess.ID, "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: entry.ID, Slug: entry.Slug, DisplayName: entry.DisplayName}}, []skills.CatalogEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	body, _ := json.Marshal(map[string]string{"session_id": sess.ID, "continuation_token": v.ContinuationToken})
	req, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/skill-recommendations/"+v.RecommendationID+"/continue", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(desktopDeviceHeader, device)
	req.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("continue status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	approvalID := ""
	doneSeen := false
	var seenLines []string
	for scanner.Scan() {
		line := scanner.Text()
		seenLines = append(seenLines, line)
		if line == "event: approval" && scanner.Scan() {
			dataLine := scanner.Text()
			var approval struct {
				RequestID string `json:"request_id"`
				Tool      string `json:"tool"`
			}
			if !strings.HasPrefix(dataLine, "data: ") || json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &approval) != nil {
				t.Fatalf("invalid approval frame: %q", dataLine)
			}
			if approval.Tool != "continuation_approval_probe" || approval.RequestID == "" {
				t.Fatalf("approval=%+v", approval)
			}
			approvalID = approval.RequestID
			// A real concurrent HTTP retry observes the durable single-flight
			// claim and returns 202 without starting another agent run.
			duplicateRequest, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/skill-recommendations/"+v.RecommendationID+"/continue", bytes.NewReader(body))
			duplicateRequest.Header.Set("Content-Type", "application/json")
			duplicateRequest.Header.Set(desktopDeviceHeader, device)
			duplicateRequest.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
			duplicateResponse, duplicateErr := http.DefaultClient.Do(duplicateRequest)
			if duplicateErr != nil {
				t.Fatal(duplicateErr)
			}
			duplicateData, _ := io.ReadAll(duplicateResponse.Body)
			duplicateResponse.Body.Close()
			if duplicateResponse.StatusCode != http.StatusAccepted || !bytes.Contains(duplicateData, []byte(`"status":"accepted"`)) {
				t.Fatalf("duplicate continue status=%d body=%s", duplicateResponse.StatusCode, duplicateData)
			}
			approvalBody := strings.NewReader(fmt.Sprintf(`{"request_id":%q,"decision":"allow"}`, approvalID))
			approvalRequest, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/approval", approvalBody)
			approvalRequest.Header.Set("Content-Type", "application/json")
			approvalResponse, approveErr := http.DefaultClient.Do(approvalRequest)
			if approveErr != nil {
				t.Fatal(approveErr)
			}
			approvalResponse.Body.Close()
			if approvalResponse.StatusCode != http.StatusOK {
				t.Fatalf("approval response=%d", approvalResponse.StatusCode)
			}
		}
		if line == "event: done" {
			doneSeen = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if approvalID == "" || !doneSeen || tool.runs.Load() != 1 {
		gateway.mu.Lock()
		requests := append([]client.CompletionRequest(nil), gateway.reqs...)
		gateway.mu.Unlock()
		t.Fatalf("approval=%q done=%v tool_runs=%d gateway_calls=%d lines=%v requests=%+v", approvalID, doneSeen, tool.runs.Load(), gateway.calls.Load(), seenLines, requests)
	}
	toolFrames, usageFrames, preambleFrames := 0, 0, 0
	for _, line := range seenLines {
		switch line {
		case "event: tool":
			toolFrames++
		case "event: usage":
			usageFrames++
		case "event: assistant_text":
			preambleFrames++
		}
	}
	if toolFrames != 2 || usageFrames == 0 || preambleFrames == 0 {
		t.Fatalf("continuation event routing tool=%d usage=%d preamble=%d lines=%v", toolFrames, usageFrames, preambleFrames, seenLines)
	}
	s.skillRecommendations.mu.Lock()
	state := s.skillRecommendations.byID[v.RecommendationID].State
	s.skillRecommendations.mu.Unlock()
	if state != "completed" {
		t.Fatalf("continuation state=%s", state)
	}
}

func TestSkillRecommendationContinuationRestartKeepsStableIdempotency(t *testing.T) {
	dir := t.TempDir()
	s := newSkillRecommendationStore(dir)
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "researcher", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart, run, err := s.beginContinuation("acct", v.OwnerDeviceID, v.SessionID, v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("initial claim run=%v err=%v", run, err)
	}
	reloaded := newSkillRecommendationStore(dir)
	afterRestart, run, err := reloaded.beginContinuation("acct", v.OwnerDeviceID, v.SessionID, v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("restart claim run=%v err=%v", run, err)
	}
	firstReq := skillRecommendationContinuationRequest(beforeRestart, "follow-up")
	secondReq := skillRecommendationContinuationRequest(afterRestart, "follow-up")
	if firstReq.IdempotencyKey == "" || firstReq.IdempotencyKey != secondReq.IdempotencyKey || secondReq.Agent != "researcher" {
		t.Fatalf("restart requests before=%+v after=%+v", firstReq, secondReq)
	}
}

func TestSkillRecommendationContinuationDisconnectDoesNotReplay(t *testing.T) {
	gateway := &continuationBlockingGateway{started: make(chan struct{}), release: make(chan struct{})}
	upstream := httptest.NewServer(gateway.handler())
	defer upstream.Close()
	deps := runAgentContractTestDeps(t, upstream.URL)
	defer deps.SessionCache.CloseAll()
	provider, entry := recommendationCatalogFixture(t, "disconnect-fixture")
	deps.CatalogProvider = provider
	mgr := deps.SessionCache.GetOrCreate("")
	sess := mgr.NewSession()
	sess.Messages = []client.Message{{Role: "user", Content: client.NewTextContent("prior")}, {Role: "assistant", Content: client.NewTextContent("waiting")}}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: deps.ShannonDir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	s.auth = auth
	device := "12345678-1234-1234-1234-123456789abc"
	v, _, err := s.skillRecommendations.offer("acct", device, "", sess.ID, "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: entry.ID, Slug: entry.Slug, DisplayName: entry.DisplayName}}, []skills.CatalogEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	body, _ := json.Marshal(map[string]string{"session_id": sess.ID, "continuation_token": v.ContinuationToken})
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/skill-recommendations/"+v.RecommendationID+"/continue", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(desktopDeviceHeader, device)
	request.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-gateway.started:
	case <-time.After(time.Second):
		cancel()
		response.Body.Close()
		t.Fatal("continuation gateway request did not start")
	}
	cancel()
	close(gateway.release)
	response.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	state := ""
	for time.Now().Before(deadline) {
		s.skillRecommendations.mu.Lock()
		state = s.skillRecommendations.byID[v.RecommendationID].State
		s.skillRecommendations.mu.Unlock()
		if state == "installation_failed" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if state != "installation_failed" {
		t.Fatalf("disconnect state=%s", state)
	}

	retry, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/skill-recommendations/"+v.RecommendationID+"/continue", bytes.NewReader(body))
	retry.Header.Set("Content-Type", "application/json")
	retry.Header.Set(desktopDeviceHeader, device)
	retry.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	retryResponse, err := http.DefaultClient.Do(retry)
	if err != nil {
		t.Fatal(err)
	}
	retryResponse.Body.Close()
	if retryResponse.StatusCode != http.StatusOK || gateway.calls.Load() != 1 {
		t.Fatalf("retry status=%d gateway_calls=%d", retryResponse.StatusCode, gateway.calls.Load())
	}
}

func TestSkillRecommendationWireDoesNotLeakOwnerIdentity(t *testing.T) {
	v := skillRecommendationV1{SchemaVersion: 1, RecommendationID: "r", OwnerAccountID: "acct", OwnerDeviceID: "device", ContinuationToken: "secret"}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "acct") || strings.Contains(string(b), "device") || !strings.Contains(string(b), `"continuation_token":"secret"`) {
		t.Fatalf("owner leaked: %s", b)
	}
}

func TestSkillRecommendationAuditSummariesNeverContainUserText(t *testing.T) {
	discoverInput, discoverOutput := (&discoverInstallableSkillsTool{}).AuditSummaries(
		`{"task_summary":"PRIVATE CUSTOMER REQUEST","intent_tags":["private.tag"]}`,
		`[{"catalog_id":"official:pptx","description":"controlled"}]`,
	)
	offerInput, offerOutput := (&offerSkillInstallationTool{}).AuditSummaries(
		`{"catalog_ids":["official:pptx","official:pptx","PRIVATE_MODEL_ID"],"reason":"PRIVATE MODEL REASON"}`,
		"A skill installation card is ready. Stop here and wait for the user's choice.",
	)
	joined := strings.Join([]string{discoverInput, discoverOutput, offerInput, offerOutput}, " ")
	for _, forbidden := range []string{"PRIVATE CUSTOMER REQUEST", "private.tag", "PRIVATE MODEL REASON", "PRIVATE_MODEL_ID", "controlled"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("private text leaked into audit summaries: %s", joined)
		}
	}
	if !strings.Contains(joined, "official:pptx") || !strings.Contains(joined, "task_capability_missing") || !strings.Contains(joined, "offered") {
		t.Fatalf("content-free audit fields missing: %s", joined)
	}
}

func TestSkillRecommendationStoreFailsClosedWhenPersistenceFails(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	s.path = t.TempDir() // rename-over-directory fails after the state mutation.
	if got, created, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}}); err == nil || created || got.RecommendationID != "" {
		t.Fatalf("offer persisted despite write failure: got=%+v created=%v err=%v", got, created, err)
	}
	if len(s.byID) != 0 {
		t.Fatalf("failed offer leaked into memory: %+v", s.byID)
	}
}

func TestSkillRecommendationAdmissionRequiresDesktopDeviceAndCapability(t *testing.T) {
	r := httptest.NewRequest("GET", "/events", nil)
	r.Header.Set(desktopDeviceHeader, "12345678-1234-1234-1234-123456789abc")
	r.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	device, caps := skillRecommendationAdmission(r, "desktop")
	if device == "" || !caps[CapSkillInstallRecommendationV1] {
		t.Fatal("valid Desktop admission rejected")
	}
	if device, caps := skillRecommendationAdmission(r, "kocoro"); device == "" || !caps[CapSkillInstallRecommendationV1] {
		t.Fatal("valid Kocoro Quick Panel admission rejected")
	}
	if device, _ := skillRecommendationAdmission(r, "slack"); device != "" {
		t.Fatal("non-Desktop source admitted")
	}
	r.Header.Del(skillRecommendationHeader)
	if device, _ := skillRecommendationAdmission(r, "desktop"); device != "" {
		t.Fatal("missing capability admitted")
	}
}

func TestSkillRecommendationToolsRequireAuthenticatedConsumerCapability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   string
		admitted bool
		liveSink bool
	}{
		{name: "old Desktop", source: "kocoro", admitted: false},
		{name: "capable signed-in Desktop without live sink", source: "kocoro", admitted: true},
		{name: "capable signed-in Desktop with live sink", source: "kocoro", admitted: true, liveSink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeGatewayBackend{reply: "ordinary reply"}
			upstream := httptest.NewServer(gw.handler())
			defer upstream.Close()
			deps := runAgentContractTestDeps(t, upstream.URL)
			defer deps.SessionCache.CloseAll()
			auth := NewAuthManager(AuthManagerConfig{ShannonDir: deps.ShannonDir})
			auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "opaque-account"}, "")
			deps.AuthManager = auth
			deps.SkillRecommendations = newSkillRecommendationStore(deps.ShannonDir)
			mgr := deps.SessionCache.GetOrCreate("")
			sess := mgr.NewSession()
			sess.Messages = []client.Message{
				{Role: "user", Content: client.NewTextContent("prior turn")},
				{Role: "assistant", Content: client.NewTextContent("prior answer")},
			}
			if err := mgr.Save(); err != nil {
				t.Fatal(err)
			}
			req := RunAgentRequest{Text: "hello", Source: tc.source, SessionID: sess.ID}
			if tc.admitted {
				req.DesktopDeviceID = "12345678-1234-1234-1234-123456789abc"
				req.ConsumerCapabilities = map[string]bool{CapSkillInstallRecommendationV1: true}
			}
			if tc.liveSink {
				req.SkillRecommendationEmit = func(skillRecommendationV1) bool { return true }
			}
			if _, err := RunAgent(context.Background(), deps, req, nullEventHandler{}); err != nil {
				t.Fatal(err)
			}
			requests := gw.requests()
			if len(requests) == 0 {
				t.Fatal("gateway received no completion request")
			}
			seen := map[string]bool{}
			for _, captured := range requests {
				for _, tool := range captured.Tools {
					name := tool.Function.Name
					if name == "" {
						name = tool.Name
					}
					seen[name] = true
				}
			}
			for _, name := range []string{"discover_installable_skills", "offer_skill_installation"} {
				if seen[name] != tc.admitted {
					t.Fatalf("tool %s present=%v admitted=%v all=%v", name, seen[name], tc.admitted, seen)
				}
			}
		})
	}
}

func TestDownloadableCatalogOrderPreservesReleasedDesktopContract(t *testing.T) {
	entries, err := skills.OfficialCatalog()
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, skills.CatalogEntry{Slug: "future-skill"})
	orderDownloadableCatalogEntries(entries)
	want := []string{
		"pdf-reader", "algorithmic-art", "brand-guidelines", "canvas-design",
		"claude-api", "doc-coauthoring", "frontend-design", "internal-comms",
		"mcp-builder", "skill-creator", "slack-gif-creator", "theme-factory",
		"web-artifacts-builder", "webapp-testing", "docx", "pdf", "pptx", "xlsx",
		"future-skill",
	}
	got := make([]string, len(entries))
	for i := range entries {
		got[i] = entries[i].Slug
	}
	if !slices.Equal(got, want) {
		t.Fatalf("downloadable order=%v, want %v", got, want)
	}
}

func TestSkillRecommendationKillSwitchDefaultsOn(t *testing.T) {
	if !skillRecommendationsEnabled(&config.Config{}) {
		t.Fatal("unset kill switch must preserve protocol")
	}
	off := false
	if skillRecommendationsEnabled(&config.Config{Daemon: config.DaemonConfig{SkillRecommendationsEnabled: &off}}) {
		t.Fatal("explicit kill switch did not disable protocol")
	}
}

func TestSkillRecommendationContinuationIsSingleFlightAndRetryableAfterFailure(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("first continuation run=%v err=%v", run, err)
	}
	if _, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken); err != nil || run {
		t.Fatalf("concurrent continuation run=%v err=%v", run, err)
	}
	if err := s.finishContinuation(accepted, "installation_failed"); err != nil {
		t.Fatal(err)
	}
	if _, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken); err != nil || !run {
		t.Fatalf("failed continuation was not retryable run=%v err=%v", run, err)
	}
}

func TestSkillRecommendationContinuationConcurrentRequestsRunOnce(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	var started atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
			if err == nil && run {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := started.Load(); got != 1 {
		t.Fatalf("continuations started=%d", got)
	}
}

func TestSkillRecommendationAccountInvalidationCancelsAcceptedContinuation(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	cancelled := make(chan struct{})
	if !s.registerContinuation(v.RecommendationID, accepted.Generation, func() { close(cancelled) }) {
		t.Fatal("could not register continuation")
	}
	if err := s.invalidateAccount("acct"); err != nil {
		t.Fatal(err)
	}
	if err := s.finishContinuation(accepted, "installation_failed"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("account switch did not cancel continuation")
	}
	s.mu.Lock()
	state := s.byID[v.RecommendationID].State
	s.mu.Unlock()
	if state != "expired" {
		t.Fatalf("continuation state=%s", state)
	}
}

func TestSkillRecommendationInvalidationWinsStaleFinishGoroutine(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	cancelled := make(chan struct{})
	if !s.registerContinuation(v.RecommendationID, accepted.Generation, func() { close(cancelled) }) {
		t.Fatal("could not register continuation")
	}
	invalidateDone := make(chan error, 1)
	go func() { invalidateDone <- s.invalidateAccount("acct") }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("invalidation did not cancel active generation")
	}
	finishDone := make(chan error, 1)
	go func() { finishDone <- s.finishContinuation(accepted, "completed") }()
	if err := <-invalidateDone; err != nil {
		t.Fatal(err)
	}
	if err := <-finishDone; err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	state := s.byID[v.RecommendationID].State
	s.mu.Unlock()
	if state != "expired" {
		t.Fatalf("stale finish overwrote invalidation: %s", state)
	}
}

func TestSkillRecommendationInvalidationPersistenceRollbackKeepsCancelOwnership(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	cancelled := atomic.Bool{}
	if !s.registerContinuation(v.RecommendationID, accepted.Generation, func() { cancelled.Store(true) }) {
		t.Fatal("could not register continuation")
	}
	s.path = t.TempDir() // rename-over-directory fails.
	if err := s.invalidateAccount("acct"); err == nil {
		t.Fatal("invalidation unexpectedly persisted")
	}
	s.mu.Lock()
	current := cloneSkillRecommendation(s.byID[v.RecommendationID])
	owner, owned := s.cancels[v.RecommendationID]
	s.mu.Unlock()
	if current.State != "accepted" || !current.ContinuationRunning || current.Generation != accepted.Generation {
		t.Fatalf("rollback state=%+v", current)
	}
	if !owned || owner.generation != accepted.Generation || cancelled.Load() {
		t.Fatalf("cancel ownership lost owned=%v generation=%d cancelled=%v", owned, owner.generation, cancelled.Load())
	}
}

func TestSkillRecommendationAccountSwitchFailsClosedWhenPersistenceBreaks(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(0, nil, &ServerDeps{Config: &config.Config{}, ShannonDir: dir}, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct-old"}, "")
	s.SetAuth(auth)
	v, _, err := s.skillRecommendations.offer("acct-old", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.skillRecommendations.beginContinuation("acct-old", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	cancelled := atomic.Bool{}
	if !s.skillRecommendations.registerContinuation(v.RecommendationID, accepted.Generation, func() { cancelled.Store(true) }) {
		t.Fatal("could not register continuation")
	}
	s.skillRecommendations.path = t.TempDir()
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct-new"}, "")
	s.skillRecommendations.mu.Lock()
	current := cloneSkillRecommendation(s.skillRecommendations.byID[v.RecommendationID])
	loadErr := s.skillRecommendations.loadErr
	_, owned := s.skillRecommendations.cancels[v.RecommendationID]
	s.skillRecommendations.mu.Unlock()
	if current.State != "expired" || current.ContinuationRunning || !cancelled.Load() || owned || loadErr == nil {
		t.Fatalf("account switch state=%+v cancelled=%v owned=%v loadErr=%v", current, cancelled.Load(), owned, loadErr)
	}
}

func TestSkillRecommendationFinishPersistenceFailureFailsClosed(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	v, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s", "t", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	cancelled := atomic.Bool{}
	if !s.registerContinuation(v.RecommendationID, accepted.Generation, func() { cancelled.Store(true) }) {
		t.Fatal("could not register continuation")
	}
	s.path = t.TempDir()
	if err := s.finishContinuation(accepted, "completed"); err == nil {
		t.Fatal("finish unexpectedly persisted")
	}
	s.mu.Lock()
	current := cloneSkillRecommendation(s.byID[v.RecommendationID])
	_, owned := s.cancels[v.RecommendationID]
	loadErr := s.loadErr
	s.mu.Unlock()
	if current.State != "expired" || current.ContinuationRunning || owned || !cancelled.Load() || loadErr == nil {
		t.Fatalf("fail-closed state=%+v owned=%v cancelled=%v loadErr=%v", current, owned, cancelled.Load(), loadErr)
	}
	if _, _, err := s.beginContinuation("acct", v.OwnerDeviceID, "s", v.RecommendationID, v.ContinuationToken); err == nil {
		t.Fatal("store accepted retry after uncertain durable finish")
	}
}

func TestSkillRecommendationKillSwitchInvalidatesEveryPendingState(t *testing.T) {
	s := newSkillRecommendationStore(t.TempDir())
	offered, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s1", "t1", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, _, err := s.offer("acct", "12345678-1234-1234-1234-123456789abc", "", "s2", "t2", "test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, run, err := s.beginContinuation("acct", accepted.OwnerDeviceID, "s2", accepted.RecommendationID, accepted.ContinuationToken); err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	if err := s.invalidateAll(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range []string{offered.RecommendationID, accepted.RecommendationID} {
		if got := s.byID[id].State; got != "expired" {
			t.Fatalf("%s state=%s", id, got)
		}
	}
}

func TestSkillRecommendationConfigReloadKillSwitchIsImmediate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	viper.Reset()
	t.Cleanup(viper.Reset)
	configDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("daemon:\n  skill_recommendations_enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldConfig, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[]}`))
	}))
	defer upstream.Close()
	shannonDir := t.TempDir()
	deps := &ServerDeps{
		Config: oldConfig, GW: client.NewGatewayClient(upstream.URL, "test-key"),
		Registry: agent.NewToolRegistry(), BaselineReg: agent.NewToolRegistry(),
		ShannonDir: shannonDir, AgentsDir: filepath.Join(shannonDir, "agents"),
		SessionCache: NewSessionCache(shannonDir), CatalogProvider: skills.NewEmbeddedCatalogProvider(shannonDir),
	}
	defer deps.SessionCache.CloseAll()
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: shannonDir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	s.SetAuth(auth)
	device := "12345678-1234-1234-1234-123456789abc"
	offered, _, err := s.skillRecommendations.offer("acct", device, "", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.skillRecommendations.beginContinuation("acct", device, offered.SessionID, offered.RecommendationID, offered.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	cancelled := atomic.Bool{}
	if !s.skillRecommendations.registerContinuation(accepted.RecommendationID, accepted.Generation, func() { cancelled.Store(true) }) {
		t.Fatal("could not register running continuation")
	}
	sinkClosed := atomic.Bool{}
	s.registerSkillRecommendationSink("acct", device, func(skillRecommendationV1) bool { return true }, func() { sinkClosed.Store(true) })
	// Force invalidateAll's atomic rename to fail. The recommendation protocol
	// must fail closed, but the rest of this reload must still commit.
	s.skillRecommendations.path = t.TempDir()
	reloadCalled := make(chan struct{}, 1)
	s.SetOnReload(func() { reloadCalled <- struct{}{} })

	if err := os.WriteFile(configPath, []byte("daemon:\n  skill_recommendations_enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	reload := httptest.NewRequest(http.MethodPost, "/config/reload", nil)
	reloadResponse := httptest.NewRecorder()
	s.Handler().ServeHTTP(reloadResponse, reload)
	if reloadResponse.Code != http.StatusOK {
		t.Fatalf("reload status=%d body=%s", reloadResponse.Code, reloadResponse.Body.String())
	}
	select {
	case <-reloadCalled:
	case <-time.After(time.Second):
		t.Fatal("reload callback was skipped after recommendation persistence failure")
	}
	reloadedConfig, _, _ := deps.Snapshot()
	if skillRecommendationsEnabled(reloadedConfig) {
		t.Fatal("config swap was skipped after recommendation persistence failure")
	}
	if s.skillRecommendationsEnabled() || !cancelled.Load() || !sinkClosed.Load() || s.hasSkillRecommendationSink("acct", device) {
		t.Fatalf("kill switch enabled=%v cancelled=%v sinkClosed=%v sinkPresent=%v", s.skillRecommendationsEnabled(), cancelled.Load(), sinkClosed.Load(), s.hasSkillRecommendationSink("acct", device))
	}
	s.skillRecommendations.mu.Lock()
	state := s.skillRecommendations.byID[offered.RecommendationID].State
	s.skillRecommendations.mu.Unlock()
	if state != "expired" {
		t.Fatalf("running recommendation state=%s", state)
	}

	continueBody, _ := json.Marshal(map[string]string{"session_id": offered.SessionID, "continuation_token": offered.ContinuationToken})
	continueRequest := httptest.NewRequest(http.MethodPost, "/skill-recommendations/"+offered.RecommendationID+"/continue", bytes.NewReader(continueBody))
	continueRequest.Header.Set("Content-Type", "application/json")
	continueRequest.Header.Set(desktopDeviceHeader, device)
	continueRequest.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	continueResponse := httptest.NewRecorder()
	s.Handler().ServeHTTP(continueResponse, continueRequest)
	if continueResponse.Code != http.StatusNotFound {
		t.Fatalf("disabled continue status=%d body=%s", continueResponse.Code, continueResponse.Body.String())
	}

	// The switch disables only recommendations; the ordinary manual Skill
	// install surface remains available.
	installRequest := httptest.NewRequest(http.MethodPost, "/skills/install/pdf-reader", nil)
	installResponse := httptest.NewRecorder()
	s.Handler().ServeHTTP(installResponse, installRequest)
	if installResponse.Code != http.StatusCreated {
		t.Fatalf("manual install status=%d body=%s", installResponse.Code, installResponse.Body.String())
	}
}

func TestSkillRecommendationOfferRejectedAfterSideEffect(t *testing.T) {
	dir := t.TempDir()
	store := newSkillRecommendationStore(dir)
	run := &skillRecommendationRunContext{accountID: "acct", deviceID: "12345678-1234-1234-1234-123456789abc", sessionID: "s", turnID: "t", store: store, emit: func(skillRecommendationV1) bool { return true }, discovered: map[string]bool{"official:pptx": true}}
	run.markSideEffect()
	ctx := withSkillRecommendationRun(context.Background(), run)
	result, err := (&offerSkillInstallationTool{shannonDir: dir}).Run(ctx, `{"catalog_ids":["official:pptx"],"reason":"presentation"}`)
	if err != nil || !result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSkillRecommendationSinkIsDeviceScopedAndReplacementSafe(t *testing.T) {
	s := &Server{skillRecommendationSinks: make(map[string]skillRecommendationSink)}
	deviceA := "12345678-1234-1234-1234-123456789abc"
	deviceB := "abcdefab-cdef-cdef-cdef-abcdefabcdef"
	var a, b int
	unregisterA := s.registerSkillRecommendationSink("acct", deviceA, func(skillRecommendationV1) bool { a++; return true }, func() {})
	defer unregisterA()
	unregisterB := s.registerSkillRecommendationSink("acct", deviceB, func(skillRecommendationV1) bool { b++; return true }, func() {})
	defer unregisterB()
	s.emitSkillRecommendation(skillRecommendationV1{State: "offered", OwnerAccountID: "acct", OwnerDeviceID: deviceA, ExpiresAt: time.Now().Add(time.Hour)})
	if a != 1 || b != 0 {
		t.Fatalf("device routing a=%d b=%d", a, b)
	}
	var replacement int
	unregisterReplacement := s.registerSkillRecommendationSink("acct", deviceA, func(skillRecommendationV1) bool { replacement++; return true }, func() {})
	defer unregisterReplacement()
	// The old deferred cleanup must not remove the newer connection.
	unregisterA()
	s.emitSkillRecommendation(skillRecommendationV1{State: "offered", OwnerAccountID: "acct", OwnerDeviceID: deviceA, ExpiresAt: time.Now().Add(time.Hour)})
	if replacement != 1 {
		t.Fatalf("replacement sink removed by stale cleanup: %d", replacement)
	}
}
