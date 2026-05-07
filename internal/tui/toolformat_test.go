package tui

import (
	"strings"
	"testing"
)

func TestToolKeyArgPublishToWebShowsPurpose(t *testing.T) {
	got := toolKeyArg("publish_to_web", `{"path":"/tmp/landing.html","purpose":"send landing page draft to user via Slack reply"}`)
	if !strings.Contains(got, "purpose: send landing page") {
		t.Fatalf("expected publish approval summary to show purpose, got %q", got)
	}
	if strings.Contains(got, "/tmp/landing.html") {
		t.Fatalf("expected purpose to take precedence over path, got %q", got)
	}
}
