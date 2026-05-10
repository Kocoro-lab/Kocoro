package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestErrorObject_SubCode(t *testing.T) {
	cases := []struct {
		name string
		in   *ErrorObject
		want string
	}{
		{"nil", nil, ""},
		{"no details", &ErrorObject{Code: "x"}, ""},
		{"sub_code present", &ErrorObject{Details: map[string]any{"sub_code": "schema_validation"}}, "schema_validation"},
		{"sub_code wrong type", &ErrorObject{Details: map[string]any{"sub_code": 42}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.SubCode(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResponseEnvelope_RoundTrip(t *testing.T) {
	raw := `{"protocol_version":1,"bundle_version":"0.4.0","bundle_created_at":"2026-04-19T03:14:00Z","bundle_dir":"/x","request_id":"req-abc","candidates":[{"value":"v","score":0.87,"evidence":"observed","supporting_event_ids":["e1"],"entity_id":"e","scope":"s"}],"warnings":[],"reason":"ok","error":null,"latency_ms":4.2}`
	var env ResponseEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if env.Reason != "ok" || len(env.Candidates) != 1 || env.Candidates[0].Score != 0.87 {
		t.Fatalf("decode mismatch: %+v", env)
	}
	out, _ := json.Marshal(env)
	var env2 ResponseEnvelope
	if err := json.Unmarshal(out, &env2); err != nil {
		t.Fatal(err)
	}
	if env2.Reason != env.Reason {
		t.Fatal("round-trip mismatch")
	}
}

// decodeStrict decodes raw JSON into v with DisallowUnknownFields, so the
// test fails if ResponseEnvelope (or any nested type) is missing a field
// that the sidecar actually emits.
func decodeStrict(t *testing.T, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("strict decode failed (likely missing field on Go side): %v", err)
	}
}

// TestResponseEnvelope_MemoryBlockFixtureRoundTrip exercises the wire-shape
// contract against a fixture captured from a real sidecar /query response.
// Three guarantees, in order of importance:
//  1. Strict decode of the fixture — Go struct knows every field the
//     sidecar actually emits (DisallowUnknownFields catches any field a
//     future TLM release adds without bumping ShanClaw).
//  2. Strict decode of our re-marshal — our serialization is itself a
//     valid envelope (no fields invented, none corrupted).
//  3. Canonical-JSON equality of first and second marshal — no semantic
//     loss in the loop. We compare normalized maps rather than
//     reflect.DeepEqual on the structs to dodge Go's nil-slice vs
//     empty-slice false-positives.
//
// Refresh the fixture only when the sidecar wire shape changes. See
// internal/memory/testdata/README.md for the capture procedure.
func TestResponseEnvelope_MemoryBlockFixtureRoundTrip(t *testing.T) {
	for _, fixture := range []string{
		"memoryblock_direct.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			var first ResponseEnvelope
			decodeStrict(t, raw, &first)

			if first.MemoryBlock == nil {
				t.Fatal("expected memory_block present in fixture but got nil")
			}
			if len(first.MemoryBlock.Groups) == 0 {
				t.Fatal("expected at least one group in memory_block")
			}

			out, err := json.Marshal(first)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var second ResponseEnvelope
			decodeStrict(t, out, &second)

			out2, err := json.Marshal(second)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}

			if !canonicalJSONEqual(t, out, out2) {
				t.Fatalf("round-trip JSON differs:\nfirst-marshal:  %s\nsecond-marshal: %s", out, out2)
			}
		})
	}
}

// canonicalJSONEqual decodes a and b into untyped values and compares with
// reflect.DeepEqual. Robust against ordering and against Go-side struct
// nil-slice vs empty-slice differences that don't affect the wire shape.
func canonicalJSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("canonical decode a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("canonical decode b: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}

// TestResponseEnvelope_MemoryBlockNilPreserved confirms that a nil
// MemoryBlock pointer survives the round-trip — the sidecar emits no
// "memory_block" key, the decoder leaves it nil, and re-marshal omits it
// (so older sidecars stay distinguishable from empty MemoryBlock with
// NoDataReason set).
func TestResponseEnvelope_MemoryBlockNilPreserved(t *testing.T) {
	raw := `{"protocol_version":1,"request_id":"r","candidates":[],"warnings":[],"reason":"ok","latency_ms":1.0}`
	var env ResponseEnvelope
	decodeStrict(t, []byte(raw), &env)
	if env.MemoryBlock != nil {
		t.Fatalf("expected nil MemoryBlock when wire omits it, got %+v", env.MemoryBlock)
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(out, []byte(`"memory_block"`)) {
		t.Fatalf("nil MemoryBlock should be omitted on marshal, got: %s", out)
	}
}
