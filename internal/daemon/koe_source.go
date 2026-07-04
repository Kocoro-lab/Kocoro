package daemon

import "strings"

// isKoeSource reports whether a request source is the Koe voice front-brain on
// any carrier. "koe" is the macOS desktop carrier; future carriers append a
// suffix ("koe-reachy", "koe-bot") and inherit EVERY koe behavior automatically
// through this single predicate. Matching is case-insensitive and whitespace-
// trimmed to mirror the other source helpers (isCloudSource,
// isAutonomousLocalSource, IsMessagingPlatform).
//
// koe is a HYBRID source: messaging-platform ROUTING (it joins IsMessagingPlatform
// → thread/burst keying, kindOf → SessionKindIM) but daemon-LOCAL transport
// (deliberately NOT in cloudSourceSet → keeps desktop skills/artifacts, no scratch
// CWD, Cloud owns no rendering). One predicate is what lets koe-reachy land in a
// single line later, so every koe-aware daemon site MUST call this rather than
// compare against the "koe" literal.
func isKoeSource(source string) bool {
	s := strings.ToLower(strings.TrimSpace(source))
	return s == ChannelKoe || strings.HasPrefix(s, ChannelKoe+"-")
}
