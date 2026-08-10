package agent

import (
	"strings"
	"testing"
)

// Split out of promptaudit_test.go when that file moved behind the
// `promptaudit` build tag: this is a real pass/fail contract over the runtime
// prompt and must keep running in the default suite, while the audit beside it
// is a report generator that can never fail.
func TestCoreOperationalRulesDoNotSuppressOperationalPreambles(t *testing.T) {
	for _, forbidden := range []string{
		"No reasoning preamble.",
		"Never apologize for, comment on, or explain your own tool calls.",
		"Reserve narration for reporting the result after the action is complete.",
	} {
		if strings.Contains(coreOperationalRules+contrastExamplesCore, forbidden) {
			t.Errorf("runtime prompt still contains preamble-suppressing instruction %q", forbidden)
		}
	}

	const requiredPreambleGuard = "give one brief user-facing preamble and continue with the tool calls in the same response"
	if !strings.Contains(coreOperationalRules+contrastExamplesCore, requiredPreambleGuard) {
		t.Errorf("runtime prompt missing operational-preamble guard %q", requiredPreambleGuard)
	}
	if !strings.Contains(coreOperationalRules, "Do not apologize for routine tool use") {
		t.Error("runtime prompt missing routine tool-use apology guard")
	}
}
