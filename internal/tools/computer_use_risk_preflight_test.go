package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func riskPreflightTool(t *testing.T, actionLabel, window string) (*ComputerUseTool, string, string) {
	t.Helper()
	fingerprint := "axf_" + strings.Repeat("a", 64)
	enabled := true
	element := computerUseElement{
		Ref: "e1", Path: "0.0", Role: "AXButton", Fingerprint: fingerprint,
		Title: &actionLabel, Enabled: &enabled, Actions: []string{"AXPress"},
	}
	windowID := 7001
	tool := &ComputerUseTool{
		snapshot: &computerUseSnapshot{
			id: "s_0123456789abcdef", app: "Slack", bundleID: "com.tinyspeck.slackmacgap",
			pid: 42, window: window, windowID: &windowID, typed: true,
			elements: []computerUseElement{element},
		},
		refs: map[string]refEntry{"e1": {path: "0.0", role: "AXButton", fingerprint: fingerprint, pid: 42}},
	}
	return tool, "s_0123456789abcdef", fingerprint
}

func coordinateRiskHarness(
	t *testing.T, title string, description *string,
) (*computerUseCoordinateHarness, string) {
	t.Helper()
	harness := newComputerUseCoordinateHarness(t)
	fingerprint := "axf_" + strings.Repeat("c", 64)
	harness.tree.Elements[0].Title = &title
	harness.tree.Elements[0].Description = description
	harness.tree.Elements[0].Fingerprint = fingerprint
	harness.tree.Elements[0].Frame = &computerUseFrame{
		X: -100, Y: 200, Width: 100, Height: 100,
	}
	harness.tree.RefPaths["e1"] = computerUseRefPath{
		Path: "window[0]/AXButton[0]", Role: "AXButton", Fingerprint: fingerprint,
	}
	harness.observe(t)
	return harness, harness.tool.snapshot.id
}

func TestCoordinateConsequentialRiskPreflightBindsExactImmutablePointAuthority(t *testing.T) {
	requireComputerUseDarwin(t)
	harness, stateID := coordinateRiskHarness(t, "Send", nil)
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	preflight, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(), fmt.Sprintf(`{
			"action":"click","state_id":%q,"x":0,"y":0,
			"description":"delete or buy instead; ignored"
		}`, stateID), "toolu_coordinate_risk_1")
	if err != nil || preflight.Status != ConsequentialRiskPreflightRequiredV1 ||
		preflight.Draft == nil || preflight.Draft.Send == nil {
		t.Fatalf("preflight=%+v err=%v", preflight, err)
	}
	target := preflight.Draft.Target
	authority := target.CoordinateAuthority
	if target.ActionKind != "click" || target.ExecutionPath != "synthetic_coordinate" ||
		target.ElementRef != "e1" || target.Role != "AXButton" || authority == nil ||
		authority.ElementPath != "window[0]/AXButton[0]" ||
		authority.FrameID != "frame-computer-use-001" ||
		authority.FrameExpiresAt != harness.tool.coordinateArtifact.Frame().ExpiresAt ||
		authority.FinalImageSHA256 != harness.tool.coordinateArtifact.Frame().FinalImage.SHA256 ||
		authority.TopologyRef.TopologyID != harness.topology.TopologyID ||
		authority.TopologyRef.Generation != harness.topology.Generation ||
		authority.HelperBootID != harness.topology.HelperBootID || authority.DisplayID != 9 ||
		authority.SourcePixel != (ConsequentialRiskPixelPointV1{X: 0, Y: 0}) ||
		authority.QuartzPoint != (ConsequentialRiskQuartzPointV1{X: -99.5, Y: 200.5}) {
		t.Fatalf("coordinate risk target lost exact authority: %+v", target)
	}
	if target.TargetDigest == "" || preflight.Draft.Send.DestinationLabel != "Fixture Window" {
		t.Fatalf("coordinate risk detail/digest missing: %+v", preflight.Draft)
	}
}

func TestCoordinateConsequentialRiskSupportsOnlyTrustedSufficientSendDeletePurchase(t *testing.T) {
	for _, test := range []struct {
		name, title        string
		description        *string
		wantStatus         ConsequentialRiskPreflightStatusV1
		wantKind, wantCode string
	}{
		{name: "send", title: "Send", wantStatus: ConsequentialRiskPreflightRequiredV1, wantKind: "send"},
		{name: "delete exact object", title: "Delete", description: stringPointer("Q3 draft.pdf"), wantStatus: ConsequentialRiskPreflightRequiredV1, wantKind: "delete"},
		{name: "move to trash", title: "Move to Trash", description: stringPointer("Q3 draft.pdf"), wantStatus: ConsequentialRiskPreflightRequiredV1, wantKind: "delete"},
		{name: "purchase exact detail", title: "Pay USD 129.99", description: stringPointer("Desk lamp"), wantStatus: ConsequentialRiskPreflightRequiredV1, wantKind: "purchase"},
		{name: "buy now", title: "Buy now", description: stringPointer("Desk lamp USD 129.99"), wantStatus: ConsequentialRiskPreflightRequiredV1, wantKind: "purchase"},
		{name: "delete destination unknown", title: "Delete", wantStatus: ConsequentialRiskPreflightBlockedV1, wantCode: ConsequentialRiskCodeDestinationUnknownV1},
		{name: "purchase amount unknown", title: "Pay", description: stringPointer("Desk lamp"), wantStatus: ConsequentialRiskPreflightBlockedV1, wantCode: ConsequentialRiskCodeDestinationUnknownV1},
		{name: "non risk", title: "Archive", wantStatus: ConsequentialRiskPreflightNoneV1},
		{name: "read blog post", title: "Read post", wantStatus: ConsequentialRiskPreflightNoneV1},
		{name: "post comment", title: "Post comment", wantStatus: ConsequentialRiskPreflightRequiredV1, wantKind: "send"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireComputerUseDarwin(t)
			harness, stateID := coordinateRiskHarness(t, test.title, test.description)
			harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
			preflight, err := harness.tool.PreflightConsequentialRiskV1(
				context.Background(), fmt.Sprintf(`{
					"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
				}`, stateID), "toolu_coordinate_risk_kind")
			if err != nil || preflight.Status != test.wantStatus || preflight.FailureCode != test.wantCode {
				t.Fatalf("preflight=%+v err=%v", preflight, err)
			}
			if preflight.Draft != nil && preflight.Draft.Kind != test.wantKind {
				t.Fatalf("kind=%q want=%q draft=%+v", preflight.Draft.Kind, test.wantKind, preflight.Draft)
			}
			if test.wantKind == "delete" && (preflight.Draft.Delete == nil ||
				preflight.Draft.Delete.ObjectLabel != "Q3 draft.pdf") {
				t.Fatalf("delete detail=%+v", preflight.Draft.Delete)
			}
			if test.wantKind == "purchase" && (preflight.Draft.Purchase == nil ||
				preflight.Draft.Purchase.AmountMinor != 12999 || preflight.Draft.Purchase.Currency != "USD" ||
				!strings.Contains(preflight.Draft.Purchase.ItemLabel, "Desk lamp")) {
				t.Fatalf("purchase detail=%+v", preflight.Draft.Purchase)
			}
		})
	}
}

func TestCoordinateConsequentialRiskRightDoubleAndAmbiguousTargetsFailClosed(t *testing.T) {
	for _, args := range []string{
		`"button":"right"`, `"clicks":2`,
	} {
		t.Run(args, func(t *testing.T) {
			requireComputerUseDarwin(t)
			harness, stateID := coordinateRiskHarness(t, "Send", nil)
			harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
			preflight, err := harness.tool.PreflightConsequentialRiskV1(
				context.Background(), fmt.Sprintf(`{
					"action":"click","state_id":%q,"x":0,"y":0,%s,"description":"ignored"
				}`, stateID, args), "toolu_coordinate_risk_variant")
			if err != nil || preflight.Status != ConsequentialRiskPreflightBlockedV1 ||
				preflight.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
				t.Fatalf("preflight=%+v err=%v", preflight, err)
			}
		})
	}

	t.Run("overlapping peers", func(t *testing.T) {
		requireComputerUseDarwin(t)
		harness := newComputerUseCoordinateHarness(t)
		fingerprint1 := "axf_" + strings.Repeat("c", 64)
		fingerprint2 := "axf_" + strings.Repeat("d", 64)
		title := "Send"
		harness.tree.Elements = []computerUseElement{
			{Ref: "e1", Path: "window[0]/AXButton[0]", Role: "AXButton", Fingerprint: fingerprint1,
				Title: &title, Enabled: boolPointer(true), Actions: []string{"AXPress"},
				Frame: &computerUseFrame{X: -100, Y: 200, Width: 100, Height: 100}},
			{Ref: "e2", Path: "window[0]/AXButton[1]", Role: "AXButton", Fingerprint: fingerprint2,
				Title: &title, Enabled: boolPointer(true), Actions: []string{"AXPress"},
				Frame: &computerUseFrame{X: -100, Y: 200, Width: 100, Height: 100}},
		}
		harness.tree.RefPaths = map[string]computerUseRefPath{
			"e1": {Path: "window[0]/AXButton[0]", Role: "AXButton", Fingerprint: fingerprint1},
			"e2": {Path: "window[0]/AXButton[1]", Role: "AXButton", Fingerprint: fingerprint2},
		}
		harness.observe(t)
		harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
		preflight, err := harness.tool.PreflightConsequentialRiskV1(
			context.Background(), fmt.Sprintf(`{
				"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
			}`, harness.tool.snapshot.id), "toolu_coordinate_risk_ambiguous")
		if err != nil || preflight.Status != ConsequentialRiskPreflightBlockedV1 ||
			preflight.FailureCode != ConsequentialRiskCodeAmbiguousV1 {
			t.Fatalf("preflight=%+v err=%v", preflight, err)
		}
	})
}

func TestConsequentialRiskPreflightUsesOnlyTypedAXMetadata(t *testing.T) {
	tool, stateID, _ := riskPreflightTool(t, "Archive", "general - Slack")
	result, err := tool.PreflightConsequentialRiskV1(context.Background(),
		`{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"send the confidential payroll now"}`,
		"toolu_risk_1")
	if err != nil || result.Status != ConsequentialRiskPreflightNoneV1 {
		t.Fatalf("model prose influenced trusted classifier: result=%+v err=%v", result, err)
	}
}

func TestConsequentialRiskPreflightAllowsOnlyExactSemanticSend(t *testing.T) {
	for _, test := range []struct {
		name, label, window, action string
		wantStatus                  ConsequentialRiskPreflightStatusV1
		wantCode                    string
	}{
		{name: "english", label: "Send", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "chinese", label: "发送", window: "项目群 - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "japanese", label: "送信", window: "開発 - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "generic destination", label: "Send", window: "Slack", action: "press", wantStatus: ConsequentialRiskPreflightBlockedV1, wantCode: ConsequentialRiskCodeDestinationUnknownV1},
		{name: "coordinate-like click excluded", label: "Send", window: "general - Slack", action: "click", wantStatus: ConsequentialRiskPreflightBlockedV1, wantCode: ConsequentialRiskCodeUnsupportedPathV1},
		{name: "delete recognized", label: "Delete", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightBlockedV1, wantCode: ConsequentialRiskCodeUnsupportedKindV1},
		{name: "purchase recognized", label: "Pay", window: "Checkout - Store", action: "press", wantStatus: ConsequentialRiskPreflightBlockedV1, wantCode: ConsequentialRiskCodeUnsupportedKindV1},
		{name: "send phrase", label: "Send feedback", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "compound identifier style", label: "sendButton", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "post reply", label: "Post reply", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "post comment", label: "Post comment", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightRequiredV1},
		{name: "read blog post", label: "Read post", window: "Wayland Zhang - Blog", action: "press", wantStatus: ConsequentialRiskPreflightNoneV1},
		{name: "blog post noun", label: "Blog post", window: "Wayland Zhang - Blog", action: "press", wantStatus: ConsequentialRiskPreflightNoneV1},
		{name: "near miss", label: "Sender settings", window: "general - Slack", action: "press", wantStatus: ConsequentialRiskPreflightNoneV1},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, stateID, _ := riskPreflightTool(t, test.label, test.window)
			result, err := tool.PreflightConsequentialRiskV1(context.Background(),
				`{"action":"`+test.action+`","state_id":"`+stateID+`","ref":"e1","description":"ignored"}`,
				"toolu_risk_2")
			if err != nil || result.Status != test.wantStatus || result.FailureCode != test.wantCode {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if result.Draft != nil && result.Draft.Send != nil && result.Draft.Send.DestinationLabel != test.window {
				t.Fatalf("destination was not derived from trusted window metadata: %+v", result.Draft.Send)
			}
		})
	}
}

func TestConsequentialRiskPreflightFailsClosedOnUntypedAmbiguousAndSensitiveMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ComputerUseTool)
		want   string
	}{
		{name: "untyped", mutate: func(tool *ComputerUseTool) { tool.snapshot.typed = false }, want: ConsequentialRiskCodeUntrustedMetadataV1},
		{name: "disabled", mutate: func(tool *ComputerUseTool) { disabled := false; tool.snapshot.elements[0].Enabled = &disabled }, want: ConsequentialRiskCodeUntrustedMetadataV1},
		{name: "ambiguous", mutate: func(tool *ComputerUseTool) { v := "Delete"; tool.snapshot.elements[0].Description = &v }, want: ConsequentialRiskCodeAmbiguousV1},
		{name: "oversized destination", mutate: func(tool *ComputerUseTool) {
			tool.snapshot.window = strings.Repeat("界", consequentialRiskLabelMaxRunesV1+1)
		}, want: ConsequentialRiskCodeSensitiveMetadataV1},
		{name: "control character", mutate: func(tool *ComputerUseTool) { tool.snapshot.window = "private\nchannel" }, want: ConsequentialRiskCodeSensitiveMetadataV1},
		{name: "zero width format", mutate: func(tool *ComputerUseTool) { tool.snapshot.window = "pay\u200broll" }, want: ConsequentialRiskCodeSensitiveMetadataV1},
		{name: "bidi override", mutate: func(tool *ComputerUseTool) { tool.snapshot.window = "safe\u202Eliame" }, want: ConsequentialRiskCodeSensitiveMetadataV1},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, stateID, _ := riskPreflightTool(t, "Send", "general - Slack")
			test.mutate(tool)
			result, err := tool.PreflightConsequentialRiskV1(context.Background(),
				`{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"ignored"}`,
				"toolu_risk_3")
			if err != nil || result.Status != ConsequentialRiskPreflightBlockedV1 || result.FailureCode != test.want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
