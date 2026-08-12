package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const ToolExecutionSchemaVersion = 1

// MaxRetainedTerminalToolExecutions bounds completed ledger history per
// session. A long editing run can create hundreds of checkpointed writes; once
// this binds, only the oldest terminal audit records are removed while every
// unresolved/outcome-unknown record remains. It is not operator-configurable:
// changing retention affects the session recovery schema and requires a code
// review rather than a per-install tuning knob.
const MaxRetainedTerminalToolExecutions = 256

type ToolExecutionState string

const (
	ToolExecutionPrepared       ToolExecutionState = "prepared"
	ToolExecutionDispatching    ToolExecutionState = "dispatching"
	ToolExecutionCommitted      ToolExecutionState = "committed"
	ToolExecutionFailedNoEffect ToolExecutionState = "failed_no_effect"
	ToolExecutionCheckpointed   ToolExecutionState = "checkpointed"
	ToolExecutionOutcomeUnknown ToolExecutionState = "outcome_unknown"
	ToolExecutionAbandoned      ToolExecutionState = "abandoned"
)

var ErrInvalidToolExecutionLedger = errors.New("invalid tool execution ledger")

// ToolExecutionRecord is intentionally content-free. ToolUseIDDigest pairs a
// record with the model-visible transcript without persisting the provider's
// identifier, while ArgumentsDigest and ResultDigest permit audit comparison
// without retaining user or tool payloads.
type ToolExecutionRecord struct {
	SchemaVersion   int                `json:"schema_version"`
	ExecutionID     string             `json:"execution_id"`
	IdempotencyKey  string             `json:"idempotency_key"`
	RunID           string             `json:"run_id"`
	AttemptID       string             `json:"attempt_id"`
	ToolName        string             `json:"tool_name"`
	ToolUseIDDigest string             `json:"tool_use_id_digest"`
	ArgumentsDigest string             `json:"arguments_digest"`
	ResultDigest    string             `json:"result_digest,omitempty"`
	State           ToolExecutionState `json:"state"`
	PreparedAt      time.Time          `json:"prepared_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// NewToolExecutionRecord creates a prepared record. The raw tool-use ID and
// arguments exist only for the duration of this call and are never retained.
func NewToolExecutionRecord(runID, attemptID, toolName, toolUseID, arguments string, now time.Time) (ToolExecutionRecord, error) {
	return NewToolExecutionRecordFromDigest(runID, attemptID, toolName, toolUseID, ToolExecutionDigest(arguments), now)
}

// NewToolExecutionRecordFromDigest avoids double-hashing when the execution
// boundary already discarded raw arguments before entering the session layer.
func NewToolExecutionRecordFromDigest(runID, attemptID, toolName, toolUseID, argumentsDigest string, now time.Time) (ToolExecutionRecord, error) {
	if !validToolExecutionDigest(argumentsDigest) {
		return ToolExecutionRecord{}, fmt.Errorf("%w: invalid arguments digest", ErrInvalidToolExecutionLedger)
	}
	executionID, err := newToolExecutionOpaqueID("tex_")
	if err != nil {
		return ToolExecutionRecord{}, fmt.Errorf("%w: mint execution id: %v", ErrInvalidToolExecutionLedger, err)
	}
	idempotencyKey, err := newToolExecutionOpaqueID("tik_")
	if err != nil {
		return ToolExecutionRecord{}, fmt.Errorf("%w: mint idempotency key: %v", ErrInvalidToolExecutionLedger, err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	record := ToolExecutionRecord{
		SchemaVersion:   ToolExecutionSchemaVersion,
		ExecutionID:     executionID,
		IdempotencyKey:  idempotencyKey,
		RunID:           runID,
		AttemptID:       attemptID,
		ToolName:        toolName,
		ToolUseIDDigest: ToolExecutionDigest(toolUseID),
		ArgumentsDigest: argumentsDigest,
		State:           ToolExecutionPrepared,
		PreparedAt:      now.UTC(),
		UpdatedAt:       now.UTC(),
	}
	if err := record.validate(); err != nil {
		return ToolExecutionRecord{}, err
	}
	return record, nil
}

// ToolExecutionDigest returns the lowercase SHA-256 digest of value.
func ToolExecutionDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func newToolExecutionOpaqueID(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(entropy[:]), nil
}

func (s *Session) AddToolExecution(record ToolExecutionRecord) error {
	if err := record.validate(); err != nil {
		return err
	}
	if record.State != ToolExecutionPrepared {
		return fmt.Errorf("%w: new record must be prepared", ErrInvalidToolExecutionLedger)
	}
	for i := range s.ToolExecutions {
		if s.ToolExecutions[i].ExecutionID == record.ExecutionID {
			return fmt.Errorf("%w: duplicate execution id", ErrInvalidToolExecutionLedger)
		}
	}
	s.ToolExecutions = append(s.ToolExecutions, record)
	return nil
}

func (s *Session) MarkToolExecutionDispatching(executionID string, now time.Time) error {
	return s.transitionToolExecution(executionID, ToolExecutionDispatching, "", now)
}

func (s *Session) MarkToolExecutionCommitted(executionID, resultDigest string, now time.Time) error {
	return s.transitionToolExecution(executionID, ToolExecutionCommitted, resultDigest, now)
}

func (s *Session) MarkToolExecutionFailedNoEffect(executionID, resultDigest string, now time.Time) error {
	return s.transitionToolExecution(executionID, ToolExecutionFailedNoEffect, resultDigest, now)
}

func (s *Session) MarkToolExecutionOutcomeUnknown(executionID, resultDigest string, now time.Time) error {
	return s.transitionToolExecution(executionID, ToolExecutionOutcomeUnknown, resultDigest, now)
}

// MarkToolExecutionCheckpointed records that the matching result has joined
// the durable transcript. The result digest is retained from committed state.
func (s *Session) MarkToolExecutionCheckpointed(executionID string, now time.Time) error {
	record, err := s.toolExecution(executionID)
	if err != nil {
		return err
	}
	if record.State != ToolExecutionCommitted || !validToolExecutionDigest(record.ResultDigest) {
		return fmt.Errorf("%w: only committed execution can be checkpointed", ErrInvalidToolExecutionLedger)
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if now.Before(record.UpdatedAt) {
		return fmt.Errorf("%w: transition timestamp moved backwards", ErrInvalidToolExecutionLedger)
	}
	record.State = ToolExecutionCheckpointed
	record.UpdatedAt = now
	return record.validate()
}

func (s *Session) AbandonToolExecution(executionID string, now time.Time) error {
	return s.transitionToolExecution(executionID, ToolExecutionAbandoned, "", now)
}

func (s *Session) transitionToolExecution(executionID string, next ToolExecutionState, resultDigest string, now time.Time) error {
	record, err := s.toolExecution(executionID)
	if err != nil {
		return err
	}
	valid := false
	switch record.State {
	case ToolExecutionPrepared:
		valid = next == ToolExecutionDispatching || next == ToolExecutionAbandoned
	case ToolExecutionDispatching:
		valid = next == ToolExecutionCommitted || next == ToolExecutionFailedNoEffect || next == ToolExecutionOutcomeUnknown
	case ToolExecutionCommitted:
		valid = next == ToolExecutionOutcomeUnknown
	}
	if !valid {
		return fmt.Errorf("%w: invalid transition %s to %s", ErrInvalidToolExecutionLedger, record.State, next)
	}
	if (next == ToolExecutionCommitted || next == ToolExecutionFailedNoEffect) && !validToolExecutionDigest(resultDigest) {
		return fmt.Errorf("%w: definitive execution result requires result digest", ErrInvalidToolExecutionLedger)
	}
	if resultDigest != "" && !validToolExecutionDigest(resultDigest) {
		return fmt.Errorf("%w: invalid result digest", ErrInvalidToolExecutionLedger)
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if now.Before(record.UpdatedAt) {
		return fmt.Errorf("%w: transition timestamp moved backwards", ErrInvalidToolExecutionLedger)
	}
	record.State = next
	record.ResultDigest = resultDigest
	record.UpdatedAt = now
	return record.validate()
}

func (s *Session) toolExecution(executionID string) (*ToolExecutionRecord, error) {
	for i := range s.ToolExecutions {
		if s.ToolExecutions[i].ExecutionID == executionID {
			return &s.ToolExecutions[i], nil
		}
	}
	return nil, fmt.Errorf("%w: execution id not found", ErrInvalidToolExecutionLedger)
}

// ReconcileToolExecutionCheckpoints upgrades committed records only when the
// transcript being saved already contains their matching tool_result. Because
// Store.save invokes this before its atomic rename, the result and checkpoint
// state become durable together.
func (s *Session) ReconcileToolExecutionCheckpoints(now time.Time) error {
	if err := s.ValidateToolExecutions(); err != nil {
		return err
	}
	resultIDs := make(map[string]struct{})
	legacyResultTools := make(map[string]string)
	for _, message := range s.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				resultIDs[ToolExecutionDigest(block.ToolUseID)] = struct{}{}
			}
		}
		if legacyText, ok := legacyToolExecutionMessageText(message); ok {
			for _, result := range parseLegacyToolExecutionResults(legacyText) {
				legacyResultTools[ToolExecutionDigest(result.callID)] = result.toolName
			}
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	matching := make([]int, 0)
	for i := range s.ToolExecutions {
		record := &s.ToolExecutions[i]
		if record.State != ToolExecutionCommitted {
			continue
		}
		if _, ok := resultIDs[record.ToolUseIDDigest]; !ok {
			if toolName, legacyOK := legacyResultTools[record.ToolUseIDDigest]; !legacyOK || toolName != record.ToolName {
				continue
			}
		}
		matching = append(matching, i)
	}
	for _, i := range matching {
		record := &s.ToolExecutions[i]
		record.State = ToolExecutionCheckpointed
		if !now.Before(record.UpdatedAt) {
			record.UpdatedAt = now
		}
	}
	return s.ValidateToolExecutions()
}

type legacyToolExecutionResult struct {
	toolName string
	callID   string
}

// legacyToolExecutionResultPattern mirrors the exact fallback emitted by
// agent.formatToolExec, whose attributes use Go's %q escaping. Each candidate
// block is anchored so attributes or prose cannot be smuggled around it.
var legacyToolExecutionResultPattern = regexp.MustCompile(`(?s)^<tool_exec tool=("(?:\\.|[^"\\])*") call_id=("(?:\\.|[^"\\])*")>\n<input>.*?</input>\n<output status="(?:ok|error)">.*?</output>\n</tool_exec>$`)

const legacyToolExecutionCloseTag = "</tool_exec>"

func legacyToolExecutionMessageText(message client.Message) (string, bool) {
	if message.Role != "user" {
		return "", false
	}
	if !message.Content.HasBlocks() {
		return message.Content.Text(), true
	}
	var text strings.Builder
	hasText := false
	for _, block := range message.Content.Blocks() {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
			hasText = true
		case "image":
			// Legacy image results store their structural execution record in a
			// sibling text block. Image bytes are irrelevant to the ledger join.
		default:
			return "", false
		}
	}
	return text.String(), hasText
}

func parseLegacyToolExecutionResults(text string) []legacyToolExecutionResult {
	remaining := strings.TrimSpace(text)
	if remaining == "" {
		return nil
	}
	results := make([]legacyToolExecutionResult, 0, 1)
	for remaining != "" {
		// Requiring every non-whitespace segment to be a bounded structural
		// sibling rejects quoted/prose text. A malformed but bounded sibling is
		// ignored independently so it cannot suppress valid evidence beside it.
		if !strings.HasPrefix(remaining, "<tool_exec ") {
			return nil
		}
		closeAt := strings.Index(remaining, legacyToolExecutionCloseTag)
		if closeAt < 0 {
			return nil
		}
		blockEnd := closeAt + len(legacyToolExecutionCloseTag)
		block := remaining[:blockEnd]
		if match := legacyToolExecutionResultPattern.FindStringSubmatch(block); match != nil {
			toolName, toolErr := strconv.Unquote(match[1])
			callID, callErr := strconv.Unquote(match[2])
			if toolErr == nil && callErr == nil && toolName != "" && callID != "" && len(toolName) <= 256 && len(callID) <= 256 {
				results = append(results, legacyToolExecutionResult{toolName: toolName, callID: callID})
			}
		}
		remaining = strings.TrimSpace(remaining[blockEnd:])
	}
	return results
}

// TrimTerminalToolExecutions retains the newest bounded set of terminal
// records. Prepared, dispatching, committed, and outcome-unknown entries are
// preserved regardless of age so recovery evidence cannot be discarded.
// Failed-no-effect is terminal because the tool provided structured proof that
// no external mutation committed even if the process stopped before transcript
// checkpointing.
func (s *Session) TrimTerminalToolExecutions(limit int) int {
	if limit < 0 {
		limit = 0
	}
	type terminalEntry struct {
		index     int
		updatedAt time.Time
	}
	terminals := make([]terminalEntry, 0, len(s.ToolExecutions))
	for i, record := range s.ToolExecutions {
		if record.State == ToolExecutionCheckpointed || record.State == ToolExecutionFailedNoEffect || record.State == ToolExecutionAbandoned {
			terminals = append(terminals, terminalEntry{index: i, updatedAt: record.UpdatedAt})
		}
	}
	if len(terminals) <= limit {
		return 0
	}
	sort.Slice(terminals, func(i, j int) bool {
		if terminals[i].updatedAt.Equal(terminals[j].updatedAt) {
			return terminals[i].index > terminals[j].index
		}
		return terminals[i].updatedAt.After(terminals[j].updatedAt)
	})
	keep := make(map[int]struct{}, limit)
	for _, entry := range terminals[:limit] {
		keep[entry.index] = struct{}{}
	}
	trimmed := len(terminals) - limit
	retained := make([]ToolExecutionRecord, 0, len(s.ToolExecutions)-trimmed)
	for i, record := range s.ToolExecutions {
		if record.State != ToolExecutionCheckpointed && record.State != ToolExecutionFailedNoEffect && record.State != ToolExecutionAbandoned {
			retained = append(retained, record)
			continue
		}
		if _, ok := keep[i]; ok {
			retained = append(retained, record)
		}
	}
	s.ToolExecutions = retained
	return trimmed
}

// AbandonPreparedToolExecutions marks executions which durably never crossed
// the dispatch boundary. Dispatched or ambiguous states are deliberately left
// untouched for fail-closed recovery.
func (s *Session) AbandonPreparedToolExecutions(runID string, now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}
	count := 0
	for i := range s.ToolExecutions {
		record := &s.ToolExecutions[i]
		if record.State != ToolExecutionPrepared || (runID != "" && record.RunID != runID) {
			continue
		}
		record.State = ToolExecutionAbandoned
		abandonedAt := now.UTC()
		if abandonedAt.Before(record.UpdatedAt) {
			abandonedAt = record.UpdatedAt
		}
		record.UpdatedAt = abandonedAt
		count++
	}
	return count
}

// BlockingToolExecutions returns records which make automatic replay unsafe.
// The returned slice is a copy so callers cannot bypass state validation.
func (s *Session) BlockingToolExecutions(runID string) []ToolExecutionRecord {
	var blocked []ToolExecutionRecord
	for _, record := range s.ToolExecutions {
		if runID != "" && record.RunID != runID {
			continue
		}
		switch record.State {
		case ToolExecutionDispatching, ToolExecutionCommitted, ToolExecutionOutcomeUnknown:
			blocked = append(blocked, record)
		}
	}
	return blocked
}

func (s *Session) ValidateToolExecutions() error {
	seen := make(map[string]struct{}, len(s.ToolExecutions))
	for i := range s.ToolExecutions {
		record := &s.ToolExecutions[i]
		if err := record.validate(); err != nil {
			return fmt.Errorf("%w: record %d", err, i)
		}
		if _, ok := seen[record.ExecutionID]; ok {
			return fmt.Errorf("%w: duplicate execution id", ErrInvalidToolExecutionLedger)
		}
		seen[record.ExecutionID] = struct{}{}
	}
	return nil
}

func (r ToolExecutionRecord) validate() error {
	if r.SchemaVersion != ToolExecutionSchemaVersion {
		return fmt.Errorf("%w: unsupported schema version", ErrInvalidToolExecutionLedger)
	}
	if !validOpaqueToolExecutionValue(r.ExecutionID, 128) || !validOpaqueToolExecutionValue(r.IdempotencyKey, 128) {
		return fmt.Errorf("%w: invalid opaque identifier", ErrInvalidToolExecutionLedger)
	}
	if !validOpaqueToolExecutionValue(r.RunID, 256) || !validOpaqueToolExecutionValue(r.AttemptID, 256) || !validOpaqueToolExecutionValue(r.ToolName, 256) {
		return fmt.Errorf("%w: invalid execution metadata", ErrInvalidToolExecutionLedger)
	}
	if !validToolExecutionDigest(r.ToolUseIDDigest) || !validToolExecutionDigest(r.ArgumentsDigest) {
		return fmt.Errorf("%w: invalid input digest", ErrInvalidToolExecutionLedger)
	}
	if r.ResultDigest != "" && !validToolExecutionDigest(r.ResultDigest) {
		return fmt.Errorf("%w: invalid result digest", ErrInvalidToolExecutionLedger)
	}
	switch r.State {
	case ToolExecutionPrepared, ToolExecutionDispatching, ToolExecutionAbandoned:
		if r.ResultDigest != "" {
			return fmt.Errorf("%w: result digest before completion", ErrInvalidToolExecutionLedger)
		}
	case ToolExecutionCommitted, ToolExecutionCheckpointed, ToolExecutionFailedNoEffect:
		if r.ResultDigest == "" {
			return fmt.Errorf("%w: missing result digest", ErrInvalidToolExecutionLedger)
		}
	case ToolExecutionOutcomeUnknown:
	default:
		return fmt.Errorf("%w: unknown state", ErrInvalidToolExecutionLedger)
	}
	if r.PreparedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.PreparedAt) {
		return fmt.Errorf("%w: invalid timestamps", ErrInvalidToolExecutionLedger)
	}
	return nil
}

func validOpaqueToolExecutionValue(value string, max int) bool {
	if strings.TrimSpace(value) != value || value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func validToolExecutionDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
