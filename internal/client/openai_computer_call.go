package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	OpenAIComputerCallType            = "computer_call"
	OpenAIComputerProvider            = "openai"
	OpenAIComputerCallStatusCompleted = "completed"
)

// OpenAIComputerCall is Cloud's normalized Responses API computer_call
// envelope. Actions deliberately remain raw JSON at this transport boundary:
// the provider adapter owns the strict action union, while this layer preserves
// the complete batch and its provenance without translating it into Kocoro's
// public function-tool schema.
type OpenAIComputerCall struct {
	Type                string                      `json:"type"`
	Provider            string                      `json:"provider"`
	APISurface          string                      `json:"api_surface"`
	ToolContract        string                      `json:"tool_contract"`
	ResponseID          string                      `json:"response_id"`
	CallID              string                      `json:"call_id"`
	Actions             json.RawMessage             `json:"actions"`
	PendingSafetyChecks []OpenAIComputerSafetyCheck `json:"pending_safety_checks"`
	Status              string                      `json:"status"`
}

// OpenAIComputerSafetyCheck is the exact normalized representation of the
// Responses API ComputerCallSafetyCheckParam. Cloud canonicalizes the two
// optional provider fields as explicit strings or null so Kocoro can preserve
// the complete object when an attended user later acknowledges it.
type OpenAIComputerSafetyCheck struct {
	ID      string  `json:"id"`
	Code    *string `json:"code"`
	Message *string `json:"message"`
}

func (check OpenAIComputerSafetyCheck) MarshalJSON() ([]byte, error) {
	if err := check.Validate(); err != nil {
		return nil, err
	}
	type wire OpenAIComputerSafetyCheck
	return json.Marshal(wire(check))
}

func (check *OpenAIComputerSafetyCheck) UnmarshalJSON(data []byte) error {
	if check == nil {
		return fmt.Errorf("cannot unmarshal OpenAI computer safety check into nil receiver")
	}
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return fmt.Errorf("decode OpenAI computer safety check: %w", err)
	}
	var raw struct {
		ID      *string         `json:"id"`
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode OpenAI computer safety check: %w", err)
	}
	if raw.ID == nil || raw.Code == nil || raw.Message == nil {
		return fmt.Errorf(
			"OpenAI computer safety check requires id, code, and message members",
		)
	}
	code, err := decodeOpenAIComputerNullableString(raw.Code)
	if err != nil {
		return fmt.Errorf("decode OpenAI computer safety check code: %w", err)
	}
	message, err := decodeOpenAIComputerNullableString(raw.Message)
	if err != nil {
		return fmt.Errorf("decode OpenAI computer safety check message: %w", err)
	}
	candidate := OpenAIComputerSafetyCheck{
		ID:      *raw.ID,
		Code:    code,
		Message: message,
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*check = candidate
	return nil
}

func (check OpenAIComputerSafetyCheck) Validate() error {
	if strings.TrimSpace(check.ID) == "" || len(check.ID) > 512 {
		return fmt.Errorf("OpenAI computer safety check id is missing or invalid")
	}
	if check.Code != nil && len(*check.Code) > 4096 {
		return fmt.Errorf("OpenAI computer safety check code is too large")
	}
	if check.Message != nil && len(*check.Message) > 16*1024 {
		return fmt.Errorf("OpenAI computer safety check message is too large")
	}
	return nil
}

func decodeOpenAIComputerNullableString(raw json.RawMessage) (*string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("must be a string or null")
	}
	return &value, nil
}

// CloneOpenAIComputerSafetyChecks returns a deep copy so provider metadata
// cannot be changed after it has been bound to an approval or continuation.
func CloneOpenAIComputerSafetyChecks(
	checks []OpenAIComputerSafetyCheck,
) []OpenAIComputerSafetyCheck {
	if checks == nil {
		return nil
	}
	cloned := make([]OpenAIComputerSafetyCheck, len(checks))
	cloneString := func(value *string) *string {
		if value == nil {
			return nil
		}
		exact := *value
		return &exact
	}
	for index, check := range checks {
		cloned[index] = OpenAIComputerSafetyCheck{
			ID:      check.ID,
			Code:    cloneString(check.Code),
			Message: cloneString(check.Message),
		}
	}
	return cloned
}

func (call OpenAIComputerCall) MarshalJSON() ([]byte, error) {
	if err := call.Validate(); err != nil {
		return nil, err
	}
	type wire OpenAIComputerCall
	return json.Marshal(wire(call))
}

func (call *OpenAIComputerCall) UnmarshalJSON(data []byte) error {
	if call == nil {
		return fmt.Errorf("cannot unmarshal OpenAI computer_call into nil receiver")
	}
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return fmt.Errorf("decode OpenAI computer_call: %w", err)
	}
	type wire OpenAIComputerCall
	var decoded wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode OpenAI computer_call: %w", err)
	}
	candidate := OpenAIComputerCall(decoded)
	if err := candidate.Validate(); err != nil {
		return err
	}
	candidate.Actions = append(json.RawMessage(nil), candidate.Actions...)
	*call = candidate
	return nil
}

// Validate checks only transport provenance and batch completeness. Action
// semantics are intentionally left to the OpenAI adapter, which validates the
// full action union before acquiring execution authority.
func (call OpenAIComputerCall) Validate() error {
	if call.Type != OpenAIComputerCallType {
		return fmt.Errorf("OpenAI computer call type %q is unsupported", call.Type)
	}
	if call.Provider != OpenAIComputerProvider {
		return fmt.Errorf("OpenAI computer provider provenance %q is invalid", call.Provider)
	}
	if call.APISurface != APISurfaceOpenAIResponses {
		return fmt.Errorf("OpenAI computer api_surface provenance %q is invalid", call.APISurface)
	}
	if call.ToolContract != ToolContractOpenAIComputerV1 {
		return fmt.Errorf("OpenAI computer tool contract provenance %q is invalid", call.ToolContract)
	}
	if !validOpenAIComputerResponseID(call.ResponseID) {
		return fmt.Errorf("OpenAI computer response_id is missing or invalid")
	}
	if !validOpenAIComputerCallID(call.CallID) {
		return fmt.Errorf("OpenAI computer call_id is missing or invalid")
	}
	if call.Status != OpenAIComputerCallStatusCompleted {
		return fmt.Errorf("OpenAI computer call status %q is not executable", call.Status)
	}
	if err := rejectDuplicateJSONMembers(call.Actions); err != nil {
		return fmt.Errorf("OpenAI computer actions: %w", err)
	}
	var actions []json.RawMessage
	if err := json.Unmarshal(call.Actions, &actions); err != nil {
		return fmt.Errorf("OpenAI computer actions must be a JSON array: %w", err)
	}
	if len(actions) == 0 {
		return fmt.Errorf("OpenAI computer_call actions must not be empty")
	}
	if call.PendingSafetyChecks == nil {
		return fmt.Errorf("OpenAI computer_call pending_safety_checks must be an array")
	}
	if err := validateOpenAIComputerSafetyCheckList(call.PendingSafetyChecks); err != nil {
		return fmt.Errorf("OpenAI computer_call pending safety checks: %w", err)
	}
	return nil
}

func validateOpenAIComputerSafetyCheckList(
	checks []OpenAIComputerSafetyCheck,
) error {
	seenSafetyChecks := make(map[string]struct{}, len(checks))
	for index, check := range checks {
		if err := check.Validate(); err != nil {
			return fmt.Errorf(
				"safety check %d: %w",
				index,
				err,
			)
		}
		if _, duplicate := seenSafetyChecks[check.ID]; duplicate {
			return fmt.Errorf(
				"safety check id %q is duplicated",
				check.ID,
			)
		}
		seenSafetyChecks[check.ID] = struct{}{}
	}
	return nil
}

func validOpenAIComputerResponseID(value string) bool {
	return ValidOpenAIComputerContinuationToken(value)
}

// ValidOpenAIComputerContinuationToken accepts only Cloud's tenant-bound
// opaque continuation token. Raw upstream Responses API IDs never cross this
// wire boundary.
func ValidOpenAIComputerContinuationToken(value string) bool {
	const prefix = "shct_"
	const payloadLength = 43
	return strings.HasPrefix(value, prefix) &&
		len(value) == len(prefix)+payloadLength &&
		validOpenAIComputerOpaqueID(value)
}

func validOpenAIComputerCallID(value string) bool {
	if !strings.HasPrefix(value, "call_") ||
		len(value) == len("call_") ||
		len(value) > 256 {
		return false
	}
	return validOpenAIComputerOpaqueID(value)
}

func validOpenAIComputerOpaqueID(value string) bool {
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
