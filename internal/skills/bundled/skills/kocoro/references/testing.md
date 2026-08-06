# Live E2E daemon isolation

Live daemon tests must start the binary built from the current checkout with a
temporary absolute state directory and a random non-default localhost port:

```bash
SHANNON_STATE_DIR=/absolute/path/to/temp-state \
  ./shan daemon start --isolated --port 17533
```

`--isolated` is a development-only mode. It rejects `--detach`, `--force`, the
production port `7533`, relative or missing state directories, and Desktop RPC.
It preserves the real HTTP and agent execution path while suppressing Cloud WS,
MCP connections and browser cleanup, watchers, heartbeat, the scheduler, sync,
marketplace warming, memory services, interrupted-turn recovery, and other
daemon-wide background workers.

Never stop or replace the Desktop-owned daemon to run a live E2E test. Verify
the isolated process through its own `/status` endpoint and terminate that exact
child process during cleanup.
