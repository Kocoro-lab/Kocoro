#!/usr/bin/env python3
"""Compare paired Koe selector qualification reports without network access."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def load_report(path: Path) -> dict:
    with path.open(encoding="utf-8") as handle:
        report = json.load(handle)
    required = {
        "variant",
        "seed",
        "repeats",
        "trials",
        "fast_accuracy",
        "full_accuracy",
        "false_fast_trials",
        "decision_latency_ms",
        "total_tokens",
    }
    missing = sorted(required - report.keys())
    if missing:
        raise ValueError(f"{path}: missing fields: {', '.join(missing)}")
    return report


def case_keys(report: dict) -> set[tuple[str, int]]:
    return {(trial["case_id"], trial["repeat"]) for trial in report["trials"]}


def delta(candidate: float, control: float) -> float:
    return candidate - control


def compare(control: dict, candidate: dict) -> dict:
    if control["seed"] != candidate["seed"] or control["repeats"] != candidate["repeats"]:
        raise ValueError("reports are not paired: seed or repeats differ")
    if case_keys(control) != case_keys(candidate):
        raise ValueError("reports are not paired: case/repeat coverage differs")

    control_p95 = control["decision_latency_ms"].get("p95", 0)
    candidate_p95 = candidate["decision_latency_ms"].get("p95", 0)
    checks = {
        "critical_false_fast_zero": candidate["false_fast_trials"] == 0,
        "fast_accuracy_not_worse": candidate["fast_accuracy"] >= control["fast_accuracy"],
        "decision_p95_delta_lte_150ms": candidate_p95 - control_p95 <= 150,
        "no_unknown_trials": candidate.get("unknown_trials", 0) == 0,
    }
    return {
        "schema_version": "koe.mode_classifier_comparison.v1",
        "control": control["variant"],
        "candidate": candidate["variant"],
        "seed": control["seed"],
        "repeats": control["repeats"],
        "trial_count": len(control["trials"]),
        "metrics": {
            "aggregate_accuracy": {
                "control": control["aggregate_accuracy"],
                "candidate": candidate["aggregate_accuracy"],
                "delta": delta(candidate["aggregate_accuracy"], control["aggregate_accuracy"]),
            },
            "fast_accuracy": {
                "control": control["fast_accuracy"],
                "candidate": candidate["fast_accuracy"],
                "delta": delta(candidate["fast_accuracy"], control["fast_accuracy"]),
            },
            "full_accuracy": {
                "control": control["full_accuracy"],
                "candidate": candidate["full_accuracy"],
                "delta": delta(candidate["full_accuracy"], control["full_accuracy"]),
            },
            "false_fast_trials": {
                "control": control["false_fast_trials"],
                "candidate": candidate["false_fast_trials"],
                "delta": candidate["false_fast_trials"] - control["false_fast_trials"],
            },
            "decision_p95_ms": {
                "control": control_p95,
                "candidate": candidate_p95,
                "delta": candidate_p95 - control_p95,
            },
            "total_tokens": {
                "control": control["total_tokens"],
                "candidate": candidate["total_tokens"],
                "delta": candidate["total_tokens"] - control["total_tokens"],
            },
        },
        "checks": checks,
        "adopt": all(checks.values()),
        "cost_note": "Token counts are measured. USD is intentionally not estimated without a dated pricing snapshot split by token modality.",
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("control", type=Path)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        result = compare(load_report(args.control), load_report(args.candidate))
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"comparison error: {exc}", file=sys.stderr)
        return 2
    rendered = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(rendered, encoding="utf-8")
    print(rendered, end="")
    return 0 if result["adopt"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
