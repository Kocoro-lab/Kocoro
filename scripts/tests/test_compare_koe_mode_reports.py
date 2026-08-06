from __future__ import annotations

import importlib.util
import unittest
from pathlib import Path


SCRIPT_PATH = Path(__file__).resolve().parents[1] / "compare-koe-mode-reports.py"
SPEC = importlib.util.spec_from_file_location("compare_koe_mode_reports", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def make_report(variant: str, changed_dimensions: list[str]) -> dict:
    trials = []
    cases = [
        ("fast_lookup", "bounded_current_lookup", "fast"),
        ("critical_incident", "production_incident", "full"),
    ]
    execution_index = 0
    for repeat in range(1, 4):
        for pair_order, (case_id, category, expected) in enumerate(cases, start=1):
            first_variant = "baseline" if pair_order % 2 else "candidate"
            for current_variant in (first_variant, "candidate" if first_variant == "baseline" else "baseline"):
                execution_index += 1
                if current_variant != variant:
                    continue
                variant_order = 1 if current_variant == first_variant else 2
                trials.append(
                    {
                        "variant": variant,
                        "case_id": case_id,
                        "category": category,
                        "expected": expected,
                        "observed": expected,
                        "correct": True,
                        "repeat": repeat,
                        "pair_order": pair_order,
                        "variant_order": variant_order,
                        "execution_index": execution_index,
                        "decision_latency_ms": 100,
                        "total_tokens": 10,
                    }
                )
    return {
        "schema_version": "koe.mode_classifier.v5",
        "variant": variant,
        "seed": 7,
        "repeats": 3,
        "model": "test-model",
        "trials": trials,
        "cases": [
            {"id": case_id, "majority_correct": True}
            for case_id, _, _ in cases
        ],
        "passed": True,
        "aggregate_accuracy": 1.0,
        "fast_accuracy": 1.0,
        "full_accuracy": 1.0,
        "false_fast_trials": 0,
        "unknown_trials": 0,
        "decision_latency_ms": {"p95": 100},
        "total_tokens": len(trials) * 10,
        "provenance": {
            "run_id": "run-1",
            "source_commit": "1234567890abcdef",
            "source_dirty": False,
            "case_set_sha256": "sha256:case",
            "persona_sha256": "sha256:persona",
            "instructions_sha256": "sha256:instructions",
            "tool_schema_sha256": "sha256:tools",
            "session_config_sha256": "sha256:config",
            "changed_dimensions": changed_dimensions,
        },
    }


class CompareModeReportsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.control = make_report("baseline", [])
        self.candidate = make_report("candidate", ["instructions"])

    def test_clean_single_dimension_interleaved_candidate_is_adoptable(self) -> None:
        result = MODULE.compare(self.control, self.candidate)
        self.assertTrue(result["adopt"])
        self.assertTrue(result["checks"]["interleaved_adjacent_pairs"])
        self.assertEqual(
            result["paired_statistics"]["mcnemar_exact_two_sided_p"], 1.0
        )

    def test_multiple_changed_dimensions_fail_closed(self) -> None:
        self.candidate["provenance"]["changed_dimensions"] = [
            "instructions",
            "tool_schema",
        ]
        result = MODULE.compare(self.control, self.candidate)
        self.assertFalse(result["adopt"])
        self.assertFalse(result["checks"]["single_changed_dimension"])

    def test_critical_false_fast_fails_closed(self) -> None:
        trial = next(
            item
            for item in self.candidate["trials"]
            if item["case_id"] == "critical_incident"
        )
        trial["observed"] = "fast"
        trial["correct"] = False
        self.candidate["passed"] = False
        self.candidate["full_accuracy"] = 2 / 3
        self.candidate["false_fast_trials"] = 1
        result = MODULE.compare(self.control, self.candidate)
        self.assertFalse(result["adopt"])
        self.assertFalse(result["checks"]["critical_false_fast_zero"])
        self.assertEqual(len(result["critical_false_fast_trials"]), 1)

    def test_non_adjacent_execution_fails_closed(self) -> None:
        self.candidate["trials"][0]["execution_index"] += 10
        result = MODULE.compare(self.control, self.candidate)
        self.assertFalse(result["adopt"])
        self.assertFalse(result["checks"]["interleaved_adjacent_pairs"])

    def test_repeats_below_three_fail_closed(self) -> None:
        self.control["repeats"] = 2
        self.candidate["repeats"] = 2
        result = MODULE.compare(self.control, self.candidate)
        self.assertFalse(result["adopt"])
        self.assertFalse(result["checks"]["minimum_three_repeats"])

    def test_duplicate_trial_is_rejected(self) -> None:
        self.candidate["trials"].append(dict(self.candidate["trials"][0]))
        with self.assertRaisesRegex(ValueError, "duplicate trial"):
            MODULE.compare(self.control, self.candidate)


if __name__ == "__main__":
    unittest.main()
