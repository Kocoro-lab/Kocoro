package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const testLocalPresenceToken = "desktop-local-presence-token-for-tests"

func newKoeLANConfigRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/koe/lan/configure", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestKoeLANConfigureRequiresLoopbackAndLocalPresence(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	s := &Server{deps: &ServerDeps{ShannonDir: t.TempDir()}}
	body := `{"token":"wireless-bearer-token-long-enough"}`

	for _, tt := range []struct {
		name       string
		remoteAddr string
		header     string
	}{
		{name: "missing presence", remoteAddr: "127.0.0.1:1234"},
		{name: "wrong presence", remoteAddr: "127.0.0.1:1234", header: "wrong"},
		{name: "non loopback", remoteAddr: "192.168.1.2:1234", header: testLocalPresenceToken},
		{name: "malformed remote", remoteAddr: "not-an-address", header: testLocalPresenceToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := newKoeLANConfigRequest(body)
			req.RemoteAddr = tt.remoteAddr
			if tt.header != "" {
				req.Header.Set(localPresenceHeader, tt.header)
			}
			rr := httptest.NewRecorder()
			s.handleKoeLANConfigure(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestKoeLANConfigureValidatesTokenWithoutWriting(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}

	for _, token := range []string{"short", " leading-or-trailing-token-that-is-long ", strings.Repeat("x", maxKoeLANTokenLength+1)} {
		payload, _ := json.Marshal(map[string]string{"token": token, "unit_id": "50531443cbaa7e08"})
		req := newKoeLANConfigRequest(string(payload))
		req.Header.Set(localPresenceHeader, testLocalPresenceToken)
		rr := httptest.NewRecorder()
		s.handleKoeLANConfigure(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("token length %d: status = %d, want 400; body=%s", len(token), rr.Code, rr.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("invalid requests mutated config: %v", err)
	}
}

func TestKoeLANConfigureWritesProtectedFieldsAndRedactsResponse(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("model_tier: medium\nkoe:\n  language: zh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}
	token := "wireless-bearer-token-long-enough"
	payload, _ := json.Marshal(map[string]string{"token": token, "unit_id": "50531443cbaa7e08"})
	req := newKoeLANConfigRequest(string(payload))
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.handleKoeLANConfigure(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(token)) {
		t.Fatal("response leaked the LAN bearer")
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["restart_required"] != true || response["lan_enabled"] != true {
		t.Fatalf("unexpected response: %#v", response)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]interface{}
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["model_tier"] != "medium" {
		t.Fatalf("unrelated config was not preserved: %#v", saved)
	}
	koe, ok := saved["koe"].(map[string]interface{})
	if !ok {
		t.Fatalf("koe config missing: %#v", saved)
	}
	tokens, ok := koe["lan_tokens"].(map[string]interface{})
	if !ok {
		t.Fatalf("lan_tokens missing: %#v", koe)
	}
	if koe["language"] != "zh" || koe["lan_bind"] != true || tokens["50531443cbaa7e08"] != token {
		t.Fatalf("unexpected koe config: %#v", koe)
	}
}

func TestKoeLANConfigureRouteIsRegistered(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	s := &Server{deps: &ServerDeps{ShannonDir: t.TempDir()}}
	req := newKoeLANConfigRequest(`{"token":"wireless-bearer-token-long-enough","unit_id":"50531443cbaa7e08"}`)
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// --- per-robot slots ---

func newKoeLANRevokeRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/koe/lan/revoke", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	return req
}

// savedKoeConfig reads the koe block back off disk. patchGlobalConfig writes the
// file, not s.deps.Config, so the file is the only truth here.
func savedKoeConfig(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]interface{}
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	koe, ok := saved["koe"].(map[string]interface{})
	if !ok {
		t.Fatalf("koe config missing: %#v", saved)
	}
	return koe
}

func savedLANTokens(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	tokens, ok := savedKoeConfig(t, dir)["lan_tokens"].(map[string]interface{})
	if !ok {
		t.Fatalf("lan_tokens missing or wrong shape: %#v", savedKoeConfig(t, dir))
	}
	return tokens
}

func TestKoeLANConfigure_UpsertsOneSlotAndLeavesOthersAlone(t *testing.T) {
	// The bug this exists to prevent: pairing robot B wiping robot A's token.
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("koe:\n  lan_bind: true\n  lan_tokens:\n    50531443cbaa7e08: token-robot-a-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}

	payload, _ := json.Marshal(map[string]string{
		"token": "token-robot-b-0123456789", "unit_id": "be92ee93cacbff5f",
	})
	req := newKoeLANConfigRequest(string(payload))
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.handleKoeLANConfigure(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	tokens := savedLANTokens(t, dir)
	if tokens["50531443cbaa7e08"] != "token-robot-a-0123456789" {
		t.Fatalf("pairing a second robot revoked the first: %#v", tokens)
	}
	if tokens["be92ee93cacbff5f"] != "token-robot-b-0123456789" {
		t.Fatalf("the new robot's token was not stored: %#v", tokens)
	}
}

func TestKoeLANConfigure_ReplacesTheSameRobotsToken(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("koe:\n  lan_tokens:\n    50531443cbaa7e08: token-old-0123456789xxxx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}
	payload, _ := json.Marshal(map[string]string{
		"token": "token-new-0123456789xxxx", "unit_id": "50531443cbaa7e08",
	})
	req := newKoeLANConfigRequest(string(payload))
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	s.handleKoeLANConfigure(httptest.NewRecorder(), req)

	tokens := savedLANTokens(t, dir)
	if len(tokens) != 1 || tokens["50531443cbaa7e08"] != "token-new-0123456789xxxx" {
		t.Fatalf("re-pairing must replace in place: %#v", tokens)
	}
}

func TestKoeLANConfigure_RejectsAMissingUnitID(t *testing.T) {
	// Without an id this robot could never be revoked on its own, only by
	// clobbering the map - which is the failure this change removes.
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}
	payload, _ := json.Marshal(map[string]string{"token": "token-robot-a-0123456789"})
	req := newKoeLANConfigRequest(string(payload))
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.handleKoeLANConfigure(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("an invalid request mutated config")
	}
}

func TestKoeLANRevoke_RemovesOnlyThatRobot(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("koe:\n  lan_bind: true\n  lan_tokens:\n    50531443cbaa7e08: token-robot-a-0123456789\n    be92ee93cacbff5f: token-robot-b-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}
	req := newKoeLANRevokeRequest(`{"unit_id":"be92ee93cacbff5f"}`)
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.handleKoeLANRevoke(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	tokens := savedLANTokens(t, dir)
	if _, present := tokens["be92ee93cacbff5f"]; present {
		t.Fatalf("the unpaired robot's token survived: %#v", tokens)
	}
	if tokens["50531443cbaa7e08"] != "token-robot-a-0123456789" {
		t.Fatalf("unpairing one robot revoked another: %#v", tokens)
	}
}

func TestKoeLANRevoke_IsIdempotent(t *testing.T) {
	// Desktop may retry. The caller asked for an end state, and an unpaired robot
	// already satisfies it.
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	dir := t.TempDir()
	s := &Server{deps: &ServerDeps{ShannonDir: dir}}
	req := newKoeLANRevokeRequest(`{"unit_id":"never-paired"}`)
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.handleKoeLANRevoke(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestKoeLANRevoke_RequiresLoopbackAndLocalPresence(t *testing.T) {
	// Revoking access is as sensitive as granting it.
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	s := &Server{deps: &ServerDeps{ShannonDir: t.TempDir()}}
	for _, tt := range []struct {
		name       string
		remoteAddr string
		header     string
	}{
		{name: "missing presence", remoteAddr: "127.0.0.1:1234"},
		{name: "wrong presence", remoteAddr: "127.0.0.1:1234", header: "wrong"},
		{name: "non loopback", remoteAddr: "192.168.1.2:1234", header: testLocalPresenceToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := newKoeLANRevokeRequest(`{"unit_id":"50531443cbaa7e08"}`)
			req.RemoteAddr = tt.remoteAddr
			if tt.header != "" {
				req.Header.Set(localPresenceHeader, tt.header)
			}
			rr := httptest.NewRecorder()
			s.handleKoeLANRevoke(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rr.Code)
			}
		})
	}
}
