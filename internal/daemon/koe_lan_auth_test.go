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
			host, token := koeLANBind(tc.cfg)
			if host != "localhost" || token != "" {
				t.Fatalf("koeLANBind = (%q,%q), want (localhost, \"\")", host, token)
			}
		})
	}
}

func TestKoeLANBind_AllInterfacesWhenBindAndToken(t *testing.T) {
	host, token := koeLANBind(&config.Config{Koe: config.KoeConfig{LANBind: true, LANToken: "s3cr3t"}})
	if host != "" || token != "s3cr3t" {
		t.Fatalf("koeLANBind = (%q,%q), want (\"\", s3cr3t)", host, token)
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
	withKoeLANAuth(token, inner).ServeHTTP(rec, req)
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
