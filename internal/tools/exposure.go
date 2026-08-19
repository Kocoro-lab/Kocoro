package tools

import "github.com/Kocoro-lab/ShanClaw/internal/agent"

// Explicit local exposure is tool-owned so it survives registry clones,
// filtering, MCP health rebuilds, and dynamic overlay extraction.
//
// Small, common local utilities such as clipboard and notify intentionally
// inherit the Direct local default. Process and GUI automation are occasional,
// schema-heavy capabilities, so they stay discoverable but Deferred.

func (*AskUserQuestionTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDirect
}

func (*ProcessTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*AppleScriptTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*AccessibilityTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*ComputerUseTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*ComputerUseTool) ToolProfileRequirement() agent.ToolProfileRequirement {
	return agent.ToolProfileComputer
}

func (*GhosttyTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*BrowserTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*ScreenshotTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*ComputerTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*ComputerTool) ToolProfileRequirement() agent.ToolProfileRequirement {
	return agent.ToolProfileComputer
}

func (*WaitTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*XUploadMediaTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (t *ScheduleTool) ToolExposure() agent.ToolExposure {
	switch t.action {
	case "create", "update", "remove":
		return agent.ToolExposureDeferred
	case "list", "show":
		return agent.ToolExposureDirect
	default:
		return agent.ToolExposureDefault
	}
}

func (*CalendarCheckPermissionTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDirect
}

func (*CalendarRequestPermissionTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*CalendarListSourcesTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDirect
}

func (*CalendarListEventsTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDirect
}

func (*CalendarGetEventTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDirect
}

func (*CalendarCreateEventTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*CalendarUpdateEventTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*CalendarDeleteEventTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (t *ServerTool) ToolExposure() agent.ToolExposure {
	// Only Cloud's canonical research openers are trusted as always-present
	// Direct tools. x_search is the X-native research opener and deliberately
	// matches web_search's always-visible discovery experience. Same-named MCP
	// or integration tools retain their source default so a third-party catalog
	// cannot expand the base schema surface.
	if t.source == agent.SourceGateway {
		switch t.schema.Name {
		case "web_search", "web_fetch", "x_search":
			return agent.ToolExposureDirect
		}
	}
	return agent.ToolExposureDefault
}

func (t *ServerTool) ToolSearchNamespace() string {
	return string(t.source)
}

func (t *MCPTool) ToolSearchNamespace() string {
	return t.ServerName()
}

func (*PublishToWebTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*GenerateImageTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*EditImageTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*ListPublishedFilesTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*RetractPublishedFileTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}
