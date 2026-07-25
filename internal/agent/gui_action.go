package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
)

// GUIActionEffect is the mandatory side-effect classification for a local GUI
// action. It is intentionally independent from provider tool names so legacy
// wrappers cannot bypass the daemon's process-wide control lease.
type GUIActionEffect string

const (
	GUIActionObservation GUIActionEffect = "observation"
	GUIActionMutation    GUIActionEffect = "mutation"
)

// GUIActionDescriptor contains only redacted admission metadata. Arguments,
// scripts, typed text, key content, AX values, and screenshots must never be
// copied into this structure because it is projected into Desktop activity.
type GUIActionDescriptor struct {
	Participates   bool
	ActionKind     string
	Effect         GUIActionEffect
	TargetBundleID string
	TargetAppName  string
	ExecutionPath  string
}

// GUIActionResult is a redacted executor-authored result. It is carried only
// inside the process from a GUI tool to the daemon control wrapper; it is not
// provider output and must never contain typed text, AX values, or screenshots.
type GUIActionResult string

const (
	GUIActionResultVerified            GUIActionResult = "verified"
	GUIActionResultCompletedUnverified GUIActionResult = "completed_unverified"
	GUIActionResultFailed              GUIActionResult = "failed"
	GUIActionResultCancelled           GUIActionResult = "cancelled"
	GUIActionResultUserInterference    GUIActionResult = "user_interference"
)

type GUIActionPhase string

const (
	GUIActionPhaseIdle           GUIActionPhase = "idle"
	GUIActionPhaseObserving      GUIActionPhase = "observing"
	GUIActionPhaseMoving         GUIActionPhase = "moving"
	GUIActionPhaseActing         GUIActionPhase = "acting"
	GUIActionPhaseInputCommitted GUIActionPhase = "input_committed"
	GUIActionPhaseVerifying      GUIActionPhase = "verifying"
)

type GUIActionPointer struct {
	DisplayID          uint32
	TopologyID         string
	TopologyGeneration uint64
	X                  float64
	Y                  float64
}

type GUIActionOutcome struct {
	Result      GUIActionResult
	Phase       GUIActionPhase
	Pointer     *GUIActionPointer
	FailureCode string
}

var guiActionFailureCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)
var guiActivityActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)

const GUIActivityResultRedacted = "[GUI result redacted]"

func isGUIActivityToolName(name string) bool {
	switch name {
	case "computer_use", "computer", "accessibility", "applescript", "ghostty":
		return true
	default:
		return false
	}
}

// RedactGUIActivityArguments is the mandatory event/audit boundary for local
// GUI-control calls. Tool.Run receives the original arguments on its separate
// executor path, while approval presentation, routine status streams, TUI
// history, remote-run progress, and audit logs may retain only a bounded action
// enum. In particular they must never retain typed text, key content,
// AppleScript, AX values, descriptions, or screenshot data.
func RedactGUIActivityArguments(toolName, args string) string {
	if !isGUIActivityToolName(toolName) {
		return args
	}
	redacted := struct {
		Action   string `json:"action,omitempty"`
		Redacted bool   `json:"redacted"`
	}{Redacted: true}
	var candidate struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(args), &candidate) == nil && guiActivityActionPattern.MatchString(candidate.Action) {
		redacted.Action = candidate.Action
	}
	payload, _ := json.Marshal(redacted)
	return string(payload)
}

// RedactGUIActivityResult prevents compatibility tools from copying legacy
// executor strings such as "Typed: <content>" into routine events and audit
// logs. The authoritative redacted computer_use.activity channel carries the
// result/phase/failure code needed by Desktop separately.
func RedactGUIActivityResult(toolName, output string) string {
	if !isGUIActivityToolName(toolName) {
		return output
	}
	return GUIActivityResultRedacted
}

func (o GUIActionOutcome) Validate() error {
	switch o.Result {
	case GUIActionResultVerified, GUIActionResultCompletedUnverified, GUIActionResultFailed,
		GUIActionResultCancelled, GUIActionResultUserInterference:
	default:
		return fmt.Errorf("invalid GUI action result %q", o.Result)
	}
	switch o.Phase {
	case GUIActionPhaseIdle, GUIActionPhaseObserving, GUIActionPhaseMoving, GUIActionPhaseActing,
		GUIActionPhaseInputCommitted, GUIActionPhaseVerifying:
	default:
		return fmt.Errorf("invalid GUI action phase %q", o.Phase)
	}
	if o.FailureCode != "" && !guiActionFailureCodePattern.MatchString(o.FailureCode) {
		return fmt.Errorf("invalid GUI action failure code")
	}
	if o.Result == GUIActionResultVerified && o.FailureCode != "" {
		return fmt.Errorf("verified GUI action cannot carry a failure code")
	}
	if o.Result == GUIActionResultCompletedUnverified && o.FailureCode == "" {
		return fmt.Errorf("unverified GUI action requires a failure code")
	}
	if (o.Result == GUIActionResultFailed || o.Result == GUIActionResultCancelled ||
		o.Result == GUIActionResultUserInterference) && o.FailureCode == "" {
		return fmt.Errorf("GUI action failure result requires a failure code")
	}
	if o.Pointer != nil {
		if o.Pointer.DisplayID == 0 || o.Pointer.TopologyID == "" || o.Pointer.TopologyGeneration == 0 ||
			math.IsNaN(o.Pointer.X) || math.IsInf(o.Pointer.X, 0) ||
			math.IsNaN(o.Pointer.Y) || math.IsInf(o.Pointer.Y, 0) {
			return fmt.Errorf("invalid GUI action pointer authority")
		}
	}
	return nil
}

// GUIActionDescriber is the narrow seam used to classify GUI work before
// Tool.Run. Daemon wrappers admit participating actions through the process
// coordinator; the shared production registry also gates the final execution
// seam, so CLI/TUI mutations fail closed without daemon-minted authority while
// observation actions retain their existing direct behavior.
type GUIActionDescriber interface {
	DescribeGUIAction(ctx context.Context, argsJSON string) (GUIActionDescriptor, error)
}
