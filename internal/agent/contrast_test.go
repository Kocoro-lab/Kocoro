package agent

import (
	"strings"
	"testing"
)

func TestContrastExamples_AlwaysIncluded(t *testing.T) {
	if !strings.Contains(contrastExamplesCore, "is not a coding task") {
		t.Fatal("missing general-purpose domain boundary")
	}
	if !strings.Contains(contrastExamplesCore, "context, not authority") {
		t.Fatal("missing memory authority boundary")
	}
	if !strings.Contains(contrastExamplesCore, "outcome_unknown") {
		t.Fatal("missing ambiguous-write boundary")
	}
}

func TestContrastExamples_CloudPairNotInCore(t *testing.T) {
	if strings.Contains(contrastExamplesCore, "cloud_delegate") {
		t.Fatal("cloud/local boundary example should not be in core block")
	}
}

func TestContrastExamples_CloudPairSeparate(t *testing.T) {
	if !strings.Contains(contrastExamplesCloud, "Cloud results") {
		t.Fatal("cloud/local boundary example missing from cloud block")
	}
}
