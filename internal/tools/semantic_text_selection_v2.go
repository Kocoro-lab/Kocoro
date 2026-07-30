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

const semanticTextSelectionTransportGraceV2 = 150 * time.Millisecond

type SemanticTextRangeV2 struct {
	Location int `json:"location"`
	Length   int `json:"length"`
}

// SemanticTextSelectionRequestV2 is one immutable AX selection authority. The
// helper may report that coordinate fallback is needed, but it can never
// perform that fallback implicitly.
type SemanticTextSelectionRequestV2 struct {
	SchemaVersion       int                 `json:"schema_version"`
	PID                 int                 `json:"pid"`
	BundleID            string              `json:"bundle_id"`
	WindowID            uint32              `json:"window_id"`
	Ref                 string              `json:"ref"`
	Path                string              `json:"path"`
	ExpectedRole        string              `json:"expected_role"`
	ExpectedFingerprint string              `json:"expected_fingerprint"`
	Range               SemanticTextRangeV2 `json:"range"`
	FallbackPolicy      string              `json:"fallback_policy"`
	CommitDeadlineAt    string              `json:"commit_deadline_at"`
}

type SemanticTextSelectionRPCRequestV2 struct {
	ID     int64                          `json:"id"`
	Method string                         `json:"method"`
	Params SemanticTextSelectionRequestV2 `json:"params"`
}

type SemanticTextSelectionResultV2 struct {
	SchemaVersion int                  `json:"schema_version"`
	Status        string               `json:"status"`
	CommitState   string               `json:"commit_state"`
	Phase         string               `json:"phase"`
	FailureCode   *string              `json:"failure_code"`
	RetrySafe     bool                 `json:"retry_safe"`
	Postcondition *string              `json:"postcondition"`
	SelectedRange *SemanticTextRangeV2 `json:"selected_range"`
}

var semanticTextRangeWireShapeV2 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"location": coordinateScalarWireShape(false),
	"length":   coordinateScalarWireShape(false),
})

var semanticTextSelectionRequestWireShapeV2 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
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
		"range":                semanticTextRangeWireShapeV2,
		"fallback_policy":      coordinateScalarWireShape(false),
		"commit_deadline_at":   coordinateScalarWireShape(false),
	}),
})

var semanticTextSelectionResultWireShapeV2 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version": coordinateScalarWireShape(false),
	"status":         coordinateScalarWireShape(false),
	"commit_state":   coordinateScalarWireShape(false),
	"phase":          coordinateScalarWireShape(false),
	"failure_code":   coordinateScalarWireShape(true),
	"retry_safe":     coordinateScalarWireShape(false),
	"postcondition":  coordinateScalarWireShape(true),
	"selected_range": coordinateNullableWireShape(semanticTextRangeWireShapeV2),
})

func (request SemanticTextSelectionRequestV2) Validate() error {
	if request.SchemaVersion != 2 || request.PID <= 0 || request.WindowID == 0 {
		return fmt.Errorf("semantic_text_selection_v2 authority is required")
	}
	for name, value := range map[string]string{
		"bundle_id":            request.BundleID,
		"ref":                  request.Ref,
		"path":                 request.Path,
		"expected_role":        request.ExpectedRole,
		"expected_fingerprint": request.ExpectedFingerprint,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("semantic_text_selection_v2 %s is invalid", name)
		}
	}
	if !validComputerUseRef(request.Ref) ||
		(request.Path != "window[0]" && !strings.HasPrefix(request.Path, "window[0]/")) ||
		request.FallbackPolicy != "report_unsupported" {
		return fmt.Errorf("semantic_text_selection_v2 ref/path/fallback policy is invalid")
	}
	if request.Range.Location < 0 || request.Range.Length <= 0 ||
		request.Range.Location > int(^uint(0)>>1)-request.Range.Length {
		return fmt.Errorf("semantic_text_selection_v2 range is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("semantic_text_selection_v2 commit_deadline_at must be RFC3339: %w", err)
	}
	return nil
}

func DecodeSemanticTextSelectionRPCRequestV2(payload []byte) (SemanticTextSelectionRPCRequestV2, error) {
	if err := validateCoordinateWireShape(
		"semantic_text_selection_v2 request", payload, semanticTextSelectionRequestWireShapeV2); err != nil {
		return SemanticTextSelectionRPCRequestV2{}, err
	}
	var envelope SemanticTextSelectionRPCRequestV2
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return SemanticTextSelectionRPCRequestV2{}, fmt.Errorf("decode semantic_text_selection_v2 request: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "semantic_text_selection_v2" {
		return SemanticTextSelectionRPCRequestV2{}, fmt.Errorf("invalid semantic_text_selection_v2 RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return SemanticTextSelectionRPCRequestV2{}, err
	}
	return envelope, nil
}

func EncodeSemanticTextSelectionRPCRequestV2(envelope SemanticTextSelectionRPCRequestV2) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "semantic_text_selection_v2" {
		return nil, fmt.Errorf("invalid semantic_text_selection_v2 RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (result SemanticTextSelectionResultV2) ValidateTaggedUnion() error {
	if result.SchemaVersion != 2 || result.RetrySafe {
		return fmt.Errorf("semantic_text_selection_v2 result schema/retry policy is invalid")
	}
	if result.SelectedRange != nil &&
		(result.SelectedRange.Location < 0 || result.SelectedRange.Length <= 0 ||
			result.SelectedRange.Location > int(^uint(0)>>1)-result.SelectedRange.Length) {
		return fmt.Errorf("semantic_text_selection_v2 selected_range is invalid")
	}
	switch result.Status {
	case "verified":
		if result.CommitState != "committed" || result.Phase != "post_verification" ||
			result.FailureCode != nil || result.Postcondition == nil ||
			*result.Postcondition != "selected_range_matches" || result.SelectedRange == nil {
			return fmt.Errorf("invalid verified semantic_text_selection_v2 result")
		}
	case "completed_unverified":
		if result.CommitState == "unknown" {
			if result.Phase != "action" || result.FailureCode == nil ||
				*result.FailureCode != "ax_selection_commit_unknown" ||
				result.Postcondition != nil || result.SelectedRange != nil {
				return fmt.Errorf("invalid commit-unknown semantic_text_selection_v2 result")
			}
			return nil
		}
		if result.CommitState != "committed" || result.Phase != "post_verification" ||
			result.FailureCode == nil || result.Postcondition != nil {
			return fmt.Errorf("invalid completed_unverified semantic_text_selection_v2 result")
		}
		if *result.FailureCode == "selected_range_mismatch" {
			if result.SelectedRange == nil {
				return fmt.Errorf("selection mismatch omitted observed range")
			}
		} else if (*result.FailureCode != "selected_range_not_observed" &&
			*result.FailureCode != "interference_detection_unavailable" &&
			*result.FailureCode != "ax_messaging_timeout_restore_failed") || result.SelectedRange != nil {
			return fmt.Errorf("invalid unobserved semantic_text_selection_v2 result")
		}
	case "user_interference":
		if (result.CommitState != "not_committed" && result.CommitState != "committed" &&
			result.CommitState != "unknown") || result.Phase != "user_interference" ||
			result.FailureCode == nil || *result.FailureCode != "physical_input_interference" ||
			result.Postcondition != nil || result.SelectedRange != nil {
			return fmt.Errorf("invalid user_interference semantic_text_selection_v2 result")
		}
	case "fallback_required":
		if result.CommitState != "not_committed" || result.Phase != "preflight" ||
			result.FailureCode == nil || *result.FailureCode != "ax_text_range_unsupported" ||
			result.Postcondition != nil || result.SelectedRange != nil {
			return fmt.Errorf("invalid fallback_required semantic_text_selection_v2 result")
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
			"ax_messaging_timeout_unavailable":   true,
		}
		exactPhase := result.FailureCode != nil &&
			((preflight[*result.FailureCode] && result.Phase == "preflight") ||
				(*result.FailureCode == "ax_selection_failed" && result.Phase == "action"))
		if result.CommitState != "not_committed" || !exactPhase ||
			result.Postcondition != nil || result.SelectedRange != nil {
			return fmt.Errorf("invalid failed semantic_text_selection_v2 result")
		}
	default:
		return fmt.Errorf("invalid semantic_text_selection_v2 status %q", result.Status)
	}
	return nil
}

func DecodeSemanticTextSelectionResultV2(payload []byte) (SemanticTextSelectionResultV2, error) {
	if err := validateCoordinateWireShape(
		"semantic_text_selection_v2 result", payload, semanticTextSelectionResultWireShapeV2); err != nil {
		return SemanticTextSelectionResultV2{}, err
	}
	var result SemanticTextSelectionResultV2
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return SemanticTextSelectionResultV2{}, fmt.Errorf("decode semantic_text_selection_v2 result: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return SemanticTextSelectionResultV2{}, err
	}
	return result, nil
}

func EncodeSemanticTextSelectionResultV2(result SemanticTextSelectionResultV2) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

type SemanticTextSelectionCommitUnknownErrorV2 struct{ cause error }

func (err *SemanticTextSelectionCommitUnknownErrorV2) Error() string {
	return fmt.Sprintf("semantic_text_selection_v2 commit unknown (not retry-safe): %v", err.cause)
}
func (err *SemanticTextSelectionCommitUnknownErrorV2) Unwrap() error       { return err.cause }
func (err *SemanticTextSelectionCommitUnknownErrorV2) RetrySafe() bool     { return false }
func (err *SemanticTextSelectionCommitUnknownErrorV2) CommitUnknown() bool { return true }

func newSemanticTextSelectionCommitUnknownV2(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &SemanticTextSelectionCommitUnknownErrorV2{cause: cause}
}

func (client *AXClient) SemanticTextSelectionV2(
	ctx context.Context, request SemanticTextSelectionRequestV2,
) (SemanticTextSelectionResultV2, error) {
	if runtime.GOOS != "darwin" {
		return SemanticTextSelectionResultV2{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.semanticTextSelectionV2(ctx, request)
}

func (client *AXClient) semanticTextSelectionV2(
	ctx context.Context, request SemanticTextSelectionRequestV2,
) (SemanticTextSelectionResultV2, error) {
	if err := ctx.Err(); err != nil {
		return SemanticTextSelectionResultV2{}, err
	}
	if err := request.Validate(); err != nil {
		return SemanticTextSelectionResultV2{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return SemanticTextSelectionResultV2{}, err
	}
	id := client.nextID.Add(1)
	payload, err := EncodeSemanticTextSelectionRPCRequestV2(SemanticTextSelectionRPCRequestV2{
		ID: id, Method: "semantic_text_selection_v2", Params: request,
	})
	if err != nil {
		return SemanticTextSelectionResultV2{}, err
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
		return SemanticTextSelectionResultV2{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return SemanticTextSelectionResultV2{}, newSemanticTextSelectionCommitUnknownV2(
			fmt.Errorf("ax_server semantic_text_selection_v2 write: %w", writeErr))
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	now := time.Now()
	hardDeadline := commitDeadline.Add(semanticTextSelectionTransportGraceV2)
	if minimum := now.Add(semanticTextSelectionTransportGraceV2); hardDeadline.Before(minimum) {
		hardDeadline = minimum
	}
	if maximum := now.Add(2*time.Second + semanticTextSelectionTransportGraceV2); hardDeadline.After(maximum) {
		hardDeadline = maximum
	}
	timer := time.NewTimer(time.Until(hardDeadline))
	defer timer.Stop()
	select {
	case response := <-responses:
		removePending()
		if response.Error != nil {
			return SemanticTextSelectionResultV2{}, newSemanticTextSelectionCommitUnknownV2(fmt.Errorf(
				"ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
		}
		result, decodeErr := DecodeSemanticTextSelectionResultV2(response.Result)
		if decodeErr != nil {
			return SemanticTextSelectionResultV2{}, newSemanticTextSelectionCommitUnknownV2(decodeErr)
		}
		if result.Status == "verified" &&
			(result.SelectedRange == nil || *result.SelectedRange != request.Range) {
			return SemanticTextSelectionResultV2{}, newSemanticTextSelectionCommitUnknownV2(
				fmt.Errorf("semantic_text_selection_v2 response range mismatch"))
		}
		return result, nil
	case <-timer.C:
		removePending()
		return SemanticTextSelectionResultV2{}, newSemanticTextSelectionCommitUnknownV2(
			fmt.Errorf("helper acknowledgement timed out after commit deadline"))
	}
}
