package agents

import "fmt"

// BuiltinCommands is the set of slash command names reserved by the TUI.
// Agent commands and skills must not use these names.
var BuiltinCommands = map[string]bool{
	"quit": true, "exit": true, "help": true, "clear": true,
	"sessions": true, "session": true, "model": true, "config": true,
	"setup": true, "update": true, "copy": true, "research": true,
	"swarm": true, "search": true, "dag": true,
}

// ValidateCommandName checks that a command/skill name is valid and doesn't
// collide with built-in slash commands.
func ValidateCommandName(name string) error {
	if err := ValidateAgentName(name); err != nil {
		return fmt.Errorf("invalid command name: %w", err)
	}
	if BuiltinCommands[name] {
		return fmt.Errorf("command name %q conflicts with built-in slash command", name)
	}
	return nil
}

// ValidateToolsFilter checks that allow and deny are not both set.
func ValidateToolsFilter(f *AgentToolsFilter) error {
	if f == nil {
		return nil
	}
	if len(f.Allow) > 0 && len(f.Deny) > 0 {
		return fmt.Errorf("tools.allow and tools.deny are mutually exclusive")
	}
	return nil
}
