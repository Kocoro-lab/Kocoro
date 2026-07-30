# Synthetic loop benchmark example

This directory contains a deliberately small, synthetic example of how to
exercise and inspect the agent loop. It is not Kocoro's release QA matrix and
does not publish product-performance baselines, acceptance thresholds, customer
workflows, or expected production trajectories.

The sample prompts use only this checkout and are read-only. They do not access
Calendar, email, cloud storage, chat, or other personal accounts. All task
names, counts, prompts, timeouts, and results produced by the sample are
synthetic and must not be interpreted as product claims.

## Files

- `synthetic_driver.sh` — two harmless local source-reading examples.
- `analyze.py` — generic parser for a locally produced session and audit log.

## Running

Running the driver invokes the locally configured model and may consume paid
quota. It is therefore disabled unless explicitly acknowledged:

```bash
KOCORO_RUN_SYNTHETIC_BENCHMARK=1 \
  bash test/benchmarks/synthetic_driver.sh
```

Override the disposable output location with
`BENCHMARK_RESULTS_DIR=/tmp/your-directory`. Real release scenarios,
measurements, budgets, and pass/fail thresholds are maintained outside this
public repository.
