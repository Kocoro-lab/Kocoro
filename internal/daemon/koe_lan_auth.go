package daemon

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// koeLANBind resolves the daemon's HTTP listen host and every bearer token that
// may reach it from the LAN. Binding ("" = all interfaces) requires BOTH
// Koe.LANBind AND at least one token, so a mis-set flag alone cannot open an
// unauthenticated hole — with no tokens the daemon stays loopback-only
// ("localhost"), unchanged.
//
// Tokens come from Koe.LANTokens, one per paired robot: a Mac pairs several
// robots, and each must authenticate on its own so that pairing one cannot
// revoke another. Koe.LANToken is the pre-map form and is still honoured, so a
// robot paired before that field existed keeps working without re-pairing.
func koeLANBind(cfg *config.Config) (bindHost string, tokens []string) {
	if cfg == nil || !cfg.Koe.LANBind {
		return "localhost", nil
	}
	for _, token := range cfg.Koe.LANTokens {
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	if cfg.Koe.LANToken != "" {
		tokens = append(tokens, cfg.Koe.LANToken)
	}
	if len(tokens) == 0 {
		return "localhost", nil
	}
	return "", tokens
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
func withKoeLANAuth(tokens []string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || isLoopbackRemoteAddr(r.RemoteAddr) {
			h.ServeHTTP(w, r)
			return
		}
		if !bearerTokenMatchesAny(r.Header.Get("Authorization"), tokens) {
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

// bearerTokenMatchesAny constant-time compares the `Authorization: Bearer <token>`
// header value against every token a paired robot may hold. An empty set never
// matches (fail-closed).
//
// Every candidate is compared even after a hit: returning early would make the
// time taken depend on which robot matched, leaking the map's ordering.
func bearerTokenMatchesAny(header string, tokens []string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := []byte(header[len(prefix):])
	matched := 0
	for _, token := range tokens {
		if token == "" {
			continue
		}
		matched |= subtle.ConstantTimeCompare(got, []byte(token))
	}
	return matched == 1
}
