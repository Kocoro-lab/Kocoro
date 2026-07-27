package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

var consequentialRiskPurchaseAmountV1 = regexp.MustCompile(`(?i)\b(USD|EUR|GBP)\s+([0-9]{1,10})\.([0-9]{2})\b`)

func mustMarshalComputerUseArgsV1(args computerUseArgs) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func consequentialRiskToolFailureV1(code string) agent.ToolResult {
	result := agent.BusinessError("computer-use consequential action was blocked by exact local confirmation policy")
	result.GUIOutcome = &agent.GUIActionOutcome{
		Result: agent.GUIActionResultFailed, Phase: agent.GUIActionPhaseActing, FailureCode: code,
	}
	return result
}

type consequentialRiskSemanticKindV1 uint8

const (
	consequentialRiskSemanticNoneV1 consequentialRiskSemanticKindV1 = iota
	consequentialRiskSemanticSendV1
	consequentialRiskSemanticDeleteV1
	consequentialRiskSemanticPurchaseV1
	consequentialRiskSemanticAmbiguousV1
)

var consequentialRiskExactLabelsV1 = map[string]consequentialRiskSemanticKindV1{
	"send": consequentialRiskSemanticSendV1, "post": consequentialRiskSemanticSendV1,
	"publish": consequentialRiskSemanticSendV1, "发送": consequentialRiskSemanticSendV1,
	"發送": consequentialRiskSemanticSendV1, "发布": consequentialRiskSemanticSendV1,
	"發佈": consequentialRiskSemanticSendV1, "送信": consequentialRiskSemanticSendV1,
	"投稿": consequentialRiskSemanticSendV1, "公開": consequentialRiskSemanticSendV1,
	"delete": consequentialRiskSemanticDeleteV1, "remove": consequentialRiskSemanticDeleteV1,
	"trash": consequentialRiskSemanticDeleteV1,
	"删除":    consequentialRiskSemanticDeleteV1, "刪除": consequentialRiskSemanticDeleteV1,
	"削除": consequentialRiskSemanticDeleteV1, "消去": consequentialRiskSemanticDeleteV1,
	"purchase": consequentialRiskSemanticPurchaseV1, "pay": consequentialRiskSemanticPurchaseV1,
	"buy":   consequentialRiskSemanticPurchaseV1,
	"order": consequentialRiskSemanticPurchaseV1, "购买": consequentialRiskSemanticPurchaseV1,
	"購買": consequentialRiskSemanticPurchaseV1, "支付": consequentialRiskSemanticPurchaseV1,
	"付款": consequentialRiskSemanticPurchaseV1, "購入": consequentialRiskSemanticPurchaseV1,
	"支払う": consequentialRiskSemanticPurchaseV1, "注文": consequentialRiskSemanticPurchaseV1,
}

var consequentialRiskTokenLabelsV1 = map[string]consequentialRiskSemanticKindV1{
	"send": consequentialRiskSemanticSendV1, "sending": consequentialRiskSemanticSendV1,
	"sent": consequentialRiskSemanticSendV1, "post": consequentialRiskSemanticSendV1,
	"publish": consequentialRiskSemanticSendV1, "submit": consequentialRiskSemanticSendV1,
	"resend": consequentialRiskSemanticSendV1, "reply": consequentialRiskSemanticSendV1,
	"delete": consequentialRiskSemanticDeleteV1, "remove": consequentialRiskSemanticDeleteV1,
	"erase": consequentialRiskSemanticDeleteV1, "discard": consequentialRiskSemanticDeleteV1,
	"trash":    consequentialRiskSemanticDeleteV1,
	"purchase": consequentialRiskSemanticPurchaseV1, "pay": consequentialRiskSemanticPurchaseV1,
	"buy":   consequentialRiskSemanticPurchaseV1,
	"order": consequentialRiskSemanticPurchaseV1, "checkout": consequentialRiskSemanticPurchaseV1,
}

var consequentialRiskCJKLabelsV1 = map[string]consequentialRiskSemanticKindV1{
	"发送": consequentialRiskSemanticSendV1, "發送": consequentialRiskSemanticSendV1,
	"发布": consequentialRiskSemanticSendV1, "發佈": consequentialRiskSemanticSendV1,
	"送信": consequentialRiskSemanticSendV1, "投稿": consequentialRiskSemanticSendV1,
	"公開": consequentialRiskSemanticSendV1, "回复": consequentialRiskSemanticSendV1,
	"回覆": consequentialRiskSemanticSendV1, "返信": consequentialRiskSemanticSendV1,
	"提交": consequentialRiskSemanticSendV1,
	"删除": consequentialRiskSemanticDeleteV1, "刪除": consequentialRiskSemanticDeleteV1,
	"削除": consequentialRiskSemanticDeleteV1, "消去": consequentialRiskSemanticDeleteV1,
	"购买": consequentialRiskSemanticPurchaseV1, "購買": consequentialRiskSemanticPurchaseV1,
	"支付": consequentialRiskSemanticPurchaseV1, "付款": consequentialRiskSemanticPurchaseV1,
	"購入": consequentialRiskSemanticPurchaseV1, "支払": consequentialRiskSemanticPurchaseV1,
	"注文": consequentialRiskSemanticPurchaseV1,
}

func normalizeConsequentialRiskLabelV1(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func consequentialRiskTokenKindsV1(value string) []consequentialRiskSemanticKindV1 {
	var normalized strings.Builder
	var previous rune
	for index, current := range value {
		if index > 0 && unicode.IsLower(previous) && unicode.IsUpper(current) {
			normalized.WriteRune(' ')
		}
		if unicode.IsLetter(current) || unicode.IsDigit(current) {
			normalized.WriteRune(unicode.ToLower(current))
		} else {
			normalized.WriteRune(' ')
		}
		previous = current
	}
	var kinds []consequentialRiskSemanticKindV1
	for _, token := range strings.Fields(normalized.String()) {
		if kind, found := consequentialRiskTokenLabelsV1[token]; found {
			kinds = append(kinds, kind)
		}
	}
	compact := strings.ReplaceAll(normalizeConsequentialRiskLabelV1(value), " ", "")
	for token, kind := range consequentialRiskCJKLabelsV1 {
		if strings.Contains(compact, token) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func classifyConsequentialRiskLabelsV1(values ...*string) consequentialRiskSemanticKindV1 {
	kind := consequentialRiskSemanticNoneV1
	for _, value := range values {
		if value == nil {
			continue
		}
		var candidates []consequentialRiskSemanticKindV1
		if candidate, found := consequentialRiskExactLabelsV1[normalizeConsequentialRiskLabelV1(*value)]; found {
			candidates = append(candidates, candidate)
		}
		candidates = append(candidates, consequentialRiskTokenKindsV1(*value)...)
		for _, candidate := range candidates {
			if kind != consequentialRiskSemanticNoneV1 && kind != candidate {
				return consequentialRiskSemanticAmbiguousV1
			}
			kind = candidate
		}
	}
	return kind
}

func genericConsequentialDestinationV1(window, app string) bool {
	window = normalizeConsequentialRiskLabelV1(window)
	app = normalizeConsequentialRiskLabelV1(app)
	if window == "" || window == app {
		return true
	}
	switch window {
	case "slack", "messages", "mail", "home", "untitled", "无标题", "未命名", "名称未設定":
		return true
	default:
		return false
	}
}

func (t *ComputerUseTool) PreflightConsequentialRiskV1(
	ctx context.Context,
	argsJSON string,
	requestID string,
) (ConsequentialRiskPreflightResultV1, error) {
	var args computerUseArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ConsequentialRiskPreflightResultV1{}, err
	}
	if computerUseKeyboardNeedsExactIntentV1(args) {
		if t.allowsLocationNavigationCommitV1(args) {
			return ConsequentialRiskPreflightResultV1{
				Status: ConsequentialRiskPreflightNoneV1,
			}, nil
		}
		if _, ok := t.ordinaryKeyboardFocusWitnessV1(args); ok {
			return ConsequentialRiskPreflightResultV1{
				Status: ConsequentialRiskPreflightNoneV1,
			}, nil
		}
		return ConsequentialRiskPreflightResultV1{
			Status:      ConsequentialRiskPreflightBlockedV1,
			FailureCode: ConsequentialRiskCodeUnsupportedPathV1,
		}, nil
	}
	if args.Ref == "" {
		return t.preflightCoordinateConsequentialRiskV1(ctx, args, requestID)
	}
	if args.Ref == "" || t.snapshot == nil || args.StateID == "" || args.StateID != t.snapshot.id {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightNoneV1}, nil
	}
	entry, exists := t.refs[args.Ref]
	if !exists || entry.fingerprint == "" {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightNoneV1}, nil
	}
	element, err := resolveComputerUseFingerprint(t.snapshot.elements, entry.fingerprint)
	if err != nil {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightBlockedV1, FailureCode: ConsequentialRiskCodeUntrustedMetadataV1}, nil
	}
	for _, label := range []*string{element.Title, element.Description, element.Desc, element.Identifier} {
		if label != nil && (validateConsequentialRiskLabelV1("semantic_label", *label) != nil ||
			strings.IndexFunc(*label, unicode.IsControl) >= 0) {
			return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightBlockedV1, FailureCode: ConsequentialRiskCodeSensitiveMetadataV1}, nil
		}
	}
	kind := classifyConsequentialRiskLabelsV1(element.Title, element.Description, element.Desc, element.Identifier)
	if kind == consequentialRiskSemanticNoneV1 {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightNoneV1}, nil
	}
	blocked := func(code string) (ConsequentialRiskPreflightResultV1, error) {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightBlockedV1, FailureCode: code}, nil
	}
	if kind == consequentialRiskSemanticAmbiguousV1 {
		return blocked(ConsequentialRiskCodeAmbiguousV1)
	}
	if !t.snapshot.typed || element.Enabled == nil || !*element.Enabled || element.Role != "AXButton" ||
		entry.path == "" || entry.role != element.Role || entry.fingerprint != element.Fingerprint ||
		t.snapshot.bundleID == "" || t.snapshot.pid <= 0 || t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	hasPress := false
	for _, action := range element.Actions {
		if action == "AXPress" {
			hasPress = true
		}
	}
	if !hasPress {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	if kind != consequentialRiskSemanticSendV1 {
		return blocked(ConsequentialRiskCodeUnsupportedKindV1)
	}
	if args.Action != "press" {
		return blocked(ConsequentialRiskCodeUnsupportedPathV1)
	}
	if genericConsequentialDestinationV1(t.snapshot.window, t.snapshot.app) {
		return blocked(ConsequentialRiskCodeDestinationUnknownV1)
	}
	if validateConsequentialRiskLabelV1("destination_label", t.snapshot.window) != nil ||
		validateConsequentialRiskLabelV1("app_name", t.snapshot.app) != nil ||
		strings.IndexFunc(t.snapshot.window, unicode.IsControl) >= 0 {
		return blocked(ConsequentialRiskCodeSensitiveMetadataV1)
	}
	if !consequentialRiskRequestIDPatternV1.MatchString(requestID) {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	target := ConsequentialRiskTargetAuthorityV1{
		BundleID: t.snapshot.bundleID, AppName: t.snapshot.app, PID: t.snapshot.pid,
		WindowID: uint32(*t.snapshot.windowID), StateID: t.snapshot.id,
		ActionKind: "press", ExecutionPath: "accessibility", ElementRef: args.Ref,
		Role: entry.role, Fingerprint: entry.fingerprint,
	}
	digest, err := ComputeConsequentialRiskTargetDigestV1(target)
	if err != nil {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	target.TargetDigest = digest
	draft := &ConsequentialRiskDraftV1{
		RequestID: requestID, Kind: "send", Target: target,
		Send: &ConsequentialSendDetailV1{
			DestinationKind: "conversation", DestinationLabel: t.snapshot.window,
			PayloadKind: "current_composer",
		},
	}
	return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightRequiredV1, Draft: draft}, nil
}

type computerUseKeyboardFocusWitnessV1 struct {
	path string
	role string
}

func (t *ComputerUseTool) ordinaryKeyboardFocusWitnessV1(
	args computerUseArgs,
) (computerUseKeyboardFocusWitnessV1, bool) {
	if !computerUseKeyboardMayUseFocusedWitnessV1(args) ||
		t == nil || t.snapshot == nil || !t.snapshot.typed ||
		!validComputerUseRef(t.snapshot.focusedRef) {
		return computerUseKeyboardFocusWitnessV1{}, false
	}
	entry, exists := t.refs[t.snapshot.focusedRef]
	if !exists || entry.path == "" || entry.role == "" ||
		entry.fingerprint == "" {
		return computerUseKeyboardFocusWitnessV1{}, false
	}
	element, err := resolveComputerUseFingerprint(
		t.snapshot.elements,
		entry.fingerprint,
	)
	if err != nil ||
		element.Ref != t.snapshot.focusedRef ||
		entry.role != element.Role ||
		entry.fingerprint != element.Fingerprint ||
		!computerUseKeyboardFocusRoleAllowedV1(args, element) ||
		computerUseFocusedKeyboardElementConsequentialV1(element) {
		return computerUseKeyboardFocusWitnessV1{}, false
	}
	return computerUseKeyboardFocusWitnessV1{
		path: entry.path,
		role: entry.role,
	}, true
}

func computerUseKeyboardFocusRoleAllowedV1(
	args computerUseArgs,
	element computerUseElement,
) bool {
	if element.Enabled != nil && !*element.Enabled {
		return false
	}
	switch element.Role {
	case "AXTextField", "AXTextArea", "AXComboBox", "AXButton":
		return true
	case "AXCheckBox", "AXRadioButton":
		return !computerUsePlainReturnKeypressV1(args)
	default:
		return false
	}
}

func computerUseFocusedKeyboardElementConsequentialV1(
	element computerUseElement,
) bool {
	if classifyConsequentialRiskLabelsV1(
		element.Title,
		element.Description,
		element.Desc,
		element.Identifier,
	) != consequentialRiskSemanticNoneV1 {
		return true
	}
	for _, label := range []*string{
		element.Title,
		element.Description,
		element.Desc,
		element.Identifier,
	} {
		if label == nil {
			continue
		}
		if validateConsequentialRiskLabelV1("semantic_label", *label) != nil ||
			strings.IndexFunc(*label, unicode.IsControl) >= 0 {
			return true
		}
		normalized := normalizeConsequentialRiskLabelV1(*label)
		for _, marker := range []string{
			"message", "composer", "chat", "reply", "comment",
		} {
			if strings.Contains(normalized, marker) {
				return true
			}
		}
	}
	return false
}

type consequentialRiskCoordinateHitV1 struct {
	element computerUseElement
	entry   refEntry
	kind    consequentialRiskSemanticKindV1
	depth   int
}

func (t *ComputerUseTool) preflightCoordinateConsequentialRiskV1(
	ctx context.Context,
	args computerUseArgs,
	requestID string,
) (ConsequentialRiskPreflightResultV1, error) {
	if args.Action != "click" || args.X == nil || args.Y == nil || t.snapshot == nil ||
		args.StateID == "" || args.StateID != t.snapshot.id || t.coordinateArtifact == nil {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightNoneV1}, nil
	}
	frame := t.coordinateArtifact.Frame()
	if frame.StateID != args.StateID {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightBlockedV1,
			FailureCode: ConsequentialRiskCodeUntrustedMetadataV1}, nil
	}
	topology, err := ReadDisplayTopologyV1(ctx, t.client)
	if err != nil {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightBlockedV1,
			FailureCode: ConsequentialRiskCodeUntrustedMetadataV1}, nil
	}
	mapped, err := MapCoordinatePixelCenterV1(
		frame,
		CoordinateTopologyRefV1{TopologyID: topology.TopologyID, Generation: topology.Generation},
		args.StateID,
		frame.FrameID,
		t.computerUseCoordinateNowV1(),
		float64(*args.X),
		float64(*args.Y),
	)
	if err != nil {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightBlockedV1,
			FailureCode: ConsequentialRiskCodeUntrustedMetadataV1}, nil
	}
	return t.preflightMappedCoordinateConsequentialRiskV1(
		args, requestID, frame, topology, mapped)
}

func (t *ComputerUseTool) preflightMappedCoordinateConsequentialRiskV1(
	args computerUseArgs,
	requestID string,
	frame CoordinateFrameV1,
	topology DisplayTopologyV1,
	mapped CoordinateMappedPointV1,
) (ConsequentialRiskPreflightResultV1, error) {
	blocked := func(code string) (ConsequentialRiskPreflightResultV1, error) {
		return ConsequentialRiskPreflightResultV1{
			Status: ConsequentialRiskPreflightBlockedV1, FailureCode: code,
		}, nil
	}
	hit, status := t.consequentialRiskHitAtQuartzPointV1(mapped.X, mapped.Y)
	switch status {
	case ConsequentialRiskPreflightNoneV1:
		return ConsequentialRiskPreflightResultV1{Status: status}, nil
	case ConsequentialRiskPreflightBlockedV1:
		return blocked(ConsequentialRiskCodeAmbiguousV1)
	}
	if hit == nil {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	button := args.Button
	if button == "" {
		button = "left"
	}
	clicks := int(args.Clicks)
	if clicks == 0 {
		clicks = 1
	}
	if args.Action != "click" || button != "left" || clicks != 1 {
		return blocked(ConsequentialRiskCodeUnsupportedPathV1)
	}
	if !t.snapshot.typed || hit.element.Enabled == nil || !*hit.element.Enabled ||
		hit.element.Role != "AXButton" || hit.entry.path == "" ||
		hit.entry.role != hit.element.Role || hit.entry.fingerprint != hit.element.Fingerprint ||
		t.snapshot.bundleID == "" || t.snapshot.pid <= 0 || t.snapshot.windowID == nil ||
		*t.snapshot.windowID <= 0 || frame.FinalImage.SHA256 == "" ||
		frame.FrameID == "" || topology.HelperBootID == "" {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	hasPress := false
	for _, action := range hit.element.Actions {
		if action == "AXPress" {
			hasPress = true
		}
	}
	if !hasPress || !consequentialRiskRequestIDPatternV1.MatchString(requestID) {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	if err := validateConsequentialRiskTrustedElementLabelsV1(hit.element); err != nil {
		return blocked(ConsequentialRiskCodeSensitiveMetadataV1)
	}
	if validateConsequentialRiskLabelV1("app_name", t.snapshot.app) != nil ||
		validateConsequentialRiskLabelV1("destination_label", t.snapshot.window) != nil {
		return blocked(ConsequentialRiskCodeSensitiveMetadataV1)
	}

	target := ConsequentialRiskTargetAuthorityV1{
		BundleID: t.snapshot.bundleID, AppName: t.snapshot.app, PID: t.snapshot.pid,
		WindowID: uint32(*t.snapshot.windowID), StateID: t.snapshot.id,
		ActionKind: "click", ExecutionPath: "synthetic_coordinate",
		ElementRef: hit.element.Ref, Role: hit.entry.role, Fingerprint: hit.entry.fingerprint,
		CoordinateAuthority: &ConsequentialRiskCoordinateAuthorityV1{
			ElementPath: hit.entry.path, FrameID: frame.FrameID,
			FrameExpiresAt: frame.ExpiresAt, FinalImageSHA256: frame.FinalImage.SHA256,
			TopologyRef: CoordinateTopologyRefV1{
				TopologyID: topology.TopologyID, Generation: topology.Generation,
			},
			HelperBootID: topology.HelperBootID, DisplayID: mapped.DisplayID,
			SourcePixel: ConsequentialRiskPixelPointV1{X: int(*args.X), Y: int(*args.Y)},
			QuartzPoint: ConsequentialRiskQuartzPointV1{X: mapped.X, Y: mapped.Y},
		},
	}
	digest, err := ComputeConsequentialRiskTargetDigestV1(target)
	if err != nil {
		return blocked(ConsequentialRiskCodeUntrustedMetadataV1)
	}
	target.TargetDigest = digest
	draft := &ConsequentialRiskDraftV1{RequestID: requestID, Target: target}
	switch hit.kind {
	case consequentialRiskSemanticSendV1:
		if genericConsequentialDestinationV1(t.snapshot.window, t.snapshot.app) {
			return blocked(ConsequentialRiskCodeDestinationUnknownV1)
		}
		draft.Kind = "send"
		draft.Send = &ConsequentialSendDetailV1{
			DestinationKind: "conversation", DestinationLabel: t.snapshot.window,
			PayloadKind: "current_composer",
		}
	case consequentialRiskSemanticDeleteV1:
		objectLabel, ok := trustedConsequentialRiskObjectLabelV1(hit.element)
		if !ok {
			return blocked(ConsequentialRiskCodeDestinationUnknownV1)
		}
		draft.Kind = "delete"
		draft.Delete = &ConsequentialDeleteDetailV1{
			ObjectKind: "other", ObjectLabel: objectLabel, Scope: "single_visible_object",
		}
	case consequentialRiskSemanticPurchaseV1:
		itemLabel, ok := trustedConsequentialRiskObjectLabelV1(hit.element)
		amountMinor, currency, amountOK := trustedConsequentialRiskPurchaseAmountV1(hit.element)
		if !ok || !amountOK || genericConsequentialDestinationV1(t.snapshot.window, t.snapshot.app) {
			return blocked(ConsequentialRiskCodeDestinationUnknownV1)
		}
		draft.Kind = "purchase"
		draft.Purchase = &ConsequentialPurchaseDetailV1{
			MerchantLabel: t.snapshot.window, ItemLabel: itemLabel,
			AmountMinor: amountMinor, Currency: currency,
		}
	default:
		return blocked(ConsequentialRiskCodeAmbiguousV1)
	}
	return ConsequentialRiskPreflightResultV1{
		Status: ConsequentialRiskPreflightRequiredV1, Draft: draft,
	}, nil
}

func (t *ComputerUseTool) consequentialRiskHitAtQuartzPointV1(
	x, y float64,
) (*consequentialRiskCoordinateHitV1, ConsequentialRiskPreflightStatusV1) {
	var candidates []consequentialRiskCoordinateHitV1
	invalidMetadata := false
	var walk func([]computerUseElement, int)
	walk = func(elements []computerUseElement, depth int) {
		for _, element := range elements {
			if element.Frame != nil && element.Frame.Width > 0 && element.Frame.Height > 0 &&
				x >= element.Frame.X && y >= element.Frame.Y &&
				x < element.Frame.X+element.Frame.Width && y < element.Frame.Y+element.Frame.Height {
				if validateConsequentialRiskTrustedElementLabelsV1(element) != nil {
					invalidMetadata = true
				} else {
					kind := classifyConsequentialRiskLabelsV1(
						element.Title, element.Description, element.Desc, element.Identifier)
					if kind != consequentialRiskSemanticNoneV1 {
						entry, exists := t.refs[element.Ref]
						if !exists || kind == consequentialRiskSemanticAmbiguousV1 {
							invalidMetadata = true
						} else {
							candidates = append(candidates, consequentialRiskCoordinateHitV1{
								element: element, entry: entry, kind: kind, depth: depth,
							})
						}
					}
				}
			}
			walk(element.Children, depth+1)
		}
	}
	walk(t.snapshot.elements, 0)
	if invalidMetadata {
		return nil, ConsequentialRiskPreflightBlockedV1
	}
	if len(candidates) == 0 {
		return nil, ConsequentialRiskPreflightNoneV1
	}
	bestDepth := candidates[0].depth
	for _, candidate := range candidates[1:] {
		if candidate.depth > bestDepth {
			bestDepth = candidate.depth
		}
	}
	var deepest []consequentialRiskCoordinateHitV1
	for _, candidate := range candidates {
		if candidate.depth == bestDepth {
			deepest = append(deepest, candidate)
		}
	}
	if len(deepest) != 1 {
		return nil, ConsequentialRiskPreflightBlockedV1
	}
	return &deepest[0], ConsequentialRiskPreflightRequiredV1
}

func validateConsequentialRiskTrustedElementLabelsV1(element computerUseElement) error {
	for _, label := range []*string{element.Title, element.Description, element.Desc, element.Identifier} {
		if label != nil && validateConsequentialRiskLabelV1("semantic_label", *label) != nil {
			return validateConsequentialRiskLabelV1("semantic_label", *label)
		}
	}
	return nil
}

func trustedConsequentialRiskObjectLabelV1(element computerUseElement) (string, bool) {
	for _, label := range []*string{element.Description, element.Desc} {
		if label == nil || validateConsequentialRiskLabelV1("object_label", *label) != nil {
			continue
		}
		if classifyConsequentialRiskLabelsV1(label) == consequentialRiskSemanticNoneV1 {
			return *label, true
		}
	}
	return "", false
}

func trustedConsequentialRiskPurchaseAmountV1(element computerUseElement) (int64, string, bool) {
	var amount int64
	var currency string
	found := false
	for _, label := range []*string{element.Title, element.Description, element.Desc, element.Identifier} {
		if label == nil {
			continue
		}
		for _, match := range consequentialRiskPurchaseAmountV1.FindAllStringSubmatch(*label, -1) {
			whole, err := strconv.ParseInt(match[2], 10, 64)
			if err != nil {
				return 0, "", false
			}
			fraction, err := strconv.ParseInt(match[3], 10, 64)
			if err != nil || whole > (consequentialRiskAmountMaxMinorV1-fraction)/100 {
				return 0, "", false
			}
			candidate := whole*100 + fraction
			candidateCurrency := strings.ToUpper(match[1])
			if found && (candidate != amount || candidateCurrency != currency) {
				return 0, "", false
			}
			amount, currency, found = candidate, candidateCurrency, true
		}
	}
	return amount, currency, found && amount > 0
}
