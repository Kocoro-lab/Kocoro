# memory testdata

Fixtures for `TestResponseEnvelope_MemoryBlockFixtureRoundTrip` in `types_test.go`.
Each fixture is a synthetic JSON file that exercises the full `ResponseEnvelope` wire shape
(strict decode + round-trip). Values are fictional; the only requirement is structural completeness.

## Fixture inventory

| File | Purpose |
|------|---------|
| `memoryblock_direct.json` | Direct-relation query — candidates have `observed_path: []`, groups have `via_relations` populated |
| `memoryblock_path.json`   | Path query — candidates and groups contain multi-hop `observed_path`, `via_relations: []` |
| `memoryblock_nodata.json` | No-data response — `candidates: []`, `memory_block.no_data_reason` set |

## Refreshing fixtures

Refresh a fixture only when the sidecar wire shape changes (new field added, field renamed, etc.).

1. Run a local sidecar against a bundle: set `LOCAL_TLM_BUNDLE` and run the `localsmoke` test.
2. Capture a real `/query` response with `curl` or the smoke test helper.
3. **Replace all personal/identifying values** with synthetic placeholders before committing:
   - Names → `Alice Nakamura`, `Jordan Sato` (or any clearly fictional names)
   - Entity IDs → `ent_000000000001`, `ent_000000000002`, …
   - Event IDs → `ev_000000000001`, `ev_000000000002`, …
   - Scopes → `topic:alice nakamura`
   - URLs → `example.com/blog/intro/`
   - `bundle_dir` → `/tmp/tlm-fixture-synthetic/bundles/20250101T000000`
4. Verify `TestResponseEnvelope_MemoryBlockFixtureRoundTrip` passes with `go test ./internal/memory/`.
