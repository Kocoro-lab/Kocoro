#!/usr/bin/env python3
"""Validate physical-audio and external-write evidence for product release."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re
import sys
from typing import Any


AUDIO_SCHEMA = "kocoro.physical_audio_hil.v1"
WRITE_SCHEMA = "kocoro.external_write_crash_recovery.v1"
RESULT_SCHEMA = "kocoro.product_release_evidence_result.v1"
MAX_BARGE_STOP_MS = 1200
MAX_RING_EMPTY_MS = 250
SENSITIVE_KEY_PARTS = (
    "authorization",
    "cookie",
    "credential",
    "password",
    "secret",
    "token",
)
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--audio", required=True, help="Physical audio HIL JSON report")
    parser.add_argument(
        "--external-write",
        required=True,
        help="External write crash-recovery JSON report",
    )
    parser.add_argument("--expected-shan-commit", required=True)
    parser.add_argument("--max-age-hours", type=float, default=24.0)
    parser.add_argument("--output", required=True, help="Validation result JSON")
    return parser.parse_args()


def load_report(
    path: str, label: str, errors: list[str]
) -> tuple[dict[str, Any] | None, str, pathlib.Path | None]:
    report_path = pathlib.Path(path)
    if not report_path.is_file():
        errors.append(f"{label}: report does not exist: {report_path}")
        return None, "", None
    raw = report_path.read_bytes()
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"{label}: invalid JSON: {exc}")
        return None, hashlib.sha256(raw).hexdigest(), report_path
    if not isinstance(value, dict):
        errors.append(f"{label}: top-level value must be an object")
        return None, hashlib.sha256(raw).hexdigest(), report_path
    return value, hashlib.sha256(raw).hexdigest(), report_path


def parse_timestamp(value: Any, field: str, errors: list[str]) -> dt.datetime | None:
    if not isinstance(value, str) or not value:
        errors.append(f"{field}: required RFC3339 timestamp")
        return None
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        errors.append(f"{field}: invalid RFC3339 timestamp")
        return None
    if parsed.tzinfo is None:
        errors.append(f"{field}: timezone is required")
        return None
    return parsed.astimezone(dt.timezone.utc)


def find_sensitive_keys(value: Any, prefix: str = "") -> list[str]:
    found: list[str] = []
    if isinstance(value, dict):
        for key, child in value.items():
            path = f"{prefix}.{key}" if prefix else str(key)
            lowered = str(key).lower()
            if any(part in lowered for part in SENSITIVE_KEY_PARTS):
                found.append(path)
            found.extend(find_sensitive_keys(child, path))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            found.extend(find_sensitive_keys(child, f"{prefix}[{index}]"))
    return found


def require_true(
    container: dict[str, Any], key: str, prefix: str, errors: list[str]
) -> None:
    if container.get(key) is not True:
        errors.append(f"{prefix}.{key}: must be true")


def require_number(
    container: dict[str, Any],
    key: str,
    prefix: str,
    errors: list[str],
    *,
    minimum: float | None = None,
    maximum: float | None = None,
) -> float | None:
    value = container.get(key)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        errors.append(f"{prefix}.{key}: must be a number")
        return None
    numeric = float(value)
    if minimum is not None and numeric < minimum:
        errors.append(f"{prefix}.{key}: must be >= {minimum:g}, got {numeric:g}")
    if maximum is not None and numeric > maximum:
        errors.append(f"{prefix}.{key}: must be <= {maximum:g}, got {numeric:g}")
    return numeric


def validate_artifacts(
    report: dict[str, Any],
    report_path: pathlib.Path,
    required_kinds: set[str],
    label: str,
    errors: list[str],
) -> None:
    artifacts = report.get("artifacts")
    if not isinstance(artifacts, list):
        errors.append(f"{label}.artifacts: must be a list")
        return
    observed: set[str] = set()
    evidence_root = report_path.resolve().parent
    for index, artifact in enumerate(artifacts):
        prefix = f"{label}.artifacts[{index}]"
        if not isinstance(artifact, dict):
            errors.append(f"{prefix}: must be an object")
            continue
        kind = artifact.get("kind")
        digest = artifact.get("sha256")
        if not isinstance(kind, str) or not kind:
            errors.append(f"{prefix}.kind: required")
        else:
            if kind in observed:
                errors.append(f"{prefix}.kind: duplicate kind {kind}")
            observed.add(kind)
        if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
            errors.append(f"{prefix}.sha256: must be a lowercase SHA-256 digest")
        relative_path = artifact.get("path")
        if not isinstance(relative_path, str) or not relative_path:
            errors.append(f"{prefix}.path: required relative artifact path")
            continue
        candidate = pathlib.Path(relative_path)
        if candidate.is_absolute():
            errors.append(f"{prefix}.path: must be relative to the report")
            continue
        resolved = (evidence_root / candidate).resolve()
        if resolved != evidence_root and evidence_root not in resolved.parents:
            errors.append(f"{prefix}.path: escapes the evidence directory")
            continue
        if not resolved.is_file():
            errors.append(f"{prefix}.path: artifact does not exist: {relative_path}")
            continue
        if resolved.stat().st_size == 0:
            errors.append(f"{prefix}.path: artifact is empty: {relative_path}")
            continue
        actual = file_sha256(resolved)
        if isinstance(digest, str) and actual != digest:
            errors.append(f"{prefix}.sha256: digest mismatch for {relative_path}")
    missing = sorted(required_kinds - observed)
    if missing:
        errors.append(f"{label}.artifacts: missing kinds: {', '.join(missing)}")


def validate_common(
    report: dict[str, Any],
    *,
    label: str,
    schema: str,
    expected_commit: str,
    max_age: dt.timedelta,
    now: dt.datetime,
    errors: list[str],
) -> None:
    if report.get("schema_version") != schema:
        errors.append(f"{label}.schema_version: expected {schema}")
    if not isinstance(report.get("run_id"), str) or not report["run_id"].strip():
        errors.append(f"{label}.run_id: required")
    started = parse_timestamp(report.get("started_at"), f"{label}.started_at", errors)
    finished = parse_timestamp(
        report.get("finished_at"), f"{label}.finished_at", errors
    )
    if started is not None and finished is not None:
        if finished < started:
            errors.append(f"{label}: finished_at precedes started_at")
        if finished > now + dt.timedelta(minutes=5):
            errors.append(f"{label}: finished_at is in the future")
        if now - finished > max_age:
            errors.append(
                f"{label}: evidence is older than {max_age.total_seconds() / 3600:g} hours"
            )

    source = report.get("source")
    if not isinstance(source, dict):
        errors.append(f"{label}.source: must be an object")
    else:
        if source.get("shan_commit") != expected_commit:
            errors.append(
                f"{label}.source.shan_commit: does not match {expected_commit}"
            )
        if source.get("shan_dirty") is not False:
            errors.append(f"{label}.source.shan_dirty: must be false")

    sensitive = find_sensitive_keys(report)
    if sensitive:
        errors.append(
            f"{label}: sensitive field names are forbidden: {', '.join(sorted(sensitive))}"
        )


def validate_audio(
    report: dict[str, Any], report_path: pathlib.Path, errors: list[str]
) -> None:
    environment = report.get("environment")
    if not isinstance(environment, dict):
        errors.append("audio.environment: must be an object")
    else:
        if environment.get("carrier") != "reachy_wireless":
            errors.append("audio.environment.carrier: must be reachy_wireless")
        for key in ("physical_microphone", "physical_speaker", "human_speech"):
            require_true(environment, key, "audio.environment", errors)

    checks = report.get("checks")
    if not isinstance(checks, dict):
        errors.append("audio.checks: must be an object")
    else:
        for key in (
            "stack_status_live",
            "audio_connected",
            "realtime_connected",
            "microphone_human_speech_detected",
            "speaker_playback_audible",
            "playback_only_aec_trial",
            "human_barge_in_detected",
            "cleanup_verified",
        ):
            require_true(checks, key, "audio.checks", errors)

    metrics = report.get("metrics")
    if not isinstance(metrics, dict):
        errors.append("audio.metrics: must be an object")
    else:
        require_number(
            metrics, "microphone_frames_delta", "audio.metrics", errors, minimum=1
        )
        require_number(
            metrics, "speaker_frames_delta", "audio.metrics", errors, minimum=1
        )
        require_number(
            metrics, "microphone_rms_peak", "audio.metrics", errors, minimum=0.001
        )
        require_number(
            metrics,
            "playback_only_false_barge_ins",
            "audio.metrics",
            errors,
            minimum=0,
            maximum=0,
        )
        require_number(
            metrics, "human_barge_in_count", "audio.metrics", errors, minimum=1
        )
        require_number(
            metrics,
            "barge_in_to_playback_stop_ms",
            "audio.metrics",
            errors,
            minimum=0,
            maximum=MAX_BARGE_STOP_MS,
        )
        require_number(
            metrics,
            "post_barge_ring_empty_ms",
            "audio.metrics",
            errors,
            minimum=0,
            maximum=MAX_RING_EMPTY_MS,
        )

    cleanup = report.get("cleanup")
    if not isinstance(cleanup, dict):
        errors.append("audio.cleanup: must be an object")
    else:
        if cleanup.get("call_state") != "disconnected":
            errors.append("audio.cleanup.call_state: must be disconnected")
        if cleanup.get("supervised_child_pids") != []:
            errors.append("audio.cleanup.supervised_child_pids: must be empty")

    validate_artifacts(
        report,
        report_path,
        {
            "stack_status_before",
            "audio_metrics",
            "realtime_events",
            "barge_in_timing",
            "cleanup_status",
        },
        "audio",
        errors,
    )


def validate_external_write(
    report: dict[str, Any], report_path: pathlib.Path, errors: list[str]
) -> None:
    authorization = report.get("test_scope")
    if not isinstance(authorization, dict):
        errors.append("external_write.test_scope: must be an object")
    else:
        for key in (
            "explicitly_authorized",
            "bounded_test_target",
            "unique_idempotency_key",
        ):
            require_true(authorization, key, "external_write.test_scope", errors)

    crash = report.get("crash_injection")
    if not isinstance(crash, dict):
        errors.append("external_write.crash_injection: must be an object")
    else:
        if crash.get("point") != "after_dispatch_before_ack":
            errors.append(
                "external_write.crash_injection.point: must be after_dispatch_before_ack"
            )
        require_true(
            crash, "process_exit_observed", "external_write.crash_injection", errors
        )

    recovery = report.get("recovery")
    if not isinstance(recovery, dict):
        errors.append("external_write.recovery: must be an object")
    else:
        for key in ("process_restarted", "receipt_recovered", "receipt_matches_effect"):
            require_true(recovery, key, "external_write.recovery", errors)
        if recovery.get("terminal_status") != "completed":
            errors.append("external_write.recovery.terminal_status: must be completed")

    effects = report.get("effects")
    if not isinstance(effects, dict):
        errors.append("external_write.effects: must be an object")
    else:
        require_number(
            effects,
            "downstream_effect_count",
            "external_write.effects",
            errors,
            minimum=1,
            maximum=1,
        )
        require_number(
            effects,
            "duplicate_side_effect_count",
            "external_write.effects",
            errors,
            minimum=0,
            maximum=0,
        )
        require_true(
            effects, "effect_matches_request", "external_write.effects", errors
        )

    cleanup = report.get("cleanup")
    if not isinstance(cleanup, dict):
        errors.append("external_write.cleanup: must be an object")
    else:
        require_true(
            cleanup,
            "test_artifact_removed_or_retained_by_policy",
            "external_write.cleanup",
            errors,
        )
        require_true(cleanup, "no_pending_retry", "external_write.cleanup", errors)

    validate_artifacts(
        report,
        report_path,
        {
            "audit_before_crash",
            "crash_observation",
            "restart_status",
            "downstream_effect",
            "recovered_receipt",
        },
        "external_write",
        errors,
    )


def main() -> int:
    args = parse_args()
    errors: list[str] = []
    if args.max_age_hours <= 0:
        errors.append("max_age_hours must be positive")
    expected_commit = args.expected_shan_commit.strip().lower()
    if (
        SHA256_RE.fullmatch(expected_commit) is None
        and re.fullmatch(r"^[0-9a-f]{40}$", expected_commit) is None
    ):
        errors.append(
            "expected_shan_commit must be a 40- or 64-character lowercase hex commit"
        )

    audio, audio_sha, audio_path = load_report(args.audio, "audio", errors)
    external_write, write_sha, write_path = load_report(
        args.external_write, "external_write", errors
    )
    now = dt.datetime.now(dt.timezone.utc)
    max_age = dt.timedelta(hours=max(args.max_age_hours, 0))

    if audio is not None and audio_path is not None:
        validate_common(
            audio,
            label="audio",
            schema=AUDIO_SCHEMA,
            expected_commit=expected_commit,
            max_age=max_age,
            now=now,
            errors=errors,
        )
        validate_audio(audio, audio_path, errors)
    if external_write is not None and write_path is not None:
        validate_common(
            external_write,
            label="external_write",
            schema=WRITE_SCHEMA,
            expected_commit=expected_commit,
            max_age=max_age,
            now=now,
            errors=errors,
        )
        validate_external_write(external_write, write_path, errors)

    result = {
        "schema_version": RESULT_SCHEMA,
        "validated_at": now.isoformat().replace("+00:00", "Z"),
        "expected_shan_commit": expected_commit,
        "max_age_hours": args.max_age_hours,
        "inputs": {
            "audio_sha256": audio_sha,
            "external_write_sha256": write_sha,
        },
        "passed": not errors,
        "errors": errors,
    }
    output = pathlib.Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print(f"Product release evidence passed: {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
