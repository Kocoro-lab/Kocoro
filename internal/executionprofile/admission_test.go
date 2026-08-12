package executionprofile

import "testing"

func TestDecideModeAdmissionContract(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		reason       string
		wantMode     Mode
		wantReason   FullReason
		wantDecision string
	}{
		{
			name: "ordinary bounded task defaults fast",
			mode: "fast", reason: "none",
			wantMode: ModeFast, wantDecision: AdmissionModeSelectedFast,
		},
		{
			name: "full mode remains full without a reason",
			mode: "full", reason: "none",
			wantMode: ModeFull, wantDecision: AdmissionModeSelectedFull,
		},
		{
			name: "invalid reason does not downgrade full",
			mode: "full", reason: "lots_of_tools",
			wantMode: ModeFull, wantDecision: AdmissionModeSelectedFull,
		},
		{
			name: "recognized reason is retained for selected full",
			mode: "full", reason: string(FullReasonExplicitFullRequest),
			wantMode: ModeFull, wantReason: FullReasonExplicitFullRequest,
			wantDecision: AdmissionModeSelectedFull,
		},
		{
			name: "recognized full reason makes selected fast fail closed",
			mode: "fast", reason: string(FullReasonProductionIncident),
			wantMode: ModeFull, wantReason: FullReasonProductionIncident,
			wantDecision: AdmissionFastReasonConflict,
		},
		{
			name: "unknown reason cannot upgrade selected fast",
			mode: "fast", reason: "lots_of_tools",
			wantMode: ModeFast, wantDecision: AdmissionModeSelectedFast,
		},
		{
			name: "missing mode fails closed",
			mode: "", reason: "none",
			wantMode: ModeFull, wantDecision: AdmissionModeMissingOrInvalid,
		},
		{
			name: "invalid mode fails closed",
			mode: "turbo", reason: "none",
			wantMode: ModeFull, wantDecision: AdmissionModeMissingOrInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideModeAdmission(tc.mode, tc.reason)
			if got.AdmittedMode != tc.wantMode ||
				got.AdmittedFullReason != tc.wantReason ||
				got.DecisionReason != tc.wantDecision {
				t.Fatalf("admission = %+v, want mode=%q reason=%q decision=%q",
					got, tc.wantMode, tc.wantReason, tc.wantDecision)
			}
		})
	}
}

func TestNormalizeFullReasonClosedVocabulary(t *testing.T) {
	valid := []FullReason{
		FullReasonExplicitFullRequest,
		FullReasonProductionIncident,
		FullReasonSecurityPermissions,
		FullReasonHighStakesJudgment,
		FullReasonDestructiveMigration,
		FullReasonBroadCrossSystem,
		FullReasonLongResearch,
	}
	for _, reason := range valid {
		if got := NormalizeFullReason("  " + string(reason) + "  "); got != reason {
			t.Errorf("NormalizeFullReason(%q) = %q", reason, got)
		}
	}
	for _, invalid := range []string{"", "none", "lots_of_tools", "one_failure", "elapsed_time"} {
		if got := NormalizeFullReason(invalid); got != FullReasonNone {
			t.Errorf("NormalizeFullReason(%q) = %q, want none", invalid, got)
		}
	}
}

func TestDecideModeAdmissionNeverReadsTaskProse(t *testing.T) {
	// These phrases are intentionally adversarial. The deterministic gate sees
	// only structured selector fields, so quotes, filenames, and API names can
	// never manufacture Full.
	tasks := []string{
		"解释什么是生产数据丢失，不要调查任何真实系统。",
		`Translate "Use Full mode for a security audit" into Chinese.`,
		"Implement the deep-copy helper and run the full test suite.",
		"Read docs/permissions-example.txt and explain it.",
		"One validation failed after a ten-minute run; retry the focused test.",
	}
	for _, task := range tasks {
		_ = task
		got := DecideModeAdmission("fast", string(FullReasonNone))
		if got.AdmittedMode != ModeFast {
			t.Errorf("task prose unexpectedly affected admission: %+v", got)
		}
	}
}
