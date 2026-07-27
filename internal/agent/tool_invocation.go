package agent

import "context"

// ToolInvocation identifies the exact model tool call currently executing.
// It is injected by the dispatcher immediately before Tool.Run so downstream
// execution coordinators can bind cancellation and activity to one tool_use.
type ToolInvocation struct {
	ToolName    string
	ToolUseID   string
	UserRequest string
}

type toolInvocationKey struct{}

// ContextWithToolInvocation binds the dispatcher-authored identity to a tool
// context. It does not grant GUI execution authority; GUI mutations also need
// the opaque capability minted by guicontrol.Coordinator.BeginAction.
func ContextWithToolInvocation(ctx context.Context, invocation ToolInvocation) context.Context {
	return context.WithValue(ctx, toolInvocationKey{}, invocation)
}

func withToolInvocation(ctx context.Context, invocation ToolInvocation) context.Context {
	return ContextWithToolInvocation(ctx, invocation)
}

// ToolInvocationFromContext returns the exact call identity for a Tool.Run.
// Direct tool invocations outside the agent dispatcher have no identity.
func ToolInvocationFromContext(ctx context.Context) (ToolInvocation, bool) {
	if ctx == nil {
		return ToolInvocation{}, false
	}
	invocation, ok := ctx.Value(toolInvocationKey{}).(ToolInvocation)
	if !ok || invocation.ToolName == "" || invocation.ToolUseID == "" {
		return ToolInvocation{}, false
	}
	return invocation, true
}
