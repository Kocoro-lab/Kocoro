package executionprofile

import "strings"

// FullReason is bounded diagnostic context for Koe's soft initial mode choice.
// It never authorizes or denies Full. It is deliberately about the initial task
// only: tool count, elapsed time, validation failures, and other runtime loop
// state are not admission inputs.
type FullReason string

const (
	FullReasonNone                 FullReason = "none"
	FullReasonExplicitFullRequest  FullReason = "explicit_full_mode_request"
	FullReasonProductionIncident   FullReason = "production_incident_or_recovery"
	FullReasonSecurityPermissions  FullReason = "security_or_permissions"
	FullReasonHighStakesJudgment   FullReason = "high_stakes_judgment"
	FullReasonDestructiveMigration FullReason = "destructive_migration"
	FullReasonBroadCrossSystem     FullReason = "broad_cross_system_change"
	FullReasonLongResearch         FullReason = "long_research_synthesis"
)

const (
	AdmissionModeSelectedFast     = "mode_selected_fast"
	AdmissionModeSelectedFull     = "mode_selected_full"
	AdmissionModeMissingOrInvalid = "mode_missing_or_invalid"
)

// ModeAdmission is the deterministic boundary between Realtime's advisory
// selector and the daemon's authoritative execution mode.
type ModeAdmission struct {
	RequestedMode       Mode       `json:"requested_mode"`
	RequestedFullReason FullReason `json:"requested_full_reason"`
	AdmittedMode        Mode       `json:"admitted_mode"`
	AdmittedFullReason  FullReason `json:"admitted_full_reason,omitempty"`
	DecisionReason      string     `json:"decision_reason"`
}

// NormalizeFullReason canonicalizes the closed telemetry vocabulary. Unknown
// values become none and never affect the selected mode.
func NormalizeFullReason(value string) FullReason {
	switch FullReason(strings.ToLower(strings.TrimSpace(value))) {
	case FullReasonExplicitFullRequest:
		return FullReasonExplicitFullRequest
	case FullReasonProductionIncident:
		return FullReasonProductionIncident
	case FullReasonSecurityPermissions:
		return FullReasonSecurityPermissions
	case FullReasonHighStakesJudgment:
		return FullReasonHighStakesJudgment
	case FullReasonDestructiveMigration:
		return FullReasonDestructiveMigration
	case FullReasonBroadCrossSystem:
		return FullReasonBroadCrossSystem
	case FullReasonLongResearch:
		return FullReasonLongResearch
	default:
		return FullReasonNone
	}
}

// DecideModeAdmission normalizes Koe's soft initial routing decision. A valid
// execution_mode is authoritative; FullReason is diagnostic context and never
// upgrades or downgrades the selected mode. Task prose is intentionally not
// scanned for keywords because quoted text, filenames, and multilingual wording
// make lexical routing high-variance. Missing or invalid execution_mode remains
// a legacy/protocol failure and therefore preserves the pre-Fast Full behavior.
func DecideModeAdmission(requestedMode, requestedFullReason string) ModeAdmission {
	mode, validMode := parseRequestedMode(requestedMode)
	reason := NormalizeFullReason(requestedFullReason)
	admission := ModeAdmission{
		RequestedMode:       mode,
		RequestedFullReason: reason,
		AdmittedMode:        ModeFast,
		DecisionReason:      AdmissionModeSelectedFast,
	}
	if !validMode {
		admission.AdmittedMode = ModeFull
		admission.DecisionReason = AdmissionModeMissingOrInvalid
		return admission
	}
	if mode == ModeFull {
		admission.AdmittedMode = ModeFull
		if reason != FullReasonNone {
			admission.AdmittedFullReason = reason
		}
		admission.DecisionReason = AdmissionModeSelectedFull
	}
	return admission
}

func parseRequestedMode(value string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(ModeFast):
		return ModeFast, true
	case string(ModeFull):
		return ModeFull, true
	default:
		return "", false
	}
}
