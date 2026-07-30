package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const executionProfileFixturesDir = "../../docs/desktop-wire-fixtures/execution-profiles-v1"

func loadExecutionProfileFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(executionProfileFixturesDir, name))
	if err != nil {
		t.Fatalf("read execution profile fixture %q: %v", name, err)
	}
	return data
}

func TestExecutionProfileFixtureManifestAndCanonicalRoundTrip(t *testing.T) {
	var manifest struct {
		SchemaVersion int               `json:"schema_version"`
		Algorithm     string            `json:"algorithm"`
		Files         map[string]string `json:"files"`
	}
	if err := json.Unmarshal(loadExecutionProfileFixture(t, "manifest.json"), &manifest); err != nil {
		t.Fatalf("decode execution profile fixture manifest: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Algorithm != "sha256" {
		t.Fatalf("manifest header = version %d algorithm %q", manifest.SchemaVersion, manifest.Algorithm)
	}
	wantFiles := map[string]bool{
		"completion-request.openai-native-continuation.json":  true,
		"completion.openai-generic-ax-only.json":              true,
		"completion.openai-native-computer-call.json":         true,
		"error.execution-profile-mismatch.json":               true,
		"error.execution-profile-stale.json":                  true,
		"invalid.openai-native-duplicate-call-id.json":        true,
		"invalid.openai-native-cross-call-safety-replay.json": true,
		"invalid.openai-native-cross-response-replay.json":    true,
		"invalid.openai-native-duplicate-safety-ack.json":     true,
		"invalid.openai-native-missing-provenance.json":       true,
		"invalid.openai-native-missing-safety-ack.json":       true,
		"invalid.openai-native-same-envelope-replay.json":     true,
		"invalid.openai-native-wrong-call-id.json":            true,
		"openai-request.computer-call-output.json":            true,
		"profile.anthropic-native.json":                       true,
		"profile.openai-generic-ax-only.json":                 true,
		"profile.openai-native.json":                          true,
		"stream.openai-generic-ax-only.sse":                   true,
		"stream.openai-native-computer-call.sse":              true,
	}
	if len(manifest.Files) != len(wantFiles) {
		t.Fatalf("manifest files = %v, want exactly %v", manifest.Files, wantFiles)
	}
	for name, want := range manifest.Files {
		if !wantFiles[name] {
			t.Errorf("manifest contains unexpected fixture %q", name)
		}
		sum := sha256.Sum256(loadExecutionProfileFixture(t, name))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("fixture %q sha256 = %s, want %s", name, got, want)
		}
	}

	for _, name := range []string{
		"profile.anthropic-native.json",
		"profile.openai-generic-ax-only.json",
		"profile.openai-native.json",
	} {
		data := loadExecutionProfileFixture(t, name)
		var original executionProfileWire
		if err := json.Unmarshal(data, &original); err != nil {
			t.Fatalf("decode profile fixture %q: %v", name, err)
		}
		if err := validateExecutionProfileWire(original); err != nil {
			t.Fatalf("validate profile fixture %q: %v", name, err)
		}
		canonicalID, err := canonicalExecutionProfileID(original)
		if err != nil {
			t.Fatalf("canonical id for %q: %v", name, err)
		}
		if canonicalID != original.ProfileID {
			t.Errorf("profile fixture %q id = %s, canonical = %s", name, original.ProfileID, canonicalID)
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatalf("decode profile map %q: %v", name, err)
		}
		reordered, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("reorder profile fixture %q: %v", name, err)
		}
		var roundTrip executionProfileWire
		if err := json.Unmarshal(reordered, &roundTrip); err != nil {
			t.Fatalf("round-trip profile fixture %q: %v", name, err)
		}
		if !reflect.DeepEqual(roundTrip, original) {
			t.Errorf("profile fixture %q changed across field-order round-trip", name)
		}
		reorderedID, err := canonicalExecutionProfileID(roundTrip)
		if err != nil || reorderedID != original.ProfileID {
			t.Errorf("reordered profile fixture %q id = %s, %v; want %s", name, reorderedID, err, original.ProfileID)
		}
	}
}
