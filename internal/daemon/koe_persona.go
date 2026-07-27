package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/instructions"
	"gopkg.in/yaml.v3"
)

// koePersonaDistillPrompt turns the user's global instructions + memory into a
// SPOKEN-voice persona context — who the user is and how Kocoro should address
// them — dropping every task-execution / tooling rule, which has no place in a
// spoken conversation. The model replies NONE when there's nothing useful, so
// the daemon can fall back to Koe's base persona.
const koePersonaDistillPrompt = `You are preparing a voice assistant named Kocoro to speak with its user.
From the user's notes below, extract ONLY what helps Kocoro hold a natural spoken conversation:
the user's name and how to address them, their preferred language and communication style, their role
or domain, and any standing personal context. IGNORE every coding rule, tool usage, file path, git
convention, model id, and task-execution instruction — none of that belongs in spoken conversation.
Write 1-3 short sentences in the third person ("The user ..."), spoken-friendly, no markdown, no lists,
no quoting of the rules. If there is nothing useful, reply with exactly: NONE`

// buildKoePersona distills a compact spoken-persona context from the user's
// instructions + memory via the small tier. Returns "" when there's no usable
// context, no gateway, or the model finds nothing — Koe then uses its base
// persona only. Never injects the raw instructions (those carry task rules).
func (s *Server) buildKoePersona(ctx context.Context) (string, error) {
	// Custom source: the user authored a spoken persona in Kocoro Desktop, so use it
	// verbatim (already voice-friendly) and skip the distill call entirely. Empty
	// custom text falls through to "" so Koe uses its base persona only.
	source, custom := s.freshKoePersonaConfig()
	if source == "custom" {
		return strings.TrimSpace(custom), nil
	}
	// Global (default) source: distill the user's instructions + memory. projectDir
	// is empty: the persona is about the user, not the cwd.
	instr, _ := instructions.LoadInstructions(s.deps.ShannonDir, "", "", 8000)
	mem, _ := instructions.LoadMemory(s.deps.ShannonDir, 200)
	src := strings.TrimSpace(strings.TrimSpace(instr) + "\n\n" + strings.TrimSpace(mem))
	if src == "" {
		return "", nil
	}
	gw := s.cloudGateway()
	if gw == nil {
		return "", nil
	}
	resp, err := gw.Complete(ctx, client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(koePersonaDistillPrompt)},
			{Role: "user", Content: client.NewTextContent(src)},
		},
		ModelTier:   "small",
		Temperature: 0.3,
		MaxTokens:   220,
		CacheSource: "helper",
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.OutputText)
	if out == "" || strings.EqualFold(out, "NONE") {
		return "", nil
	}
	return out, nil
}

// freshKoePersonaConfig reads koe.persona_source / koe.custom_persona straight from
// the on-disk config.yaml, falling back to the in-memory config on any read error.
// PATCH /config (patchGlobalConfig) writes the file but does NOT refresh the daemon's
// in-memory s.deps.Config until a reload, so reading s.deps.Config here would return a
// persona the user saved from Kocoro Desktop only after a reload/restart. Koe fetches
// the persona on (re)start, right after Desktop PATCHes it — so the file is the fresh
// source of truth, matching how GET /config surfaces just-saved koe fields.
func (s *Server) freshKoePersonaConfig() (source, custom string) {
	if s.deps != nil && s.deps.Config != nil {
		source, custom = s.deps.Config.Koe.PersonaSource, s.deps.Config.Koe.CustomPersona
	}
	if s.deps == nil || s.deps.ShannonDir == "" {
		return source, custom
	}
	data, err := os.ReadFile(filepath.Join(s.deps.ShannonDir, "config.yaml"))
	if err != nil {
		return source, custom
	}
	var raw struct {
		Koe struct {
			PersonaSource string `yaml:"persona_source"`
			CustomPersona string `yaml:"custom_persona"`
		} `yaml:"koe"`
	}
	if yaml.Unmarshal(data, &raw) == nil {
		source, custom = raw.Koe.PersonaSource, raw.Koe.CustomPersona
	}
	return source, custom
}

// handleKoePersona returns the distilled spoken-persona context for Koe to append
// to its base persona. Fail-soft: any error yields an empty persona (200), since a
// missing persona must never block a voice call — Koe just uses its base persona.
func (s *Server) handleKoePersona(w http.ResponseWriter, r *http.Request) {
	persona, err := s.buildKoePersona(r.Context())
	if err != nil {
		persona = "" // fail-soft: base persona only
	}
	writeJSON(w, http.StatusOK, map[string]string{"persona": persona})
}
