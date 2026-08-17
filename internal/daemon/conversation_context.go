package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const maxSideChatHistoryMessages = 100

type forkSessionRequest struct {
	Agent        string  `json:"agent,omitempty"`
	TargetAgent  *string `json:"target_agent,omitempty"`
	MessageIndex int     `json:"message_index"`
}

type forkSessionResponse struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

type sideChatHistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type sideChatRequest struct {
	Agent        string                   `json:"agent,omitempty"`
	MessageIndex int                      `json:"message_index"`
	Text         string                   `json:"text"`
	History      []sideChatHistoryMessage `json:"history,omitempty"`
}

func (s *Server) handleForkSession(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id, body, ok := s.decodeConversationContextRequest(w, r)
	if !ok {
		return
	}
	sourceManager := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(body.Agent))
	targetAgent := body.Agent
	if body.TargetAgent != nil {
		targetAgent = *body.TargetAgent
	}
	// GetOrCreateManager creates the target session directory, so a typo'd
	// agent would silently file the fork where no UI ever lists it.
	if !s.conversationAgentExists(targetAgent) {
		writeErrorCode(w, http.StatusBadRequest, "agent_not_found",
			fmt.Sprintf("agent not found: %s", targetAgent))
		return
	}
	targetManager := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(targetAgent))
	fork, err := sourceManager.ForkSessionInto(id, body.MessageIndex, targetManager)
	if err != nil {
		writeConversationContextError(w, id, err)
		return
	}
	writeJSON(w, http.StatusCreated, forkSessionResponse{SessionID: fork.ID, Title: fork.Title})
}

func (s *Server) handleSideChat(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		writeError(w, http.StatusInternalServerError, "daemon deps not configured")
		return
	}
	id := r.PathValue("id")
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body sideChatRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Agent == "default" {
		body.Agent = ""
	}
	if body.Agent != "" {
		if err := agents.ValidateAgentName(body.Agent); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// The ephemeral run executes AS this agent; failing here beats a late
	// "agent not found" 500 out of RunAgent.
	if !s.conversationAgentExists(body.Agent) {
		writeErrorCode(w, http.StatusBadRequest, "agent_not_found",
			fmt.Sprintf("agent not found: %s", body.Agent))
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if len(body.History) > maxSideChatHistoryMessages {
		writeError(w, http.StatusBadRequest, "side chat history is too long")
		return
	}
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(body.Agent))
	source, err := mgr.Load(id)
	if err != nil {
		writeConversationContextError(w, id, err)
		return
	}
	index := body.MessageIndex
	if index == 0 {
		index = len(source.Messages)
	}
	history, err := mgr.CopyHistoryThrough(id, index)
	if err != nil {
		writeConversationContextError(w, id, err)
		return
	}
	for _, item := range body.History {
		role := strings.TrimSpace(item.Role)
		content := strings.TrimSpace(item.Content)
		if (role != "user" && role != "assistant") || content == "" {
			writeError(w, http.StatusBadRequest, "side chat history must contain non-empty user/assistant messages")
			return
		}
		history = append(history, client.Message{Role: role, Content: client.NewTextContent(content)})
	}
	req := RunAgentRequest{
		Text:              body.Text,
		Agent:             body.Agent,
		Source:            "desktop",
		CWD:               source.CWD,
		ProjectID:         source.ProjectID,
		NewSession:        true,
		Ephemeral:         true,
		BypassRouting:     true,
		SessionHistory:    history,
		DisableTools:      true,
		SuppressBusEvents: true,
		StickyContext:     "This is a temporary side conversation. Answer from the provided conversation context. Do not modify or claim to modify the primary conversation.",
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleMessageSSE(w, r, req)
		return
	}
	handler := &httpEventHandler{}
	result, err := RunAgent(r.Context(), s.deps, req, handler)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) decodeConversationContextRequest(w http.ResponseWriter, r *http.Request) (string, forkSessionRequest, bool) {
	id := r.PathValue("id")
	if err := ValidateSessionID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", forkSessionRequest{}, false
	}
	var body forkSessionRequest
	if !decodeBody(w, r, &body) {
		return "", forkSessionRequest{}, false
	}
	if body.Agent == "default" {
		body.Agent = ""
	}
	if body.Agent != "" {
		if err := agents.ValidateAgentName(body.Agent); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return "", forkSessionRequest{}, false
		}
	}
	if body.TargetAgent != nil {
		target := *body.TargetAgent
		if target == "default" {
			target = ""
		}
		if target != "" {
			if err := agents.ValidateAgentName(target); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return "", forkSessionRequest{}, false
			}
		}
		body.TargetAgent = &target
	}
	return id, body, true
}

// conversationAgentExists reports whether a named agent is defined (user dir
// or _builtin fallback, mirroring agents.LoadAgent's resolution). The empty
// name is the default agent, which always exists.
func (s *Server) conversationAgentExists(name string) bool {
	if name == "" {
		return true
	}
	if _, err := os.Stat(filepath.Join(s.deps.AgentsDir, name, "AGENT.md")); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(s.deps.AgentsDir, "_builtin", name, "AGENT.md"))
	return err == nil
}

func writeConversationContextError(w http.ResponseWriter, id string, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
	case errors.Is(err, session.ErrMessageIndexOutOfRange), errors.Is(err, session.ErrIncompleteTurnBoundary):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
