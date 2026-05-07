package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

type useSkillTool struct {
	skills *[]*skills.Skill
}

type useSkillArgs struct {
	SkillName string `json:"skill_name"`
	Args      string `json:"args"`
}

func newUseSkillTool(s *[]*skills.Skill) *useSkillTool {
	return &useSkillTool{skills: s}
}

func (t *useSkillTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "use_skill",
		Description: "Activate a named skill to load its specialized instructions. Call this when the user's request matches a skill's purpose. Available skill names are listed in the conversation context.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill_name": map[string]any{
					"type":        "string",
					"description": "Name of the skill to activate",
				},
				"args": map[string]any{
					"type":        "string",
					"description": "Optional context or arguments for the skill",
				},
			},
		},
		Required: []string{"skill_name"},
	}
}

func (t *useSkillTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args useSkillArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}
	if args.SkillName == "" {
		return agent.ToolResult{Content: "skill_name is required", IsError: true}, nil
	}
	if t.skills == nil || len(*t.skills) == 0 {
		return agent.ToolResult{Content: "no skills available", IsError: true}, nil
	}

	// Match by Name first (the identifier shown to the LLM in the skill
	// list), then fall back to Slug so callers that use the directory
	// name also resolve.
	var skill *skills.Skill
	for _, s := range *t.skills {
		if s.Name == args.SkillName {
			skill = s
			break
		}
	}
	if skill == nil {
		for _, s := range *t.skills {
			if s.Slug == args.SkillName {
				skill = s
				break
			}
		}
	}
	if skill == nil {
		available := make([]string, 0, len(*t.skills))
		for _, s := range *t.skills {
			available = append(available, s.Name)
		}
		return agent.ToolResult{
			Content: fmt.Sprintf("unknown skill %q. Available skills: %s", args.SkillName, strings.Join(available, ", ")),
			IsError: true,
		}, nil
	}

	// Register this skill's Slug as activated for the current run, so
	// subsequent bash commands can access its declared secrets via env
	// vars. Slug is the key used by SecretsStore (marketplace slug =
	// directory name); Name can differ and must not be used here.
	// Secret values are NEVER substituted into the prompt body — they
	// only reach the shell child process, never the LLM context or
	// session transcript.
	skills.ActivatedFromContext(ctx).Add(skill.Slug)

	body := skill.Prompt
	if skill.Dir != "" {
		body = rewriteRelativePaths(body, skill.Dir)
	}
	if args.Args != "" {
		body += "\n\n## User Context\n\n" + args.Args
	}
	var filter []string
	var hint string
	if len(skill.AllowedTools) > 0 {
		filter = skill.AllowedTools
		allowed := append([]string{}, skill.AllowedTools...)
		allowed = append(allowed, "use_skill")
		hint = "\n\n<system-reminder>\nFor this skill session, restrict yourself to ONLY these tools: " +
			strings.Join(allowed, ", ") +
			".\nDo not call other tools unless the user explicitly asks.\n</system-reminder>"
	}
	return agent.ToolResult{Content: body, SkillToolFilter: filter, SkillToolHint: hint}, nil
}

var relativePathPattern = regexp.MustCompile("(?m)(^|[\\s`])((?:scripts|references|assets)/[^\\s)`]+)")

func rewriteRelativePaths(body, dir string) string {
	return relativePathPattern.ReplaceAllStringFunc(body, func(match string) string {
		trimmed := strings.TrimLeft(match, " \t`")
		prefix := match[:len(match)-len(trimmed)]
		absPath := filepath.Join(dir, trimmed)
		if _, err := os.Stat(absPath); err == nil {
			return prefix + absPath
		}
		return match
	})
}

func (t *useSkillTool) RequiresApproval() bool { return false }

func (t *useSkillTool) IsReadOnlyCall(string) bool { return false }

// SkillExempt keeps use_skill callable regardless of the active skill's
// allowed-tools list — without this, a skill that omitted "use_skill" would
// trap the model into the active skill with no way to switch out.
func (t *useSkillTool) SkillExempt() bool { return true }
