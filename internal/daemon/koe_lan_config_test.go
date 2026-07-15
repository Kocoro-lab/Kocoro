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
		payload, _ := json.Marshal(map[string]string{"token": token})
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
	payload, _ := json.Marshal(map[string]string{"token": token})
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
	if koe["language"] != "zh" || koe["lan_bind"] != true || koe["lan_token"] != token {
		t.Fatalf("unexpected koe config: %#v", koe)
	}
}

func TestKoeLANConfigureRouteIsRegistered(t *testing.T) {
	t.Setenv(localPresenceEnv, testLocalPresenceToken)
	s := &Server{deps: &ServerDeps{ShannonDir: t.TempDir()}}
	req := newKoeLANConfigRequest(`{"token":"wireless-bearer-token-long-enough"}`)
	req.Header.Set(localPresenceHeader, testLocalPresenceToken)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
