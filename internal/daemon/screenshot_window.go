package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// captureWindowVia is the seam the handler uses to reach ax_server.
// Overridable in tests so the handler's status mapping can be exercised
// without a live ax_server / Screen Recording grant.
var captureWindowVia = func(ctx context.Context, params map[string]any) (json.RawMessage, error) {
	return tools.SharedAXClient().Call(ctx, "capture_window", params)
}

type screenshotWindowRequest struct {
	PID         int    `json:"pid,omitempty"`
	AppName     string `json:"app_name,omitempty"`
	WindowTitle string `json:"window_title,omitempty"`
}

type captureWindowResult struct {
	OK          bool   `json:"ok"`
	Code        string `json:"code"`
	ImageBase64 string `json:"image_base64"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// formatForegroundHint renders a one-line system-context note steering the agent
// to read the user's foreground app on-demand. Empty when there is nothing usable.
func formatForegroundHint(h *ForegroundHint) string {
	if h == nil {
		return ""
	}
	if h.AppName == "" && h.PID <= 0 {
		return ""
	}
	target := h.AppName
	if target == "" {
		target = fmt.Sprintf("pid %d", h.PID)
	}
	return fmt.Sprintf(
		"[Active app when the user asked: %q (pid %d). When the user refers to "+
			"the current app / what they're looking at / this screen, use the "+
			"accessibility tool (app: %q) to read its real content, or the screenshot "+
			"tool for purely visual content. Do not target Kocoro itself.]",
		target, h.PID, target,
	)
}

// handleScreenshotWindow serves POST /local/screenshot/window. It delegates a
// programmatic per-window capture to ax_server (which holds the Screen Recording
// grant) and returns the PNG synchronously so the Desktop quick panel can attach
// it before sending.
func (s *Server) handleScreenshotWindow(w http.ResponseWriter, r *http.Request) {
	var req screenshotWindowRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.PID <= 0 && req.AppName == "" {
		writeError(w, http.StatusBadRequest, "pid or app_name required")
		return
	}
	if req.AppName != "" && !tools.ValidAppNamePattern.MatchString(req.AppName) {
		writeError(w, http.StatusBadRequest, "invalid app_name — only letters, numbers, spaces, dots, hyphens, underscores, and parentheses allowed")
		return
	}

	params := map[string]any{}
	if req.PID > 0 {
		params["pid"] = req.PID
	}
	if req.AppName != "" {
		params["app_name"] = req.AppName
	}
	if req.WindowTitle != "" {
		params["window_title"] = req.WindowTitle
	}

	raw, err := captureWindowVia(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ax_server: "+err.Error())
		return
	}
	var res captureWindowResult
	if err := json.Unmarshal(raw, &res); err != nil {
		writeError(w, http.StatusBadGateway, "ax_server: bad capture result")
		return
	}
	if !res.OK {
		switch res.Code {
		case "screen_recording_denied":
			writeErrorCode(w, http.StatusForbidden, res.Code, "Screen Recording permission not granted for Kocoro AX")
		case "app_not_found", "window_not_found":
			writeErrorCode(w, http.StatusNotFound, res.Code, "target window not found")
		default:
			writeErrorCode(w, http.StatusBadGateway, "capture_failed", "window capture failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"image_base64": res.ImageBase64,
		"width":        res.Width,
		"height":       res.Height,
	})
}
