package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// DeliverableHandler surfaces a finished deliverable file to an attached daemon
// client (the Kocoro Desktop app) so it appears in the session's Deliverables
// sidebar. The daemon runner attaches one per run, wired to the EventBus; it
// returns true when at least one client received the event.
//
// This is the emit half of the trust boundary: present_deliverable validates
// the path locally (a real regular file resolved by the daemon) and only then
// calls this, so what reaches the client is a daemon-vouched local file
// reference, never raw model text. The path may be outside the session working
// directory; clients should treat the event as a user-visible local file card,
// not as proof that the file was created inside the session sandbox.
type DeliverableHandler func(d Deliverable) bool

// Deliverable is the validated metadata for one produced file. present_deliverable
// fills it in after path validation; the runner's handler adds session/agent/
// source/ts before emitting it on the bus.
type Deliverable struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Title    string `json:"title,omitempty"`
	MIME     string `json:"mime,omitempty"`
	ByteSize int64  `json:"byte_size"`
}

type deliverableHandlerKey struct{}

// WithDeliverableHandler returns a context carrying a DeliverableHandler.
func WithDeliverableHandler(ctx context.Context, h DeliverableHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, deliverableHandlerKey{}, h)
}

// DeliverableHandlerFrom returns the DeliverableHandler from ctx, or nil.
func DeliverableHandlerFrom(ctx context.Context) DeliverableHandler {
	h, _ := ctx.Value(deliverableHandlerKey{}).(DeliverableHandler)
	return h
}

// PresentDeliverableTool lets the agent surface a finished artifact to the
// user's Deliverables sidebar. It is intentionally NOT routed through the
// Desktop RPC channel (unlike the calendar tools): the daemon does not need the
// Desktop to *do* anything, only to be notified, and the notification rides the
// EventBus the same way notify does.
type PresentDeliverableTool struct{}

type presentDeliverableArgs struct {
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
}

type presentDeliverableResult struct {
	Delivered   bool        `json:"delivered"`
	Deliverable Deliverable `json:"deliverable"`
}

func (t *PresentDeliverableTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "present_deliverable",
		Description: "Surface a FINISHED deliverable file to the user in the Deliverables sidebar so they " +
			"can preview and open it. ALWAYS call this immediately after you finish writing any file the " +
			"user would want to keep or look at — a report, slide deck, spreadsheet, document, generated " +
			"web page, chart, diagram, or image (PDF, PPTX, XLSX, DOCX, HTML, SVG, MD, PNG, …). Call it " +
			"once per finished artifact, with the file's final path; do NOT call it for intermediate, " +
			"scratch, temporary, or unrelated existing files. The file may be relative to the session " +
			"working directory or absolute, but must be a real regular local file.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the finished file (relative to the working directory, or absolute).",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "Optional human-friendly label for the card (e.g. \"Q3 Sales Deck\"). Defaults to the filename.",
				},
			},
		},
		Required: []string{"path"},
	}
}

// RequiresApproval is false: this tool does not read, write, upload, or disclose
// file contents. It records daemon-validated metadata for a local regular file
// so the trusted local Desktop client can show an open/preview card. Because
// paths may be outside the session CWD, clients must not treat the event as a
// sandbox-authorization grant or automatically publish the file elsewhere.
func (t *PresentDeliverableTool) RequiresApproval() bool { return false }

func (t *PresentDeliverableTool) IsReadOnlyCall(string) bool { return false }

func (t *PresentDeliverableTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args presentDeliverableArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("present_deliverable: invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return agent.ValidationError("present_deliverable: missing required `path` parameter"), nil
	}

	// Resolve against the session working directory; reject a relative path
	// when no working directory is set rather than silently joining $HOME.
	resolved, err := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if err != nil {
		if errors.Is(err, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError("present_deliverable: no session working directory is set; pass an absolute path."), nil
		}
		return agent.ValidationError(fmt.Sprintf("present_deliverable: %v", err)), nil
	}

	// The path is resolved (absolute / ~ / relative-to-CWD) but NOT confined to
	// the session working directory — the agent may surface a deliverable it
	// wrote anywhere the user can read (Desktop, Downloads, a temp dir, …). We
	// still require a real, regular file so a fabricated or nonexistent path
	// can't be surfaced, and the event remains metadata-only.
	info, err := os.Stat(resolved)
	if err != nil {
		return agent.ValidationError(fmt.Sprintf("present_deliverable: cannot read %q: %v", args.Path, err)), nil
	}
	if !info.Mode().IsRegular() {
		return agent.ValidationError(fmt.Sprintf("present_deliverable: %q is not a regular file", args.Path)), nil
	}

	filename := filepath.Base(resolved)
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = filename
	}
	d := Deliverable{
		ID:       newDeliverableID(),
		Path:     resolved,
		Filename: filename,
		Title:    title,
		MIME:     mimeForPath(resolved),
		ByteSize: info.Size(),
	}

	// Emit to the attached Desktop client (if any). With no handler (headless,
	// CLI, no client subscribed) this is a no-op: the tool_use/tool_result still
	// persists in session history, so the deliverable surfaces when the session
	// is later opened in Kocoro Desktop.
	delivered := false
	if h := DeliverableHandlerFrom(ctx); h != nil {
		delivered = h(d)
	}

	// Echo the VALIDATED metadata as pure JSON so (a) the model gets a
	// confirmation and (b) this daemon-authored record persists in session
	// history for the reload path. Desktop reads this tool_result (not the
	// model-authored tool_use args) for the trusted path.
	result, _ := json.Marshal(presentDeliverableResult{
		Delivered:   delivered,
		Deliverable: d,
	})
	return agent.ToolResult{Content: string(result)}, nil
}

func newDeliverableID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "dlv_" + hex.EncodeToString(b)
}

// mimeForPath best-effort maps an extension to a MIME type. The common
// deliverable formats are hardcoded (deterministic across machines, and Go's
// mime package does not know the OOXML office types); the long tail falls back
// to the mime package and finally application/octet-stream.
func mimeForPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".pdf":
		return "application/pdf"
	case ".html", ".htm":
		return "text/html"
	case ".svg":
		return "image/svg+xml"
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	case ".txt", ".text":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	if m := mime.TypeByExtension(ext); m != "" {
		if i := strings.IndexByte(m, ';'); i >= 0 {
			m = strings.TrimSpace(m[:i])
		}
		return m
	}
	return "application/octet-stream"
}
