package client

import (
	"errors"
	"strings"
)

// TransportErrorShape categorizes an error as a transient transport-layer
// failure that both the retry decision (internal/agent) and the user-facing
// failure label (internal/runstatus) must agree on. It lives in the client
// package because client is a leaf (imports no other internal package), so
// both consumers can import it without an import cycle — runstatus cannot
// import agent, but both already import client.
//
// "Transport shape" means the wire connection to the gateway failed or was
// interrupted (dial/read error, mid-stream drop, premature stream end,
// truncated body, silent stream idle). It deliberately classifies *shape*
// only — it does NOT encode retry policy. ErrStreamIdleTimeout is a transport
// shape (so it labels as a network interruption rather than "unexpected"), but
// the retry path keeps its own veto on top because retrying a silent idle
// timeout just re-hangs. Callers that need retry semantics must apply that
// veto themselves; see isRetryableLLMError.
//
// Status-coded *APIError values are NOT transport shapes — they carry an HTTP
// response, so their classification (429 sub-codes, 5xx, etc.) is handled by
// the typed path in each consumer. This function only covers errors that
// surfaced as rendered strings from the gateway client's transport wrapping.
func TransportErrorShape(err error) bool {
	if err == nil {
		return false
	}
	// A status-coded API error reached the server and got an HTTP response;
	// it is not a transport-layer failure. Let consumers classify it by code.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	if errors.Is(err, ErrStreamIdleTimeout) {
		return true
	}
	msg := err.Error()
	for _, marker := range transportErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// transportErrorMarkers are the rendered substrings the gateway client wraps
// transport failures with. Kept in one place so the retry path and the
// failure-label path classify identical shapes. See Complete / CompleteStream
// in gateway.go for the wrapping sites:
//   - "request failed:"               dial/connection error (Complete, CompleteStream)
//   - "stream read error:"            mid-stream scanner read error (HTTP/2, unexpected EOF)
//   - "stream ended without done event" premature stream end before the done event
//   - "decode response:"              truncated body during JSON decode (stream→non-stream fallback)
var transportErrorMarkers = []string{
	"request failed:",
	"stream read error:",
	"stream ended without done event",
	"decode response:",
}
