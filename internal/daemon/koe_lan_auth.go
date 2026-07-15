package daemon

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// koeLANBind resolves the daemon's HTTP listen host and the bearer token from the
// opt-in LAN-exposure config. LAN binding ("" = all interfaces) requires BOTH
// Koe.LANBind AND a non-empty Koe.LANToken, so a mis-set flag alone cannot open
// an unauthenticated hole — without a token the daemon stays loopback-only
// ("localhost"), unchanged.
func koeLANBind(cfg *config.Config) (bindHost, token string) {
	if cfg != nil && cfg.Koe.LANBind && cfg.Koe.LANToken != "" {
		return "", cfg.Koe.LANToken
	}
	return "localhost", ""
}

// withKoeLANAuth gates the daemon's HTTP surface when it is exposed on the LAN
// for a remote Koe front-brain (Wireless / W-中: Koe runs on the robot's CM4 and
// reaches the Mac back-brain daemon over the LAN for mint / do_task).
//
// Threat model (server.go): the daemon is localhost-only and local-trusted by
// default. The moment it binds a non-loopback interface it MUST authenticate, or
// anyone on the WiFi can mint OpenAI tokens and delegate tasks (§14 hole ②).
//
//   - Loopback callers (Desktop, a co-located Koe on Lite) stay local-trusted and
//     pass unauthenticated — the localhost path is unchanged.
//   - Non-loopback callers MUST present `Authorization: Bearer <token>`, compared
//     in constant time. Otherwise 401.
//   - Fail-closed: an empty token rejects EVERY non-loopback request, so enabling
//     the LAN listener without provisioning a token cannot silently open a hole.
//   - CORS preflight (OPTIONS) is exempt so browser origins can still probe.
func withKoeLANAuth(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || isLoopbackRemoteAddr(r.RemoteAddr) {
			h.ServeHTTP(w, r)
			return
		}
		if !bearerTokenMatches(r.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// isLoopbackRemoteAddr reports whether an `http.Request.RemoteAddr` ("host:port")
// is a loopback address. A malformed or non-IP host is treated as non-loopback
// (fail-closed toward requiring auth).
func isLoopbackRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerTokenMatches constant-time compares the `Authorization: Bearer <token>`
// header value against the configured token. An empty configured token never
// matches (fail-closed).
func bearerTokenMatches(header, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
