# Execution Profile V1 Fixtures

This directory is the canonical home for the Cloud-to-Kocoro execution-profile
wire examples. Python resolver tests and the Go gateway proxy tests read these
files directly. Consumer repositories vendor the same named bytes and verify
the same `manifest.json`; their CI must not read this repository by path.

Profile IDs are `ep1_` plus the SHA-256 of compact, sorted-key UTF-8 JSON for
all profile fields except `profile_id`. The manifest hashes the committed raw
fixture bytes, including the final newline.

OpenAI native fixtures describe the release-admitted
`openai.computer.v1:r1` adapter boundary. They pin:

- the server-minted native profile (`supports_function_tools=false`);
- Cloud's normalized, provenance-bearing `computer_call` action batch;
- the required, ordered `pending_safety_checks` array with exact
  `id` / nullable `code` / nullable `message` objects;
- a tenant-bound opaque continuation token copied into `request_id` and every
  normalized computer-call envelope, then required as the exact next
  `previous_response_id`; the upstream response ID stays server-side;
- Kocoro's assistant-call plus one screenshot-result continuation request;
- Cloud's exact Responses API `computer_call_output` /
  `computer_screenshot` (`detail: "original"`) provider request, including
  `acknowledged_safety_checks` only after the input carries an exact match;
- shared Redis-backed authoritative pending-safety state and an atomic
  one-shot consume made before the SDK call; tenant/profile/call/safety
  mismatches cannot burn another tenant's token, while failed or cancelled
  paid attempts stay consumed across service instances;
- the equivalent SSE done event and fail-closed invalid continuations,
  including missing, duplicate, cross-call replayed acknowledgements, and
  cross-response or same-envelope replay.

The repository's live OpenAI catalog enables this native profile only for
`gpt-5.6-sol`; other OpenAI models retain the generic function-tool fallback.
Kocoro installs the matching daemon-private batch executor only after its
guarded `computer_use` core, lease, per-action approval, and final-observation
boundaries are available.
