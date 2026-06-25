package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMintEphemeralRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dev-key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Session struct {
				Type  string `json:"type"`
				Model string `json:"model"`
			} `json:"session"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Session.Type != "realtime" || !strings.Contains(body.Session.Model, "gpt-realtime") {
			t.Errorf("bad mint body: %+v", body.Session)
		}
		json.NewEncoder(w).Encode(map[string]any{"value": "ek_test123"})
	}))
	defer srv.Close()

	ek, err := mintEphemeralAt(context.Background(), srv.URL, "dev-key", "gpt-realtime-mini-2025-12-15")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ek != "ek_test123" {
		t.Errorf("ek = %q, want ek_test123", ek)
	}
}
