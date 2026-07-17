package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestKoeLANBind_LoopbackByDefault(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{"nil config", nil},
		{"lan_bind off", &config.Config{Koe: config.KoeConfig{LANToken: "x"}}},
		{"lan_bind on but no token (fail-closed)", &config.Config{Koe: config.KoeConfig{LANBind: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, tokens := koeLANBind(tc.cfg)
			if host != "localhost" || len(tokens) != 0 {
				t.Fatalf("koeLANBind = (%q,%v), want (localhost, no tokens)", host, tokens)
			}
		})
	}
}

func TestKoeLANBind_AllInterfacesWhenBindAndToken(t *testing.T) {
	host, tokens := koeLANBind(&config.Config{Koe: config.KoeConfig{LANBind: true, LANToken: "s3cr3t"}})
	if host != "" || len(tokens) != 1 || tokens[0] != "s3cr3t" {
		t.Fatalf("koeLANBind = (%q,%v), want (\"\", [s3cr3t])", host, tokens)
	}
}

func lanAuthProbe(t *testing.T, token, remoteAddr, method, authHeader string) int {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel: reached the inner handler
	})
	req := httptest.NewRequest(method, "/koe/realtime/mint", nil)
	req.RemoteAddr = remoteAddr
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	withKoeLANAuth([]string{token}, inner).ServeHTTP(rec, req)
	return rec.Code
}

func TestKoeLANAuth_LoopbackPassesWithoutToken(t *testing.T) {
	// Desktop / a local Koe on 127.0.0.1 stay local-trusted (backward compat).
	if got := lanAuthProbe(t, "secret", "127.0.0.1:51000", "POST", ""); got != http.StatusTeapot {
		t.Fatalf("loopback should pass to inner handler, got %d", got)
	}
	if got := lanAuthProbe(t, "secret", "[::1]:51000", "POST", ""); got != http.StatusTeapot {
		t.Fatalf("IPv6 loopback should pass, got %d", got)
	}
}

func TestKoeLANAuth_NonLoopbackRequiresBearer(t *testing.T) {
	if got := lanAuthProbe(t, "secret", "192.168.1.50:51000", "POST", ""); got != http.StatusUnauthorized {
		t.Fatalf("LAN without bearer must 401, got %d", got)
	}
	if got := lanAuthProbe(t, "secret", "192.168.1.50:51000", "POST", "Bearer wrong"); got != http.StatusUnauthorized {
		t.Fatalf("LAN with wrong bearer must 401, got %d", got)
	}
}

func TestKoeLANAuth_NonLoopbackWithCorrectBearerPasses(t *testing.T) {
	if got := lanAuthProbe(t, "secret", "192.168.1.50:51000", "POST", "Bearer secret"); got != http.StatusTeapot {
		t.Fatalf("LAN with correct bearer should pass, got %d", got)
	}
}

func TestKoeLANAuth_FailClosedOnEmptyToken(t *testing.T) {
	// An empty token must reject every non-loopback caller — enabling the LAN
	// listener without provisioning a token cannot silently open a hole.
	if got := lanAuthProbe(t, "", "192.168.1.50:51000", "POST", "Bearer anything"); got != http.StatusUnauthorized {
		t.Fatalf("empty token must fail-closed for LAN, got %d", got)
	}
	// Loopback still passes even with an empty token (local-trusted).
	if got := lanAuthProbe(t, "", "127.0.0.1:51000", "POST", ""); got != http.StatusTeapot {
		t.Fatalf("loopback should pass with empty token, got %d", got)
	}
}

func TestKoeLANAuth_PreflightOptionsPasses(t *testing.T) {
	// CORS preflight from a non-loopback origin must not be blocked by auth.
	if got := lanAuthProbe(t, "secret", "192.168.1.50:51000", "OPTIONS", ""); got != http.StatusTeapot {
		t.Fatalf("OPTIONS preflight should pass, got %d", got)
	}
}

// --- per-robot LAN tokens ---

func TestKoeLANBind_CollectsEveryPairedToken(t *testing.T) {
	cfg := &config.Config{Koe: config.KoeConfig{
		LANBind: true,
		LANTokens: map[string]string{
			"50531443cbaa7e08": "token-robot-a-0123456789",
			"be92ee93cacbff5f": "token-robot-b-0123456789",
		},
	}}
	host, tokens := koeLANBind(cfg)
	if host != "" {
		t.Fatalf("want LAN bind, got host %q", host)
	}
	if len(tokens) != 2 {
		t.Fatalf("want both robots' tokens, got %d", len(tokens))
	}
}

func TestKoeLANBind_StillHonoursTheLegacySingleToken(t *testing.T) {
	// A robot paired before lan_tokens existed must keep working untouched.
	cfg := &config.Config{Koe: config.KoeConfig{
		LANBind:  true,
		LANToken: "legacy-single-token-0123456789",
	}}
	host, tokens := koeLANBind(cfg)
	if host != "" || len(tokens) != 1 {
		t.Fatalf("legacy token must still bind LAN: host=%q tokens=%d", host, len(tokens))
	}
}

func TestKoeLANBind_EmptyTokensInTheMapDoNotOpenTheListener(t *testing.T) {
	// Fail-closed: a map of blanks is no tokens at all.
	cfg := &config.Config{Koe: config.KoeConfig{
		LANBind:   true,
		LANTokens: map[string]string{"50531443cbaa7e08": ""},
	}}
	host, tokens := koeLANBind(cfg)
	if host != "localhost" || len(tokens) != 0 {
		t.Fatalf("want loopback-only, got host=%q tokens=%d", host, len(tokens))
	}
}

func TestKoeLANAuth_EachPairedRobotAuthenticatesIndependently(t *testing.T) {
	tokens := []string{"token-robot-a-0123456789", "token-robot-b-0123456789"}
	handler := withKoeLANAuth(tokens, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, token := range tokens {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.RemoteAddr = "192.168.1.9:5000"
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("token %q rejected: %d", token, rec.Code)
		}
	}
}

func TestKoeLANAuth_AnUnknownTokenIsStillRejected(t *testing.T) {
	handler := withKoeLANAuth([]string{"token-robot-a-0123456789"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "192.168.1.9:5000"
	req.Header.Set("Authorization", "Bearer token-of-an-unpaired-robot")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for an unknown token, got %d", rec.Code)
	}
}

func TestKoeLANAuth_AnEmptyTokenSetRejectsEveryRemoteCaller(t *testing.T) {
	handler := withKoeLANAuth(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "192.168.1.9:5000"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want fail-closed 401, got %d", rec.Code)
	}
}

func TestKoeLANAuth_LoopbackStaysExempt(t *testing.T) {
	handler := withKoeLANAuth([]string{"token-robot-a-0123456789"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback must stay local-trusted, got %d", rec.Code)
	}
}
