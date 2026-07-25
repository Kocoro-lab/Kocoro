package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	consequentialRiskLabelMaxRunesV1   = 128
	consequentialRiskIntentMaxFutureV1 = 5 * time.Minute
	consequentialRiskAmountMaxMinorV1  = int64(9_999_999_999_999)
)

var (
	consequentialRiskRequestIDPatternV1 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	consequentialRiskBundleIDPatternV1  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$`)
	consequentialRiskStateIDPatternV1   = regexp.MustCompile(`^s_[a-f0-9]{16}$`)
	consequentialRiskRolePatternV1      = regexp.MustCompile(`^AX[A-Za-z0-9]{1,62}$`)
	consequentialRiskFingerprintV1      = regexp.MustCompile(`^axf_[a-f0-9]{64}$`)
	consequentialRiskCurrencyPatternV1  = regexp.MustCompile(`^[A-Z]{3}$`)
	consequentialRiskDigestPatternV1    = regexp.MustCompile(`^tdv1_[a-f0-9]{64}$`)
	consequentialRiskFrameIDPatternV1   = regexp.MustCompile(`^frame[-_][A-Za-z0-9._:-]{1,127}$`)
	consequentialRiskElementPathV1      = regexp.MustCompile(`^window\[0\](?:/AX[A-Za-z0-9]{1,62}\[[0-9]+\])+$`)
)

// ConsequentialRiskIntentV1 is the process-local, authoritative description
// of one exact point-of-risk confirmation. Display labels may contain private
// user text and therefore this type is not a persistence or activity-event
// format. Persist ConsequentialRiskMarkerV1 instead.
type ConsequentialRiskIntentV1 struct {
	SchemaVersion int                                `json:"schema_version"`
	IntentID      string                             `json:"intent_id"`
	RequestID     string                             `json:"request_id"`
	Kind          string                             `json:"kind"`
	ExpiresAt     string                             `json:"expires_at"`
	Target        ConsequentialRiskTargetAuthorityV1 `json:"target"`
	Send          *ConsequentialSendDetailV1         `json:"send"`
	Delete        *ConsequentialDeleteDetailV1       `json:"delete"`
	Purchase      *ConsequentialPurchaseDetailV1     `json:"purchase"`
}

// ConsequentialRiskTargetAuthorityV1 binds a confirmation to the exact
// observed UI target. TargetDigest is a process-local integrity binding over
// every other field. Because that digest also commits to short display labels,
// it must not be copied into the persistent marker.
type ConsequentialRiskTargetAuthorityV1 struct {
	BundleID            string                                  `json:"bundle_id"`
	AppName             string                                  `json:"app_name"`
	PID                 int                                     `json:"pid"`
	WindowID            uint32                                  `json:"window_id"`
	StateID             string                                  `json:"state_id"`
	ActionKind          string                                  `json:"action_kind"`
	ExecutionPath       string                                  `json:"execution_path"`
	ElementRef          string                                  `json:"element_ref"`
	Role                string                                  `json:"role"`
	Fingerprint         string                                  `json:"fingerprint"`
	CoordinateAuthority *ConsequentialRiskCoordinateAuthorityV1 `json:"coordinate_authority"`
	TargetDigest        string                                  `json:"target_digest"`
}

// ConsequentialRiskCoordinateAuthorityV1 binds a synthetic click confirmation
// to the exact immutable image and its pixel-to-Quartz mapping. It contains
// only opaque/non-content authority: no screenshot bytes, OCR, AX values, or
// model-authored prose may enter this contract.
type ConsequentialRiskCoordinateAuthorityV1 struct {
	ElementPath      string                         `json:"element_path"`
	FrameID          string                         `json:"frame_id"`
	FrameExpiresAt   string                         `json:"frame_expires_at"`
	FinalImageSHA256 string                         `json:"final_image_sha256"`
	TopologyRef      CoordinateTopologyRefV1        `json:"topology_ref"`
	HelperBootID     string                         `json:"helper_boot_id"`
	DisplayID        uint32                         `json:"display_id"`
	SourcePixel      ConsequentialRiskPixelPointV1  `json:"source_pixel"`
	QuartzPoint      ConsequentialRiskQuartzPointV1 `json:"quartz_point"`
}

type ConsequentialRiskPixelPointV1 struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type ConsequentialRiskQuartzPointV1 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type ConsequentialSendDetailV1 struct {
	DestinationKind  string `json:"destination_kind"`
	DestinationLabel string `json:"destination_label"`
	PayloadKind      string `json:"payload_kind"`
}

type ConsequentialDeleteDetailV1 struct {
	ObjectKind  string `json:"object_kind"`
	ObjectLabel string `json:"object_label"`
	Scope       string `json:"scope"`
}

type ConsequentialPurchaseDetailV1 struct {
	MerchantLabel string `json:"merchant_label"`
	ItemLabel     string `json:"item_label"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
}

// ConsequentialRiskMarkerV1 is the only persistence-safe projection of an
// intent. It deliberately carries neither target authority/digests nor labels.
type ConsequentialRiskMarkerV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Required      bool   `json:"required"`
	Kind          string `json:"kind"`
	IntentID      string `json:"intent_id"`
	ExpiresAt     string `json:"expires_at"`
}

// ConsequentialRiskDraftV1 is the neutral tools-layer preflight result. It
// deliberately omits broker-owned intent identity and expiry so daemon can
// consume it without tools importing daemon or guicontrol.
type ConsequentialRiskDraftV1 struct {
	RequestID string
	Kind      string
	Target    ConsequentialRiskTargetAuthorityV1
	Send      *ConsequentialSendDetailV1
	Delete    *ConsequentialDeleteDetailV1
	Purchase  *ConsequentialPurchaseDetailV1
}

type ConsequentialRiskPreflightStatusV1 string

const (
	ConsequentialRiskPreflightNoneV1     ConsequentialRiskPreflightStatusV1 = "none"
	ConsequentialRiskPreflightRequiredV1 ConsequentialRiskPreflightStatusV1 = "required"
	ConsequentialRiskPreflightBlockedV1  ConsequentialRiskPreflightStatusV1 = "blocked"

	ConsequentialRiskCodeDestinationUnknownV1 = "consequential_risk_destination_unknown"
	ConsequentialRiskCodeUnsupportedPathV1    = "consequential_risk_unsupported_path"
	ConsequentialRiskCodeUnsupportedKindV1    = "consequential_risk_unsupported_kind"
	ConsequentialRiskCodeAmbiguousV1          = "consequential_risk_ambiguous"
	ConsequentialRiskCodeUntrustedMetadataV1  = "consequential_risk_untrusted_metadata"
	ConsequentialRiskCodeSensitiveMetadataV1  = "consequential_risk_sensitive_metadata"
	ConsequentialRiskCodeMissingGrantV1       = "consequential_risk_confirmation_required"
	ConsequentialRiskCodeGrantMismatchV1      = "consequential_risk_confirmation_mismatch"
)

type ConsequentialRiskPreflightResultV1 struct {
	Status      ConsequentialRiskPreflightStatusV1
	Draft       *ConsequentialRiskDraftV1
	FailureCode string
}

// ConsequentialRiskPreflighterV1 is implemented only by tools that can derive
// consequential intent from a current typed, trusted observation. Model prose
// is never part of this interface's authority.
type ConsequentialRiskPreflighterV1 interface {
	PreflightConsequentialRiskV1(context.Context, string, string) (ConsequentialRiskPreflightResultV1, error)
}

func EqualConsequentialRiskDraftV1(a, b ConsequentialRiskDraftV1) bool {
	if a.RequestID != b.RequestID || a.Kind != b.Kind ||
		!equalConsequentialRiskTargetV1(a.Target, b.Target) {
		return false
	}
	return equalConsequentialSendDetailV1(a.Send, b.Send) &&
		equalConsequentialDeleteDetailV1(a.Delete, b.Delete) &&
		equalConsequentialPurchaseDetailV1(a.Purchase, b.Purchase)
}

func equalConsequentialRiskTargetV1(a, b ConsequentialRiskTargetAuthorityV1) bool {
	aCoordinate, bCoordinate := a.CoordinateAuthority, b.CoordinateAuthority
	a.CoordinateAuthority, b.CoordinateAuthority = nil, nil
	if a != b || aCoordinate == nil != (bCoordinate == nil) {
		return false
	}
	return aCoordinate == nil || *aCoordinate == *bCoordinate
}

func equalConsequentialSendDetailV1(a, b *ConsequentialSendDetailV1) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func equalConsequentialDeleteDetailV1(a, b *ConsequentialDeleteDetailV1) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func equalConsequentialPurchaseDetailV1(a, b *ConsequentialPurchaseDetailV1) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

var consequentialRiskCoordinateAuthorityWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"element_path":       coordinateScalarWireShape(false),
	"frame_id":           coordinateScalarWireShape(false),
	"frame_expires_at":   coordinateScalarWireShape(false),
	"final_image_sha256": coordinateScalarWireShape(false),
	"topology_ref":       coordinateTopologyRefWireShapeV1,
	"helper_boot_id":     coordinateScalarWireShape(false),
	"display_id":         coordinateScalarWireShape(false),
	"source_pixel": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"x": coordinateScalarWireShape(false),
		"y": coordinateScalarWireShape(false),
	}),
	"quartz_point": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"x": coordinateScalarWireShape(false),
		"y": coordinateScalarWireShape(false),
	}),
})

var consequentialRiskTargetWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"bundle_id":            coordinateScalarWireShape(false),
	"app_name":             coordinateScalarWireShape(false),
	"pid":                  coordinateScalarWireShape(false),
	"window_id":            coordinateScalarWireShape(false),
	"state_id":             coordinateScalarWireShape(false),
	"action_kind":          coordinateScalarWireShape(false),
	"execution_path":       coordinateScalarWireShape(false),
	"element_ref":          coordinateScalarWireShape(false),
	"role":                 coordinateScalarWireShape(false),
	"fingerprint":          coordinateScalarWireShape(false),
	"coordinate_authority": coordinateNullableWireShape(consequentialRiskCoordinateAuthorityWireShapeV1),
	"target_digest":        coordinateScalarWireShape(false),
})

var consequentialRiskIntentWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version": coordinateScalarWireShape(false),
	"intent_id":      coordinateScalarWireShape(false),
	"request_id":     coordinateScalarWireShape(false),
	"kind":           coordinateScalarWireShape(false),
	"expires_at":     coordinateScalarWireShape(false),
	"target":         consequentialRiskTargetWireShapeV1,
	// V1 uses one uniform representation: all detail members are required,
	// exactly one is an object, and the two inactive members are JSON null.
	"send": coordinateNullableWireShape(coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"destination_kind":  coordinateScalarWireShape(false),
		"destination_label": coordinateScalarWireShape(false),
		"payload_kind":      coordinateScalarWireShape(false),
	})),
	"delete": coordinateNullableWireShape(coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"object_kind":  coordinateScalarWireShape(false),
		"object_label": coordinateScalarWireShape(false),
		"scope":        coordinateScalarWireShape(false),
	})),
	"purchase": coordinateNullableWireShape(coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"merchant_label": coordinateScalarWireShape(false),
		"item_label":     coordinateScalarWireShape(false),
		"amount_minor":   coordinateScalarWireShape(false),
		"currency":       coordinateScalarWireShape(false),
	})),
})

var consequentialRiskMarkerWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version": coordinateScalarWireShape(false),
	"required":       coordinateScalarWireShape(false),
	"kind":           coordinateScalarWireShape(false),
	"intent_id":      coordinateScalarWireShape(false),
	"expires_at":     coordinateScalarWireShape(false),
})

func (intent ConsequentialRiskIntentV1) Validate(now time.Time) error {
	if intent.SchemaVersion != 1 {
		return fmt.Errorf("unsupported consequential risk intent schema_version %d", intent.SchemaVersion)
	}
	if err := validateConsequentialRiskIntentIDV1(intent.IntentID); err != nil {
		return err
	}
	if !consequentialRiskRequestIDPatternV1.MatchString(intent.RequestID) {
		return fmt.Errorf("consequential risk request_id must be an opaque bounded identifier")
	}
	if !validConsequentialRiskKindV1(intent.Kind) {
		return fmt.Errorf("unsupported consequential risk kind %q", intent.Kind)
	}
	if err := validateConsequentialRiskExpiryV1(intent.ExpiresAt, now); err != nil {
		return err
	}
	if err := intent.Target.Validate(); err != nil {
		return err
	}
	if coordinate := intent.Target.CoordinateAuthority; coordinate != nil {
		intentExpiry, _ := time.Parse(time.RFC3339, intent.ExpiresAt)
		frameExpiry, _ := time.Parse(time.RFC3339Nano, coordinate.FrameExpiresAt)
		if intentExpiry.After(frameExpiry) {
			return fmt.Errorf("consequential risk intent outlives its coordinate frame")
		}
	}

	switch intent.Kind {
	case "send":
		if intent.Send == nil || intent.Delete != nil || intent.Purchase != nil {
			return fmt.Errorf("send risk intent requires only send detail")
		}
		if err := intent.Send.Validate(); err != nil {
			return err
		}
	case "delete":
		if intent.Send != nil || intent.Delete == nil || intent.Purchase != nil {
			return fmt.Errorf("delete risk intent requires only delete detail")
		}
		if err := intent.Delete.Validate(); err != nil {
			return err
		}
	case "purchase":
		if intent.Send != nil || intent.Delete != nil || intent.Purchase == nil {
			return fmt.Errorf("purchase risk intent requires only purchase detail")
		}
		if err := intent.Purchase.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (target ConsequentialRiskTargetAuthorityV1) Validate() error {
	if err := target.validateWithoutDigest(); err != nil {
		return err
	}
	if !consequentialRiskDigestPatternV1.MatchString(target.TargetDigest) {
		return fmt.Errorf("consequential risk target_digest is invalid")
	}
	want, err := ComputeConsequentialRiskTargetDigestV1(target)
	if err != nil {
		return err
	}
	if target.TargetDigest != want {
		return fmt.Errorf("consequential risk target_digest does not bind the exact target")
	}
	return nil
}

func (target ConsequentialRiskTargetAuthorityV1) validateWithoutDigest() error {
	if !consequentialRiskBundleIDPatternV1.MatchString(target.BundleID) {
		return fmt.Errorf("consequential risk target bundle_id is invalid")
	}
	if err := validateConsequentialRiskLabelV1("app_name", target.AppName); err != nil {
		return err
	}
	if target.PID <= 0 || int64(target.PID) > math.MaxInt32 || target.WindowID == 0 {
		return fmt.Errorf("consequential risk target pid/window authority is invalid")
	}
	if !consequentialRiskStateIDPatternV1.MatchString(target.StateID) {
		return fmt.Errorf("consequential risk target state_id is invalid")
	}
	if target.ActionKind != "press" && target.ActionKind != "click" {
		return fmt.Errorf("consequential risk target action_kind is invalid")
	}
	if target.ExecutionPath != "accessibility" && target.ExecutionPath != "synthetic_coordinate" {
		return fmt.Errorf("consequential risk target execution_path is invalid")
	}
	if target.ExecutionPath == "accessibility" && target.CoordinateAuthority != nil {
		return fmt.Errorf("accessibility consequential target cannot carry coordinate authority")
	}
	if target.ExecutionPath == "synthetic_coordinate" {
		if target.ActionKind != "click" || target.CoordinateAuthority == nil {
			return fmt.Errorf("synthetic consequential target must be a coordinate-bound click")
		}
		if err := target.CoordinateAuthority.Validate(); err != nil {
			return err
		}
	}
	if !validComputerUseRef(target.ElementRef) || !consequentialRiskRolePatternV1.MatchString(target.Role) ||
		!consequentialRiskFingerprintV1.MatchString(target.Fingerprint) {
		return fmt.Errorf("consequential risk target element authority is invalid")
	}
	return nil
}

func (authority ConsequentialRiskCoordinateAuthorityV1) Validate() error {
	if !consequentialRiskElementPathV1.MatchString(authority.ElementPath) || len(authority.ElementPath) > 512 {
		return fmt.Errorf("consequential risk coordinate element_path is invalid")
	}
	if !consequentialRiskFrameIDPatternV1.MatchString(authority.FrameID) ||
		!validLowerHexSHA256(authority.FinalImageSHA256) {
		return fmt.Errorf("consequential risk coordinate frame/image authority is invalid")
	}
	frameExpiry, err := time.Parse(time.RFC3339Nano, authority.FrameExpiresAt)
	if err != nil || frameExpiry.Location() != time.UTC {
		return fmt.Errorf("consequential risk coordinate frame expiry is invalid")
	}
	if err := validateConsequentialRiskOpaqueAuthorityV1("topology_id", authority.TopologyRef.TopologyID); err != nil {
		return err
	}
	if authority.TopologyRef.Generation == 0 {
		return fmt.Errorf("consequential risk coordinate topology generation is invalid")
	}
	if err := validateConsequentialRiskOpaqueAuthorityV1("helper_boot_id", authority.HelperBootID); err != nil {
		return err
	}
	if authority.DisplayID == 0 || authority.SourcePixel.X < 0 || authority.SourcePixel.Y < 0 ||
		int64(authority.SourcePixel.X) > math.MaxInt32 || int64(authority.SourcePixel.Y) > math.MaxInt32 ||
		math.IsNaN(authority.QuartzPoint.X) || math.IsInf(authority.QuartzPoint.X, 0) ||
		math.IsNaN(authority.QuartzPoint.Y) || math.IsInf(authority.QuartzPoint.Y, 0) {
		return fmt.Errorf("consequential risk coordinate point authority is invalid")
	}
	return nil
}

func validateConsequentialRiskOpaqueAuthorityV1(field, value string) error {
	if !consequentialRiskRequestIDPatternV1.MatchString(value) {
		return fmt.Errorf("consequential risk coordinate %s is invalid", field)
	}
	return nil
}

func (detail ConsequentialSendDetailV1) Validate() error {
	if detail.DestinationKind != "conversation" && detail.DestinationKind != "recipient" {
		return fmt.Errorf("consequential send destination_kind is invalid")
	}
	if err := validateConsequentialRiskLabelV1("destination_label", detail.DestinationLabel); err != nil {
		return err
	}
	if detail.PayloadKind != "current_composer" {
		return fmt.Errorf("consequential send payload_kind must be current_composer")
	}
	return nil
}

func (detail ConsequentialDeleteDetailV1) Validate() error {
	switch detail.ObjectKind {
	case "message", "file", "event", "other":
	default:
		return fmt.Errorf("consequential delete object_kind is invalid")
	}
	if err := validateConsequentialRiskLabelV1("object_label", detail.ObjectLabel); err != nil {
		return err
	}
	if detail.Scope != "single_visible_object" {
		return fmt.Errorf("consequential delete scope must be single_visible_object")
	}
	return nil
}

func (detail ConsequentialPurchaseDetailV1) Validate() error {
	if err := validateConsequentialRiskLabelV1("merchant_label", detail.MerchantLabel); err != nil {
		return err
	}
	if err := validateConsequentialRiskLabelV1("item_label", detail.ItemLabel); err != nil {
		return err
	}
	if detail.AmountMinor <= 0 || detail.AmountMinor > consequentialRiskAmountMaxMinorV1 {
		return fmt.Errorf("consequential purchase amount_minor is out of bounds")
	}
	if !consequentialRiskCurrencyPatternV1.MatchString(detail.Currency) {
		return fmt.Errorf("consequential purchase currency must be three uppercase ASCII letters")
	}
	return nil
}

func (marker ConsequentialRiskMarkerV1) Validate(now time.Time) error {
	if marker.SchemaVersion != 1 || !marker.Required {
		return fmt.Errorf("invalid consequential risk marker schema/required tuple")
	}
	if !validConsequentialRiskKindV1(marker.Kind) {
		return fmt.Errorf("unsupported consequential risk marker kind %q", marker.Kind)
	}
	if err := validateConsequentialRiskIntentIDV1(marker.IntentID); err != nil {
		return err
	}
	return validateConsequentialRiskExpiryV1(marker.ExpiresAt, now)
}

func (intent ConsequentialRiskIntentV1) PersistentMarker(now time.Time) (ConsequentialRiskMarkerV1, error) {
	if err := intent.Validate(now); err != nil {
		return ConsequentialRiskMarkerV1{}, err
	}
	marker := ConsequentialRiskMarkerV1{
		SchemaVersion: 1,
		Required:      true,
		Kind:          intent.Kind,
		IntentID:      intent.IntentID,
		ExpiresAt:     intent.ExpiresAt,
	}
	if err := marker.Validate(now); err != nil {
		return ConsequentialRiskMarkerV1{}, err
	}
	return marker, nil
}

func DecodeConsequentialRiskIntentV1(payload []byte, now time.Time) (ConsequentialRiskIntentV1, error) {
	if err := validateCoordinateWireShape(
		"consequential risk intent v1", payload, consequentialRiskIntentWireShapeV1); err != nil {
		return ConsequentialRiskIntentV1{}, err
	}
	var intent ConsequentialRiskIntentV1
	if err := decodeStrictCoordinateJSON(payload, &intent); err != nil {
		return ConsequentialRiskIntentV1{}, fmt.Errorf("decode consequential risk intent v1: %w", err)
	}
	if err := intent.Validate(now); err != nil {
		return ConsequentialRiskIntentV1{}, err
	}
	return intent, nil
}

func EncodeConsequentialRiskIntentV1(intent ConsequentialRiskIntentV1, now time.Time) ([]byte, error) {
	if err := intent.Validate(now); err != nil {
		return nil, err
	}
	return marshalCanonicalConsequentialRiskV1(intent)
}

func DecodeConsequentialRiskMarkerV1(payload []byte, now time.Time) (ConsequentialRiskMarkerV1, error) {
	if err := validateCoordinateWireShape(
		"consequential risk marker v1", payload, consequentialRiskMarkerWireShapeV1); err != nil {
		return ConsequentialRiskMarkerV1{}, err
	}
	var marker ConsequentialRiskMarkerV1
	if err := decodeStrictCoordinateJSON(payload, &marker); err != nil {
		return ConsequentialRiskMarkerV1{}, fmt.Errorf("decode consequential risk marker v1: %w", err)
	}
	if err := marker.Validate(now); err != nil {
		return ConsequentialRiskMarkerV1{}, err
	}
	return marker, nil
}

func EncodeConsequentialRiskMarkerV1(marker ConsequentialRiskMarkerV1, now time.Time) ([]byte, error) {
	if err := marker.Validate(now); err != nil {
		return nil, err
	}
	return marshalCanonicalConsequentialRiskV1(marker)
}

func marshalCanonicalConsequentialRiskV1(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// ComputeConsequentialRiskTargetDigestV1 returns a versioned SHA-256 binding
// over the exact target authority. The domain separator and ordered JSON
// struct are the v1 canonicalization contract. The digest is process-local;
// PersistentMarker intentionally has nowhere to store it.
func ComputeConsequentialRiskTargetDigestV1(target ConsequentialRiskTargetAuthorityV1) (string, error) {
	if err := target.validateWithoutDigest(); err != nil {
		return "", err
	}
	canonical := struct {
		BundleID            string                                  `json:"bundle_id"`
		AppName             string                                  `json:"app_name"`
		PID                 int                                     `json:"pid"`
		WindowID            uint32                                  `json:"window_id"`
		StateID             string                                  `json:"state_id"`
		ActionKind          string                                  `json:"action_kind"`
		ExecutionPath       string                                  `json:"execution_path"`
		ElementRef          string                                  `json:"element_ref"`
		Role                string                                  `json:"role"`
		Fingerprint         string                                  `json:"fingerprint"`
		CoordinateAuthority *ConsequentialRiskCoordinateAuthorityV1 `json:"coordinate_authority"`
	}{
		BundleID: target.BundleID, AppName: target.AppName, PID: target.PID,
		WindowID: target.WindowID, StateID: target.StateID, ActionKind: target.ActionKind,
		ExecutionPath: target.ExecutionPath, ElementRef: target.ElementRef,
		Role: target.Role, Fingerprint: target.Fingerprint,
		CoordinateAuthority: target.CoordinateAuthority,
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("kocoro.consequential-risk-target.v1\x00"))
	_, _ = hasher.Write(payload)
	return "tdv1_" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateConsequentialRiskIntentIDV1(value string) error {
	if !strings.HasPrefix(value, "cri_") {
		return fmt.Errorf("consequential risk intent_id must have opaque random v1 shape")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(value, "cri_"))
	if err != nil || len(raw) != 16 {
		return fmt.Errorf("consequential risk intent_id must encode exactly 128 random bits")
	}
	return nil
}

func validateConsequentialRiskExpiryV1(value string, now time.Time) error {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return fmt.Errorf("consequential risk expires_at must be canonical RFC3339 UTC seconds: %w", err)
	}
	if parsed.Location() != time.UTC || parsed.Format(time.RFC3339) != value {
		return fmt.Errorf("consequential risk expires_at must be canonical RFC3339 UTC seconds")
	}
	now = now.UTC()
	if !parsed.After(now) {
		return fmt.Errorf("consequential risk intent is expired")
	}
	if parsed.Sub(now) > consequentialRiskIntentMaxFutureV1 {
		return fmt.Errorf("consequential risk expiry exceeds the v1 five-minute bound")
	}
	return nil
}

func validateConsequentialRiskLabelV1(field, value string) error {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		utf8.RuneCountInString(value) > consequentialRiskLabelMaxRunesV1 {
		return fmt.Errorf("consequential risk %s is empty, untrimmed, invalid UTF-8, or too long", field)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf) {
			return fmt.Errorf("consequential risk %s contains a control or format character", field)
		}
	}
	return nil
}

func validConsequentialRiskKindV1(kind string) bool {
	return kind == "send" || kind == "delete" || kind == "purchase"
}
