package daemon

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

var consequentialRiskBrokerFixtureNow = time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)

func consequentialRiskBrokerDraft(t *testing.T, requestID string) ConsequentialRiskDraftV1 {
	t.Helper()
	target := tools.ConsequentialRiskTargetAuthorityV1{
		BundleID:      "com.tinyspeck.slackmacgap",
		AppName:       "Slack",
		PID:           420,
		WindowID:      1001,
		StateID:       "s_0123456789abcdef",
		ActionKind:    "press",
		ExecutionPath: "accessibility",
		ElementRef:    "e12",
		Role:          "AXButton",
		Fingerprint:   "axf_1111111111111111111111111111111111111111111111111111111111111111",
	}
	digest, err := tools.ComputeConsequentialRiskTargetDigestV1(target)
	if err != nil {
		t.Fatal(err)
	}
	target.TargetDigest = digest
	return ConsequentialRiskDraftV1{
		RequestID: requestID,
		Kind:      "send",
		Target:    target,
		Send: &tools.ConsequentialSendDetailV1{
			DestinationKind:  "conversation",
			DestinationLabel: "#shipping-ops",
			PayloadKind:      "current_composer",
		},
	}
}

func consequentialRiskCoordinateBrokerDraft(
	t *testing.T,
	requestID string,
	frameExpiresAt time.Time,
) ConsequentialRiskDraftV1 {
	t.Helper()
	target := tools.ConsequentialRiskTargetAuthorityV1{
		BundleID:      "com.tinyspeck.slackmacgap",
		AppName:       "Slack",
		PID:           420,
		WindowID:      1001,
		StateID:       "s_0123456789abcdef",
		ActionKind:    "click",
		ExecutionPath: "synthetic_coordinate",
		ElementRef:    "e12",
		Role:          "AXButton",
		Fingerprint:   "axf_1111111111111111111111111111111111111111111111111111111111111111",
		CoordinateAuthority: &tools.ConsequentialRiskCoordinateAuthorityV1{
			ElementPath:      "window[0]/AXButton[0]",
			FrameID:          "frame_coordinate_broker_001",
			FrameExpiresAt:   frameExpiresAt.UTC().Format(time.RFC3339Nano),
			FinalImageSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			TopologyRef: tools.CoordinateTopologyRefV1{
				TopologyID: "topology_coordinate_001",
				Generation: 7,
			},
			HelperBootID: "helper_coordinate_001",
			DisplayID:    9,
			SourcePixel:  tools.ConsequentialRiskPixelPointV1{X: 320, Y: 240},
			QuartzPoint:  tools.ConsequentialRiskQuartzPointV1{X: 100.5, Y: 200.5},
		},
	}
	digest, err := tools.ComputeConsequentialRiskTargetDigestV1(target)
	if err != nil {
		t.Fatal(err)
	}
	target.TargetDigest = digest
	return ConsequentialRiskDraftV1{
		RequestID: requestID,
		Kind:      "send",
		Target:    target,
		Send: &tools.ConsequentialSendDetailV1{
			DestinationKind:  "conversation",
			DestinationLabel: "#shipping-ops",
			PayloadKind:      "current_composer",
		},
	}
}

func consequentialRiskGrantClaim(intent tools.ConsequentialRiskIntentV1) ConsequentialRiskGrantClaimV1 {
	return ConsequentialRiskGrantClaimV1{
		IntentID: intent.IntentID, RequestID: intent.RequestID,
		TargetDigest: intent.Target.TargetDigest, Kind: intent.Kind,
		Send: intent.Send, Delete: intent.Delete, Purchase: intent.Purchase,
	}
}

func newConsequentialRiskBrokerFixture(t *testing.T, random []byte) (*ConsequentialRiskBroker, *time.Time) {
	t.Helper()
	now := consequentialRiskBrokerFixtureNow
	broker, err := NewConsequentialRiskBroker(ConsequentialRiskBrokerOptions{
		Now:        func() time.Time { return now },
		Random:     bytes.NewReader(random),
		PendingTTL: 60 * time.Second,
		GrantTTL:   10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return broker, &now
}

func sequentialConsequentialRiskRandom(blocks int) []byte {
	result := make([]byte, blocks*16)
	for block := range blocks {
		for offset := range 16 {
			result[block*16+offset] = byte(block*17 + offset)
		}
	}
	return result
}

func TestConsequentialRiskBrokerRegistersOnlyContentFreeMarker(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, []byte{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	})
	intent, marker, err := broker.Register(consequentialRiskBrokerDraft(t, "req_send_01HXYZ"))
	if err != nil {
		t.Fatal(err)
	}
	if intent.IntentID != "cri_AAECAwQFBgcICQoLDA0ODw" {
		t.Fatalf("intent_id = %q", intent.IntentID)
	}
	if intent.ExpiresAt != "2026-07-23T10:01:00Z" || marker.ExpiresAt != intent.ExpiresAt {
		t.Fatalf("expiry intent=%q marker=%q", intent.ExpiresAt, marker.ExpiresAt)
	}
	encoded, err := tools.EncodeConsequentialRiskMarkerV1(marker, consequentialRiskBrokerFixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"request_id", "target", "target_digest", "Slack", "#shipping-ops"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("persistent marker leaked %q: %s", forbidden, encoded)
		}
	}

	detail, err := broker.Detail(intent.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.RequestID != "req_send_01HXYZ" || detail.Target.TargetDigest != intent.Target.TargetDigest ||
		detail.Send == nil || detail.Send.DestinationLabel != "#shipping-ops" {
		t.Fatalf("authoritative detail changed: %+v", detail)
	}
	// Returned details are copies; a caller cannot mutate the broker's authority.
	detail.Send.DestinationLabel = "#tampered"
	again, err := broker.Detail(intent.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Send == nil || again.Send.DestinationLabel != "#shipping-ops" {
		t.Fatalf("detail alias mutated broker state: %+v", again.Send)
	}
}

func TestConsequentialRiskBrokerRejectsInvalidOptionsAndRandomFailure(t *testing.T) {
	for _, options := range []ConsequentialRiskBrokerOptions{
		{PendingTTL: 61 * time.Second},
		{GrantTTL: 11 * time.Second},
		{PendingTTL: 500 * time.Millisecond},
		{GrantTTL: 500 * time.Millisecond},
	} {
		if _, err := NewConsequentialRiskBroker(options); err == nil {
			t.Fatalf("NewConsequentialRiskBroker(%+v) succeeded", options)
		}
	}
	broker, err := NewConsequentialRiskBroker(ConsequentialRiskBrokerOptions{
		Now:    func() time.Time { return consequentialRiskBrokerFixtureNow },
		Random: bytes.NewReader(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_random_failure")); err == nil {
		t.Fatal("Register succeeded without 128 random bits")
	}
}

func TestConsequentialRiskBrokerExpiryAndDecisionLifecycle(t *testing.T) {
	broker, now := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(3))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_allow"))
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Decision != ConsequentialRiskDecisionAllowed || allowed.GrantExpiresAt == nil ||
		*allowed.GrantExpiresAt != "2026-07-23T10:00:10Z" {
		t.Fatalf("allow response = %+v", allowed)
	}
	if _, err := broker.Detail(intent.IntentID); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("allowed intent remained pending: %v", err)
	}
	claim := consequentialRiskGrantClaim(intent)
	if err := broker.ConsumeGrant(claim); err != nil {
		t.Fatalf("ConsumeGrant: %v", err)
	}
	if err := broker.ConsumeGrant(claim); !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
		t.Fatalf("replay error = %v", err)
	}

	deniedIntent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_deny"))
	if err != nil {
		t.Fatal(err)
	}
	denied, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: deniedIntent.IntentID, Decision: ConsequentialRiskDecisionDeny,
	})
	if err != nil {
		t.Fatal(err)
	}
	if denied.Decision != ConsequentialRiskDecisionDenied || denied.GrantExpiresAt != nil {
		t.Fatalf("deny response = %+v", denied)
	}
	if err := broker.ConsumeGrant(ConsequentialRiskGrantClaimV1{
		IntentID: deniedIntent.IntentID, RequestID: deniedIntent.RequestID,
		TargetDigest: deniedIntent.Target.TargetDigest,
	}); !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
		t.Fatalf("denied grant error = %v", err)
	}

	expiring, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_expire"))
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(61 * time.Second)
	if _, err := broker.Detail(expiring.IntentID); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("expired detail error = %v", err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: expiring.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("expired allow error = %v", err)
	}
}

func TestConsequentialRiskBrokerCoordinateFrameCapsIntentAndGrantAuthority(t *testing.T) {
	broker, now := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(2))
	frameExpiry := consequentialRiskBrokerFixtureNow.Add(30 * time.Second)
	intent, _, err := broker.Register(consequentialRiskCoordinateBrokerDraft(
		t, "req_coordinate_29_9", frameExpiry))
	if err != nil {
		t.Fatal(err)
	}
	if intent.ExpiresAt != "2026-07-23T10:00:30Z" {
		t.Fatalf("broker extended or shortened exact frame boundary: %q", intent.ExpiresAt)
	}
	if intent.Target.CoordinateAuthority == nil ||
		intent.Target.CoordinateAuthority.FrameExpiresAt != "2026-07-23T10:00:30Z" {
		t.Fatalf("coordinate authority changed: %+v", intent.Target.CoordinateAuthority)
	}

	// Confirmation at 29.9s is still inside the exact 30s frame authority.
	*now = consequentialRiskBrokerFixtureNow.Add(29*time.Second + 900*time.Millisecond)
	allowed, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	})
	if err != nil {
		t.Fatalf("29.9s confirmation was rejected: %v", err)
	}
	if allowed.GrantExpiresAt == nil || *allowed.GrantExpiresAt != "2026-07-23T10:00:30Z" {
		t.Fatalf("grant was not capped at frame authority: %+v", allowed)
	}
	if err := broker.ConsumeGrant(consequentialRiskGrantClaim(intent)); err != nil {
		t.Fatalf("29.9s one-shot grant could not be consumed: %v", err)
	}

	// The broker must reject the same exact boundary and never revive it.
	expiredBroker, expiredNow := newConsequentialRiskBrokerFixture(
		t, sequentialConsequentialRiskRandom(1))
	expired, _, err := expiredBroker.Register(consequentialRiskCoordinateBrokerDraft(
		t, "req_coordinate_expired", frameExpiry))
	if err != nil {
		t.Fatal(err)
	}
	*expiredNow = frameExpiry
	if _, err := expiredBroker.Detail(expired.IntentID); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("frame boundary remained usable: %v", err)
	}
	if _, err := expiredBroker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: expired.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("expired frame was allowed: %v", err)
	}
}

func TestConsequentialRiskBrokerCoordinateDetailIsDeepCopied(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(1))
	intent, _, err := broker.Register(consequentialRiskCoordinateBrokerDraft(
		t, "req_coordinate_copy", consequentialRiskBrokerFixtureNow.Add(30*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	intent.Target.CoordinateAuthority.FrameID = "frame_tampered"
	detail, err := broker.Detail(intent.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Target.CoordinateAuthority == nil ||
		detail.Target.CoordinateAuthority.FrameID != "frame_coordinate_broker_001" {
		t.Fatalf("caller mutated broker coordinate authority: %+v", detail.Target.CoordinateAuthority)
	}
}

func TestConsequentialRiskGrantMismatchConsumesGrantAndExpiryRejects(t *testing.T) {
	broker, now := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(2))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_bound"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	wrong := consequentialRiskGrantClaim(intent)
	wrong.RequestID = "req_other"
	if err := broker.ConsumeGrant(wrong); !errors.Is(err, ErrConsequentialRiskGrantMismatch) {
		t.Fatalf("wrong binding error = %v", err)
	}
	correct := consequentialRiskGrantClaim(intent)
	if err := broker.ConsumeGrant(correct); !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
		t.Fatalf("mismatch did not burn grant: %v", err)
	}

	expiring, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_grant_expiry"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: expiring.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Second)
	if err := broker.ConsumeGrant(ConsequentialRiskGrantClaimV1{
		IntentID: expiring.IntentID, RequestID: expiring.RequestID,
		TargetDigest: expiring.Target.TargetDigest,
	}); !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
		t.Fatalf("expired grant error = %v", err)
	}
}

func TestConsequentialRiskBrokerInvalidationAPIs(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(5))
	one, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_cancel"))
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_cancel"))
	if err != nil {
		t.Fatal(err)
	}
	three, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: two.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	if removed := broker.InvalidateRequest("req_cancel"); removed != 2 {
		t.Fatalf("InvalidateRequest removed %d, want 2", removed)
	}
	if _, err := broker.Detail(one.IntentID); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("pending request survived invalidation: %v", err)
	}
	if err := broker.ConsumeGrant(ConsequentialRiskGrantClaimV1{
		IntentID: two.IntentID, RequestID: two.RequestID, TargetDigest: two.Target.TargetDigest,
	}); !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
		t.Fatalf("grant survived request invalidation: %v", err)
	}
	if removed := broker.InvalidateIntent(three.IntentID); removed != 1 {
		t.Fatalf("InvalidateIntent removed %d, want 1", removed)
	}

	four, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_all"))
	if err != nil {
		t.Fatal(err)
	}
	five, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_all_2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: five.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	if removed := broker.InvalidateAll(); removed != 2 {
		t.Fatalf("InvalidateAll removed %d, want 2", removed)
	}
	if _, err := broker.Detail(four.IntentID); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("pending survived InvalidateAll: %v", err)
	}
}

func TestConsequentialRiskGrantConsumptionIsAtomic(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, bytes.Repeat([]byte{10}, 16))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_race"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	claim := consequentialRiskGrantClaim(intent)
	var successes atomic.Int32
	var unexpectedMu sync.Mutex
	var unexpected []error
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := broker.ConsumeGrant(claim)
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
				unexpectedMu.Lock()
				unexpected = append(unexpected, err)
				unexpectedMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || len(unexpected) != 0 {
		t.Fatalf("successes=%d unexpected=%v", successes.Load(), unexpected)
	}
}

func TestConsequentialRiskDecisionIsAtomic(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(1))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_decision_race"))
	if err != nil {
		t.Fatal(err)
	}
	request := ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}
	var successes atomic.Int32
	var unavailable atomic.Int32
	var unexpectedMu sync.Mutex
	var unexpected []error
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := broker.Decide(request)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrConsequentialRiskIntentUnavailable):
				unavailable.Add(1)
			default:
				unexpectedMu.Lock()
				unexpected = append(unexpected, err)
				unexpectedMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || unavailable.Load() != 31 || len(unexpected) != 0 {
		t.Fatalf("successes=%d unavailable=%d unexpected=%v",
			successes.Load(), unavailable.Load(), unexpected)
	}
}

func TestConsequentialRiskAwaitDecisionHandlesDecisionBeforeAwaitAndCancellation(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(2))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_wait_allow"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	response, err := broker.AwaitDecision(context.Background(), intent.IntentID)
	if err != nil || response.Decision != ConsequentialRiskDecisionAllowed {
		t.Fatalf("decision-before-await response=%+v err=%v", response, err)
	}

	cancelled, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_wait_cancel"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := broker.AwaitDecision(ctx, cancelled.IntentID); !errors.Is(err, ErrConsequentialRiskDecisionCancelled) {
		t.Fatalf("cancel error=%v", err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: cancelled.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); !errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		t.Fatalf("external allow survived cancellation: %v", err)
	}
}

func TestConsequentialRiskGrantBindsExactDestinationDetail(t *testing.T) {
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(1))
	intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_detail_bind"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intent.IntentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	claim := consequentialRiskGrantClaim(intent)
	changed := *claim.Send
	changed.DestinationLabel = "#different-channel"
	claim.Send = &changed
	if err := broker.ConsumeGrant(claim); !errors.Is(err, ErrConsequentialRiskGrantMismatch) {
		t.Fatalf("destination swap error=%v", err)
	}
	if err := broker.ConsumeGrant(consequentialRiskGrantClaim(intent)); !errors.Is(err, ErrConsequentialRiskGrantUnavailable) {
		t.Fatalf("mismatch did not burn grant: %v", err)
	}
}
