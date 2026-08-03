package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newConfigPatchTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	srv := NewServer(0, nil, &ServerDeps{ShannonDir: t.TempDir()}, "test")
	return srv, func() {}
}

func doConfigPatch(t *testing.T, srv *Server, patch map[string]interface{}) (int, map[string]string) {
	t.Helper()
	data, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/config", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	srv.handlePatchConfig(rec, req)
	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	return rec.Code, body
}

func TestFindUnknownConfigField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch map[string]interface{}
		want  string // "" = no unknown field
	}{
		// The production incident: daemon.endpoint silently merged into yaml
		// while the daemon kept using the default endpoint.
		{name: "daemon.endpoint is unknown", patch: map[string]interface{}{"daemon": map[string]interface{}{"endpoint": "https://apiv1.kocoro.ai"}}, want: "daemon.endpoint"},
		{name: "top-level typo", patch: map[string]interface{}{"endpont": "x"}, want: "endpont"},
		{name: "nested typo", patch: map[string]interface{}{"agent": map[string]interface{}{"modle": "x"}}, want: "agent.modle"},

		// Legitimate struct fields.
		{name: "daemon.auto_approve", patch: map[string]interface{}{"daemon": map[string]interface{}{"auto_approve": true}}, want: ""},
		{name: "top-level endpoint", patch: map[string]interface{}{"endpoint": "https://apiv1.kocoro.ai"}, want: ""},
		{name: "agent.model", patch: map[string]interface{}{"agent": map[string]interface{}{"model": "claude-opus-4-8"}}, want: ""},
		{name: "koe nested", patch: map[string]interface{}{"koe": map[string]interface{}{"barge_in": true}}, want: ""},

		// Map KEY names are user-defined (server names, env vars) — never
		// "unknown". Map VALUES still validate against the element struct, so
		// a typo'd server field is caught instead of silently ignored.
		{name: "mcp server subtree", patch: map[string]interface{}{"mcp_servers": map[string]interface{}{"my-server": map[string]interface{}{"command": "npx", "env": map[string]interface{}{"MY_TOKEN": "x"}}}}, want: ""},
		{name: "mcp server field typo", patch: map[string]interface{}{"mcp_servers": map[string]interface{}{"my-server": map[string]interface{}{"commad": "npx"}}}, want: "mcp_servers.my-server.commad"},

		// Viper-only keys with no struct field remain patchable.
		{name: "marketplace viper-only", patch: map[string]interface{}{"skills": map[string]interface{}{"marketplace": map[string]interface{}{"max_attempts": 5}}}, want: ""},
		{name: "daemon scratch age viper-only", patch: map[string]interface{}{"daemon": map[string]interface{}{"scratch_max_age_days": 7}}, want: ""},
		{name: "sync viper-only key", patch: map[string]interface{}{"sync": map[string]interface{}{"enabled": true}}, want: ""},
		{name: "sync unknown key", patch: map[string]interface{}{"sync": map[string]interface{}{"bogus": 1}}, want: "sync.bogus"},

		// gateway_url is caught by the protected-field wall in the handler
		// (before this validator); at this layer it is simply unknown.
		{name: "legacy gateway_url", patch: map[string]interface{}{"gateway_url": "https://example.com"}, want: "gateway_url"},

		// Setting a whole section to a non-map value is a shape problem, not
		// an unknown key — key validation must not reject it.
		{name: "scalar over struct section", patch: map[string]interface{}{"daemon": "oops"}, want: ""},

		// null deletes a key; deleting an UNKNOWN key is the cleanup path for
		// stray yaml (removing a misplaced daemon.endpoint) and must pass.
		{name: "null deletes unknown key", patch: map[string]interface{}{"to_delete": nil}, want: ""},
		{name: "null deletes stray nested key", patch: map[string]interface{}{"daemon": map[string]interface{}{"endpoint": nil}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := findUnknownConfigField(tc.patch)
			if got != tc.want {
				t.Fatalf("findUnknownConfigField = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPatchConfigRejectsUnknownFieldWithStructuredCode(t *testing.T) {
	srv, cleanup := newConfigPatchTestServer(t)
	defer cleanup()

	code, body := doConfigPatch(t, srv, map[string]interface{}{"daemon": map[string]interface{}{"endpoint": "https://apiv1.kocoro.ai"}})
	if code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", code, body)
	}
	if body["error"] != "unknown_config_field" {
		t.Fatalf("error code = %q, want unknown_config_field", body["error"])
	}
	if body["field"] != "daemon.endpoint" {
		t.Fatalf("field = %q, want daemon.endpoint", body["field"])
	}
}

func TestPatchConfigProtectedAliasesAndTargets(t *testing.T) {
	srv, cleanup := newConfigPatchTestServer(t)
	defer cleanup()

	for name, patch := range map[string]map[string]interface{}{
		// viper aliases cloud.endpoint → endpoint: the nested spelling must
		// hit the same protected-field wall as the top-level key.
		"cloud.endpoint": {"cloud": map[string]interface{}{"endpoint": "https://evil.example"}},
		"cloud.api_key":  {"cloud": map[string]interface{}{"api_key": "sk-x"}},
		// Legacy alias that also re-arms migrateOldConfig (rewrites
		// config.yaml down to four keys on next load).
		"gateway_url": {"gateway_url": "https://evil.example"},
		// Protection is presence-based: even a null (delete) is refused —
		// the skill reference documents this exact contract.
		"gateway_url null": {"gateway_url": nil},
		// Redirects session-content uploads — same class as endpoint.
		"sync.endpoint": {"sync": map[string]interface{}{"endpoint": "https://evil.example"}},
	} {
		code, body := doConfigPatch(t, srv, patch)
		if code != 409 {
			t.Fatalf("%s: status = %d, want 409 (body %v)", name, code, body)
		}
		if body["error"] != "protected_field" {
			t.Fatalf("%s: error code = %q, want protected_field", name, body["error"])
		}
	}
}
