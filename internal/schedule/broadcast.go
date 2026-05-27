package schedule

// ParseBroadcastEnum maps the schedule `broadcast` enum string to a *bool for
// storage. Returns (*bool, ok). ok=false means the input wasn't one of the
// allowed values; callers should return a validation error so the LLM / API
// client gets a clear correction signal.
//
// Shared by the schedule_create / schedule_update LLM tools and the daemon
// HTTP PATCH /schedules/{id} handler so the wire-level enum stays canonical.
//
// Mapping:
//   - ""    → (nil, true)   // absent/empty = smart default
//   - "auto"→ (nil, true)   // smart default
//   - "on"  → (*true, true) // always broadcast
//   - "off" → (*false, true)// never broadcast
//   - other → (nil, false)  // invalid
func ParseBroadcastEnum(s string) (*bool, bool) {
	switch s {
	case "", "auto":
		return nil, true
	case "on":
		b := true
		return &b, true
	case "off":
		b := false
		return &b, true
	default:
		return nil, false
	}
}
