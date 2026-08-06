import copy
import datetime as dt
import hashlib
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


COMMIT = "a" * 40


def timestamps(age_hours: float = 0.0) -> tuple[str, str]:
    now = dt.datetime.now(dt.timezone.utc) - dt.timedelta(hours=age_hours)
    started = now - dt.timedelta(minutes=2)
    finished = now - dt.timedelta(minutes=1)
    return (
        started.isoformat().replace("+00:00", "Z"),
        finished.isoformat().replace("+00:00", "Z"),
    )


def artifacts(*kinds: str) -> list[dict[str, str]]:
    return [{"kind": kind} for kind in kinds]


def valid_audio(age_hours: float = 0.0) -> dict:
    started, finished = timestamps(age_hours)
    return {
        "schema_version": "kocoro.physical_audio_hil.v1",
        "run_id": "audio-hil-test",
        "started_at": started,
        "finished_at": finished,
        "source": {"shan_commit": COMMIT, "shan_dirty": False},
        "environment": {
            "carrier": "reachy_wireless",
            "physical_microphone": True,
            "physical_speaker": True,
            "human_speech": True,
        },
        "checks": {
            "stack_status_live": True,
            "audio_connected": True,
            "realtime_connected": True,
            "microphone_human_speech_detected": True,
            "speaker_playback_audible": True,
            "playback_only_aec_trial": True,
            "human_barge_in_detected": True,
            "cleanup_verified": True,
        },
        "metrics": {
            "microphone_frames_delta": 240,
            "speaker_frames_delta": 180,
            "microphone_rms_peak": 0.08,
            "playback_only_false_barge_ins": 0,
            "human_barge_in_count": 1,
            "barge_in_to_playback_stop_ms": 640,
            "post_barge_ring_empty_ms": 40,
        },
        "cleanup": {"call_state": "disconnected", "supervised_child_pids": []},
        "artifacts": artifacts(
            "stack_status_before",
            "audio_metrics",
            "realtime_events",
            "barge_in_timing",
            "cleanup_status",
        ),
    }


def valid_external_write(age_hours: float = 0.0) -> dict:
    started, finished = timestamps(age_hours)
    return {
        "schema_version": "kocoro.external_write_crash_recovery.v1",
        "run_id": "external-write-test",
        "started_at": started,
        "finished_at": finished,
        "source": {"shan_commit": COMMIT, "shan_dirty": False},
        "test_scope": {
            "explicitly_authorized": True,
            "bounded_test_target": True,
            "unique_idempotency_key": True,
        },
        "crash_injection": {
            "point": "after_dispatch_before_ack",
            "process_exit_observed": True,
        },
        "recovery": {
            "process_restarted": True,
            "receipt_recovered": True,
            "receipt_matches_effect": True,
            "terminal_status": "completed",
        },
        "effects": {
            "downstream_effect_count": 1,
            "duplicate_side_effect_count": 0,
            "effect_matches_request": True,
        },
        "cleanup": {
            "test_artifact_removed_or_retained_by_policy": True,
            "no_pending_retry": True,
        },
        "artifacts": artifacts(
            "audit_before_crash",
            "crash_observation",
            "restart_status",
            "downstream_effect",
            "recovered_receipt",
        ),
    }


class ProductReleaseEvidenceTest(unittest.TestCase):
    def run_validator(
        self,
        audio: dict,
        external_write: dict,
        expected_commit: str = COMMIT,
        corrupt_artifact_digest: bool = False,
        escape_artifact_path: bool = False,
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            audio_path = root / "audio.json"
            write_path = root / "write.json"
            output_path = root / "result.json"
            for label, report in (("audio", audio), ("write", external_write)):
                for artifact in report["artifacts"]:
                    artifact_path = root / f"{label}-{artifact['kind']}.jsonl"
                    artifact_path.write_text(
                        f"{artifact['kind']} observed\n", encoding="utf-8"
                    )
                    artifact["path"] = artifact_path.name
                    artifact["sha256"] = hashlib.sha256(
                        artifact_path.read_bytes()
                    ).hexdigest()
            if corrupt_artifact_digest:
                audio["artifacts"][0]["sha256"] = "0" * 64
            if escape_artifact_path:
                audio["artifacts"][0]["path"] = "../outside-evidence.log"
            audio_path.write_text(json.dumps(audio), encoding="utf-8")
            write_path.write_text(json.dumps(external_write), encoding="utf-8")
            script = (
                pathlib.Path(__file__).resolve().parents[1]
                / "validate-product-release-evidence.py"
            )
            completed = subprocess.run(
                [
                    sys.executable,
                    str(script),
                    "--audio",
                    str(audio_path),
                    "--external-write",
                    str(write_path),
                    "--expected-shan-commit",
                    expected_commit,
                    "--output",
                    str(output_path),
                ],
                check=False,
                capture_output=True,
                text=True,
            )
            result = json.loads(output_path.read_text(encoding="utf-8"))
            return completed, result

    def test_valid_physical_and_crash_recovery_evidence_passes(self):
        completed, result = self.run_validator(valid_audio(), valid_external_write())
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertTrue(result["passed"])
        self.assertEqual(result["errors"], [])

    def test_simulated_audio_is_rejected(self):
        audio = valid_audio()
        audio["environment"]["physical_microphone"] = False
        completed, result = self.run_validator(audio, valid_external_write())
        self.assertEqual(completed.returncode, 1)
        self.assertFalse(result["passed"])
        self.assertIn(
            "audio.environment.physical_microphone: must be true", completed.stderr
        )

    def test_slow_barge_in_and_false_self_interrupt_are_rejected(self):
        audio = valid_audio()
        audio["metrics"]["playback_only_false_barge_ins"] = 1
        audio["metrics"]["barge_in_to_playback_stop_ms"] = 1500
        completed, _ = self.run_validator(audio, valid_external_write())
        self.assertEqual(completed.returncode, 1)
        self.assertIn("playback_only_false_barge_ins", completed.stderr)
        self.assertIn("barge_in_to_playback_stop_ms", completed.stderr)

    def test_duplicate_external_effect_is_rejected(self):
        external_write = valid_external_write()
        external_write["effects"]["downstream_effect_count"] = 2
        external_write["effects"]["duplicate_side_effect_count"] = 1
        completed, _ = self.run_validator(valid_audio(), external_write)
        self.assertEqual(completed.returncode, 1)
        self.assertIn("downstream_effect_count", completed.stderr)
        self.assertIn("duplicate_side_effect_count", completed.stderr)

    def test_stale_or_wrong_commit_evidence_is_rejected(self):
        completed, _ = self.run_validator(
            valid_audio(age_hours=30),
            valid_external_write(age_hours=30),
            expected_commit="c" * 40,
        )
        self.assertEqual(completed.returncode, 1)
        self.assertIn("older than 24 hours", completed.stderr)
        self.assertIn("source.shan_commit", completed.stderr)

    def test_sensitive_fields_are_rejected(self):
        audio = copy.deepcopy(valid_audio())
        audio["debug_token"] = "must-never-enter-release-evidence"
        completed, _ = self.run_validator(audio, valid_external_write())
        self.assertEqual(completed.returncode, 1)
        self.assertIn("sensitive field names are forbidden", completed.stderr)

    def test_artifact_digest_mismatch_is_rejected(self):
        completed, _ = self.run_validator(
            valid_audio(),
            valid_external_write(),
            corrupt_artifact_digest=True,
        )
        self.assertEqual(completed.returncode, 1)
        self.assertIn("digest mismatch", completed.stderr)

    def test_artifact_path_escape_is_rejected(self):
        completed, _ = self.run_validator(
            valid_audio(),
            valid_external_write(),
            escape_artifact_path=True,
        )
        self.assertEqual(completed.returncode, 1)
        self.assertIn("escapes the evidence directory", completed.stderr)


if __name__ == "__main__":
    unittest.main()
