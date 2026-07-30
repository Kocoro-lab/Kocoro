package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestCloneWithGenericComputerUseForRunDisablesLegacyGUIFallbacks(t *testing.T) {
	baseline, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	cloned, err := CloneWithGenericComputerUseForRun(baseline, &config.Config{}, true)
	if err != nil {
		t.Fatalf("CloneWithGenericComputerUseForRun: %v", err)
	}
	if _, ok := cloned.Get("computer_use"); !ok {
		t.Fatal("generic profile lost computer_use")
	}
	for _, legacy := range []string{"computer", "accessibility", "applescript"} {
		if _, ok := cloned.Get(legacy); ok {
			t.Fatalf("generic profile exposed legacy GUI tool %q", legacy)
		}
	}
	rawBash, ok := cloned.Get("bash")
	if !ok {
		t.Fatal("generic profile lost bash")
	}
	bash, ok := rawBash.(*BashTool)
	if !ok {
		t.Fatalf("generic profile bash type = %T", rawBash)
	}
	if !bash.LegacyGUIAutomationDisabled {
		t.Fatal("generic profile left bash GUI automation enabled")
	}
}

func TestCloneWithGenericComputerUseForRunForcesAXOnlyWithoutToolResultImages(t *testing.T) {
	baseline, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	cloned, err := CloneWithGenericComputerUseForRun(baseline, &config.Config{}, false)
	if err != nil {
		t.Fatalf("CloneWithGenericComputerUseForRun: %v", err)
	}
	public, ok := cloned.Get("computer_use")
	if !ok {
		t.Fatal("AX-only profile lost computer_use")
	}
	info := public.Info()
	if info.Name != "computer_use" {
		t.Fatalf("tool name = %q, want computer_use", info.Name)
	}
	properties, _ := info.Parameters["properties"].(map[string]any)
	for _, forbidden := range []string{
		"x", "y", "start_x", "start_y", "end_x", "end_y",
		"duration_ms", "button", "clicks", "include_screenshot",
	} {
		if _, ok := properties[forbidden]; ok {
			t.Errorf("AX-only schema still exposes %q", forbidden)
		}
	}
	action, _ := properties["action"].(map[string]any)
	description, _ := action["description"].(string)
	for _, forbiddenAction := range []string{"screenshot", "move", "drag"} {
		if strings.Contains(description, forbiddenAction) {
			t.Errorf("AX-only action description still exposes %q: %s", forbiddenAction, description)
		}
	}

	result, err := public.Run(context.Background(), `{"action":"screenshot","description":"inspect"}`)
	if err != nil {
		t.Fatalf("screenshot rejection returned Go error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "AX-only") {
		t.Fatalf("screenshot result = %+v, want explicit AX-only rejection", result)
	}
	if len(result.Images) != 0 {
		t.Fatalf("AX-only rejection returned %d images", len(result.Images))
	}

	result, err = public.Run(context.Background(), `{"action":"get_app_state","description":"inspect","include_screenshot":true}`)
	if err != nil {
		t.Fatalf("include_screenshot rejection returned Go error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "AX-only") {
		t.Fatalf("include_screenshot result = %+v, want explicit AX-only rejection", result)
	}
	if len(result.Images) != 0 {
		t.Fatalf("AX-only include_screenshot rejection returned %d images", len(result.Images))
	}

	for _, trait := range []struct {
		name string
		ok   bool
	}{
		{"GUIActionDescriber", implements[agent.GUIActionDescriber](public)},
		{"SafeChecker", implements[agent.SafeChecker](public)},
		{"ReadOnlyChecker", implements[agent.ReadOnlyChecker](public)},
		{"ConcurrencySafeChecker", implements[agent.ConcurrencySafeChecker](public)},
	} {
		if !trait.ok {
			t.Errorf("AX-only wrapper lost %s", trait.name)
		}
	}
}

func implements[T any](value any) bool {
	_, ok := value.(T)
	return ok
}
