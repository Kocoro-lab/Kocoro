package tools

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var consequentialRiskFixtureNowV1 = time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)

func TestConsequentialRiskIntentV1CanonicalFixturesRoundTripExactly(t *testing.T) {
	for _, name := range []string{
		"computer_use_risk_intent.send.v1.json",
		"computer_use_risk_intent.delete.v1.json",
		"computer_use_risk_intent.purchase.v1.json",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := loadCoordinateFixture(t, name)
			intent, err := DecodeConsequentialRiskIntentV1(fixture, consequentialRiskFixtureNowV1)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeConsequentialRiskIntentV1(intent, consequentialRiskFixtureNowV1)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, fixture) {
				t.Fatalf("canonical bytes changed\nwant: %s\n got: %s", fixture, encoded)
			}
			roundTripped, err := DecodeConsequentialRiskIntentV1(encoded, consequentialRiskFixtureNowV1)
			if err != nil || !reflect.DeepEqual(intent, roundTripped) {
				t.Fatalf("struct round trip changed: %#v, %v", roundTripped, err)
			}
		})
	}
}

func TestConsequentialRiskMarkerV1CanonicalFixturesRoundTripExactly(t *testing.T) {
	for _, pair := range []struct {
		marker string
		intent string
	}{
		{marker: "computer_use_risk_intent.marker.send.v1.json", intent: "computer_use_risk_intent.send.v1.json"},
		{marker: "computer_use_risk_intent.marker.delete.v1.json", intent: "computer_use_risk_intent.delete.v1.json"},
		{marker: "computer_use_risk_intent.marker.purchase.v1.json", intent: "computer_use_risk_intent.purchase.v1.json"},
	} {
		t.Run(pair.marker, func(t *testing.T) {
			fixture := loadCoordinateFixture(t, pair.marker)
			marker, err := DecodeConsequentialRiskMarkerV1(fixture, consequentialRiskFixtureNowV1)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeConsequentialRiskMarkerV1(marker, consequentialRiskFixtureNowV1)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, fixture) {
				t.Fatalf("canonical marker bytes changed\nwant: %s\n got: %s", fixture, encoded)
			}
			for _, forbidden := range []string{
				"target_digest", "destination_label", "object_label", "merchant_label", "item_label",
			} {
				if bytes.Contains(encoded, []byte(forbidden)) {
					t.Fatalf("persistent marker contains %q", forbidden)
				}
			}
			intent, err := DecodeConsequentialRiskIntentV1(
				loadCoordinateFixture(t, pair.intent), consequentialRiskFixtureNowV1)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := intent.PersistentMarker(consequentialRiskFixtureNowV1)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(projected, marker) {
				t.Fatalf("marker is not the exact content-free projection: %#v", projected)
			}
		})
	}
}

func TestConsequentialRiskIntentV1RejectsUnknownAndDuplicateMembers(t *testing.T) {
	payload := loadCoordinateFixture(t, "computer_use_risk_intent.send.v1.json")

	unknown := bytes.Replace(payload, []byte("\n  \"kind\":"), []byte("\n  \"body\": \"must fail\",\n  \"kind\":"), 1)
	if _, err := DecodeConsequentialRiskIntentV1(unknown, consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("unknown content-bearing member passed strict decoder")
	}

	duplicate := bytes.Replace(payload, []byte(`"kind": "send"`), []byte(`"kind": "send", "\u006bind": "send"`), 1)
	if _, err := DecodeConsequentialRiskIntentV1(duplicate, consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("escaped-equivalent duplicate member passed strict decoder")
	}

	nestedUnknown := bytes.Replace(payload, []byte(`"payload_kind": "current_composer"`), []byte(`"payload_kind": "current_composer", "clipboard": "must fail"`), 1)
	if _, err := DecodeConsequentialRiskIntentV1(nestedUnknown, consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("nested sensitive member passed strict decoder")
	}

	marker := loadCoordinateFixture(t, "computer_use_risk_intent.marker.send.v1.json")
	markerUnknown := bytes.Replace(marker, []byte("\n}"), []byte(",\n  \"target_digest\": \"must-not-persist\"\n}"), 1)
	if _, err := DecodeConsequentialRiskMarkerV1(markerUnknown, consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("content-bearing marker member passed strict decoder")
	}

	coordinate := loadCoordinateFixture(t, "computer_use_risk_intent.delete.v1.json")
	coordinateUnknown := bytes.Replace(
		coordinate,
		[]byte(`"frame_id": "frame_00112233445566778899aabbccddeeff",`),
		[]byte(`"frame_id": "frame_00112233445566778899aabbccddeeff", "screenshot": "must fail",`),
		1,
	)
	if _, err := DecodeConsequentialRiskIntentV1(coordinateUnknown, consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("coordinate authority accepted unknown screenshot field")
	}
	coordinateDuplicate := bytes.Replace(
		coordinate,
		[]byte(`"element_path": "window[0]/AXButton[0]",`),
		[]byte(`"element_path": "window[0]/AXButton[0]", "\u0065lement_path": "window[0]/AXButton[0]",`),
		1,
	)
	if _, err := DecodeConsequentialRiskIntentV1(coordinateDuplicate, consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("coordinate authority accepted escaped-equivalent duplicate member")
	}
}

func TestConsequentialRiskIntentV1SchemaCannotCarryRawContentFields(t *testing.T) {
	forbidden := map[string]bool{
		"description": true, "text": true, "value": true, "keys": true,
		"prompt": true, "body": true, "clipboard": true, "screenshot": true,
	}
	types := []reflect.Type{
		reflect.TypeOf(ConsequentialRiskIntentV1{}),
		reflect.TypeOf(ConsequentialRiskTargetAuthorityV1{}),
		reflect.TypeOf(ConsequentialRiskCoordinateAuthorityV1{}),
		reflect.TypeOf(ConsequentialRiskPixelPointV1{}),
		reflect.TypeOf(ConsequentialRiskQuartzPointV1{}),
		reflect.TypeOf(ConsequentialSendDetailV1{}),
		reflect.TypeOf(ConsequentialDeleteDetailV1{}),
		reflect.TypeOf(ConsequentialPurchaseDetailV1{}),
		reflect.TypeOf(ConsequentialRiskMarkerV1{}),
	}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if forbidden[name] {
				t.Fatalf("%s exposes forbidden content field %q", typ.Name(), name)
			}
		}
	}

	payload := loadCoordinateFixture(t, "computer_use_risk_intent.send.v1.json")
	for field := range forbidden {
		var object map[string]any
		if err := json.Unmarshal(payload, &object); err != nil {
			t.Fatal(err)
		}
		object[field] = "must fail"
		malformed, _ := json.Marshal(object)
		if _, err := DecodeConsequentialRiskIntentV1(malformed, consequentialRiskFixtureNowV1); err == nil {
			t.Fatalf("unknown sensitive field %q passed", field)
		}
	}
}

func TestConsequentialRiskIntentV1TaggedUnionAndDetailBounds(t *testing.T) {
	intent := canonicalConsequentialRiskIntentV1(t, "send")
	intent.Delete = &ConsequentialDeleteDetailV1{
		ObjectKind: "message", ObjectLabel: "visible message", Scope: "single_visible_object",
	}
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("send intent accepted delete detail")
	}

	intent = canonicalConsequentialRiskIntentV1(t, "delete")
	intent.Delete.Scope = "all_messages"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("delete intent accepted broad scope")
	}

	intent = canonicalConsequentialRiskIntentV1(t, "purchase")
	intent.Purchase.AmountMinor = 0
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("purchase accepted a non-positive amount")
	}
	intent = canonicalConsequentialRiskIntentV1(t, "purchase")
	intent.Purchase.AmountMinor = consequentialRiskAmountMaxMinorV1 + 1
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("purchase accepted an unbounded amount")
	}
	intent = canonicalConsequentialRiskIntentV1(t, "purchase")
	intent.Purchase.Currency = "usd"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("purchase accepted non-canonical currency")
	}

	intent = canonicalConsequentialRiskIntentV1(t, "send")
	intent.Send.DestinationLabel = strings.Repeat("界", consequentialRiskLabelMaxRunesV1+1)
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("oversized label passed")
	}
	intent.Send.DestinationLabel = "trusted\nlabel"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("control character in label passed")
	}
	intent.Send.DestinationLabel = " untrimmed"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("untrimmed label passed")
	}

	// The contract can bound and structurally isolate a trusted display label,
	// but it cannot truthfully classify arbitrary label text as secret/non-secret.
	// Such labels remain process-local; only ConsequentialRiskMarkerV1 is safe to persist.
	intent.Send.DestinationLabel = "token-shaped-user-label"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err != nil {
		t.Fatalf("bounded trusted label was content-guessed: %v", err)
	}
}

func TestConsequentialRiskIntentV1AuthorityDigestIsVersionedAndExact(t *testing.T) {
	intent := canonicalConsequentialRiskIntentV1(t, "send")
	digest, err := ComputeConsequentialRiskTargetDigestV1(intent.Target)
	if err != nil {
		t.Fatal(err)
	}
	if digest != intent.Target.TargetDigest || !strings.HasPrefix(digest, "tdv1_") {
		t.Fatalf("digest = %q, fixture = %q", digest, intent.Target.TargetDigest)
	}

	mutations := []func(*ConsequentialRiskTargetAuthorityV1){
		func(target *ConsequentialRiskTargetAuthorityV1) { target.BundleID = "com.example.Other" },
		func(target *ConsequentialRiskTargetAuthorityV1) { target.AppName = "Other" },
		func(target *ConsequentialRiskTargetAuthorityV1) { target.PID++ },
		func(target *ConsequentialRiskTargetAuthorityV1) { target.WindowID++ },
		func(target *ConsequentialRiskTargetAuthorityV1) { target.StateID = "s_fedcba9876543210" },
		func(target *ConsequentialRiskTargetAuthorityV1) { target.ActionKind = "click" },
		func(target *ConsequentialRiskTargetAuthorityV1) {
			target.ActionKind = "click"
			target.ExecutionPath = "synthetic_coordinate"
		},
		func(target *ConsequentialRiskTargetAuthorityV1) { target.ElementRef = "e9" },
		func(target *ConsequentialRiskTargetAuthorityV1) { target.Role = "AXLink" },
		func(target *ConsequentialRiskTargetAuthorityV1) {
			target.Fingerprint = "axf_" + strings.Repeat("f", 64)
		},
	}
	for _, mutate := range mutations {
		changed := intent
		mutate(&changed.Target)
		if err := changed.Validate(consequentialRiskFixtureNowV1); err == nil {
			t.Fatal("mutated target retained valid exact digest")
		}
	}
}

func TestConsequentialRiskIntentV1CoordinateAuthorityIsRequiredOnlyForSyntheticClicks(t *testing.T) {
	accessibility := canonicalConsequentialRiskIntentV1(t, "send")
	coordinate := canonicalConsequentialRiskIntentV1(t, "delete").Target.CoordinateAuthority
	accessibility.Target.CoordinateAuthority = coordinate
	if err := accessibility.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("accessibility target accepted coordinate authority")
	}

	synthetic := canonicalConsequentialRiskIntentV1(t, "delete")
	synthetic.Target.CoordinateAuthority = nil
	if err := synthetic.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("synthetic coordinate target accepted null coordinate authority")
	}

	synthetic = canonicalConsequentialRiskIntentV1(t, "delete")
	synthetic.Target.ActionKind = "press"
	if err := synthetic.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("synthetic coordinate target accepted non-click action")
	}
}

func TestConsequentialRiskIntentV1CoordinateDigestBindsEveryAuthorityField(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ConsequentialRiskCoordinateAuthorityV1)
	}{
		{name: "element path", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.ElementPath = "window[0]/AXButton[1]" }},
		{name: "frame id", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.FrameID = "frame_ffeeddccbbaa99887766554433221100" }},
		{name: "frame expiry", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.FrameExpiresAt = "2026-07-23T00:04:00Z" }},
		{name: "image digest", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.FinalImageSHA256 = strings.Repeat("b", 64) }},
		{name: "topology id", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.TopologyRef.TopologyID = "topology_fixture_002" }},
		{name: "topology generation", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.TopologyRef.Generation++ }},
		{name: "helper boot", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.HelperBootID = "helper_fixture_002" }},
		{name: "display", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.DisplayID++ }},
		{name: "source x", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.SourcePixel.X++ }},
		{name: "source y", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.SourcePixel.Y++ }},
		{name: "quartz x", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.QuartzPoint.X += 0.25 }},
		{name: "quartz y", mutate: func(v *ConsequentialRiskCoordinateAuthorityV1) { v.QuartzPoint.Y += 0.25 }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			intent := canonicalConsequentialRiskIntentV1(t, "delete")
			if intent.Target.CoordinateAuthority == nil {
				t.Fatal("coordinate fixture lacks authority")
			}
			test.mutate(intent.Target.CoordinateAuthority)
			if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
				t.Fatal("mutated coordinate authority retained valid exact digest")
			}
		})
	}
}

func TestConsequentialRiskIntentV1NumericAuthorityIsFiniteBoundedIntegerOnly(t *testing.T) {
	payload := loadCoordinateFixture(t, "computer_use_risk_intent.purchase.v1.json")
	for name, malformed := range map[string][]byte{
		"fractional pid":    bytes.Replace(payload, []byte(`"pid": 422`), []byte(`"pid": 422.5`), 1),
		"oversized pid":     bytes.Replace(payload, []byte(`"pid": 422`), []byte(`"pid": 2147483648`), 1),
		"fractional amount": bytes.Replace(payload, []byte(`"amount_minor": 12999`), []byte(`"amount_minor": 12999.5`), 1),
		"non-finite amount": bytes.Replace(payload, []byte(`"amount_minor": 12999`), []byte(`"amount_minor": NaN`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConsequentialRiskIntentV1(malformed, consequentialRiskFixtureNowV1); err == nil {
				t.Fatal("malformed numeric authority passed")
			}
		})
	}
}

func TestConsequentialRiskIntentV1IDsAndExpiryAreStrict(t *testing.T) {
	intent := canonicalConsequentialRiskIntentV1(t, "send")
	for _, invalidID := range []string{"", "send-to-bob", "cri_short", "cri_AAAAAAAAAAAAAAAAAAAAA!"} {
		intent.IntentID = invalidID
		if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
			t.Fatalf("invalid intent id %q passed", invalidID)
		}
	}

	intent = canonicalConsequentialRiskIntentV1(t, "send")
	intent.RequestID = "request with spaces"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("non-opaque request id passed")
	}
	intent = canonicalConsequentialRiskIntentV1(t, "send")
	intent.ExpiresAt = consequentialRiskFixtureNowV1.Format(time.RFC3339)
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("already expired intent passed")
	}
	intent.ExpiresAt = consequentialRiskFixtureNowV1.Add(consequentialRiskIntentMaxFutureV1 + time.Second).Format(time.RFC3339)
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("overlong intent lifetime passed")
	}
	intent.ExpiresAt = "2026-07-23T09:02:00+09:00"
	if err := intent.Validate(consequentialRiskFixtureNowV1); err == nil {
		t.Fatal("non-canonical UTC expiry passed")
	}
}

func canonicalConsequentialRiskIntentV1(t *testing.T, kind string) ConsequentialRiskIntentV1 {
	t.Helper()
	payload := loadCoordinateFixture(t, "computer_use_risk_intent."+kind+".v1.json")
	intent, err := DecodeConsequentialRiskIntentV1(payload, consequentialRiskFixtureNowV1)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
