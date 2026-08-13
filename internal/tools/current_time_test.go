package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCurrentTimeToolInRequestedTimezone(t *testing.T) {
	fixed := time.Date(2026, time.August, 7, 3, 4, 5, 0, time.UTC)
	tool := &CurrentTimeTool{now: func() time.Time { return fixed }}
	result, err := tool.Run(context.Background(), `{"timezone":"Asia/Tokyo"}`)
	if err != nil || result.IsError {
		t.Fatalf("Run err=%v result=%+v", err, result)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(result.Content), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	for key, want := range map[string]any{
		"datetime":   "2026-08-07T12:04:05+09:00",
		"date":       "2026-08-07",
		"time":       "12:04:05",
		"weekday":    "Friday",
		"timezone":   "Asia/Tokyo",
		"utc_offset": "+09:00",
	} {
		if body[key] != want {
			t.Errorf("%s=%v want %v", key, body[key], want)
		}
	}
	if body["unix_seconds"] != float64(fixed.Unix()) {
		t.Errorf("unix_seconds=%v want %d", body["unix_seconds"], fixed.Unix())
	}
}

func TestCurrentTimeToolUsesSystemTimezoneByDefault(t *testing.T) {
	fixed := time.Date(2026, time.August, 7, 3, 4, 5, 0, time.UTC)
	tool := &CurrentTimeTool{now: func() time.Time { return fixed }}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil || result.IsError {
		t.Fatalf("Run err=%v result=%+v", err, result)
	}
	if !strings.Contains(result.Content, `"timezone":`) {
		t.Fatalf("result=%s missing timezone", result.Content)
	}
}

func TestCurrentTimeToolRejectsInvalidTimezone(t *testing.T) {
	result, err := (&CurrentTimeTool{}).Run(context.Background(), `{"timezone":"Tokyo"}`)
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "unknown IANA timezone") {
		t.Fatalf("result=%+v want validation error", result)
	}
}
