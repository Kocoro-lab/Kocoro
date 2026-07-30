#!/usr/bin/env bash
# Public synthetic Koe smoke example.
#
# This script intentionally contains no release acceptance matrix, hardware
# procedure, live-account workflow, paid-model call, or product-performance
# threshold. Production QA is maintained outside this public repository.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "koe-smoke: running deterministic synthetic offline checks"
go test ./internal/koe -run 'Test(NormalizeDismissPhrase|ResultMailboxRetainsUntilCompleted|WavRoundTrip)$' -count=1
echo "koe-smoke: synthetic example complete; no production acceptance claim"
