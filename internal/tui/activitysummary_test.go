package tui

import "testing"

func TestSummarizeActivity_Empty(t *testing.T) {
	got := summarizeActivity(nil, "default description")
	if got != "default description" {
		t.Errorf("expected fallback description, got %q", got)
	}
}

func TestSummarizeActivity_SingleTool(t *testing.T) {
	tools := []recentToolEntry{
		{name: "bash", keyArg: "git status"},
	}
	got := summarizeActivity(tools, "")
	if got != "bash(git status)" {
		t.Errorf("expected single tool display, got %q", got)
	}
}

func TestSummarizeActivity_ConsecutiveReads(t *testing.T) {
	tools := []recentToolEntry{
		{name: "bash", keyArg: "ls"},
		{name: "file_read", keyArg: "main.go", isRead: true},
		{name: "file_read", keyArg: "util.go", isRead: true},
		{name: "file_read", keyArg: "test.go", isRead: true},
	}
	got := summarizeActivity(tools, "")
	if got != "Reading 3 files\u2026" {
		t.Errorf("expected 'Reading 3 files…', got %q", got)
	}
}

func TestSummarizeActivity_ConsecutiveSearches(t *testing.T) {
	tools := []recentToolEntry{
		{name: "grep", keyArg: "TODO", isSearch: true},
		{name: "grep", keyArg: "FIXME", isSearch: true},
	}
	got := summarizeActivity(tools, "")
	if got != "Searching for 2 patterns\u2026" {
		t.Errorf("expected 'Searching for 2 patterns…', got %q", got)
	}
}

func TestSummarizeActivity_MixedSearchRead(t *testing.T) {
	tools := []recentToolEntry{
		{name: "bash", keyArg: "ls"},
		{name: "grep", keyArg: "func", isSearch: true},
		{name: "glob", keyArg: "*.go", isSearch: true},
		{name: "file_read", keyArg: "main.go", isRead: true},
	}
	got := summarizeActivity(tools, "")
	if got != "Searching for 2 patterns, reading 1 file\u2026" {
		t.Errorf("expected mixed summary, got %q", got)
	}
}

func TestSummarizeActivity_OnlyOneReadNotCollapsed(t *testing.T) {
	tools := []recentToolEntry{
		{name: "file_read", keyArg: "main.go", isRead: true},
	}
	got := summarizeActivity(tools, "")
	if got != "file_read(main.go)" {
		t.Errorf("expected single tool display, got %q", got)
	}
}

func TestClassifyTool(t *testing.T) {
	tests := []struct {
		name             string
		wantRead, wantSearch bool
	}{
		{"file_read", true, false},
		{"directory_list", true, false},
		{"grep", false, true},
		{"glob", false, true},
		{"web_search", false, true},
		{"bash", false, false},
		{"file_write", false, false},
	}
	for _, tt := range tests {
		isRead, isSearch := classifyTool(tt.name)
		if isRead != tt.wantRead || isSearch != tt.wantSearch {
			t.Errorf("classifyTool(%q) = (%v, %v), want (%v, %v)",
				tt.name, isRead, isSearch, tt.wantRead, tt.wantSearch)
		}
	}
}
