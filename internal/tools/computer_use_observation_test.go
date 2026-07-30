package tools

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func loadAXObservationFixture(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile("testdata/ax_read_tree.response.v1.json")
	if err != nil {
		t.Fatalf("read AX observation fixture: %v", err)
	}
	return payload
}

func TestAXObservationV1CanonicalFixtureDecodesThroughProductionType(t *testing.T) {
	observation, err := decodeComputerUseTree(loadAXObservationFixture(t))
	if err != nil {
		t.Fatalf("decodeComputerUseTree: %v", err)
	}
	if observation.SchemaVersion != 1 || observation.AppName != "Fixture App" ||
		observation.BundleID != "run.shannon.ax-fixture" || observation.PID != 4242 ||
		observation.WindowTitle != "Fixture Window" || observation.WindowID != nil ||
		observation.WindowFrame == nil || observation.FocusedRef == nil || *observation.FocusedRef != "e2" {
		t.Fatalf("typed observation lost identity fields: %+v", observation)
	}
	if len(observation.Elements) != 2 {
		t.Fatalf("elements = %d, want 2", len(observation.Elements))
	}
	secure := observation.Elements[1]
	if secure.Value != nil || !secure.ValueRedacted || !secure.Focused || secure.Fingerprint == "" {
		t.Fatalf("secure element crossed the wire incorrectly: %+v", secure)
	}
	if secure.Enabled == nil || !*secure.Enabled {
		t.Fatalf("v1 enabled state was not decoded: %+v", secure)
	}

	// Migration compatibility: the same v1 bytes retain the aliases consumed
	// by the legacy accessibility tool and old computer_use parser.
	var legacy struct {
		App      string                       `json:"app"`
		PID      int                          `json:"pid"`
		Window   string                       `json:"window"`
		Elements []any                        `json:"elements"`
		RefPaths map[string]map[string]string `json:"ref_paths"`
	}
	if err := json.Unmarshal(loadAXObservationFixture(t), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.App != "Fixture App" || legacy.Window != "Fixture Window" || len(legacy.RefPaths) != 2 {
		t.Fatalf("legacy aliases broke: %+v", legacy)
	}
}

func TestAXObservationLegacyMissingEnabledIsUnknownNotDisabled(t *testing.T) {
	observation, err := decodeComputerUseTree([]byte(`{
		"app":"Legacy App","pid":7,"window":"Legacy Window",
		"elements":[{"ref":"e1","role":"AXButton","title":"Save"}],
		"ref_paths":{"e1":{"path":"window[0]/AXButton[0]","role":"AXButton"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Elements[0].Enabled != nil {
		t.Fatalf("legacy omitted enabled became a concrete state: %+v", observation.Elements[0])
	}
	if output := strings.Join(formatComputerUseElements(observation.Elements), "\n"); strings.Contains(output, "enabled=") {
		t.Fatalf("legacy unknown enabled state leaked as disabled: %s", output)
	}
}

func TestAXObservationV1RequiresEnabledAndFormatsExplicitDisabled(t *testing.T) {
	if _, err := decodeComputerUseTree([]byte(`{
		"schema_version":1,"app_name":"Fixture App","pid":7,"window_title":"Fixture Window",
		"elements":[{"ref":"e1","fingerprint":"axf_fixture","path":"window[0]/AXButton[0]","role":"AXButton"}],
		"ref_paths":{}
	}`)); err == nil {
		t.Fatal("schema v1 element without enabled should fail closed")
	}

	disabled := false
	lines := formatComputerUseElements([]computerUseElement{{
		Ref: "e1", Role: "AXButton", Enabled: &disabled,
	}})
	if output := strings.Join(lines, "\n"); !strings.Contains(output, "enabled=false") {
		t.Fatalf("explicit disabled state not surfaced: %s", output)
	}
}

func TestAXObservationV1RejectsInvalidElementIdentityAndRefPaths(t *testing.T) {
	base, err := decodeComputerUseTree(loadAXObservationFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*computerUseTree)
	}{
		{name: "empty ref", mutate: func(tree *computerUseTree) { tree.Elements[0].Ref = "" }},
		{name: "empty fingerprint", mutate: func(tree *computerUseTree) { tree.Elements[0].Fingerprint = "" }},
		{name: "empty path", mutate: func(tree *computerUseTree) { tree.Elements[0].Path = "" }},
		{name: "empty role", mutate: func(tree *computerUseTree) { tree.Elements[0].Role = "" }},
		{name: "duplicate ref", mutate: func(tree *computerUseTree) {
			duplicate := tree.Elements[0]
			tree.Elements = append(tree.Elements, duplicate)
		}},
		{name: "missing ref path", mutate: func(tree *computerUseTree) { delete(tree.RefPaths, "e1") }},
		{name: "path mismatch", mutate: func(tree *computerUseTree) {
			entry := tree.RefPaths["e1"]
			entry.Path = "window[0]/AXButton[9]"
			tree.RefPaths["e1"] = entry
		}},
		{name: "role mismatch", mutate: func(tree *computerUseTree) {
			entry := tree.RefPaths["e1"]
			entry.Role = "AXLink"
			tree.RefPaths["e1"] = entry
		}},
		{name: "fingerprint mismatch", mutate: func(tree *computerUseTree) {
			entry := tree.RefPaths["e1"]
			entry.Fingerprint = "axf_other"
			tree.RefPaths["e1"] = entry
		}},
		{name: "extra ref path", mutate: func(tree *computerUseTree) {
			tree.RefPaths["e99"] = computerUseRefPath{
				Path: "window[0]/AXButton[99]", Role: "AXButton", Fingerprint: "axf_other",
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Elements = append([]computerUseElement(nil), base.Elements...)
			candidate.RefPaths = make(map[string]computerUseRefPath, len(base.RefPaths))
			for ref, entry := range base.RefPaths {
				candidate.RefPaths[ref] = entry
			}
			test.mutate(&candidate)
			payload, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeComputerUseTree(payload); err == nil {
				t.Fatal("invalid schema v1 observation should fail closed")
			}
		})
	}
}

func TestResolveComputerUseFingerprintRejectsDuplicates(t *testing.T) {
	observation, err := decodeComputerUseTree(loadAXObservationFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := observation.Elements[0].Fingerprint
	resolved, err := resolveComputerUseFingerprint(observation.Elements, fingerprint)
	if err != nil || resolved.Ref != "e1" {
		t.Fatalf("unique resolve = %+v, err=%v", resolved, err)
	}

	duplicate := observation.Elements[0]
	duplicate.Ref = "e99"
	duplicate.Path = "window[0]/AXButton[9]"
	observation.Elements = append(observation.Elements, duplicate)
	if _, err := resolveComputerUseFingerprint(observation.Elements, fingerprint); !errors.Is(err, errComputerUseFingerprintAmbiguous) {
		t.Fatalf("duplicate fingerprint error = %v, want ambiguous", err)
	}
}

func TestResolveComputerUseFingerprintRejectsMissing(t *testing.T) {
	observation, err := decodeComputerUseTree(loadAXObservationFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveComputerUseFingerprint(observation.Elements, "axf_missing"); !errors.Is(err, errComputerUseFingerprintNotFound) {
		t.Fatalf("missing fingerprint error = %v", err)
	}
}
