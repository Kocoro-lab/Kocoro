package agent

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// nginx504Body is the verbatim edge-nginx 504 page seen in production
// (surfaced in the daemon as "API returned 504: <html>...").
const nginx504Body = `<html>
<head><title>504 Gateway Time-out</title></head>
<body>
<center><h1>504 Gateway Time-out</h1></center>
</body>
</html>`

// TestIsRetryableLLMError_GatewayClass guards that a 504 from the Cloud edge is
// treated as a transient gateway-class error (sibling of 502/503) and retried,
// rather than killing the agent loop on the first attempt. Regression for the
// production incident where a single edge 504 ended a run with zero retries.
func TestIsRetryableLLMError_GatewayClass(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true}, // the fix: 504 must be retried like its 502/503 siblings
		{529, true},
		{400, false},
		{401, false},
		{403, false},
	}
	for _, c := range cases {
		err := &client.APIError{StatusCode: c.status, Body: nginx504Body}
		if got := isRetryableLLMError(err); got != c.wantRetryable {
			t.Errorf("isRetryableLLMError(APIError{%d}) = %v, want %v", c.status, got, c.wantRetryable)
		}
	}
}
