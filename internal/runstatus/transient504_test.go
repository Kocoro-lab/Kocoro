package runstatus

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestClassifyAPIError_GatewayClass guards that a 504 from the Cloud edge is
// classified as CodeServiceTemporaryError ("the AI service encountered a
// temporary error. Please try again.") like its 502/503 siblings, not the
// generic CodeUnexpected ("an unexpected error occurred"). Regression for the
// production incident where a 504 surfaced to the user as an unexpected error.
func TestClassifyAPIError_GatewayClass(t *testing.T) {
	cases := []struct {
		status int
		want   Code
	}{
		{500, CodeServiceTemporaryError},
		{502, CodeServiceTemporaryError},
		{503, CodeServiceTemporaryError},
		{504, CodeServiceTemporaryError}, // the fix: 504 is transient like 502/503
		{529, CodeProviderOverloaded},
	}
	for _, c := range cases {
		err := &client.APIError{StatusCode: c.status}
		if got := CodeFromError(err); got != c.want {
			t.Errorf("CodeFromError(APIError{%d}) = %q (msg=%q), want %q",
				c.status, got, FriendlyMessage(got), c.want)
		}
	}
}
