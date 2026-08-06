# Kocoro whole-product release evidence

`provider_release` qualifies the agent runtime and its real provider lanes. It is
not a whole-product release verdict. `product_release` is the fail-closed gate
for Kocoro's voice and automation product surface.

## Invocation

The evidence reports must be produced from the exact clean ShanClaw commit being
qualified and must be less than 24 hours old.

```bash
KOCORO_PRODUCT_RELEASE_E2E=1 \
KOCORO_AUDIO_HIL_REPORT=/absolute/path/physical-audio.json \
KOCORO_EXTERNAL_WRITE_RECOVERY_REPORT=/absolute/path/external-write.json \
KOE_MODE_CLASSIFIER_E2E=1 \
KOE_SELECTOR_AGENTLOOP_E2E=1 \
KOE_PROVIDER_AGENTLOOP_E2E=1 \
KOE_PROVIDER_SAMPLE=release \
KOE_PROVIDER_REPETITIONS=30 \
AGENT_LAB_LANE=product_release \
./scripts/agent-lab.sh /absolute/path/to/artifacts
```

The gate validates the two reports before starting paid provider work. Missing,
stale, dirty, simulated, secret-bearing, or wrong-commit evidence stops the lane.
The final manifest records the validator result path and recomputed SHA-256, so
the whole-product verdict remains bound to the exact evidence decision.

## Physical audio HIL report

Schema: `kocoro.physical_audio_hil.v1`.

Required evidence:

- Reachy Wireless with a physical microphone, physical speaker, and human speech.
- Live stack, audio carrier, and Realtime connection states.
- Advancing microphone and speaker frames plus non-zero microphone RMS.
- Audible playback and a playback-only AEC trial with zero false barge-ins.
- A human barge-in that stops playback within 1200 ms and empties the playback
  ring within 250 ms.
- Final disconnected call state and no supervised child PIDs.
- Hashed artifacts for stack status, audio metrics, Realtime events, barge-in
  timing, and cleanup status.

## External-write crash-recovery report

Schema: `kocoro.external_write_crash_recovery.v1`.

Required evidence:

- Explicit authorization, a bounded test target, and a unique idempotency key.
- A real crash after dispatch but before acknowledgement.
- Process restart, durable receipt recovery, and a receipt matching the observed
  downstream effect.
- Exactly one downstream effect, zero duplicates, and no pending retry.
- Hashed artifacts for the pre-crash audit, crash observation, restart status,
  downstream effect, and recovered receipt.

Every artifact entry contains `kind`, `path`, and `sha256`. `path` is relative
to its report file. The validator opens the file, rejects empty or escaping
paths, and recomputes SHA-256; a hash without its underlying artifact is not
release evidence.

Reports must not contain fields whose names include credentials, passwords,
secrets, tokens, cookies, or authorization material. The combined validator
output stores only report hashes and validation errors, never report contents.
