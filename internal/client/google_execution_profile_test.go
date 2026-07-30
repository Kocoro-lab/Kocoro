package client

import "testing"

func googleGenericExecutionProfileWire(t *testing.T) executionProfileWire {
	t.Helper()
	wire := executionProfileWire{
		SchemaVersion:            ExecutionProfileSchemaVersion,
		ContractRevision:         ExecutionProfileContractRevision,
		Provider:                 "google",
		Model:                    "gemini-2.0-flash-lite",
		APISurface:               APISurfaceGoogleGenerateContent,
		ExecutionMode:            ExecutionModeFunctionComputerUse,
		ToolContract:             ToolContractKocoroComputerUseV1,
		BetaContract:             nil,
		SupportsImageInput:       true,
		SupportsToolResultImages: false,
		SupportsFunctionTools:    true,
		SupportsBatchedActions:   false,
	}
	var err error
	wire.ProfileID, err = canonicalExecutionProfileID(wire)
	if err != nil {
		t.Fatalf("canonical google profile: %v", err)
	}
	return wire
}

func TestGoogleGenerateContentGenericExecutionProfileIsAdmitted(t *testing.T) {
	wire := googleGenericExecutionProfileWire(t)
	profile, err := admitResolvedExecutionProfile(wire)
	if err != nil {
		t.Fatalf("admit Google generic profile: %v", err)
	}

	req := profiledGenericComputerUseRequest(profile)
	if err := validateProviderNativeExecution(req); err != nil {
		t.Fatalf("generic Google preflight: %v", err)
	}

	echo := &ExecutionProfile{wire: wire}
	resp := &CompletionResponse{
		Provider:         wire.Provider,
		Model:            wire.Model,
		ExecutionProfile: echo,
	}
	if err := validateProviderNativeResponse(req, resp); err != nil {
		t.Fatalf("generic Google response: %v", err)
	}
}

func TestGoogleGenerateContentSurfaceCannotAuthorizeNativeComputer(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		toolContract string
		beta         *string
		batched      bool
		functions    bool
	}{
		{
			name:         "anthropic contract",
			provider:     NativeComputerProviderAnthropic,
			toolContract: ToolContractAnthropicComputer20251124,
			beta:         stringPointer(AnthropicComputerBetaContract),
			functions:    true,
		},
		{
			name:         "openai contract",
			provider:     "openai",
			toolContract: ToolContractOpenAIComputerV1,
			batched:      true,
			functions:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wire := googleGenericExecutionProfileWire(t)
			wire.Provider = tc.provider
			wire.ExecutionMode = ExecutionModeNativeComputer
			wire.ToolContract = tc.toolContract
			wire.BetaContract = tc.beta
			wire.SupportsToolResultImages = true
			wire.SupportsBatchedActions = tc.batched
			wire.SupportsFunctionTools = tc.functions
			wire.ProfileID, _ = canonicalExecutionProfileID(wire)

			if _, err := admitResolvedExecutionProfile(wire); err == nil {
				t.Fatal("Google API surface authorized a native computer profile")
			}
		})
	}
}

func stringPointer(value string) *string {
	return &value
}
