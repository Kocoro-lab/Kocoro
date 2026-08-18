# Live E2E testing

Live daemon tests must build `shan` from the current checkout, start that exact
binary as a child process, and terminate the same child during cleanup. Never
stop or replace the Desktop-owned daemon on port `7533`.

The test harness uses hidden development flags with a temporary absolute state
directory and a random non-default localhost port:

```bash
./shan daemon start \
  --isolated \
  --state-dir /absolute/path/to/temp-state \
  --port 17533 \
  --isolated-api-key-stdin
```

The harness writes the authorized API key to the child's stdin pipe and closes
it immediately. It never puts the key in config.yaml, argv, environment
variables, or logs. `--isolated`, `--state-dir`, `--port`,
`--isolated-api-key-stdin`, and `--isolated-mcp` are hidden test-harness
controls, not supported operator configuration. Isolated startup rejects
detach, force, port `7533`, relative or missing state directories, Desktop RPC,
and a persisted `api_key`.

The mode isolates filesystem state and credential-store access. Tool
capabilities remain real and must be bounded explicitly:

- Config loading skips OS credential reads and the yaml-to-keychain migration.
  The authorized key exists only in parent/child process memory for the run.
- The request path retains the normal local and provider tool registry. Only run
  authorized, bounded prompts because request-triggered tools can still have
  external side effects.
- MCP is disabled unless `--isolated-mcp` names servers present in the temporary
  config. The allowlist is applied before registration/browser preflight and is
  reapplied on every config reload. Unknown names fail closed.
- Cloud WS, non-allowlisted MCP connections, browser cleanup, watchers,
  heartbeat, scheduler, sync, marketplace warming, memory services, migration
  recovery, and interrupted-turn auto-resume are disabled.

Live and release-qualification harnesses are excluded from the default test
build. Pass `-tags=live` to compile them. Real-provider tests never infer
consent from a reachable endpoint or installed credential;
`SHANNON_E2E_LIVE=1` is still required on every paid live lane.

Do not use `shan daemon stop` or `shan daemon status` for an isolated child;
those commands intentionally target the production daemon. Verify the child
through its own `/status` URL and terminate its exact PID or process handle.

The Fast/Full matrix verifies post-restart persistence readback of completed
sessions. It does not cover interrupted-turn auto-resume, physical microphone or
speaker behavior, AEC, user barge-in, or Desktop UI integration.

Run the opt-in suites on macOS with the Koe audio dependencies installed:

```bash
# Core one-shot plus bundled Explorer/Reviewer smoke. The fixture-backed
# structured oracles check exact results; this is still a smoke, not a broad
# general-agent quality score.
SHANNON_E2E_LIVE=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./test/e2e \
  -run '^TestLive_OneShotCoreAndBundledAgentsSmoke$' \
  -count=1 -v -timeout=5m

SHANNON_E2E_LIVE=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./test/e2e \
  -run '^TestLive_Daemon_(MessageAndEditRetry|AgentListIncludesBuiltins)$' \
  -count=1 -v -timeout=5m

SHANNON_E2E_LIVE=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./test/e2e \
  -run '^TestLive_(MidRunProgressNotesReachTheUser|BusinessErrorStopsWithoutRetry|ChannelDeliveryMetadataShapesReply|IsolatedMCPAllowlist_FullPath)$' \
  -count=1 -v -timeout=12m

# Browser outcome qualification on deterministic local pages. Comparison runs
# each cross-page-read and form-submit case once; release runs each five times.
SHANNON_E2E_LIVE=1 \
KOCORO_BROWSER_OUTCOME_LIVE=1 \
KOCORO_BROWSER_OUTCOME_SAMPLE=comparison \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./test/e2e \
  -run '^TestLive_BrowserOutcomeMatrix$' \
  -count=1 -v -timeout=12m

# Paid production-tool-surface matrix. Comparison is 3 repetitions per case;
# release is 5 and requires SAMPLE=release. Both require every answer, tool
# argument, and state oracle to pass; cost is capped and the report is JSON.
SHANNON_E2E_LIVE=1 \
KOCORO_TOOL_CHOICE_LIVE=1 \
KOCORO_TOOL_CHOICE_SAMPLE=comparison \
KOCORO_TOOL_CHOICE_REPETITIONS=3 \
go test -tags=live ./test/e2e \
  -run '^TestLive_ToolChoiceMatrix$' \
  -count=1 -v -timeout=30m

# General-agent outcome dataset: 24 writing, extraction/planning, file,
# research-honesty, clarification, recovery, and everyday/voice-style tasks.
# Tools and side effects are sandboxed; the provider and AgentLoop are real.
# Set SAMPLE=release and REPETITIONS=5 for release qualification.
# Set KOCORO_GENERAL_OUTCOME_MODEL and KOCORO_GENERAL_OUTCOME_EFFORT to pin an
# exact route without changing the user's persistent global configuration.
SHANNON_E2E_LIVE=1 \
KOCORO_GENERAL_OUTCOME_LIVE=1 \
KOCORO_GENERAL_OUTCOME_SAMPLE=comparison \
KOCORO_GENERAL_OUTCOME_REPETITIONS=1 \
KOCORO_GENERAL_OUTCOME_MODEL=grok-4.6 \
KOCORO_GENERAL_OUTCOME_EFFORT=low \
go test -tags=live ./test/e2e \
  -run '^TestLive_GeneralAgentOutcomeDataset$' \
  -count=1 -v -timeout=60m

# Paid response-style A/B: 2 models x 3 prompts x 3 styles = 18 calls.
SHANNON_E2E_LIVE=1 \
KOCORO_RESPONSE_DETAIL_AB=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./test/e2e \
  -run '^TestLive_ResponseDetailAcrossProviders$' \
  -count=1 -v -timeout=12m

# Paid image transport and narrow semantic check. Comparison makes one
# generate/edit pair; release makes five. The deterministic pixel oracle checks
# a simple red-circle generation and blue-square edit, not general aesthetics.
SHANNON_E2E_LIVE=1 \
KOCORO_IMAGE_QUALITY_LIVE=1 \
KOCORO_IMAGE_QUALITY_SAMPLE=comparison \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./test/e2e \
  -run '^TestLive_ImageGenerateEditQuality$' \
  -count=1 -v -timeout=20m

# Opt-in native screenshot semantic probe. The controlled Chrome page dominates
# the screen, but surrounding macOS UI may still be sent to the provider. This
# is comparison evidence only because it does not perform a desktop action.
KOE_NATIVE_COMPUTER_LIVE=1 \
TOOLSEARCH_CLOUD_ENDPOINT=http://127.0.0.1:18080 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./internal/tools \
  -run '^TestKoeNativeComputerLive$' \
  -count=1 -v -timeout=8m

KOE_LIVE_TEXT_FULL_PATH_E2E=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=live ./internal/koe \
  -run '^TestKoeLiveFullPathMatrixE2E$' \
  -count=1 -v -timeout=12m

# Human-read diagnostic only. The behavior tally is intentionally not a
# release gate and is absent unless the dedicated build tag is supplied.
KOE_CORRECTION_SUPPRESSION_DIAGNOSTIC=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test -tags=koe_diagnostic ./internal/koe \
  -run '^TestKoeCorrectionSuppressionDiagnostic$' \
  -count=1 -v -timeout=15m
```

The machine-readable change-to-eval map is
`test/e2e/testdata/change_impact_manifest.json`. Each entry separates the
deterministic lane, paid comparison evidence, and release qualification. A
`release_status` of `gap` is intentional and must not be promoted from a green
probe or mechanism test.

The Realtime call-creation POST is never automatically replayed after an error.
The public API does not document idempotency for this endpoint, so a failed
response cannot prove that no provider-side session was created.
