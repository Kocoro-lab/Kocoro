package tools

import (
	"strings"
	"testing"
)

func TestSessionSearchDescriptionSeparatesStructuredRecall(t *testing.T) {
	description := (&SessionSearchTool{}).Info().Description
	for _, rule := range []string{
		"no identifying anchor",
		"name, nickname, or name fragment is an identifying anchor",
		"use memory_recall instead",
		"Do not call session_search to confirm a matching memory_recall result",
	} {
		if !strings.Contains(description, rule) {
			t.Errorf("session_search description missing routing boundary %q", rule)
		}
	}
}
