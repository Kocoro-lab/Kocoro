package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newComputerUseAppPolicyHTTPServer(t *testing.T) *Server {
	t.Helper()
	deps := &ServerDeps{ShannonDir: t.TempDir()}
	server := NewServer(0, nil, deps, "test")
	t.Setenv(localPresenceEnv, computerUseHTTPPresenceToken)
	return server
}

func appPolicyRequest(t *testing.T, server *Server, method, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/local/computer-use/app-policy", strings.NewReader(body))
	if token != "" {
		req.Header.Set(localPresenceHeader, token)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeAppPolicySnapshot(t *testing.T, rec *httptest.ResponseRecorder) ComputerUseAppPolicySnapshot {
	t.Helper()
	var snapshot ComputerUseAppPolicySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v; body=%s", err, rec.Body.Bytes())
	}
	return snapshot
}

func TestComputerUseAppPolicyHTTPRequiresLocalPresence(t *testing.T) {
	server := newComputerUseAppPolicyHTTPServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := appPolicyRequest(t, server, method, `{}`, "")
		requireComputerUseHTTPError(t, rec, http.StatusForbidden, "")
	}
}

func TestComputerUseAppPolicyHTTPUpdateReadAndRevoke(t *testing.T) {
	server := newComputerUseAppPolicyHTTPServer(t)

	update := appPolicyRequest(t, server, http.MethodPut,
		`{"schema_version":1,"bundle_id":"com.example.editor","decision":"blocked"}`,
		computerUseHTTPPresenceToken)
	if update.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", update.Code, update.Body.Bytes())
	}
	updated := decodeAppPolicySnapshot(t, update)
	if updated.Revision != 1 || findAppPolicyEntry(updated.Entries, "com.example.editor") == nil {
		t.Fatalf("updated snapshot = %+v", updated)
	}

	read := appPolicyRequest(t, server, http.MethodGet, "", computerUseHTTPPresenceToken)
	if read.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", read.Code, read.Body.Bytes())
	}
	got := decodeAppPolicySnapshot(t, read)
	entry := findAppPolicyEntry(got.Entries, "com.example.editor")
	if entry == nil || entry.Decision != ComputerUseAppPolicyBlocked || entry.Source != ComputerUseAppPolicySourceUser {
		t.Fatalf("GET user entry = %#v", entry)
	}

	revoke := appPolicyRequest(t, server, http.MethodDelete,
		`{"schema_version":1,"bundle_id":"com.example.editor"}`,
		computerUseHTTPPresenceToken)
	if revoke.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d: %s", revoke.Code, revoke.Body.Bytes())
	}
	if entry := findAppPolicyEntry(decodeAppPolicySnapshot(t, revoke).Entries, "com.example.editor"); entry != nil {
		t.Fatalf("revoked entry still present: %#v", entry)
	}
}

func TestComputerUseAppPolicyHTTPRejectsUnknownFieldsAlwaysAllowAndBuiltInOverride(t *testing.T) {
	server := newComputerUseAppPolicyHTTPServer(t)
	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "unknown", body: `{"schema_version":1,"bundle_id":"com.example.editor","decision":"blocked","title":"secret"}`, status: http.StatusBadRequest, code: "invalid_computer_use_app_policy_request"},
		{name: "duplicate member", body: `{"schema_version":1,"bundle_id":"com.example.editor","bundle_id":"com.example.other","decision":"blocked"}`, status: http.StatusBadRequest, code: "invalid_computer_use_app_policy_request"},
		{name: "trailing object", body: `{"schema_version":1,"bundle_id":"com.example.editor","decision":"blocked"}{}`, status: http.StatusBadRequest, code: "invalid_computer_use_app_policy_request"},
		{name: "always allow", body: `{"schema_version":1,"bundle_id":"com.example.editor","decision":"always_allow"}`, status: http.StatusBadRequest, code: "invalid_computer_use_app_policy_request"},
		{name: "non canonical", body: `{"schema_version":1,"bundle_id":"COM.EXAMPLE.EDITOR","decision":"blocked"}`, status: http.StatusBadRequest, code: "invalid_computer_use_app_policy_request"},
		{name: "built in", body: `{"schema_version":1,"bundle_id":"com.apple.terminal","decision":"ask"}`, status: http.StatusConflict, code: "computer_use_app_policy_immutable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := appPolicyRequest(t, server, http.MethodPut, test.body, computerUseHTTPPresenceToken)
			requireComputerUseHTTPError(t, rec, test.status, test.code)
		})
	}
}

func TestComputerUseAppPolicyHTTPRejectsWrongMethod(t *testing.T) {
	server := newComputerUseAppPolicyHTTPServer(t)
	rec := appPolicyRequest(t, server, http.MethodPost, `{}`, computerUseHTTPPresenceToken)
	requireComputerUseHTTPError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != "GET, PUT, DELETE" {
		t.Fatalf("Allow = %q", got)
	}
}

func findAppPolicyEntry(entries []ComputerUseAppPolicyEntry, bundleID string) *ComputerUseAppPolicyEntry {
	for i := range entries {
		if entries[i].BundleID == bundleID {
			return &entries[i]
		}
	}
	return nil
}
