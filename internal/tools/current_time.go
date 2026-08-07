package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const currentTimeMaxTimezoneChars = 128

// CurrentTimeTool reads the local clock without using network retrieval.
type CurrentTimeTool struct {
	now func() time.Time
}

type currentTimeArgs struct {
	Timezone string `json:"timezone"`
}

func (t *CurrentTimeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "current_time",
		Description: "Get the current date and time from the local system clock with no network call. " +
			"Use for current time or weekday questions. Optionally provide an IANA timezone such as Asia/Tokyo or America/New_York; omit it for the system timezone. Do not use web search for time this tool can return.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"timezone": map[string]any{
					"type":        "string",
					"description": "Optional IANA timezone, for example Asia/Tokyo. Omit for the system timezone.",
				},
			},
		},
	}
}

func (t *CurrentTimeTool) RequiresApproval() bool            { return false }
func (t *CurrentTimeTool) IsReadOnlyCall(string) bool        { return true }
func (t *CurrentTimeTool) IsConcurrencySafeCall(string) bool { return true }

func (t *CurrentTimeTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args currentTimeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid input: %v", err)), nil
	}

	timezone := strings.TrimSpace(args.Timezone)
	if len([]rune(timezone)) > currentTimeMaxTimezoneChars {
		return agent.ValidationError(fmt.Sprintf("timezone exceeds %d characters", currentTimeMaxTimezoneChars)), nil
	}
	location := time.Local
	if timezone != "" {
		var err error
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return agent.ValidationError(fmt.Sprintf("unknown IANA timezone %q", timezone)), nil
		}
	}

	now := time.Now
	if t.now != nil {
		now = t.now
	}
	current := now().In(location)
	_, offsetSeconds := current.Zone()
	result := map[string]any{
		"datetime":     current.Format(time.RFC3339),
		"date":         current.Format("2006-01-02"),
		"time":         current.Format("15:04:05"),
		"weekday":      current.Weekday().String(),
		"timezone":     location.String(),
		"utc_offset":   formatUTCOffset(offsetSeconds),
		"unix_seconds": current.Unix(),
	}
	body, _ := json.Marshal(result)
	return agent.ToolResult{Content: string(body)}, nil
}

func formatUTCOffset(offsetSeconds int) string {
	sign := "+"
	if offsetSeconds < 0 {
		sign = "-"
		offsetSeconds = -offsetSeconds
	}
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	return fmt.Sprintf("%s%02d:%02d", sign, hours, minutes)
}
