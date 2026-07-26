package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	OpenAIComputerCallTypeV1            = "computer_call"
	OpenAIComputerProviderV1            = "openai"
	OpenAIComputerCallStatusCompletedV1 = "completed"

	OpenAIComputerActionClickV1       = "click"
	OpenAIComputerActionDoubleClickV1 = "double_click"
	OpenAIComputerActionDragV1        = "drag"
	OpenAIComputerActionKeypressV1    = "keypress"
	OpenAIComputerActionMoveV1        = "move"
	OpenAIComputerActionScreenshotV1  = "screenshot"
	OpenAIComputerActionScrollV1      = "scroll"
	OpenAIComputerActionTypeTextV1    = "type"
	OpenAIComputerActionWaitV1        = "wait"
)

// OpenAIComputerPointV1 is one provider-image pixel center. It remains in the
// provider coordinate space until the authorized action executor binds it to
// Kocoro's immutable CoordinateFrame.
type OpenAIComputerPointV1 struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// OpenAIComputerActionV1 is the strict internal representation of one action
// from a Responses API computer_call. Only fields valid for Type are populated.
// Unknown action types and unknown per-action fields are rejected before the
// first action can acquire execution authority.
type OpenAIComputerActionV1 struct {
	Type string `json:"type"`

	Button  string                  `json:"button,omitempty"`
	X       *int                    `json:"x,omitempty"`
	Y       *int                    `json:"y,omitempty"`
	ScrollX *int                    `json:"scroll_x,omitempty"`
	ScrollY *int                    `json:"scroll_y,omitempty"`
	Text    string                  `json:"text,omitempty"`
	Keys    []string                `json:"keys,omitempty"`
	Path    []OpenAIComputerPointV1 `json:"path,omitempty"`
}

// OpenAIComputerActionPlanV1 is one daemon-private projection of a provider
// action onto Kocoro's guarded computer_use tool. Args may contain sensitive
// text and therefore must never be logged or copied into activity events.
// Images are forbidden at this boundary; the batch adapter emits one final
// exact screenshot after the ordered action list terminates.
type OpenAIComputerActionPlanV1 struct {
	Tool     agent.Tool
	Args     string
	Mutation bool
}

// OpenAIComputerCallV1 is the Cloud-normalized provider call. provider,
// api_surface, and tool_contract are transport-authored provenance rather than
// model arguments. Keeping them in this IR prevents the ambiguous public tool
// name "computer" from erasing which API surface owns call_id and actions[].
type OpenAIComputerCallV1 struct {
	Type                string                             `json:"type"`
	Provider            string                             `json:"provider"`
	APISurface          string                             `json:"api_surface"`
	ToolContract        string                             `json:"tool_contract"`
	ResponseID          string                             `json:"response_id"`
	CallID              string                             `json:"call_id"`
	Actions             []OpenAIComputerActionV1           `json:"actions"`
	PendingSafetyChecks []client.OpenAIComputerSafetyCheck `json:"pending_safety_checks"`
	Status              string                             `json:"status"`
}

type openAIComputerCallWireV1 struct {
	Type                string                             `json:"type"`
	Provider            string                             `json:"provider"`
	APISurface          string                             `json:"api_surface"`
	ToolContract        string                             `json:"tool_contract"`
	ResponseID          string                             `json:"response_id"`
	CallID              string                             `json:"call_id"`
	Actions             []json.RawMessage                  `json:"actions"`
	PendingSafetyChecks []client.OpenAIComputerSafetyCheck `json:"pending_safety_checks"`
	Status              string                             `json:"status"`
}

type openAIComputerExecutionProvenanceSealV1 struct{}

var trustedOpenAIComputerExecutionProvenanceSealV1 = &openAIComputerExecutionProvenanceSealV1{}

// OpenAIComputerExecutionProvenanceV1 is a process-local binding between one
// authenticated Cloud resolution and one Cloud-issued opaque continuation
// token. Its fields are intentionally private: JSON, config, and model output
// cannot mint the authority consumed by the daemon executor.
type OpenAIComputerExecutionProvenanceV1 struct {
	seal       *openAIComputerExecutionProvenanceSealV1
	profileID  string
	model      string
	responseID string
}

// NewOpenAIComputerExecutionProvenanceV1 admits only the exact production
// contract Kocoro understands. The profile must carry GatewayClient's private
// trusted-resolution seal; a completion echo decoded from JSON is insufficient.
func NewOpenAIComputerExecutionProvenanceV1(
	profile *client.ExecutionProfile,
	responseID string,
) (OpenAIComputerExecutionProvenanceV1, error) {
	if profile == nil || !profile.IsTrustedResolution() ||
		profile.Provider() != OpenAIComputerProviderV1 ||
		profile.APISurface() != client.APISurfaceOpenAIResponses ||
		profile.ExecutionMode() != client.ExecutionModeNativeComputer ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 ||
		!profile.SupportsImageInput() ||
		!profile.SupportsToolResultImages() ||
		profile.SupportsFunctionTools() ||
		!profile.SupportsBatchedActions() {
		return OpenAIComputerExecutionProvenanceV1{},
			fmt.Errorf("OpenAI computer execution profile is not a trusted supported contract")
	}
	if !validOpenAIComputerResponseIDV1(responseID) {
		return OpenAIComputerExecutionProvenanceV1{},
			fmt.Errorf("OpenAI computer response_id is missing or invalid")
	}
	return OpenAIComputerExecutionProvenanceV1{
		seal:       trustedOpenAIComputerExecutionProvenanceSealV1,
		profileID:  profile.ProfileID(),
		model:      profile.Model(),
		responseID: responseID,
	}, nil
}

func (p OpenAIComputerExecutionProvenanceV1) IsTrusted() bool {
	return p.seal == trustedOpenAIComputerExecutionProvenanceSealV1 &&
		p.profileID != "" && p.model != "" &&
		validOpenAIComputerResponseIDV1(p.responseID)
}

func (p OpenAIComputerExecutionProvenanceV1) ProfileID() string {
	if !p.IsTrusted() {
		return ""
	}
	return p.profileID
}

func (p OpenAIComputerExecutionProvenanceV1) Model() string {
	if !p.IsTrusted() {
		return ""
	}
	return p.model
}

func (p OpenAIComputerExecutionProvenanceV1) ResponseID() string {
	if !p.IsTrusted() {
		return ""
	}
	return p.responseID
}

func validOpenAIComputerResponseIDV1(value string) bool {
	return client.ValidOpenAIComputerContinuationToken(value)
}

// DecodeOpenAIComputerCallV1 accepts exactly one canonical call object. It
// validates the entire action list before returning, so a future/unknown action
// cannot appear after already-committed earlier actions.
func DecodeOpenAIComputerCallV1(payload []byte) (OpenAIComputerCallV1, error) {
	var wire openAIComputerCallWireV1
	if err := decodeOneStrictOpenAIComputerJSONV1(payload, &wire); err != nil {
		return OpenAIComputerCallV1{}, fmt.Errorf("decode OpenAI computer_call: %w", err)
	}
	call := OpenAIComputerCallV1{
		Type: wire.Type, Provider: wire.Provider, APISurface: wire.APISurface,
		ToolContract: wire.ToolContract, ResponseID: wire.ResponseID,
		CallID: wire.CallID, Status: wire.Status,
		PendingSafetyChecks: client.CloneOpenAIComputerSafetyChecks(
			wire.PendingSafetyChecks,
		),
	}
	if err := validateOpenAIComputerCallProvenanceV1(call, len(wire.Actions)); err != nil {
		return OpenAIComputerCallV1{}, err
	}
	call.Actions = make([]OpenAIComputerActionV1, 0, len(wire.Actions))
	for index, raw := range wire.Actions {
		action, err := decodeOpenAIComputerActionV1(raw)
		if err != nil {
			return OpenAIComputerCallV1{}, fmt.Errorf(
				"OpenAI computer_call action %d: %w",
				index+1,
				err,
			)
		}
		call.Actions = append(call.Actions, action)
	}
	return call, nil
}

// ValidateOpenAIComputerCallV1 protects direct executor-interface callers.
// The ordinary adapter starts from strict JSON, but daemon capability methods
// accept the typed struct and must not assume it passed through that decoder.
func ValidateOpenAIComputerCallV1(call OpenAIComputerCallV1) error {
	payload, err := json.Marshal(call)
	if err != nil {
		return fmt.Errorf("encode OpenAI computer_call for validation: %w", err)
	}
	decoded, err := DecodeOpenAIComputerCallV1(payload)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decoded, call) {
		return fmt.Errorf("OpenAI computer_call typed representation is non-canonical")
	}
	return nil
}

func validateOpenAIComputerCallProvenanceV1(
	call OpenAIComputerCallV1,
	actionCount int,
) error {
	if call.Type != OpenAIComputerCallTypeV1 {
		return fmt.Errorf("OpenAI computer call type %q is unsupported", call.Type)
	}
	if call.Provider != OpenAIComputerProviderV1 {
		return fmt.Errorf("OpenAI computer provider provenance %q is invalid", call.Provider)
	}
	if call.APISurface != client.APISurfaceOpenAIResponses {
		return fmt.Errorf("OpenAI computer api_surface provenance %q is invalid", call.APISurface)
	}
	if call.ToolContract != client.ToolContractOpenAIComputerV1 {
		return fmt.Errorf("OpenAI computer tool contract provenance %q is invalid", call.ToolContract)
	}
	if !validOpenAIComputerResponseIDV1(call.ResponseID) {
		return fmt.Errorf("OpenAI computer response_id is missing or invalid")
	}
	if !validOpenAIComputerOpaqueIDV1(call.CallID) {
		return fmt.Errorf("OpenAI computer call_id is missing or invalid")
	}
	if call.Status != OpenAIComputerCallStatusCompletedV1 {
		return fmt.Errorf("OpenAI computer call status %q is not executable", call.Status)
	}
	if actionCount == 0 {
		return fmt.Errorf("OpenAI computer_call actions must not be empty")
	}
	if call.PendingSafetyChecks == nil {
		return fmt.Errorf("OpenAI computer_call pending_safety_checks must be an array")
	}
	seenSafetyChecks := make(map[string]struct{}, len(call.PendingSafetyChecks))
	for index, check := range call.PendingSafetyChecks {
		if err := check.Validate(); err != nil {
			return fmt.Errorf(
				"OpenAI computer pending_safety_checks item %d is invalid: %w",
				index,
				err,
			)
		}
		if _, duplicate := seenSafetyChecks[check.ID]; duplicate {
			return fmt.Errorf(
				"OpenAI computer pending_safety_checks id %q is duplicated",
				check.ID,
			)
		}
		seenSafetyChecks[check.ID] = struct{}{}
	}
	return nil
}

func validOpenAIComputerOpaqueIDV1(value string) bool {
	if !strings.HasPrefix(value, "call_") ||
		len(value) == len("call_") ||
		len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func decodeOneStrictOpenAIComputerJSONV1(payload []byte, value any) error {
	// DisallowUnknownFields does not reject duplicate members and encoding/json
	// otherwise applies last-wins semantics. Reuse the recursive scanner that
	// protects Kocoro's versioned coordinate contracts before any typed decode;
	// json.Decoder.Token also normalizes escaped member names, so "\u0078" and
	// "x" collide as required.
	return decodeStrictCoordinateJSON(payload, value)
}

func decodeOpenAIComputerActionV1(
	payload json.RawMessage,
) (OpenAIComputerActionV1, error) {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &discriminator); err != nil {
		return OpenAIComputerActionV1{}, err
	}
	switch discriminator.Type {
	case OpenAIComputerActionClickV1:
		var action struct {
			Type   string   `json:"type"`
			Button string   `json:"button"`
			X      *int     `json:"x"`
			Y      *int     `json:"y"`
			Keys   []string `json:"keys,omitempty"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		if !validOpenAIComputerClickButtonV1(action.Button) {
			return OpenAIComputerActionV1{}, fmt.Errorf(
				"click button %q is unsupported",
				action.Button,
			)
		}
		modifiers, err := normalizeOpenAIComputerModifiersV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("click: %w", err)
		}
		if err := validateOpenAIComputerPointPointersV1(action.X, action.Y); err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("click: %w", err)
		}
		return OpenAIComputerActionV1{
			Type: action.Type, Button: action.Button, X: action.X, Y: action.Y,
			Keys: modifiers,
		}, nil

	case OpenAIComputerActionDoubleClickV1:
		var action struct {
			Type string   `json:"type"`
			X    *int     `json:"x"`
			Y    *int     `json:"y"`
			Keys []string `json:"keys,omitempty"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		if err := validateOpenAIComputerPointPointersV1(action.X, action.Y); err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("double_click: %w", err)
		}
		modifiers, err := normalizeOpenAIComputerModifiersV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("double_click: %w", err)
		}
		return OpenAIComputerActionV1{
			Type: action.Type, X: action.X, Y: action.Y, Keys: modifiers,
		}, nil

	case OpenAIComputerActionDragV1:
		var action struct {
			Type string                  `json:"type"`
			Path []OpenAIComputerPointV1 `json:"path"`
			Keys []string                `json:"keys,omitempty"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		if len(action.Path) < 2 ||
			len(action.Path) > coordinateDragMaximumWaypointsV1 {
			return OpenAIComputerActionV1{}, fmt.Errorf(
				"drag path requires 2..%d points",
				coordinateDragMaximumWaypointsV1,
			)
		}
		for index, point := range action.Path {
			if point.X < 0 || point.Y < 0 {
				return OpenAIComputerActionV1{}, fmt.Errorf("drag path contains a negative coordinate")
			}
			if index > 0 && point == action.Path[index-1] {
				return OpenAIComputerActionV1{}, fmt.Errorf(
					"drag path contains adjacent duplicate coordinates",
				)
			}
		}
		modifiers, err := normalizeOpenAIComputerModifiersV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("drag: %w", err)
		}
		return OpenAIComputerActionV1{
			Type: action.Type, Path: action.Path, Keys: modifiers,
		}, nil

	case OpenAIComputerActionKeypressV1:
		var action struct {
			Type string   `json:"type"`
			Keys []string `json:"keys"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		keys, err := normalizeOpenAIComputerKeypressV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionV1{}, err
		}
		return OpenAIComputerActionV1{Type: action.Type, Keys: keys}, nil

	case OpenAIComputerActionMoveV1:
		var action struct {
			Type string   `json:"type"`
			X    *int     `json:"x"`
			Y    *int     `json:"y"`
			Keys []string `json:"keys,omitempty"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		if err := validateOpenAIComputerPointPointersV1(action.X, action.Y); err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("move: %w", err)
		}
		modifiers, err := normalizeOpenAIComputerModifiersV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("move: %w", err)
		}
		return OpenAIComputerActionV1{
			Type: action.Type, X: action.X, Y: action.Y, Keys: modifiers,
		}, nil

	case OpenAIComputerActionScrollV1:
		var action struct {
			Type    string   `json:"type"`
			X       *int     `json:"x"`
			Y       *int     `json:"y"`
			ScrollX *int     `json:"scroll_x"`
			ScrollY *int     `json:"scroll_y"`
			Keys    []string `json:"keys,omitempty"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		if err := validateOpenAIComputerPointPointersV1(action.X, action.Y); err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("scroll: %w", err)
		}
		if action.ScrollX == nil || action.ScrollY == nil ||
			*action.ScrollX == 0 && *action.ScrollY == 0 {
			return OpenAIComputerActionV1{}, fmt.Errorf(
				"scroll requires scroll_x/scroll_y and at least one non-zero delta",
			)
		}
		const maximumProviderPixelDeltaSafetyBound = int64(^uint32(0) >> 1)
		if int64(*action.ScrollX) < -maximumProviderPixelDeltaSafetyBound ||
			int64(*action.ScrollX) > maximumProviderPixelDeltaSafetyBound ||
			int64(*action.ScrollY) < -maximumProviderPixelDeltaSafetyBound ||
			int64(*action.ScrollY) > maximumProviderPixelDeltaSafetyBound {
			return OpenAIComputerActionV1{}, fmt.Errorf(
				"scroll deltas exceed the admitted provider safety bound",
			)
		}
		modifiers, err := normalizeOpenAIComputerModifiersV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionV1{}, fmt.Errorf("scroll: %w", err)
		}
		return OpenAIComputerActionV1{
			Type: action.Type, X: action.X, Y: action.Y,
			ScrollX: action.ScrollX, ScrollY: action.ScrollY,
			Keys: modifiers,
		}, nil

	case OpenAIComputerActionTypeTextV1:
		var action struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		if !validOpenAIComputerTextV1(action.Text) {
			return OpenAIComputerActionV1{}, fmt.Errorf("type text is empty or invalid")
		}
		return OpenAIComputerActionV1{Type: action.Type, Text: action.Text}, nil

	case OpenAIComputerActionScreenshotV1, OpenAIComputerActionWaitV1:
		var action struct {
			Type string `json:"type"`
		}
		if err := decodeOneStrictOpenAIComputerJSONV1(payload, &action); err != nil {
			return OpenAIComputerActionV1{}, err
		}
		return OpenAIComputerActionV1{Type: action.Type}, nil

	default:
		return OpenAIComputerActionV1{}, fmt.Errorf(
			"unknown or unsupported action %q",
			discriminator.Type,
		)
	}
}

func validOpenAIComputerClickButtonV1(button string) bool {
	switch button {
	case "left", "right", "wheel", "back", "forward":
		return true
	default:
		return false
	}
}

func normalizeOpenAIComputerModifiersV1(keys []string) ([]string, error) {
	if len(keys) > 4 {
		return nil, fmt.Errorf("modifier list exceeds the four supported modifier keys")
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, raw := range keys {
		token, modifier, ok := normalizeOpenAIComputerKeyV1(raw)
		if !ok || !modifier {
			return nil, fmt.Errorf("keys accepts only command/control/option/shift modifiers")
		}
		if _, duplicate := seen[token]; duplicate {
			return nil, fmt.Errorf("duplicate modifier %q is unsafe", token)
		}
		seen[token] = struct{}{}
		normalized = append(normalized, token)
	}
	return normalized, nil
}

func normalizeOpenAIComputerKeypressV1(keys []string) ([]string, error) {
	// 64 ordered ordinary keys covers navigation and short shortcut sequences.
	// If this binds, the provider action fails before authority rather than
	// partially typing; a future provider contract/version is the override path.
	if len(keys) == 0 || len(keys) > 64 {
		return nil, fmt.Errorf("keypress keys must contain 1..64 entries")
	}
	normalized := make([]string, 0, len(keys))
	modifiers := make(map[string]struct{}, 4)
	ordinary := 0
	for _, raw := range keys {
		token, modifier, ok := normalizeOpenAIComputerKeyV1(raw)
		if !ok {
			return nil, fmt.Errorf("keypress contains unsupported key %q", raw)
		}
		if modifier {
			if _, duplicate := modifiers[token]; duplicate {
				return nil, fmt.Errorf("keypress contains duplicate modifier %q", token)
			}
			modifiers[token] = struct{}{}
		} else {
			ordinary++
		}
		normalized = append(normalized, token)
	}
	if ordinary == 0 {
		return nil, fmt.Errorf("keypress requires at least one non-modifier key")
	}
	return normalized, nil
}

func normalizeOpenAIComputerKeyV1(raw string) (token string, modifier bool, ok bool) {
	if raw == "" || raw != strings.TrimSpace(raw) || !utf8.ValidString(raw) {
		return "", false, false
	}
	switch strings.ToUpper(raw) {
	case "META", "CMD", "COMMAND":
		return "command", true, true
	case "CTRL", "CONTROL":
		return "control", true, true
	case "ALT", "OPTION":
		return "option", true, true
	case "SHIFT":
		return "shift", true, true
	case "RETURN", "ENTER":
		return "return", false, true
	case "ESCAPE", "ESC":
		return "escape", false, true
	case "TAB":
		return "tab", false, true
	case "DELETE", "DEL":
		return "delete", false, true
	case "BACKSPACE":
		return "backspace", false, true
	case "HOME":
		return "home", false, true
	case "END":
		return "end", false, true
	case "PAGEUP":
		return "pageup", false, true
	case "PAGEDOWN":
		return "pagedown", false, true
	case "ARROWUP", "UP":
		return "up", false, true
	case "ARROWDOWN", "DOWN":
		return "down", false, true
	case "ARROWLEFT", "LEFT":
		return "left", false, true
	case "ARROWRIGHT", "RIGHT":
		return "right", false, true
	case "SPACE":
		return "space", false, true
	}
	runes := []rune(raw)
	if len(runes) != 1 || unicode.IsControl(runes[0]) || runes[0] > unicode.MaxASCII {
		return "", false, false
	}
	return strings.ToLower(raw), false, true
}

func validateOpenAIComputerPointPointersV1(x, y *int) error {
	if x == nil || y == nil {
		return fmt.Errorf("x and y are required")
	}
	if *x < 0 || *y < 0 {
		return fmt.Errorf("coordinates must not be negative")
	}
	return nil
}

func validOpenAIComputerTextV1(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\t' && char != '\r' {
			return false
		}
	}
	return true
}

// OpenAIComputerActionScopeV1 binds a single action to its provider batch.
// A daemon implementation must use this exact identity when acquiring one
// per-action coordinator handle and attended approval/risk authority.
type OpenAIComputerActionScopeV1 struct {
	ResponseID   string
	CallID       string
	Provider     string
	APISurface   string
	ToolContract string
	ActionID     string
	ActionIndex  int
	ActionCount  int
}

// OpenAIComputerBatchAuthorityV1 identifies the one daemon GUI lease that
// owns every action and the final observation for this provider call. It is
// executor-authored and never serialized into provider-visible output.
type OpenAIComputerBatchAuthorityV1 struct {
	LeaseID      string
	ResponseID   string
	CallID       string
	Provider     string
	APISurface   string
	ToolContract string
}

type OpenAIComputerCommitStateV1 string

const (
	OpenAIComputerNotCommittedV1     OpenAIComputerCommitStateV1 = "not_committed"
	OpenAIComputerCommitVerifiedV1   OpenAIComputerCommitStateV1 = "committed_verified"
	OpenAIComputerCommitUnverifiedV1 OpenAIComputerCommitStateV1 = "committed_unverified"
	OpenAIComputerCommitUnknownV1    OpenAIComputerCommitStateV1 = "commit_status_unknown"
)

// OpenAIComputerActionExecutionV1 is the executor's redacted action
// acknowledgement. Intermediate images are forbidden: the provider receives
// one final screenshot for the whole computer_call, never one per action.
type OpenAIComputerActionExecutionV1 struct {
	CommitState OpenAIComputerCommitStateV1
	Result      agent.ToolResult
}

// OpenAIComputerBatchActionExecutorV1 is deliberately stronger than
// ComputerUseTool.Run. ExecuteAuthorizedOpenAIComputerActionV1 must, as one
// indivisible boundary, re-check cancellation, obtain an exact per-action GUI
// execution capability, re-resolve the app-policy target, and obtain any fresh
// ordinary/consequential approval before it can commit input. The daemon
// installs this interface only through its private batch runner after the
// guarded computer_use core has been detached from the provider-visible
// registry.
type OpenAIComputerBatchActionExecutorV1 interface {
	AcquireOpenAIComputerBatchAuthorityV1(
		context.Context,
		OpenAIComputerCallV1,
	) (OpenAIComputerBatchAuthorityV1, error)
	ExecuteAuthorizedOpenAIComputerActionV1(
		context.Context,
		OpenAIComputerBatchAuthorityV1,
		OpenAIComputerActionScopeV1,
		OpenAIComputerActionV1,
	) (OpenAIComputerActionExecutionV1, error)
	CaptureFinalOpenAIComputerObservationV1(
		context.Context,
		OpenAIComputerBatchAuthorityV1,
		OpenAIComputerCallV1,
	) (agent.ToolResult, error)
}

type OpenAIComputerBatchResultV1 struct {
	CallID       string
	Provider     string
	APISurface   string
	ToolContract string
	ToolResult   agent.ToolResult
}

// OpenAIComputerAdapterV1 is the strict executor seam and provider-schema
// marker for the Responses API batched computer contract. One Run is one
// provider computer_call; actions are never projected as unrelated AgentLoop
// tool calls.
type OpenAIComputerAdapterV1 struct {
	executor OpenAIComputerBatchActionExecutorV1
}

func newOpenAIComputerAdapterV1(
	executor OpenAIComputerBatchActionExecutorV1,
) *OpenAIComputerAdapterV1 {
	return &OpenAIComputerAdapterV1{executor: executor}
}

// NewOpenAIComputerAdapterV1 exposes the strict batch runner to the daemon
// integration. A nil executor is used only by the run-local registry marker;
// ordinary Tool.Run calls remain fail-closed.
func NewOpenAIComputerAdapterV1(
	executor OpenAIComputerBatchActionExecutorV1,
) *OpenAIComputerAdapterV1 {
	return newOpenAIComputerAdapterV1(executor)
}

func (a *OpenAIComputerAdapterV1) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        client.NativeComputerToolName,
		Description: "OpenAI Responses native computer batch adapter.",
		Parameters: map[string]any{
			"type": "object",
		},
	}
}

func (a *OpenAIComputerAdapterV1) RequiresApproval() bool { return true }

func (a *OpenAIComputerAdapterV1) NativeToolDef() *client.NativeToolDef {
	return &client.NativeToolDef{
		Type: client.OpenAINativeComputerToolType,
		Name: client.NativeComputerToolName,
	}
}

// The provider marker is never dispatched as an ordinary function tool.
// Keeping these traits makes both local and daemon GUI wrappers retain the
// native definition while any accidental direct invocation fails closed.
func (a *OpenAIComputerAdapterV1) IsReadOnlyCall(string) bool { return false }

func (a *OpenAIComputerAdapterV1) DescribeGUIAction(
	context.Context,
	string,
) (agent.GUIActionDescriptor, error) {
	return agent.GUIActionDescriptor{},
		fmt.Errorf("OpenAI computer batches require the dedicated daemon executor")
}

// OpenAINativeComputerBatchExecutionAvailable records that the production
// daemon path now supplies tested response-bound provenance, per-action
// approval/coordinator/risk authority, Pause/Stop boundaries, and one final
// exact screenshot.
func OpenAINativeComputerBatchExecutionAvailable() bool { return true }

func (a *OpenAIComputerAdapterV1) Run(
	ctx context.Context,
	args string,
) (agent.ToolResult, error) {
	result, err := a.ExecuteBatchV1(ctx, []byte(args))
	return result.ToolResult, err
}

func (a *OpenAIComputerAdapterV1) ExecuteBatchV1(
	ctx context.Context,
	payload []byte,
) (OpenAIComputerBatchResultV1, error) {
	call, err := DecodeOpenAIComputerCallV1(payload)
	if err != nil {
		return OpenAIComputerBatchResultV1{
			ToolResult: agent.ValidationError(err.Error()),
		}, nil
	}
	result := OpenAIComputerBatchResultV1{
		CallID: call.CallID, Provider: call.Provider, APISurface: call.APISurface,
		ToolContract: call.ToolContract,
	}
	if a == nil || a.executor == nil {
		result.ToolResult = agent.BusinessError(
			"OpenAI native computer batch execution is unavailable",
		)
		return result, nil
	}
	authority, authorityErr := a.executor.AcquireOpenAIComputerBatchAuthorityV1(
		ctx,
		call,
	)
	if authorityErr != nil || !authority.matchesOpenAIComputerCallV1(call) {
		result.ToolResult = agent.BusinessError(
			"OpenAI native computer batch authority is unavailable or does not match this provider call",
		)
		return result, nil
	}

	committed := false
	for index, action := range call.Actions {
		if err := ctx.Err(); err != nil {
			result.ToolResult = openAIComputerCancellationResultV1(committed)
			return result, nil
		}
		scope := OpenAIComputerActionScopeV1{
			ResponseID: call.ResponseID, CallID: call.CallID, Provider: call.Provider,
			APISurface: call.APISurface, ToolContract: call.ToolContract,
			ActionID:    call.CallID + "/action/" + strconv.Itoa(index+1),
			ActionIndex: index, ActionCount: len(call.Actions),
		}
		execution, executeErr := a.executor.ExecuteAuthorizedOpenAIComputerActionV1(
			ctx,
			authority,
			scope,
			action,
		)
		if execution.CommitState != OpenAIComputerNotCommittedV1 {
			committed = true
		}
		if invalid := validateOpenAIComputerActionExecutionV1(action, execution, executeErr); invalid != "" {
			failure := agent.BusinessError(
				fmt.Sprintf(
					"OpenAI computer action %d of %d returned an invalid per-action acknowledgement: %s",
					index+1,
					len(call.Actions),
					invalid,
				),
			)
			result.ToolResult = a.attachOpenAIComputerFinalObservationV1(
				ctx,
				authority,
				call,
				failure,
				committed,
			)
			return result, nil
		}
		// Commit state is the action boundary. A helper may report an error only
		// because its optional postcondition could not be proven after the input
		// was already committed. Continue known atomic/full commits and stop
		// only on no-commit, partial, or unknown outcomes.
		if !openAIComputerActionCanContinueV1(action, execution, executeErr) {
			result.ToolResult = openAIComputerActionFailureV1(
				index,
				len(call.Actions),
				execution,
				executeErr,
			)
			result.ToolResult = a.attachOpenAIComputerFinalObservationV1(
				ctx,
				authority,
				call,
				result.ToolResult,
				committed,
			)
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			result.ToolResult = openAIComputerCancellationResultV1(committed)
			return result, nil
		}
	}

	final, finalErr := a.executor.CaptureFinalOpenAIComputerObservationV1(
		ctx,
		authority,
		call,
	)
	if invalid := validateOpenAIComputerFinalObservationV1(final, finalErr); invalid != "" {
		message := "OpenAI computer batch finished, but its required final exact screenshot is unavailable"
		if committed {
			message += "; actions may have completed; do not retry automatically"
		}
		result.ToolResult = agent.BusinessError(message)
		return result, nil
	}
	final.Content = fmt.Sprintf(
		"Executed %d OpenAI computer actions in order; one final exact screenshot is attached.",
		len(call.Actions),
	)
	final.GUIOutcome = nil
	result.ToolResult = final
	return result, nil
}

func openAIComputerActionCanContinueV1(
	action OpenAIComputerActionV1,
	execution OpenAIComputerActionExecutionV1,
	executeErr error,
) bool {
	if !openAIComputerActionMutatesV1(action) {
		return executeErr == nil && !execution.Result.IsError
	}
	switch execution.CommitState {
	case OpenAIComputerCommitVerifiedV1:
		return true
	case OpenAIComputerCommitUnverifiedV1:
		return openAIComputerKnownCommitCanContinueV1(action, execution)
	default:
		return false
	}
}

func validateOpenAIComputerActionExecutionV1(
	action OpenAIComputerActionV1,
	execution OpenAIComputerActionExecutionV1,
	executeErr error,
) string {
	switch execution.CommitState {
	case OpenAIComputerNotCommittedV1, OpenAIComputerCommitVerifiedV1,
		OpenAIComputerCommitUnverifiedV1, OpenAIComputerCommitUnknownV1:
	default:
		return "commit state is invalid"
	}
	if len(execution.Result.Images) != 0 {
		return "intermediate images are forbidden"
	}
	if !openAIComputerActionMutatesV1(action) &&
		execution.CommitState != OpenAIComputerNotCommittedV1 {
		return "observation action claimed an input commit"
	}
	if openAIComputerActionMutatesV1(action) &&
		executeErr == nil && !execution.Result.IsError &&
		execution.CommitState == OpenAIComputerNotCommittedV1 {
		return "successful mutation did not acknowledge its commit"
	}
	return ""
}

func openAIComputerActionMutatesV1(action OpenAIComputerActionV1) bool {
	return action.Type != OpenAIComputerActionScreenshotV1 &&
		action.Type != OpenAIComputerActionWaitV1
}

func openAIComputerKnownCommitCanContinueV1(
	action OpenAIComputerActionV1,
	execution OpenAIComputerActionExecutionV1,
) bool {
	failureCode := ""
	if execution.Result.GUIOutcome != nil {
		failureCode = execution.Result.GUIOutcome.FailureCode
	}
	switch action.Type {
	case OpenAIComputerActionClickV1,
		OpenAIComputerActionDoubleClickV1:
		return failureCode == "click_postcondition_not_declared" ||
			failureCode == "postcondition_not_declared" ||
			failureCode == "postcondition_not_observed"
	case OpenAIComputerActionMoveV1:
		return false
	case OpenAIComputerActionTypeTextV1,
		OpenAIComputerActionKeypressV1:
		return failureCode == "postcondition_not_declared" ||
			failureCode == "postcondition_not_observed"
	case OpenAIComputerActionScrollV1:
		return failureCode == "scroll_postcondition_not_declared"
	case OpenAIComputerActionDragV1:
		return failureCode == "drop_postcondition_not_declared"
	default:
		return false
	}
}

func openAIComputerActionFailureV1(
	index int,
	count int,
	execution OpenAIComputerActionExecutionV1,
	executeErr error,
) agent.ToolResult {
	message := fmt.Sprintf(
		"OpenAI computer action %d of %d did not complete",
		index+1,
		count,
	)
	switch execution.CommitState {
	case OpenAIComputerCommitUnknownV1:
		message += "; commit status is unknown; do not retry automatically"
	case OpenAIComputerCommitUnverifiedV1, OpenAIComputerCommitVerifiedV1:
		message += "; input may have committed; do not retry automatically"
	}
	if executeErr != nil {
		message += ": " + executeErr.Error()
	} else if execution.Result.Content != "" {
		message += ": " + execution.Result.Content
	}
	return agent.BusinessError(message)
}

func openAIComputerCancellationResultV1(committed bool) agent.ToolResult {
	message := "OpenAI computer batch cancelled at an action boundary"
	if committed {
		message += "; an earlier action may have committed; do not retry automatically"
	}
	return agent.BusinessError(message)
}

func (a *OpenAIComputerAdapterV1) attachOpenAIComputerFinalObservationV1(
	ctx context.Context,
	authority OpenAIComputerBatchAuthorityV1,
	call OpenAIComputerCallV1,
	failure agent.ToolResult,
	committed bool,
) agent.ToolResult {
	if ctx.Err() != nil {
		return failure
	}
	final, err := a.executor.CaptureFinalOpenAIComputerObservationV1(
		ctx,
		authority,
		call,
	)
	if validateOpenAIComputerFinalObservationV1(final, err) == "" {
		failure.Images = append([]agent.ImageBlock(nil), final.Images...)
		return failure
	}
	if committed && !strings.Contains(failure.Content, "do not retry automatically") {
		failure.Content += "; actions may have completed and no final screenshot is available; do not retry automatically"
	}
	return failure
}

func (authority OpenAIComputerBatchAuthorityV1) matchesOpenAIComputerCallV1(
	call OpenAIComputerCallV1,
) bool {
	return authority.LeaseID != "" &&
		authority.ResponseID == call.ResponseID &&
		authority.CallID == call.CallID &&
		authority.Provider == call.Provider &&
		authority.APISurface == call.APISurface &&
		authority.ToolContract == call.ToolContract
}

func validateOpenAIComputerFinalObservationV1(
	result agent.ToolResult,
	err error,
) string {
	if err != nil {
		return "final capture failed"
	}
	if result.IsError || len(result.Images) != 1 {
		return "final capture did not return exactly one image"
	}
	image := result.Images[0]
	if image.MediaType == "" || image.Data == "" {
		return "final capture image is empty"
	}
	return ""
}

var _ agent.Tool = (*OpenAIComputerAdapterV1)(nil)
