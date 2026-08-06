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
  --port 17533
```

`--isolated`, `--state-dir`, and `--port` are paired test-harness controls, not
supported operator configuration. Isolated startup rejects detach, force, port
`7533`, relative or missing state directories, and Desktop RPC.

The mode is state-isolated, not credential- or capability-isolated:

- On macOS and Windows, the OS credential store is process-global. A paid live
  run can use the existing daemon API key even though filesystem state is
  temporary. Never print, copy, or persist that credential.
- The request path retains the normal local and provider tool registry. Only run
  authorized, bounded prompts because request-triggered tools can still have
  external side effects.
- Cloud WS, MCP connections and browser cleanup, watchers, heartbeat, scheduler,
  sync, marketplace warming, memory services, migration recovery, and
  interrupted-turn auto-resume are disabled.

Do not use `shan daemon stop` or `shan daemon status` for an isolated child;
those commands intentionally target the production daemon. Verify the child
through its own `/status` URL and terminate its exact PID or process handle.

The Fast/Full matrix verifies post-restart persistence readback of completed
sessions. It does not cover interrupted-turn auto-resume, physical microphone or
speaker behavior, AEC, user barge-in, or Desktop UI integration.

Run the opt-in suites on macOS with the Koe audio dependencies installed:

```bash
SHANNON_E2E_LIVE=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test ./test/e2e \
  -run '^TestLive_Daemon_(MessageAndEditRetry|AgentListIncludesBuiltins)$' \
  -count=1 -v -timeout=5m

KOE_LIVE_TEXT_FULL_PATH_E2E=1 \
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
go test ./internal/koe \
  -run '^TestKoeLiveFullPathMatrixE2E$' \
  -count=1 -v -timeout=12m
```

The Realtime call-creation POST is never automatically replayed after an error.
The public API does not document idempotency for this endpoint, so a failed
response cannot prove that no provider-side session was created.
