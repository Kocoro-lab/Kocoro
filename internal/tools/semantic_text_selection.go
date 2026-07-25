package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
)

type SemanticTextRangeV1 struct {
	Location int `json:"location"`
	Length   int `json:"length"`
}

type SemanticTextSelectionRequestV1 struct {
	SchemaVersion       int                 `json:"schema_version"`
	PID                 int                 `json:"pid"`
	BundleID            string              `json:"bundle_id"`
	WindowID            uint32              `json:"window_id"`
	Ref                 string              `json:"ref"`
	Path                string              `json:"path"`
	ExpectedRole        string              `json:"expected_role"`
	ExpectedFingerprint string              `json:"expected_fingerprint"`
	Range               SemanticTextRangeV1 `json:"range"`
	FallbackPolicy      string              `json:"fallback_policy"`
	CommitDeadlineAt    string              `json:"commit_deadline_at"`
}

type SemanticTextSelectionRPCRequestV1 struct {
	ID     int64                          `json:"id"`
	Method string                         `json:"method"`
	Params SemanticTextSelectionRequestV1 `json:"params"`
}

// SemanticTextSelectionResultV1 treats fallback_required as only a strategy
// signal. It never carries raw coordinates and must not be delegated to the
// legacy mouse_event path. A caller may fall back only by constructing a
// CoordinateDragRequestV1 from a current immutable CoordinateFrame.
type SemanticTextSelectionResultV1 struct {
	SchemaVersion      int                  `json:"schema_version"`
	Status             string               `json:"status"`
	SelectionCommitted bool                 `json:"selection_committed"`
	Phase              string               `json:"phase"`
	FailureCode        *string              `json:"failure_code"`
	RetrySafe          bool                 `json:"retry_safe"`
	Postcondition      *string              `json:"postcondition"`
	SelectedRange      *SemanticTextRangeV1 `json:"selected_range"`
}

var semanticTextRangeWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"location": coordinateScalarWireShape(false),
	"length":   coordinateScalarWireShape(false),
})

var semanticTextSelectionRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version":       coordinateScalarWireShape(false),
		"pid":                  coordinateScalarWireShape(false),
		"bundle_id":            coordinateScalarWireShape(false),
		"window_id":            coordinateScalarWireShape(false),
		"ref":                  coordinateScalarWireShape(false),
		"path":                 coordinateScalarWireShape(false),
		"expected_role":        coordinateScalarWireShape(false),
		"expected_fingerprint": coordinateScalarWireShape(false),
		"range":                semanticTextRangeWireShapeV1,
		"fallback_policy":      coordinateScalarWireShape(false),
		"commit_deadline_at":   coordinateScalarWireShape(false),
	}),
})

var semanticTextSelectionResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":      coordinateScalarWireShape(false),
	"status":              coordinateScalarWireShape(false),
	"selection_committed": coordinateScalarWireShape(false),
	"phase":               coordinateScalarWireShape(false),
	"failure_code":        coordinateScalarWireShape(true),
	"retry_safe":          coordinateScalarWireShape(false),
	"postcondition":       coordinateScalarWireShape(true),
	"selected_range":      coordinateNullableWireShape(semanticTextRangeWireShapeV1),
})

func DecodeSemanticTextSelectionRPCRequestV1(payload []byte) (SemanticTextSelectionRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"semantic_text_selection request v1", payload, semanticTextSelectionRequestWireShapeV1,
	); err != nil {
		return SemanticTextSelectionRPCRequestV1{}, err
	}
	var envelope SemanticTextSelectionRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return SemanticTextSelectionRPCRequestV1{}, fmt.Errorf("decode semantic_text_selection request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "semantic_text_selection" {
		return SemanticTextSelectionRPCRequestV1{}, fmt.Errorf("invalid semantic_text_selection RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return SemanticTextSelectionRPCRequestV1{}, err
	}
	return envelope, nil
}

func EncodeSemanticTextSelectionRPCRequestV1(
	envelope SemanticTextSelectionRPCRequestV1,
) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "semantic_text_selection" {
		return nil, fmt.Errorf("invalid semantic_text_selection RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (request SemanticTextSelectionRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.PID <= 0 || request.WindowID == 0 {
		return fmt.Errorf("semantic_text_selection authority is required")
	}
	for name, value := range map[string]string{
		"bundle_id":            request.BundleID,
		"ref":                  request.Ref,
		"path":                 request.Path,
		"expected_role":        request.ExpectedRole,
		"expected_fingerprint": request.ExpectedFingerprint,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("semantic_text_selection %s is invalid", name)
		}
	}
	if len(request.Ref) < 2 || request.Ref[0] != 'e' {
		return fmt.Errorf("semantic_text_selection ref is invalid")
	}
	for _, char := range request.Ref[1:] {
		if char < '0' || char > '9' {
			return fmt.Errorf("semantic_text_selection ref is invalid")
		}
	}
	if (request.Path != "window[0]" && !strings.HasPrefix(request.Path, "window[0]/")) ||
		request.FallbackPolicy != "coordinate_drag" {
		return fmt.Errorf("semantic_text_selection fallback/path policy is invalid")
	}
	if request.Range.Location < 0 || request.Range.Length <= 0 ||
		request.Range.Location > int(^uint(0)>>1)-request.Range.Length {
		return fmt.Errorf("semantic_text_selection range is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("semantic_text_selection commit_deadline_at must be RFC3339: %w", err)
	}
	return nil
}

func DecodeSemanticTextSelectionResultV1(payload []byte) (SemanticTextSelectionResultV1, error) {
	if err := validateCoordinateWireShape(
		"semantic_text_selection result v1", payload, semanticTextSelectionResultWireShapeV1,
	); err != nil {
		return SemanticTextSelectionResultV1{}, err
	}
	var result SemanticTextSelectionResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return SemanticTextSelectionResultV1{}, fmt.Errorf("decode semantic_text_selection result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return SemanticTextSelectionResultV1{}, err
	}
	return result, nil
}

func EncodeSemanticTextSelectionResultV1(result SemanticTextSelectionResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (result SemanticTextSelectionResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 || result.RetrySafe {
		return fmt.Errorf("semantic_text_selection result schema/retry policy is invalid")
	}
	if result.SelectedRange != nil {
		if result.SelectedRange.Location < 0 || result.SelectedRange.Length <= 0 {
			return fmt.Errorf("semantic_text_selection selected_range is invalid")
		}
	}
	switch result.Status {
	case "verified":
		if !result.SelectionCommitted || result.Phase != "post_verification" ||
			result.FailureCode != nil || result.Postcondition == nil ||
			*result.Postcondition != "selected_range_matches" || result.SelectedRange == nil {
			return fmt.Errorf("invalid verified semantic_text_selection result")
		}
	case "completed_unverified":
		if !result.SelectionCommitted || result.Phase != "post_verification" ||
			result.FailureCode == nil || result.Postcondition != nil {
			return fmt.Errorf("invalid completed_unverified semantic_text_selection result")
		}
		if *result.FailureCode == "selected_range_mismatch" {
			if result.SelectedRange == nil {
				return fmt.Errorf("selection mismatch omitted observed range")
			}
		} else if (*result.FailureCode != "selected_range_not_observed" &&
			*result.FailureCode != "interference_detection_unavailable") || result.SelectedRange != nil {
			return fmt.Errorf("invalid unobserved semantic_text_selection result")
		}
	case "user_interference":
		if result.Phase != "user_interference" || result.FailureCode == nil ||
			*result.FailureCode != "physical_input_interference" ||
			result.Postcondition != nil || result.SelectedRange != nil {
			return fmt.Errorf("invalid user_interference semantic_text_selection result")
		}
	case "fallback_required":
		if result.SelectionCommitted || result.Phase != "preflight" ||
			result.FailureCode == nil || *result.FailureCode != "ax_text_range_unsupported" ||
			result.Postcondition != nil || result.SelectedRange != nil {
			return fmt.Errorf("invalid fallback_required semantic_text_selection result")
		}
	case "failed":
		preflight := map[string]bool{
			"invalid_request": true, "request_expired": true,
			"process_not_live": true, "process_identity_mismatch": true,
			"window_not_found": true, "window_ambiguous": true,
			"path_not_found": true, "role_mismatch": true,
			"fingerprint_mismatch": true, "fingerprint_not_found": true,
			"fingerprint_ambiguous": true, "sensitive_target": true,
			"enabled_unknown": true, "target_disabled": true,
			"interference_detection_unavailable": true,
		}
		exactPhase := result.FailureCode != nil &&
			((preflight[*result.FailureCode] && result.Phase == "preflight") ||
				(*result.FailureCode == "ax_selection_failed" && result.Phase == "action"))
		if result.SelectionCommitted || result.FailureCode == nil ||
			!exactPhase || result.Postcondition != nil || result.SelectedRange != nil {
			return fmt.Errorf("invalid failed semantic_text_selection result")
		}
	default:
		return fmt.Errorf("invalid semantic_text_selection status %q", result.Status)
	}
	return nil
}

type SemanticTextSelectionCommitUnknownErrorV1 struct{ cause error }

const semanticTextSelectionTransportGraceV1 = 150 * time.Millisecond

func (err *SemanticTextSelectionCommitUnknownErrorV1) Error() string {
	return fmt.Sprintf("semantic_text_selection commit unknown (not retry-safe): %v", err.cause)
}
func (err *SemanticTextSelectionCommitUnknownErrorV1) Unwrap() error       { return err.cause }
func (err *SemanticTextSelectionCommitUnknownErrorV1) RetrySafe() bool     { return false }
func (err *SemanticTextSelectionCommitUnknownErrorV1) CommitUnknown() bool { return true }

func newSemanticTextSelectionCommitUnknownV1(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &SemanticTextSelectionCommitUnknownErrorV1{cause: cause}
}

func (client *AXClient) SemanticTextSelectionV1(
	ctx context.Context, request SemanticTextSelectionRequestV1,
) (SemanticTextSelectionResultV1, error) {
	if runtime.GOOS != "darwin" {
		return SemanticTextSelectionResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.semanticTextSelectionV1(ctx, request)
}

func (client *AXClient) semanticTextSelectionV1(
	ctx context.Context, request SemanticTextSelectionRequestV1,
) (SemanticTextSelectionResultV1, error) {
	if err := ctx.Err(); err != nil {
		return SemanticTextSelectionResultV1{}, err
	}
	if err := request.Validate(); err != nil {
		return SemanticTextSelectionResultV1{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return SemanticTextSelectionResultV1{}, err
	}
	id := client.nextID.Add(1)
	payload, err := EncodeSemanticTextSelectionRPCRequestV1(SemanticTextSelectionRPCRequestV1{
		ID: id, Method: "semantic_text_selection", Params: request,
	})
	if err != nil {
		return SemanticTextSelectionResultV1{}, err
	}
	payload = append(payload, '\n')
	responses := make(chan AXResponse, 1)
	client.pendingMu.Lock()
	client.pending[id] = responses
	client.pendingMu.Unlock()
	removePending := func() {
		client.pendingMu.Lock()
		delete(client.pending, id)
		client.pendingMu.Unlock()
	}
	client.writeMu.Lock()
	if err := ctx.Err(); err != nil {
		client.writeMu.Unlock()
		removePending()
		return SemanticTextSelectionResultV1{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return SemanticTextSelectionResultV1{}, newSemanticTextSelectionCommitUnknownV1(writeErr)
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	hardDeadline := commitDeadline.Add(semanticTextSelectionTransportGraceV1)
	maximumHardDeadline := time.Now().Add(3*time.Second + semanticTextSelectionTransportGraceV1)
	if hardDeadline.After(maximumHardDeadline) {
		hardDeadline = maximumHardDeadline
	}
	timer := time.NewTimer(time.Until(hardDeadline))
	defer timer.Stop()
	// AXSelectedTextRange is a mutation. After bytes are written, keep the
	// coordinator barrier occupied until the synchronous helper acknowledges
	// the result or the request's bounded hard deadline expires.
	select {
	case response := <-responses:
		removePending()
		if response.Error != nil {
			return SemanticTextSelectionResultV1{}, newSemanticTextSelectionCommitUnknownV1(
				fmt.Errorf("ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
		}
		result, err := DecodeSemanticTextSelectionResultV1(response.Result)
		if err != nil {
			return SemanticTextSelectionResultV1{}, newSemanticTextSelectionCommitUnknownV1(err)
		}
		if result.SelectedRange != nil && *result.SelectedRange != request.Range {
			return SemanticTextSelectionResultV1{}, newSemanticTextSelectionCommitUnknownV1(
				fmt.Errorf("semantic_text_selection response range mismatch"))
		}
		return result, nil
	case <-timer.C:
		removePending()
		return SemanticTextSelectionResultV1{}, newSemanticTextSelectionCommitUnknownV1(
			fmt.Errorf("helper acknowledgement timed out after commit deadline"))
	}
}
