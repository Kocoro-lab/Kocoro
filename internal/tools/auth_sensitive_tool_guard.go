package tools

import (
	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// authSensitiveToolGuard binds a tool whose client captured an API key by
// value to the verified credential/principal generation that constructed it.
// Old registry clones and approval-wait pointers retain the old generation and
// fail before their underlying client can dispatch.
type authSensitiveToolGuard struct {
	gateway    *client.GatewayClient
	generation uint64
}

type authSensitiveTool interface {
	agent.Tool
	setAuthSensitiveToolGuard(*authSensitiveToolGuard)
}

func registerAuthSensitiveTool(
	reg *agent.ToolRegistry,
	gateway *client.GatewayClient,
	tool authSensitiveTool,
) {
	if reg == nil || gateway == nil || tool == nil {
		return
	}
	generation, active := gateway.IntegrationGeneration()
	if !active {
		// SetAPIKey publishes the credential before AuthManager verifies its
		// principal. Do not expose a client captured in that unbound window;
		// the principal-change callback rebuilds it after binding.
		return
	}
	tool.setAuthSensitiveToolGuard(&authSensitiveToolGuard{
		gateway:    gateway,
		generation: generation,
	})
	reg.Register(tool)
}

func runAuthSensitiveTool(
	guard *authSensitiveToolGuard,
	run func() (agent.ToolResult, error),
) (agent.ToolResult, error) {
	if guard == nil {
		// Direct constructors remain useful for isolated unit tests. Production
		// registration always binds these six tools through the helper above.
		return run()
	}
	var result agent.ToolResult
	var runErr error
	err := guard.gateway.WithIntegrationGeneration(guard.generation, func() {
		result, runErr = run()
	})
	if err != nil {
		return withKnownNoEffect(agent.BusinessError(
			"cloud tool is no longer authorized for the current signed-in principal; rediscover the live tools before retrying",
		)), nil
	}
	return result, runErr
}
