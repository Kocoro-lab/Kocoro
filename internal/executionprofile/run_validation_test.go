package executionprofile

import (
	"errors"
	"strings"
	"testing"
)

func persistedFastRunForTest() Run {
	profile := validFastProfile()
	profile.RequestedMode = ModeFast
	profile.ResolutionReason = "cloud_profile_resolved"
	return Run{
		LogicalTaskID: "burst:t01",
		RunID:         "burst:t01.r01",
		Profile:       profile,
	}
}

func persistedFullComputerRunForTest(profile Profile, toolName string) Run {
	profile.RequestedMode = ModeFull
	profile.EffectiveMode = ModeFull
	profile.ResolutionReason = "cloud_computer_profile_resolved"
	return Run{
		LogicalTaskID: "burst:t01",
		RunID:         "burst:t01.r01",
		Profile:       FullProfile(ModeFull, "requested_full"),
		ComputerActivation: &ComputerActivation{
			Profile:            profile,
			ToolName:           toolName,
			ToolsetFingerprint: strings.Repeat("a", 64),
		},
	}
}

func TestValidatePersistedAcceptsCanonicalRuns(t *testing.T) {
	tests := []struct {
		name string
		run  Run
	}{
		{name: "fast kfp1", run: persistedFastRunForTest()},
		{
			name: "full without computer",
			run: Run{
				RunID:   "ker1_full",
				Profile: FullProfile(ModeFast, "fast_setting_disabled"),
			},
		},
		{
			name: "full with native computer",
			run:  persistedFullComputerRunForTest(validNativeComputerProfile(), "computer"),
		},
		{
			name: "full with generic computer",
			run:  persistedFullComputerRunForTest(validGenericComputerProfile(), "computer_use"),
		},
		{
			name: "successful side effect with canonical arguments digest",
			run: func() Run {
				run := persistedFastRunForTest()
				run.Evidence.ToolOutcomes = []ToolOutcomeEvidence{{
					ToolCallID: "write-1", ToolName: "file_write", Validated: true,
					Outcome: "succeeded", SideEffect: true,
					ArgumentsDigest: strings.Repeat("a", 64),
				}}
				return run
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run.ValidatePersisted(); err != nil {
				t.Fatalf("ValidatePersisted() error = %v", err)
			}
		})
	}
}

func TestValidatePersistedRejectsUnsafeRuns(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Run)
		wantErr string
	}{
		{
			name:    "old checkpoint without run",
			mutate:  func(run *Run) { *run = Run{} },
			wantErr: "run_id",
		},
		{
			name: "invalid kfp1",
			mutate: func(run *Run) {
				run.Profile.ProfileID = "ep1_not-fast"
			},
			wantErr: "invalid fast profile",
		},
		{
			name: "valid kfp1 cannot stack valid ep1",
			mutate: func(run *Run) {
				fullComputer := persistedFullComputerRunForTest(
					validNativeComputerProfile(),
					"computer",
				)
				run.ComputerActivation = fullComputer.ComputerActivation
			},
			wantErr: "fast run cannot contain a computer activation",
		},
		{
			name: "full profile cannot carry provider override",
			mutate: func(run *Run) {
				run.Profile = FullProfile(ModeFull, "requested_full")
				run.Profile.Model = "claude-sonnet-5"
			},
			wantErr: "contains execution overrides",
		},
		{
			name: "computer tool name must match contract",
			mutate: func(run *Run) {
				*run = persistedFullComputerRunForTest(
					validNativeComputerProfile(),
					"computer_use",
				)
			},
			wantErr: "does not match contract",
		},
		{
			name: "computer fingerprint must be real digest",
			mutate: func(run *Run) {
				*run = persistedFullComputerRunForTest(
					validNativeComputerProfile(),
					"computer",
				)
				run.ComputerActivation.ToolsetFingerprint = "stale-toolset"
			},
			wantErr: "lowercase SHA-256",
		},
		{
			name: "computer profile mode is immutable",
			mutate: func(run *Run) {
				*run = persistedFullComputerRunForTest(
					validNativeComputerProfile(),
					"computer",
				)
				run.ComputerActivation.Profile.EffectiveMode = ModeFast
			},
			wantErr: "modes must both be full",
		},
		{
			name: "computer profile cannot carry service tier",
			mutate: func(run *Run) {
				*run = persistedFullComputerRunForTest(
					validNativeComputerProfile(),
					"computer",
				)
				run.ComputerActivation.Profile.ServiceTier = "fast"
			},
			wantErr: "fast/full execution overrides",
		},
		{
			name: "successful side effect requires arguments digest",
			mutate: func(run *Run) {
				run.Evidence.ToolOutcomes = []ToolOutcomeEvidence{{
					ToolCallID: "write-1", ToolName: "file_write", Validated: true,
					Outcome: "succeeded", SideEffect: true,
				}}
			},
			wantErr: "arguments digest",
		},
		{
			name: "successful side effect digest must be lowercase",
			mutate: func(run *Run) {
				run.Evidence.ToolOutcomes = []ToolOutcomeEvidence{{
					ToolCallID: "write-1", ToolName: "file_write", Validated: true,
					Outcome: "succeeded", SideEffect: true,
					ArgumentsDigest: strings.Repeat("A", 64),
				}}
			},
			wantErr: "arguments digest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := persistedFastRunForTest()
			tc.mutate(&run)
			err := run.ValidatePersisted()
			if !errors.Is(err, ErrInvalidPersistedRun) ||
				!strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf(
					"ValidatePersisted() error = %v, want ErrInvalidPersistedRun containing %q",
					err,
					tc.wantErr,
				)
			}
		})
	}
}
