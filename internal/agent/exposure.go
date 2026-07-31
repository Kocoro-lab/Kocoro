package agent

// ToolExposure controls whether a tool schema is sent directly to the model or
// discovered through tool_search. It is independent from approval and
// execution safety: a Direct tool may still require approval, and a Deferred
// tool keeps all of its normal approval checks after discovery.
type ToolExposure string

const (
	// ToolExposureDefault lets the tool's source choose the effective exposure.
	ToolExposureDefault ToolExposure = ""
	// ToolExposureDirect makes the full schema available on the first turn.
	ToolExposureDirect ToolExposure = "direct"
	// ToolExposureDeferred keeps the schema behind tool_search until selected.
	ToolExposureDeferred ToolExposure = "deferred"
)

// ToolExposureProvider is an optional per-tool override. Tools without this
// interface follow their source default: local tools are Direct; MCP, gateway,
// and integration tools are Deferred.
type ToolExposureProvider interface {
	ToolExposure() ToolExposure
}

// EffectiveToolExposure resolves explicit per-tool policy before source
// defaults. Unknown and local sources fail open to Direct so adding the
// abstraction cannot silently hide existing local tools.
func EffectiveToolExposure(tool Tool) ToolExposure {
	if provider, ok := tool.(ToolExposureProvider); ok {
		switch exposure := provider.ToolExposure(); exposure {
		case ToolExposureDirect, ToolExposureDeferred:
			return exposure
		}
	}

	if sourcer, ok := tool.(ToolSourcer); ok {
		switch sourcer.ToolSource() {
		case SourceMCP, SourceGateway, SourceIntegration:
			return ToolExposureDeferred
		}
	}
	return ToolExposureDirect
}

// ToolProfileRequirement marks schemas that are unsafe to advertise or warm
// before Cloud binds them to an exact execution contract.
type ToolProfileRequirement string

const (
	ToolProfileNone     ToolProfileRequirement = ""
	ToolProfileComputer ToolProfileRequirement = "computer_use"
)

// ToolProfileRequirementProvider is an optional, tool-owned execution-profile
// policy. It is intentionally separate from ToolExposure and approval: a
// profile-bound tool is still Deferred and still keeps its normal approval.
type ToolProfileRequirementProvider interface {
	ToolProfileRequirement() ToolProfileRequirement
}

func EffectiveToolProfileRequirement(tool Tool) ToolProfileRequirement {
	if provider, ok := tool.(ToolProfileRequirementProvider); ok {
		switch requirement := provider.ToolProfileRequirement(); requirement {
		case ToolProfileComputer:
			return requirement
		}
	}
	return ToolProfileNone
}

func profileBoundToolNames(reg *ToolRegistry) map[string]bool {
	names := make(map[string]bool)
	if reg == nil {
		return names
	}
	for _, name := range reg.SortedNames() {
		if tool, ok := reg.Get(name); ok && EffectiveToolProfileRequirement(tool) != ToolProfileNone {
			names[name] = true
		}
	}
	return names
}
