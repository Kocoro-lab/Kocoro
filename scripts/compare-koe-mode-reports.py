#!/usr/bin/env python3
"""Compare interleaved Koe selector reports without network access."""

from __future__ import annotations

import argparse
import json
import math
import random
import sys
from pathlib import Path


CRITICAL_CATEGORIES = {
    "production_incident",
    "production_recovery",
    "security_permissions",
    "high_stakes_legal_judgment",
    "high_stakes_medical_judgment",
    "high_stakes_financial_judgment",
    "destructive_migration",
    "broad_cross_system_change",
}


def load_report(path: Path) -> dict:
    with path.open(encoding="utf-8") as handle:
        report = json.load(handle)
    required = {
        "schema_version",
        "variant",
        "seed",
        "repeats",
        "planned_trial_count",
        "trials",
        "cases",
        "passed",
        "aggregate_accuracy",
        "fast_accuracy",
        "full_accuracy",
        "false_fast_trials",
        "decision_latency_ms",
        "total_tokens",
        "provenance",
    }
    missing = sorted(required - report.keys())
    if missing:
        raise ValueError(f"{path}: missing fields: {', '.join(missing)}")
    if report["schema_version"] != "koe.mode_classifier.v5":
        raise ValueError(
            f"{path}: unsupported schema_version {report['schema_version']!r}"
        )
    required_provenance = {
        "run_id",
        "source_commit",
        "source_dirty",
        "case_set_sha256",
        "persona_sha256",
        "instructions_sha256",
        "tool_schema_sha256",
        "session_config_sha256",
        "changed_dimensions",
    }
    missing_provenance = sorted(required_provenance - report["provenance"].keys())
    if missing_provenance:
        raise ValueError(
            f"{path}: provenance missing fields: {', '.join(missing_provenance)}"
        )
    return report


def keyed_trials(report: dict) -> dict[tuple[str, int], dict]:
    keyed: dict[tuple[str, int], dict] = {}
    for trial in report["trials"]:
        key = (trial["case_id"], trial["repeat"])
        if key in keyed:
            raise ValueError(
                f"{report['variant']}: duplicate trial {key[0]} repeat={key[1]}"
            )
        keyed[key] = trial
    return keyed


def delta(candidate: float, control: float) -> float:
    return candidate - control


def percentile(values: list[float], quantile: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = (len(ordered) - 1) * quantile
    lower = math.floor(index)
    upper = math.ceil(index)
    if lower == upper:
        return ordered[lower]
    weight = index - lower
    return ordered[lower] * (1 - weight) + ordered[upper] * weight


def bootstrap_mean_ci(
    values: list[float], seed: int, samples: int = 10_000
) -> dict[str, float | int]:
    if not values:
        return {"samples": 0, "lower_95": 0.0, "upper_95": 0.0}
    rng = random.Random(seed)
    size = len(values)
    means = []
    for _ in range(samples):
        means.append(sum(values[rng.randrange(size)] for _ in range(size)) / size)
    return {
        "samples": samples,
        "lower_95": percentile(means, 0.025),
        "upper_95": percentile(means, 0.975),
    }


def exact_mcnemar_p(control_only: int, candidate_only: int) -> float:
    discordant = control_only + candidate_only
    if discordant == 0:
        return 1.0
    tail = min(control_only, candidate_only)
    probability = sum(math.comb(discordant, k) for k in range(tail + 1)) / (
        2**discordant
    )
    return min(1.0, 2 * probability)


def paired_execution_valid(control_trial: dict, candidate_trial: dict) -> bool:
    required = ("execution_index", "variant_order", "pair_order")
    if any(field not in control_trial or field not in candidate_trial for field in required):
        return False
    return (
        control_trial["pair_order"] == candidate_trial["pair_order"]
        and {control_trial["variant_order"], candidate_trial["variant_order"]} == {1, 2}
        and abs(control_trial["execution_index"] - candidate_trial["execution_index"])
        == 1
    )


def compare(control: dict, candidate: dict) -> dict:
    if control["variant"] == candidate["variant"]:
        raise ValueError("control and candidate variants are identical")
    if control["seed"] != candidate["seed"] or control["repeats"] != candidate["repeats"]:
        raise ValueError("reports are not paired: seed or repeats differ")

    control_trials = keyed_trials(control)
    candidate_trials = keyed_trials(candidate)
    if set(control_trials) != set(candidate_trials):
        raise ValueError("reports are not paired: case/repeat coverage differs")

    keys = sorted(control_trials)
    control_only = 0
    candidate_only = 0
    accuracy_deltas: list[float] = []
    latency_deltas: list[float] = []
    token_deltas: list[float] = []
    execution_pair_failures: list[str] = []
    critical_false_fast: list[str] = []
    known_keys = []
    for key in keys:
        control_trial = control_trials[key]
        candidate_trial = candidate_trials[key]
        both_known = (
            control_trial.get("observed") != "unknown"
            and candidate_trial.get("observed") != "unknown"
        )
        if both_known:
            known_keys.append(key)
        control_correct = bool(control_trial["correct"])
        candidate_correct = bool(candidate_trial["correct"])
        if both_known and control_correct and not candidate_correct:
            control_only += 1
        elif both_known and candidate_correct and not control_correct:
            candidate_only += 1
        if both_known:
            accuracy_deltas.append(float(candidate_correct) - float(control_correct))
        if both_known and (
            control_trial.get("decision_latency_ms", 0) > 0
            and candidate_trial.get("decision_latency_ms", 0) > 0
        ):
            latency_deltas.append(
                candidate_trial["decision_latency_ms"]
                - control_trial["decision_latency_ms"]
            )
        if both_known:
            token_deltas.append(
                candidate_trial.get("total_tokens", 0)
                - control_trial.get("total_tokens", 0)
            )
        if not paired_execution_valid(control_trial, candidate_trial):
            execution_pair_failures.append(f"{key[0]}#{key[1]}")
        if (
            candidate_trial["category"] in CRITICAL_CATEGORIES
            and candidate_trial["expected"] == "full"
            and candidate_trial["observed"] == "fast"
        ):
            critical_false_fast.append(f"{key[0]}#{key[1]}")

    accuracy_ci = bootstrap_mean_ci(accuracy_deltas, control["seed"])
    latency_p95_delta = percentile(latency_deltas, 0.95)
    changed_dimensions = candidate["provenance"]["changed_dimensions"]
    control_cases = {item["id"]: item for item in control["cases"]}
    candidate_cases = {item["id"]: item for item in candidate["cases"]}
    complete_pair_coverage = (
        len(keys) == control["planned_trial_count"]
        and len(keys) == candidate["planned_trial_count"]
    )
    no_unknown = (
        control.get("unknown_trials", 0) == 0
        and candidate.get("unknown_trials", 0) == 0
    )
    behavior_comparison_valid = complete_pair_coverage and no_unknown
    majority_regressions = []
    if behavior_comparison_valid:
        majority_regressions = sorted(
            case_id
            for case_id, control_case in control_cases.items()
            if control_case["majority_correct"]
            and not candidate_cases.get(case_id, {}).get("majority_correct", False)
        )

    same_source = (
        bool(control["provenance"]["source_commit"])
        and control["provenance"]["source_commit"]
        == candidate["provenance"]["source_commit"]
    )
    same_case_set = (
        control["provenance"]["case_set_sha256"]
        == candidate["provenance"]["case_set_sha256"]
    )
    same_persona = (
        control["provenance"]["persona_sha256"]
        == candidate["provenance"]["persona_sha256"]
    )
    checks = {
        "minimum_three_repeats": control["repeats"] >= 3,
        "complete_pair_coverage": complete_pair_coverage,
        "interleaved_adjacent_pairs": not execution_pair_failures,
        "same_source_commit": same_source,
        "clean_source": not control["provenance"]["source_dirty"]
        and not candidate["provenance"]["source_dirty"],
        "same_model": control["model"] == candidate["model"],
        "same_case_set": same_case_set,
        "same_persona": same_persona,
        "single_changed_dimension": len(changed_dimensions) == 1,
        "candidate_behavior_gate_passed": bool(candidate["passed"]),
        "no_unknown_trials": no_unknown,
        "critical_false_fast_zero": not critical_false_fast,
        "fast_accuracy_not_worse": candidate["fast_accuracy"]
        >= control["fast_accuracy"],
        "full_accuracy_not_worse": candidate["full_accuracy"]
        >= control["full_accuracy"],
        "no_case_majority_regressions": not majority_regressions,
        "paired_accuracy_ci_lower_gte_minus_2pp": accuracy_ci["lower_95"] >= -0.02,
        "paired_decision_p95_delta_lte_150ms": latency_p95_delta <= 150,
    }
    return {
        "schema_version": "koe.mode_classifier_comparison.v2",
        "control": control["variant"],
        "candidate": candidate["variant"],
        "seed": control["seed"],
        "repeats": control["repeats"],
        "trial_count": len(keys),
        "protocol": {
            "design": "paired_randomized_balanced_interleaved",
            "changed_dimensions": changed_dimensions,
            "execution_pair_failures": execution_pair_failures,
            "same_source_commit": same_source,
            "same_case_set": same_case_set,
            "same_persona": same_persona,
            "behavior_comparison_valid": behavior_comparison_valid,
        },
        "paired_statistics": {
            "control_only_correct": control_only,
            "candidate_only_correct": candidate_only,
            "discordant_pairs": control_only + candidate_only,
            "mcnemar_exact_two_sided_p": exact_mcnemar_p(
                control_only, candidate_only
            ),
            "paired_known_trial_count": len(known_keys),
            "accuracy_delta_mean": (
                sum(accuracy_deltas) / len(accuracy_deltas)
                if accuracy_deltas
                else 0.0
            ),
            "accuracy_delta_bootstrap_95_ci": accuracy_ci,
            "decision_latency_delta_ms": {
                "count": len(latency_deltas),
                "median": percentile(latency_deltas, 0.5),
                "p95": latency_p95_delta,
            },
            "total_token_delta": sum(token_deltas),
        },
        "metrics": {
            "aggregate_accuracy": {
                "control": control["aggregate_accuracy"],
                "candidate": candidate["aggregate_accuracy"],
                "delta": delta(
                    candidate["aggregate_accuracy"], control["aggregate_accuracy"]
                ),
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
                "delta": candidate["false_fast_trials"]
                - control["false_fast_trials"],
            },
            "total_tokens": {
                "control": control["total_tokens"],
                "candidate": candidate["total_tokens"],
                "delta": candidate["total_tokens"] - control["total_tokens"],
            },
        },
        "critical_false_fast_trials": critical_false_fast,
        "case_majority_regressions": majority_regressions,
        "checks": checks,
        "adopt": all(checks.values()),
        "cost_note": "Provider token counts are measured. USD and customer quota remain separate and are not inferred without a dated modality-specific pricing snapshot.",
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
