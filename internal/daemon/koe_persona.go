package daemon

import (
	"context"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/instructions"
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
	// Global instructions + MEMORY.md are the user-context source. projectDir is
	// empty: the persona is about the user, not the cwd.
	instr, _ := instructions.LoadInstructions(s.deps.ShannonDir, "", 8000)
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
