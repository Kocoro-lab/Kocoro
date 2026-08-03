package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestAuthManagerVerifiedPrincipalEpoch(t *testing.T) {
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: t.TempDir()})
	if _, _, ok := auth.VerifiedPrincipal(); ok {
		t.Fatal("signed_out reported a verified principal")
	}
	// Bootstrap's offline-optimistic signed_in has no account ID and must not
	// count as a verified principal (mirrors VerifiedAccountID).
	auth.setState(AuthStateSignedIn, nil, "")
	if _, _, ok := auth.VerifiedPrincipal(); ok {
		t.Fatal("optimistic signed_in without account id reported verified")
	}

	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	account, first, ok := auth.VerifiedPrincipal()
	if !ok || account != "acct" {
		t.Fatalf("verified sign-in account=%q ok=%v", account, ok)
	}

	// A state-stable refresh (same account, same state) keeps the epoch.
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	if _, same, _ := auth.VerifiedPrincipal(); same != first {
		t.Fatalf("same-principal refresh moved epoch %d -> %d", first, same)
	}

	auth.setState(AuthStateSignedOut, nil, "")
	if _, _, ok := auth.VerifiedPrincipal(); ok {
		t.Fatal("signed_out kept a verified principal")
	}

	// Re-login of the SAME account is a new sign-in session: the epoch must
	// differ so pre-sign-out work cannot inherit the new session's transport.
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	if _, second, _ := auth.VerifiedPrincipal(); second == first {
		t.Fatalf("same-account relogin kept epoch %d", second)
	}
}

func TestSkillRecommendationEmitterFollowsReplacementSinkWithinSameSignIn(t *testing.T) {
	s := &Server{skillRecommendationSinks: make(map[string]skillRecommendationSink)}
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: t.TempDir()})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	s.auth = auth
	device := "12345678-1234-1234-1234-123456789abc"

	oldCalls, newCalls := 0, 0
	unregisterOld := s.registerSkillRecommendationSink("acct", device, func(skillRecommendationV1) bool { oldCalls++; return true }, func() {})
	defer unregisterOld()
	_, epoch, ok := auth.VerifiedPrincipal()
	if !ok {
		t.Fatal("no verified principal")
	}
	emit := s.skillRecommendationEmitterAt("acct", device, epoch)

	// Same sign-in, transport reconnected: the card follows the live sink.
	unregisterNew := s.registerSkillRecommendationSink("acct", device, func(skillRecommendationV1) bool { newCalls++; return true }, func() {})
	defer unregisterNew()
	v := skillRecommendationV1{State: "offered", OwnerAccountID: "acct", OwnerDeviceID: device, ExpiresAt: time.Now().Add(time.Hour)}
	if !emit(v) {
		t.Fatal("same-sign-in replacement connection did not receive the card")
	}
	if oldCalls != 0 || newCalls != 1 {
		t.Fatalf("replacement routing old=%d new=%d", oldCalls, newCalls)
	}
}

func TestSkillRecommendationEmitterFailsClosedAcrossPrincipalTransitions(t *testing.T) {
	s := &Server{skillRecommendationSinks: make(map[string]skillRecommendationSink)}
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: t.TempDir()})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	s.auth = auth
	device := "12345678-1234-1234-1234-123456789abc"
	_, epoch, _ := auth.VerifiedPrincipal()
	emit := s.skillRecommendationEmitterAt("acct", device, epoch)
	v := skillRecommendationV1{State: "offered", OwnerAccountID: "acct", OwnerDeviceID: device, ExpiresAt: time.Now().Add(time.Hour)}

	// Signed out: no delivery even with a (stale) sink still registered.
	calls := 0
	unregister := s.registerSkillRecommendationSink("acct", device, func(skillRecommendationV1) bool { calls++; return true }, func() {})
	defer unregister()
	auth.setState(AuthStateSignedOut, nil, "")
	if emit(v) {
		t.Fatal("signed-out emitter delivered a card")
	}

	// Same-account relogin: new epoch, pre-transition emitter stays dead.
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	if emit(v) {
		t.Fatal("pre-sign-out emitter delivered into the new sign-in session")
	}
	if calls != 0 {
		t.Fatalf("stale emitter reached a sink %d times", calls)
	}

	// A fresh emitter minted for the new sign-in works.
	_, epoch2, ok := auth.VerifiedPrincipal()
	if !ok {
		t.Fatal("relogin has no verified principal")
	}
	if !s.skillRecommendationEmitterAt("acct", device, epoch2)(v) {
		t.Fatal("current-epoch emitter failed to deliver")
	}
	if calls != 1 {
		t.Fatalf("current-epoch emitter delivered %d times", calls)
	}
}

func TestMessageAdmissionWithNilAuthDoesNotPanic(t *testing.T) {
	// SetAuth(nil) is a supported configuration (platforms without a
	// credential store). The recommendation admission block in handleMessage
	// must degrade to "not verified" via VerifiedPrincipal's nil-receiver
	// guard — a panic here would take down every capable Desktop request.
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir, SessionCache: NewSessionCache(t.TempDir())}
	s := NewServer(0, nil, deps, "test")
	s.SetAuth(nil)

	body := bytes.NewReader([]byte(`{"text":"hi","agent":"no-such-agent-xyz","source":"kocoro"}`))
	req := httptest.NewRequest(http.MethodPost, "/message", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(desktopDeviceHeader, "12345678-1234-1234-1234-123456789abc")
	req.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	rec := httptest.NewRecorder()
	s.handleMessage(rec, req) // a panic propagates and fails the test
	if rec.Code == 0 {
		t.Fatal("handler wrote no response")
	}
}

func startRecommendationEventsStream(t *testing.T, s *Server, deviceID string) *http.Response {
	t.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	t.Cleanup(httpServer.Close)
	request, err := http.NewRequest(http.MethodGet, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(desktopDeviceHeader, deviceID)
	request.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func waitForSinkState(t *testing.T, s *Server, account, device string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.hasSkillRecommendationSink(account, device) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.hasSkillRecommendationSink(account, device) != want {
		t.Fatalf("sink(%s) live=%v, want %v", account, !want, want)
	}
}

func TestSkillRecommendationEventsSinkBindsAfterLateSignIn(t *testing.T) {
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"

	// A card offered before this connection exists must replay once the sink
	// binds (same contract as the connect-time replay).
	v, _, err := s.skillRecommendations.offer("late-account", deviceID, "researcher", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx", Source: "official", DisplayName: "Presentation", CapabilitySummary: "Create presentations"}})
	if err != nil {
		t.Fatal(err)
	}

	response := startRecommendationEventsStream(t, s, deviceID)

	// Signed out at connect time: stream lives, no sink.
	time.Sleep(100 * time.Millisecond)
	if s.hasSkillRecommendationSink("late-account", deviceID) {
		t.Fatal("sink registered without a verified principal")
	}

	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "late-account"}, "")
	waitForSinkState(t, s, "late-account", deviceID, true)

	scanner := bufio.NewScanner(response.Body)
	sawEvent := false
	var payload []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.Equal(line, []byte("event: skill.recommendation.v1")) {
			sawEvent = true
			continue
		}
		if sawEvent && bytes.HasPrefix(line, []byte("data: ")) {
			payload = append([]byte(nil), line[len("data: "):]...)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		RecommendationID string `json:"recommendation_id"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode replayed card: %v (payload=%s)", err, payload)
	}
	if decoded.RecommendationID != v.RecommendationID {
		t.Fatalf("replayed card id=%q want %q", decoded.RecommendationID, v.RecommendationID)
	}
}

func TestSkillRecommendationEventsSinkRebindsAcrossPrincipalChange(t *testing.T) {
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")

	startRecommendationEventsStream(t, s, deviceID)
	waitForSinkState(t, s, "account-a", deviceID, true)

	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-b"}, "")
	waitForSinkState(t, s, "account-b", deviceID, true)
	if s.hasSkillRecommendationSink("account-a", deviceID) {
		t.Fatal("old principal's sink survived the account switch")
	}
}

func TestLoggingInTransitionInvalidatesOffersDespiteRetainedUser(t *testing.T) {
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	if _, _, err := s.skillRecommendations.offer("acct", deviceID, "researcher", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx", Source: "official", DisplayName: "P", CapabilitySummary: "c"}}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.skillRecommendations.offeredFor("acct", deviceID)); got != 1 {
		t.Fatalf("seed offers = %d, want 1", got)
	}

	// Direct re-login WITHOUT sign-out: signed_in(A) → logging_in RETAINS
	// a.user (still A) but the verified principal is gone. Cleanup keys on
	// the verified-principal transition, so outstanding offers die here —
	// otherwise the same-account re-login below would replay cards from the
	// previous sign-in session, which the epoch forbids on the emit path.
	auth.setState(AuthStateLoggingIn, nil, "")
	if got := len(s.skillRecommendations.offeredFor("acct", deviceID)); got != 0 {
		t.Fatalf("offers survived verified-principal loss: %d", got)
	}

	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	if got := len(s.skillRecommendations.offeredFor("acct", deviceID)); got != 0 {
		t.Fatalf("offers resurrected after same-account relogin: %d", got)
	}
}

func TestSkillRecommendationEventsSinkKillSwitchLifecycle(t *testing.T) {
	dir := t.TempDir()
	off, on := false, true
	disabledCfg := &config.Config{}
	disabledCfg.Daemon.SkillRecommendationsEnabled = &off
	enabledCfg := &config.Config{}
	enabledCfg.Daemon.SkillRecommendationsEnabled = &on
	deps := &ServerDeps{Config: disabledCfg, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")

	startRecommendationEventsStream(t, s, deviceID)
	time.Sleep(100 * time.Millisecond)
	if s.hasSkillRecommendationSink("acct", deviceID) {
		t.Fatal("sink registered while the kill switch is off")
	}

	swapConfig := func(cfg *config.Config) {
		deps.mu.Lock()
		deps.Config = cfg
		deps.mu.Unlock()
		// Mirror handleConfigReload's kill-switch resync (server.go): the
		// atomic is the fast-path gate in skillRecommendationsEnabled().
		s.skillRecommendationsOff.Store(!skillRecommendationsEnabled(cfg))
	}
	// The bind consults the kill switch LIVE (frozen admission would strand
	// this connection). The synthetic auth event stands in for the 30s
	// keepalive-ticker backstop that triggers the re-check in production.
	swapConfig(enabledCfg)
	s.eventBus.Emit(Event{Type: EventAuthStateChanged, Payload: []byte("{}")})
	waitForSinkState(t, s, "acct", deviceID, true)

	swapConfig(disabledCfg)
	s.eventBus.Emit(Event{Type: EventAuthStateChanged, Payload: []byte("{}")})
	waitForSinkState(t, s, "acct", deviceID, false)
}

func TestSkillRecommendationSinkRecoversFromRapidKillSwitchFlip(t *testing.T) {
	dir := t.TempDir()
	off, on := false, true
	disabledCfg := &config.Config{}
	disabledCfg.Daemon.SkillRecommendationsEnabled = &off
	enabledCfg := &config.Config{}
	enabledCfg.Daemon.SkillRecommendationsEnabled = &on
	deps := &ServerDeps{Config: enabledCfg, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")

	startRecommendationEventsStream(t, s, deviceID)
	waitForSinkState(t, s, "acct", deviceID, true)

	swapConfig := func(cfg *config.Config) {
		deps.mu.Lock()
		deps.Config = cfg
		deps.mu.Unlock()
		s.skillRecommendationsOff.Store(!skillRecommendationsEnabled(cfg))
	}
	// off→on entirely INSIDE one ticker period: the reload cleanup wipes the
	// sink map, but this handler never observes the off state — its local
	// bound account/epoch still match on the next bind trigger. Without the
	// sink-liveness check in the early return, the connection would stay
	// unbound forever (until an auth transition or a reconnect).
	swapConfig(disabledCfg)
	s.closeSkillRecommendationSinks()
	swapConfig(enabledCfg)
	if s.hasSkillRecommendationSink("acct", deviceID) {
		t.Fatal("sink survived the kill-switch cleanup")
	}
	// Exactly ONE bind trigger after the flip (stands in for the next tick).
	s.eventBus.Emit(Event{Type: EventAuthStateChanged, Payload: []byte("{}")})
	waitForSinkState(t, s, "acct", deviceID, true)
}

func recommendationCardStream(t *testing.T, resp *http.Response) <-chan string {
	t.Helper()
	ch := make(chan string, 4)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		current := ""
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				current = strings.TrimPrefix(line, "event: ")
				continue
			}
			if strings.HasPrefix(line, "data: ") && current == "skill.recommendation.v1" {
				ch <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return ch
}

func TestSkillRecommendationSinkNoStealBackAfterReplacement(t *testing.T) {
	// Last-one-wins: when a NEWER connection for the same account+device owns
	// the sink, the replaced connection's rebind checks (auth events, ticker)
	// must NOT steal it back — cards keep flowing to the newest connection.
	// This is the one branch of the bind state machine that was previously
	// asserted only in a comment.
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")

	conn1 := startRecommendationEventsStream(t, s, deviceID)
	waitForSinkState(t, s, "acct", deviceID, true)
	cards1 := recommendationCardStream(t, conn1)

	conn2 := startRecommendationEventsStream(t, s, deviceID)
	time.Sleep(150 * time.Millisecond) // conn2's connect-time bind replaces conn1's sink
	cards2 := recommendationCardStream(t, conn2)

	// Poke every connection's rebind logic; conn1 must observe the live sink
	// and stay quiet instead of re-registering over conn2.
	s.eventBus.Emit(Event{Type: EventAuthStateChanged, Payload: []byte("{}")})
	time.Sleep(150 * time.Millisecond)

	v, _, err := s.skillRecommendations.offer("acct", deviceID, "researcher", "session", "turn", "sha256:test", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx", Source: "official", DisplayName: "P", CapabilitySummary: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.emitSkillRecommendation(v) {
		t.Fatal("card delivery failed entirely")
	}
	select {
	case payload := <-cards2:
		if !strings.Contains(payload, v.RecommendationID) {
			t.Fatalf("unexpected card on newest connection: %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("card did not reach the newest connection")
	}
	select {
	case payload := <-cards1:
		t.Fatalf("replaced connection received the card (steal-back): %s", payload)
	case <-time.After(300 * time.Millisecond):
		// quiet — last-one-wins held
	}
}

func TestSkillRecommendationEventsSinkRebindsAfterSameAccountRelogin(t *testing.T) {
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.SetEventBus(s.eventBus)
	s.SetAuth(auth)
	deviceID := "12345678-1234-1234-1234-123456789abc"
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")

	startRecommendationEventsStream(t, s, deviceID)
	waitForSinkState(t, s, "acct", deviceID, true)

	// Sign-out tears the sink down (principal-change cleanup)…
	auth.setState(AuthStateSignedOut, nil, "")
	waitForSinkState(t, s, "acct", deviceID, false)

	// …and a relogin of the SAME account on the SAME connection binds a fresh
	// sink. Account-only tracking would skip this; the bind must key on epoch.
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "acct"}, "")
	waitForSinkState(t, s, "acct", deviceID, true)
}
