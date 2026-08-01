package executionprofile

import (
	"errors"
	"strings"
	"testing"
)

func validFastProfile() Profile {
	return Profile{
		EffectiveMode:       ModeFast,
		SchemaVersion:       FastSchemaVersion,
		ProfileName:         FastProfileName,
		ProfileVersion:      FastProfileVersion,
		ProfileID:           "kfp1_test-opaque",
		Provider:            "openai",
		Model:               "gpt-5.6-luna",
		APISurface:          "openai_responses",
		ToolContract:        FastToolContract,
		ReasoningEffort:     "medium",
		ServiceTier:         "fast",
		ParallelToolCalls:   true,
		ResponseCachePolicy: ResponseCacheOff,
	}
}

func validNativeComputerProfile() Profile {
	return Profile{
		SchemaVersion:        ComputerSchemaVersion,
		ContractRevision:     ComputerContractRevision,
		ProfileID:            "ep1_native-test",
		Provider:             "anthropic",
		Model:                "claude-sonnet-5",
		APISurface:           AnthropicMessagesAPISurface,
		ExecutionMode:        ComputerExecutionModeNative,
		ToolContract:         AnthropicComputerToolContract,
		BetaContract:         AnthropicComputerBetaContract,
		SupportsImageInput:   true,
		SupportsToolImages:   true,
		SupportsFunctions:    true,
		SupportsBatchActions: false,
	}
}

func validGenericComputerProfile() Profile {
	return Profile{
		SchemaVersion:        ComputerSchemaVersion,
		ContractRevision:     ComputerContractRevision,
		ProfileID:            "ep1_generic-test",
		Provider:             "openai",
		Model:                "gpt-5-mini-2025-08-07",
		APISurface:           "openai_chat_completions",
		ExecutionMode:        ComputerExecutionModeFunction,
		ToolContract:         GenericComputerUseToolContract,
		SupportsImageInput:   true,
		SupportsFunctions:    true,
		SupportsBatchActions: false,
	}
}

func TestNormalizeModeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
	}{
		{"fast", ModeFast},
		{" FAST ", ModeFast},
		{"full", ModeFull},
		{"", ModeFull},
		{"turbo", ModeFull},
	} {
		if got := NormalizeMode(tc.in); got != tc.want {
			t.Fatalf("NormalizeMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveFastDisabledReturnsPureFullProfile(t *testing.T) {
	got := Resolve(ResolutionInput{
		RequestedMode: ModeFast,
		FastEnabled:   false,
		CloudProfile:  func() *Profile { p := validFastProfile(); return &p }(),
	})
	want := Profile{
		RequestedMode:    ModeFast,
		EffectiveMode:    ModeFull,
		ResolutionReason: "fast_setting_disabled",
	}
	if got != want {
		t.Fatalf("Resolve() = %+v, want pure full profile %+v", got, want)
	}
	if got.ResponseCachePolicy != "" {
		t.Fatalf("full response_cache_policy = %q, want empty", got.ResponseCachePolicy)
	}
}

func TestValidateFastRejectsLegacyAndDriftedProfileFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Profile)
		wantErr string
	}{
		{
			name: "legacy profile version",
			mutate: func(profile *Profile) {
				profile.ProfileVersion = FastSchemaVersion
			},
			wantErr: "profile_version",
		},
		{
			name: "legacy Terra model",
			mutate: func(profile *Profile) {
				profile.Model = "gpt-5.6-terra"
			},
			wantErr: "model",
		},
		{
			name: "legacy chat surface",
			mutate: func(profile *Profile) {
				profile.APISurface = "openai_chat_completions"
			},
			wantErr: "api_surface",
		},
		{
			name: "none effort",
			mutate: func(profile *Profile) {
				profile.ReasoningEffort = "none"
			},
			wantErr: "reasoning_effort",
		},
		{
			name: "standard service tier",
			mutate: func(profile *Profile) {
				profile.ServiceTier = "default"
			},
			wantErr: "service_tier",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profile := validFastProfile()
			tc.mutate(&profile)
			if err := profile.ValidateFast(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateFast() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateComputerAcceptsSupportedContracts(t *testing.T) {
	tests := []struct {
		name     string
		profile  Profile
		contract string
	}{
		{
			name:     "anthropic native",
			profile:  validNativeComputerProfile(),
			contract: AnthropicComputerToolContract,
		},
		{
			name:     "provider-neutral function tool",
			profile:  validGenericComputerProfile(),
			contract: GenericComputerUseToolContract,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.profile.ValidateComputer(tc.contract); err != nil {
				t.Fatalf("ValidateComputer() error = %v", err)
			}
		})
	}
}

func TestValidateComputerRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profile  func() Profile
		contract string
		wantErr  string
	}{
		{
			name: "profile id must be opaque ep1",
			profile: func() Profile {
				p := validNativeComputerProfile()
				p.ProfileID = "caller-chosen"
				return p
			},
			contract: AnthropicComputerToolContract,
			wantErr:  "opaque ep1",
		},
		{
			name: "required contract must match",
			profile: func() Profile {
				return validNativeComputerProfile()
			},
			contract: GenericComputerUseToolContract,
			wantErr:  "tool_contract",
		},
		{
			name: "native contract rejects batched actions",
			profile: func() Profile {
				p := validNativeComputerProfile()
				p.SupportsBatchActions = true
				return p
			},
			contract: AnthropicComputerToolContract,
			wantErr:  "supports_batched_actions must be false",
		},
		{
			name: "generic contract rejects batched actions",
			profile: func() Profile {
				p := validGenericComputerProfile()
				p.SupportsBatchActions = true
				return p
			},
			contract: GenericComputerUseToolContract,
			wantErr:  "supports_batched_actions must be false",
		},
		{
			name: "generic contract requires function mode",
			profile: func() Profile {
				p := validGenericComputerProfile()
				p.ExecutionMode = ComputerExecutionModeNative
				return p
			},
			contract: GenericComputerUseToolContract,
			wantErr:  "function_computer_use",
		},
		{
			name: "unsupported contract is rejected",
			profile: func() Profile {
				p := validGenericComputerProfile()
				p.ToolContract = "openai.computer.v1"
				return p
			},
			contract: "",
			wantErr:  "unsupported computer tool_contract",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.profile().ValidateComputer(tc.contract); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateComputer() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveMatrix(t *testing.T) {
	valid := validFastProfile()
	tests := []struct {
		name       string
		in         ResolutionInput
		wantMode   Mode
		wantReason string
		wantID     string
	}{
		{"full bypasses cloud", ResolutionInput{RequestedMode: ModeFull, FastEnabled: true, CloudProfile: &valid}, ModeFull, "requested_full", ""},
		{"setting disabled", ResolutionInput{RequestedMode: ModeFast, FastEnabled: false, CloudProfile: &valid}, ModeFull, "fast_setting_disabled", ""},
		{"resolver error", ResolutionInput{RequestedMode: ModeFast, FastEnabled: true, CloudError: errors.New("offline")}, ModeFull, "cloud_resolver_failed", ""},
		{"profile missing", ResolutionInput{RequestedMode: ModeFast, FastEnabled: true}, ModeFull, "cloud_profile_missing", ""},
		{"profile invalid", ResolutionInput{RequestedMode: ModeFast, FastEnabled: true, CloudProfile: &Profile{}}, ModeFull, "cloud_profile_invalid", ""},
		{"fast resolved", ResolutionInput{RequestedMode: ModeFast, FastEnabled: true, CloudProfile: &valid}, ModeFast, "cloud_profile_resolved", valid.ProfileID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.in)
			if got.EffectiveMode != tc.wantMode || got.ResolutionReason != tc.wantReason || got.ProfileID != tc.wantID {
				t.Fatalf("Resolve() = %+v, want mode=%q reason=%q id=%q", got, tc.wantMode, tc.wantReason, tc.wantID)
			}
		})
	}
}
