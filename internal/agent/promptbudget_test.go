package agent

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/prompt"
)

// Representative production assemblies, shared with the promptaudit report
// generator (build tag `promptaudit`) so the budget guard and the workbench
// dump can never describe different prompts.
//
// Default gateway + native-thinking Direct set. The exhaustive production
// registration matrix is pinned by tools.TestRegisteredLocalToolExposureMatrix;
// `think` is intentionally absent because default native thinking skips it.
func representativeFullTools() []string {
	return []string{
		"archive_extract", "archive_inspect", "ask_user_question", "bash", "calculate",
		"clipboard", "cloud_delegate", "current_time", "directory_list", "docx_to_text",
		"file_edit", "file_read", "file_write", "glob", "grep", "http", "memory_append",
		"memory_recall", "notify", "pdf_to_text", "pptx_to_text", "present_deliverable",
		"schedule_list", "schedule_show", "session_search", "system_info", "tool_search",
		"use_skill", "xlsx_to_text",
	}
}

// Koe Fast keeps uncommon schema-heavy local tools behind tool_search, so its
// provider-visible Direct set is smaller and its deferred catalog is larger.
func representativeFastTools() []string {
	return []string{
		"ask_user_question", "bash", "calculate", "clipboard", "current_time", "directory_list",
		"file_edit", "file_read", "file_write", "glob", "grep", "memory_append", "memory_recall", "notify",
		"present_deliverable", "schedule_list", "schedule_show", "session_search",
		"tool_search", "use_skill",
	}
}

func representativeCommonDeferred() []prompt.DeferredToolSummary {
	return []prompt.DeferredToolSummary{
		{Name: "accessibility", Description: "Inspect and interact with macOS accessibility elements."},
		{Name: "applescript", Description: "Run AppleScript for a supported macOS app workflow."},
		{Name: "browser", Description: "Use the local browser automation fallback."},
		{Name: "computer", Description: "Use provider-native computer interaction."},
		{Name: "computer_use", Description: "Complete a bounded macOS app workflow."},
		{Name: "ghostty", Description: "Interact with Ghostty terminal windows."},
		{Name: "process", Description: "Inspect and manage local processes."},
		{Name: "schedule_create", Description: "Create a scheduled task."},
		{Name: "schedule_remove", Description: "Remove a scheduled task."},
		{Name: "schedule_update", Description: "Update a scheduled task."},
		{Name: "screenshot", Description: "Capture a local screenshot."},
		{Name: "wait_for", Description: "Wait for a bounded local UI condition."},
	}
}

func representativeFastDeferred() []prompt.DeferredToolSummary {
	return append(representativeCommonDeferred(), []prompt.DeferredToolSummary{
		{Name: "archive_extract", Description: "Extract an archive into a destination directory."},
		{Name: "archive_inspect", Description: "Inspect archive contents without extracting them."},
		{Name: "cloud_delegate", Description: "Delegate a bounded task to the cloud workflow."},
		{Name: "docx_to_text", Description: "Extract text from a Word document."},
		{Name: "http", Description: "Make a bounded HTTP request."},
		{Name: "pdf_to_text", Description: "Extract text from a PDF."},
		{Name: "pptx_to_text", Description: "Extract text from a presentation."},
		{Name: "system_info", Description: "Read local system information."},
		{Name: "xlsx_to_text", Description: "Extract text from a spreadsheet."},
	}...)
}

// representativeKoeSystems assembles the two Koe voice profiles exactly the way
// AgentLoop.Run does, composing operational rules from the final
// provider-visible names after execution-profile filtering.
func representativeKoeSystems() (fastSystem, fullSystem string) {
	fastTools := representativeFastTools()
	fullTools := representativeFullTools()
	fastBasePrompt := defaultPersona + operationalRulesForToolNames(fastTools) + contrastExamplesCore
	fullBasePrompt := defaultPersona + operationalRulesForToolNames(fullTools) + contrastExamplesCore

	fast := prompt.BuildSystemPrompt(prompt.PromptOptions{
		BasePrompt:       fastBasePrompt,
		LocalToolNames:   fastTools,
		GatewayToolNames: []string{"web_fetch", "web_search"},
		DeferredTools:    representativeFastDeferred(),
		MemoryDir:        "/Users/test/.shannon/agents/sample",
		ModelID:          "medium",
		OutputFormat:     "koe",
		FastMode:         true,
	})
	full := prompt.BuildSystemPrompt(prompt.PromptOptions{
		BasePrompt:       fullBasePrompt,
		LocalToolNames:   fullTools,
		GatewayToolNames: []string{"web_fetch", "web_search"},
		DeferredTools:    representativeCommonDeferred(),
		MemoryDir:        "/Users/test/.shannon/agents/sample",
		ModelID:          "medium",
		OutputFormat:     "koe",
	})
	return fast.System, full.System + cloudDelegationGuidance + contrastExamplesCloud
}

// Budgets were raised from 7200/8000 when three behavior clauses returned after
// the harness review: the ask_user_question MUST gate + placeholder-option ban
// (Desktop question cards degraded to prose without it), the mid-task
// progress-update rule (2026-05 over-silence regression), and the web/browser
// empty-result honesty section. Each traces to a production incident; the
// budget guards drift, not these clauses.
//
// Raised again to 9900/10700 to hold the full ## Text output section. Its
// compression into a single bullet cost a measured behavior collapse — at
// effort_tier max, mid-run progress notes went from 6/6 runs to 0/6 — and three
// rewrites of that bullet recovered only 0/6, 2/6, 2/6. Restoring the section
// verbatim recovered 6/6. The section's effect is cumulative across its
// clauses, so it is not compressible clause-by-clause without re-measuring;
// TestLive_MidRunProgressNotesReachTheUser is the standing check.
const (
	fastSystemCharBudget = 9900
	fullSystemCharBudget = 10700
)

// TestRepresentativeSystemPromptStaysWithinBudget is the standing guard against
// prompt re-inflation. It deliberately lives OUTSIDE the `promptaudit` build
// tag: the audit beside it is a report generator that can never fail, while
// this is a real pass/fail contract. Shrinking the prompt was a one-time
// project; keeping it shrunk is what needs a test that runs on every
// `go test ./...`.
func TestRepresentativeSystemPromptStaysWithinBudget(t *testing.T) {
	fastSystem, fullSystem := representativeKoeSystems()

	if len(fastSystem) > fastSystemCharBudget {
		t.Errorf("representative Koe Fast System is %d chars, budget %d", len(fastSystem), fastSystemCharBudget)
	}
	if len(fullSystem) > fullSystemCharBudget {
		t.Errorf("representative Koe Full System is %d chars, budget %d", len(fullSystem), fullSystemCharBudget)
	}
	t.Logf("Koe Fast System %d/%d chars, Koe Full System %d/%d chars",
		len(fastSystem), fastSystemCharBudget, len(fullSystem), fullSystemCharBudget)
}

// TestRepresentativeSystemPromptKeepsIncidentBackedClauses pins the clauses the
// budget comment says were deliberately bought back. A future compression pass
// that trims one to fit the budget must delete its justification here too,
// which is the point: the budget alone would happily accept a smaller prompt
// that dropped an incident guard.
func TestRepresentativeSystemPromptKeepsIncidentBackedClauses(t *testing.T) {
	fastSystem, fullSystem := representativeKoeSystems()

	for _, clause := range []struct{ name, phrase string }{
		{"ask_user_question same-response gate", "you MUST call the tool in that same response"},
		{"placeholder-option ban", "never add a Custom, Other"},
		{"mid-run progress notes", "Brief is good — silent is not"},
		{"progress-note trigger list", "when you find something"},
		{"tool calls are invisible to the user", "Assume users can't see most tool calls"},
		{"operational preamble", "give one brief user-facing preamble"},
	} {
		if !strings.Contains(fastSystem, clause.phrase) {
			t.Errorf("Koe Fast System dropped the %s clause (%q)", clause.name, clause.phrase)
		}
		if !strings.Contains(fullSystem, clause.phrase) {
			t.Errorf("Koe Full System dropped the %s clause (%q)", clause.name, clause.phrase)
		}
	}
}
