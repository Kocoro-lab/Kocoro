package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

func canonicalHTTPComputerUseTopology(t *testing.T) tools.DisplayTopologyV1 {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(
		wireFixturesDir, "http_get.computer_use_topology.response.json"))
	if err != nil {
		t.Fatal(err)
	}
	topology, err := tools.DecodeDisplayTopologyV1(payload)
	if err != nil {
		t.Fatal(err)
	}
	return topology
}

func canonicalHelperDisplayTopology(t *testing.T) tools.DisplayTopologyV1 {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(
		"..", "tools", "testdata", "display_topology.mixed_horizontal.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	topology, err := tools.DecodeDisplayTopologyV1(payload)
	if err != nil {
		t.Fatal(err)
	}
	return topology
}

func TestComputerUseTopologyFixtureMatchesHelperContract(t *testing.T) {
	daemonFixture := canonicalHTTPComputerUseTopology(t)
	helperFixture := canonicalHelperDisplayTopology(t)
	if !reflect.DeepEqual(daemonFixture, helperFixture) {
		t.Fatalf("daemon HTTP fixture drifted from helper topology fixture\ndaemon: %+v\nhelper: %+v", daemonFixture, helperFixture)
	}
}

func TestComputerUseTopologyHandlerReturnsStrictTopologyWithoutWrapper(t *testing.T) {
	original := readDisplayTopologyVia
	defer func() { readDisplayTopologyVia = original }()

	type contextKey string
	const key contextKey = "computer-use-topology"
	want := canonicalHTTPComputerUseTopology(t)
	readDisplayTopologyVia = func(ctx context.Context) (tools.DisplayTopologyV1, error) {
		if ctx.Value(key) != "request-context" {
			t.Fatal("handler did not forward request context")
		}
		return want, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/local/computer-use/topology", nil)
	req = req.WithContext(context.WithValue(req.Context(), key, "request-context"))
	rec := httptest.NewRecorder()
	(&Server{}).handleComputerUseTopology(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	got, err := tools.DecodeDisplayTopologyV1(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("response is not a strict topology object: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %+v, want %+v", got, want)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := body["topology"]; wrapped {
		t.Fatal("topology response must not add an HTTP wrapper")
	}
}

func TestComputerUseTopologyHandlerReturnsStable502(t *testing.T) {
	original := readDisplayTopologyVia
	defer func() { readDisplayTopologyVia = original }()

	tests := []struct {
		name string
		read func(context.Context) (tools.DisplayTopologyV1, error)
	}{
		{name: "helper failure", read: func(context.Context) (tools.DisplayTopologyV1, error) {
			return tools.DisplayTopologyV1{}, errors.New("private helper detail")
		}},
		{name: "invalid typed result", read: func(context.Context) (tools.DisplayTopologyV1, error) {
			return tools.DisplayTopologyV1{SchemaVersion: 1}, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readDisplayTopologyVia = test.read
			rec := httptest.NewRecorder()
			(&Server{}).handleComputerUseTopology(
				rec,
				httptest.NewRequest(http.MethodGet, "/local/computer-use/topology", nil))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", rec.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"code":  "computer_use_topology_unavailable",
				"error": "display topology unavailable",
			}
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("error body = %#v, want %#v", body, want)
			}
		})
	}
}

func TestCapabilitiesAdvertiseComputerUseTopologyV1(t *testing.T) {
	for _, capability := range Capabilities {
		if capability == CapComputerUseTopologyV1 {
			return
		}
	}
	t.Fatalf("default Capabilities = %v, missing %q", Capabilities, CapComputerUseTopologyV1)
}
