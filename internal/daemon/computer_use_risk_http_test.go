package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type consequentialRiskHTTPFixture struct {
	server       *Server
	broker       *ConsequentialRiskBroker
	intentID     string
	requestID    string
	targetDigest string
	intent       tools.ConsequentialRiskIntentV1
}

func newConsequentialRiskHTTPFixture(t *testing.T) *consequentialRiskHTTPFixture {
	t.Helper()
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(8))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_http_send"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(0, nil, nil, "test")
	server.SetConsequentialRiskBroker(broker)
	t.Setenv(localPresenceEnv, computerUseHTTPPresenceToken)
	return &consequentialRiskHTTPFixture{
		server: server, broker: broker, intentID: intent.IntentID,
		requestID: intent.RequestID, targetDigest: intent.Target.TargetDigest,
		intent: intent,
	}
}

func (fixture *consequentialRiskHTTPFixture) request(
	t *testing.T,
	method string,
	path string,
	body string,
	token string,
	remoteAddr string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set(localPresenceHeader, token)
	}
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestConsequentialRiskDetailRequiresAuthenticatedLoopbackAndNoStore(t *testing.T) {
	for _, test := range []struct {
		name, token, remoteAddr string
		wantStatus              int
	}{
		{name: "ipv4", token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusOK},
		{name: "ipv6", token: computerUseHTTPPresenceToken, remoteAddr: "[::1]:54321", wantStatus: http.StatusOK},
		{name: "missing token", remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusForbidden},
		{name: "wrong token", token: "wrong", remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusForbidden},
		{name: "nonloopback", token: computerUseHTTPPresenceToken, remoteAddr: "192.0.2.44:54321", wantStatus: http.StatusForbidden},
		{name: "malformed remote", token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConsequentialRiskHTTPFixture(t)
			rec := fixture.request(t, http.MethodGet,
				"/local/computer-use/risk-intents/"+fixture.intentID,
				"", test.token, test.remoteAddr)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.wantStatus, rec.Body.Bytes())
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
			}
			if test.wantStatus == http.StatusOK {
				if rec.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
				}
				if !bytes.Contains(rec.Body.Bytes(), []byte("#shipping-ops")) ||
					!bytes.Contains(rec.Body.Bytes(), []byte(fixture.targetDigest)) {
					t.Fatalf("detail is not authoritative: %s", rec.Body.Bytes())
				}
			}
		})
	}
}

func TestConsequentialRiskHTTPUsesInjectedAuthorizationCheckerAfterLoopbackGate(t *testing.T) {
	fixture := newConsequentialRiskHTTPFixture(t)
	called := 0
	fixture.server.SetConsequentialRiskHTTPAuthorizer(func(r *http.Request) bool {
		called++
		return r.Header.Get("X-Test-Desktop") == "recognized"
	})

	path := "/local/computer-use/risk-intents/" + fixture.intentID
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Test-Desktop", "recognized")
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || called != 1 {
		t.Fatalf("authorized status=%d checker calls=%d body=%s", rec.Code, called, rec.Body.Bytes())
	}

	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "192.0.2.9:54321"
	req.Header.Set("X-Test-Desktop", "recognized")
	rec = httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || called != 1 {
		t.Fatalf("nonloopback status=%d checker calls=%d", rec.Code, called)
	}
}

func TestConsequentialRiskHTTPMethodFallbackStillRequiresLocalDesktop(t *testing.T) {
	fixture := newConsequentialRiskHTTPFixture(t)
	path := "/local/computer-use/risk-intents/" + fixture.intentID
	unauthorized := fixture.request(t, http.MethodDelete, path, "", "", "127.0.0.1:54321")
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized fallback status = %d: %s", unauthorized.Code, unauthorized.Body.Bytes())
	}
	authorized := fixture.request(t, http.MethodDelete, path, "",
		computerUseHTTPPresenceToken, "127.0.0.1:54321")
	if authorized.Code != http.StatusMethodNotAllowed || authorized.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("authorized fallback status=%d allow=%q body=%s",
			authorized.Code, authorized.Header().Get("Allow"), authorized.Body.Bytes())
	}
}

func TestDefaultConsequentialRiskBrokerIsInertAndEmitsNothing(t *testing.T) {
	server := NewServer(0, nil, nil, "test")
	if server.consequentialRiskBroker == nil {
		t.Fatal("default broker is nil")
	}
	if removed := server.consequentialRiskBroker.InvalidateAll(); removed != 0 {
		t.Fatalf("default broker unexpectedly held %d intent/grant records", removed)
	}
	sub := server.eventBus.Subscribe()
	defer server.eventBus.Unsubscribe(sub)
	select {
	case event := <-sub:
		t.Fatalf("inert risk broker emitted event %+v", event)
	default:
	}
}

func TestConsequentialRiskDecisionAllowEchoesIntentAndMintsBoundGrant(t *testing.T) {
	fixture := newConsequentialRiskHTTPFixture(t)
	payload := `{"schema_version":1,"intent_id":"` + fixture.intentID + `","decision":"allow"}`
	rec := fixture.request(t, http.MethodPost,
		"/local/computer-use/risk-intents/"+fixture.intentID+"/decision",
		payload, computerUseHTTPPresenceToken, "127.0.0.1:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.Bytes())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	var response ConsequentialRiskDecisionResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != 1 || response.IntentID != fixture.intentID ||
		response.Decision != ConsequentialRiskDecisionAllowed || response.GrantExpiresAt == nil {
		t.Fatalf("response = %+v", response)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(fixture.targetDigest)) ||
		bytes.Contains(rec.Body.Bytes(), []byte("#shipping-ops")) {
		t.Fatalf("decision response leaked authority: %s", rec.Body.Bytes())
	}
	if err := fixture.broker.ConsumeGrant(consequentialRiskGrantClaim(fixture.intent)); err != nil {
		t.Fatalf("ConsumeGrant: %v", err)
	}
}

func TestConsequentialRiskDecisionDenyIsContentFreeAndInvalidates(t *testing.T) {
	fixture := newConsequentialRiskHTTPFixture(t)
	payload := `{"schema_version":1,"intent_id":"` + fixture.intentID + `","decision":"deny"}`
	rec := fixture.request(t, http.MethodPost,
		"/local/computer-use/risk-intents/"+fixture.intentID+"/decision",
		payload, computerUseHTTPPresenceToken, "127.0.0.1:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.Bytes())
	}
	var response ConsequentialRiskDecisionResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.IntentID != fixture.intentID || response.Decision != ConsequentialRiskDecisionDenied ||
		response.GrantExpiresAt != nil {
		t.Fatalf("response = %+v", response)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(fixture.targetDigest)) ||
		bytes.Contains(rec.Body.Bytes(), []byte("#shipping-ops")) {
		t.Fatalf("deny response leaked authority: %s", rec.Body.Bytes())
	}
	if _, err := fixture.broker.Detail(fixture.intentID); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("denied detail remained: %v", err)
	}
}

func TestConsequentialRiskDecisionFailsClosedBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name, pathID, payload, token, remoteAddr string
		wantStatus                               int
		wantCode                                 string
	}{
		{name: "nonloopback allow", payload: "allow", token: computerUseHTTPPresenceToken, remoteAddr: "192.0.2.8:54321", wantStatus: http.StatusForbidden},
		{name: "missing token", payload: "allow", remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusForbidden},
		{name: "path mismatch", pathID: "cri_EBESExQVFhcYGRobHB0eHw", payload: "allow", token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusConflict, wantCode: "computer_use_risk_intent_mismatch"},
		{name: "unknown field", payload: `{"schema_version":1,"intent_id":"$ID","decision":"allow","extra":true}`, token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusBadRequest, wantCode: "invalid_computer_use_risk_decision"},
		{name: "duplicate", payload: `{"schema_version":1,"intent_id":"$ID","intent_id":"$ID","decision":"allow"}`, token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusBadRequest, wantCode: "invalid_computer_use_risk_decision"},
		{name: "trailing", payload: `{"schema_version":1,"intent_id":"$ID","decision":"allow"}{}`, token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusBadRequest, wantCode: "invalid_computer_use_risk_decision"},
		{name: "invalid decision", payload: `{"schema_version":1,"intent_id":"$ID","decision":"always_allow"}`, token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusBadRequest, wantCode: "invalid_computer_use_risk_decision"},
		{name: "oversized", payload: strings.Repeat("x", maxConsequentialRiskDecisionBodyBytes+1), token: computerUseHTTPPresenceToken, remoteAddr: "127.0.0.1:54321", wantStatus: http.StatusBadRequest, wantCode: "invalid_computer_use_risk_decision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConsequentialRiskHTTPFixture(t)
			pathID := test.pathID
			if pathID == "" {
				pathID = fixture.intentID
			}
			payload := test.payload
			if payload == "allow" {
				payload = `{"schema_version":1,"intent_id":"` + fixture.intentID + `","decision":"allow"}`
			}
			payload = strings.ReplaceAll(payload, "$ID", fixture.intentID)
			rec := fixture.request(t, http.MethodPost,
				"/local/computer-use/risk-intents/"+pathID+"/decision",
				payload, test.token, test.remoteAddr)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.wantStatus, rec.Body.Bytes())
			}
			if test.wantCode != "" {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["code"] != test.wantCode {
					t.Fatalf("code = %q, want %q", body["code"], test.wantCode)
				}
			}
			// Every rejected attempt occurs before decision; the exact local Desktop
			// can still allow the same pending intent afterwards.
			allow := `{"schema_version":1,"intent_id":"` + fixture.intentID + `","decision":"allow"}`
			local := fixture.request(t, http.MethodPost,
				"/local/computer-use/risk-intents/"+fixture.intentID+"/decision",
				allow, computerUseHTTPPresenceToken, "127.0.0.1:54321")
			if local.Code != http.StatusOK {
				t.Fatalf("rejected request changed pending intent: %d %s", local.Code, local.Body.Bytes())
			}
		})
	}
}

func TestConsequentialRiskDetailUnavailableAndBrokerUnavailableAreContentFree(t *testing.T) {
	fixture := newConsequentialRiskHTTPFixture(t)
	unknown := fixture.request(t, http.MethodGet,
		"/local/computer-use/risk-intents/cri_EBESExQVFhcYGRobHB0eHw", "",
		computerUseHTTPPresenceToken, "127.0.0.1:54321")
	if unknown.Code != http.StatusGone || bytes.Contains(unknown.Body.Bytes(), []byte("#shipping-ops")) ||
		bytes.Contains(unknown.Body.Bytes(), []byte(fixture.targetDigest)) {
		t.Fatalf("unknown response = %d %s", unknown.Code, unknown.Body.Bytes())
	}
	fixture.server.SetConsequentialRiskBroker(nil)
	unavailable := fixture.request(t, http.MethodGet,
		"/local/computer-use/risk-intents/"+fixture.intentID, "",
		computerUseHTTPPresenceToken, "127.0.0.1:54321")
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d: %s", unavailable.Code, unavailable.Body.Bytes())
	}
}

func TestConsequentialRiskGrantExpiryNeverExceedsTenSeconds(t *testing.T) {
	fixture := newConsequentialRiskHTTPFixture(t)
	payload := `{"schema_version":1,"intent_id":"` + fixture.intentID + `","decision":"allow"}`
	rec := fixture.request(t, http.MethodPost,
		"/local/computer-use/risk-intents/"+fixture.intentID+"/decision",
		payload, computerUseHTTPPresenceToken, "127.0.0.1:54321")
	var response ConsequentialRiskDecisionResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.GrantExpiresAt == nil {
		t.Fatal("missing grant expiry")
	}
	expiresAt, err := time.Parse(time.RFC3339, *response.GrantExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if delta := expiresAt.Sub(consequentialRiskBrokerFixtureNow); delta > 10*time.Second {
		t.Fatalf("grant TTL = %s", delta)
	}
}
