#!/usr/bin/env bash
# Koe local acceptance smoke.
#
# Layers:
# - always: focused offline Go tests for turn/audio gates
# - KOE_VPIO_TEST=1: macOS VPIO hardware tests
# - OPENAI_API_KEY set: live headless Realtime --say test with WAV metrics
# - running Desktop Koe detected: empty-room start/end smoke against the control port
# - KOE_SMOKE_SPEAKER_ECHO=1: play macOS say through speakers and assert AEC/gates do not self-trigger

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"
TMPDIR="${TMPDIR:-/tmp}"
WORKDIR="$(mktemp -d "$TMPDIR/koe-smoke.XXXXXX")"
trap 'rm -rf "$WORKDIR"' EXIT

log() { printf 'koe-smoke: %s\n' "$*"; }
fail() { printf 'koe-smoke: FAIL: %s\n' "$*" >&2; exit 1; }

maybe_load_openai_key() {
  if [[ -n "${OPENAI_API_KEY:-}" ]]; then
    return
  fi
  local env_file="${KOE_OPENAI_ENV_FILE:-$HOME/Desktop/projects/reachy/kocoro-reachy/.env}"
  if [[ -f "$env_file" ]]; then
    OPENAI_API_KEY="$(grep '^OPENAI_API_KEY=' "$env_file" | head -1 | cut -d= -f2- || true)"
    export OPENAI_API_KEY
  fi
}

wav_metrics_json() {
  local wav="$1"
  python3 - "$wav" <<'PY'
import audioop, json, sys, wave
p = sys.argv[1]
with wave.open(p, "rb") as w:
    n = w.getnframes()
    sr = w.getframerate()
    data = w.readframes(n)
width = 2
frames = [data[i:i+960*width] for i in range(0, len(data), 960*width)]
silent = 0
for fr in frames:
    if not fr:
        continue
    if audioop.rms(fr, width) / 32768 < 0.00316:
        silent += 1
samples = n
rms = audioop.rms(data, width) / 32768 if data else 0
peak = audioop.max(data, width) / 32768 if data else 0
clipped = 0
prev = None
big = 0
for i in range(0, len(data), width):
    v = int.from_bytes(data[i:i+width], "little", signed=True)
    if abs(v) >= 32000:
        clipped += 1
    if prev is not None and abs(v - prev) / 32768 > 0.5:
        big += 1
    prev = v
metrics = {
    "samples": samples,
    "seconds": samples / sr if sr else 0,
    "rms": rms,
    "peak": peak,
    "silence_ratio": silent / len(frames) if frames else 1,
    "clipping_ratio": clipped / samples if samples else 0,
    "discontinuity_ratio": big / max(samples - 1, 1),
}
print(json.dumps(metrics, sort_keys=True))
PY
}

check_live_wav_metrics() {
  local wav="$1"
  local metrics
  metrics="$(wav_metrics_json "$wav")"
  log "live wav metrics: $metrics"
  python3 - "$metrics" <<'PY'
import json, sys
m = json.loads(sys.argv[1])
if m["samples"] <= 0:
    raise SystemExit("captured WAV is empty")
if m["peak"] < 0.02:
    raise SystemExit(f"captured WAV peak too low: {m['peak']}")
if m["clipping_ratio"] > 0.01:
    raise SystemExit(f"captured WAV clips: {m['clipping_ratio']}")
if m["discontinuity_ratio"] > 0.01:
    raise SystemExit(f"captured WAV discontinuity too high: {m['discontinuity_ratio']}")
PY
}

check_live_turn_latency() {
  local runlog="$1"
  local max_ms="${KOE_SMOKE_ENDPOINT_MAX_MS:-5000}"
  python3 - "$runlog" "$max_ms" <<'PY'
import datetime as dt, re, sys
path, max_ms = sys.argv[1], int(sys.argv[2])
events = {}
pat = re.compile(r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) .*koe\[event\]: (.+)$")
with open(path, "r", encoding="utf-8", errors="replace") as f:
    for line in f:
        m = pat.match(line)
        if not m:
            continue
        ts = dt.datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S")
        ev = m.group(2).strip()
        events.setdefault(ev, ts)
required = ["input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "response.created", "response.done"]
missing = [ev for ev in required if ev not in events]
if missing:
    raise SystemExit(f"missing event(s): {missing}")
endpoint_ms = int((events["input_audio_buffer.speech_stopped"] - events["input_audio_buffer.speech_started"]).total_seconds() * 1000)
first_response_ms = int((events["response.created"] - events["input_audio_buffer.speech_stopped"]).total_seconds() * 1000)
done_ms = int((events["response.done"] - events["response.created"]).total_seconds() * 1000)
print(f"koe-smoke: live turn latency: endpoint_ms={endpoint_ms} first_response_ms={first_response_ms} response_done_ms={done_ms}")
if endpoint_ms > max_ms:
    raise SystemExit(f"input endpoint too slow: {endpoint_ms}ms > {max_ms}ms")
if first_response_ms > 2500:
    raise SystemExit(f"response creation too slow after endpoint: {first_response_ms}ms")
PY
}

run_offline_tests() {
  log "running focused offline Koe tests"
  PKG_CONFIG_PATH="$PKG_CONFIG_PATH" go test ./internal/koe -run 'Test(MicNoiseGate|FeedFrames|SessionConfig|VPIOGate|StartFile|Opus|HalfDuplex)' -v
}

run_vpio_hardware_tests() {
  if [[ "${KOE_VPIO_TEST:-}" != "1" ]]; then
    log "skipping VPIO hardware tests (set KOE_VPIO_TEST=1)"
    return
  fi
  log "running VPIO hardware tests"
  KOE_VPIO_TEST=1 PKG_CONFIG_PATH="$PKG_CONFIG_PATH" go test ./internal/koe -run 'TestStartVPIOHardwareCapturesAndPlays|TestVPIOHardwareDropsCaptureWhileSpeaking|TestVPIOHardwareBurstPlaybackDoesNotOverwriteRing' -v
}

run_live_headless() {
  maybe_load_openai_key
  if [[ -z "${OPENAI_API_KEY:-}" ]]; then
    log "skipping live headless test (OPENAI_API_KEY not set)"
    return
  fi
  log "building shan for live headless test"
  local bin="$WORKDIR/koe-shan"
  PKG_CONFIG_PATH="$PKG_CONFIG_PATH" go build -o "$bin" .

  local wav="$WORKDIR/ready.wav"
  local runlog="$WORKDIR/live.log"
  log "running live headless Realtime --say test"
  OPENAI_API_KEY="$OPENAI_API_KEY" KOE_EVENT_LOG=1 KOE_TRANSCRIPT_LOG=1 \
    "$bin" koe --say "Please say exactly: ready." --audio-out "$wav" --timeout "${KOE_SMOKE_TIMEOUT:-28}" \
    >"$runlog" 2>&1
  sed -n '1,220p' "$runlog"

  rg -q 'input_audio_buffer\.speech_stopped' "$runlog" || fail "live run did not endpoint input speech"
  rg -q 'response\.created' "$runlog" || fail "live run did not create a response"
  rg -q 'response\.done' "$runlog" || fail "live run did not finish a response"
  check_live_turn_latency "$runlog"
  check_live_wav_metrics "$wav"

  if [[ "${KOE_SMOKE_TRANSCRIBE:-}" == "1" ]] && command -v ffmpeg >/dev/null 2>&1; then
    local trim="$WORKDIR/ready-trim.wav"
    ffmpeg -hide_banner -loglevel error -y -ss 10 -t 3 -i "$wav" "$trim"
    local transcript
    transcript="$(curl -sS https://api.openai.com/v1/audio/transcriptions \
      -H "Authorization: Bearer ${OPENAI_API_KEY}" \
      -H "Content-Type: multipart/form-data" \
      -F "file=@${trim}" \
      -F model=whisper-1 \
      -F response_format=json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("text",""))')"
    log "live transcript: $transcript"
    [[ "${transcript,,}" == *ready* ]] || fail "live transcript did not contain ready"
  fi
}

detect_koe_port() {
  if [[ -n "${KOE_CONTROL_PORT:-}" ]]; then
    printf '%s\n' "$KOE_CONTROL_PORT"
    return
  fi
  ps -axo command | sed -n 's/.*shan koe .*--control-port \([0-9][0-9]*\).*/\1/p' | head -1
}

run_desktop_empty_room() {
  local port
  port="$(detect_koe_port)"
  if [[ -z "$port" ]]; then
    log "skipping Desktop empty-room smoke (no running shan koe control port)"
    return
  fi
  log "running Desktop empty-room smoke on port $port"

  local koe_log="$HOME/Library/Logs/Kocoro/koe.log"
  local start_line=0
  if [[ -f "$koe_log" ]]; then
    start_line="$(wc -l < "$koe_log" | tr -d ' ')"
  fi
  local events="$WORKDIR/desktop-events.ndjson"
  : > "$events"
  curl -sS --no-buffer "http://127.0.0.1:${port}/events" > "$events" &
  local curl_pid=$!
  sleep 0.4
  local t0 t1
  t0="$(python3 - <<'PY'
import time
print(int(time.time()*1000))
PY
)"
  curl -sS -X POST "http://127.0.0.1:${port}/call/start" >/dev/null
  t1="$(python3 - <<'PY'
import time
print(int(time.time()*1000))
PY
)"
  log "Desktop start POST ms: $((t1-t0))"
  sleep "${KOE_SMOKE_EMPTY_SECONDS:-7}"
  curl -sS -X POST "http://127.0.0.1:${port}/call/end" >/dev/null
  sleep 1
  kill "$curl_pid" >/dev/null 2>&1 || true
  wait "$curl_pid" >/dev/null 2>&1 || true

  local fresh_log="$WORKDIR/desktop-koe.log"
  if [[ -f "$koe_log" ]]; then
    tail -n "+$((start_line+1))" "$koe_log" > "$fresh_log"
  else
    : > "$fresh_log"
  fi
  sed -n '1,220p' "$fresh_log"
  rg -q 'call ready' "$fresh_log" || fail "Desktop empty-room smoke did not reach call ready"
  ! rg -q 'input_audio_buffer\.speech_started|response\.created' "$fresh_log" || fail "empty room triggered Realtime speech/response"
  rg -q 'SpeechStarts:0' "$fresh_log" || fail "empty room mic gate did not report SpeechStarts:0"
  rg -q 'PlayUnderruns:0' "$fresh_log" || fail "Desktop empty-room VPIO playback underrun"
  rg -q 'PlayOverwrites:0' "$fresh_log" || fail "Desktop empty-room VPIO playback overwrite"
  rg -q 'PlayBuffered:0' "$fresh_log" || fail "Desktop empty-room left playback buffered"
}

run_desktop_speaker_echo() {
  if [[ "${KOE_SMOKE_SPEAKER_ECHO:-}" != "1" ]]; then
    log "skipping Desktop speaker-echo smoke (set KOE_SMOKE_SPEAKER_ECHO=1)"
    return
  fi
  if ! command -v say >/dev/null 2>&1; then
    log "skipping Desktop speaker-echo smoke (macOS say unavailable)"
    return
  fi
  local port
  port="$(detect_koe_port)"
  if [[ -z "$port" ]]; then
    log "skipping Desktop speaker-echo smoke (no running shan koe control port)"
    return
  fi
  log "running Desktop speaker-echo smoke on port $port"

  local koe_log="$HOME/Library/Logs/Kocoro/koe.log"
  local start_line=0
  if [[ -f "$koe_log" ]]; then
    start_line="$(wc -l < "$koe_log" | tr -d ' ')"
  fi
  local old_volume
  old_volume="$(osascript -e 'output volume of (get volume settings)' 2>/dev/null || printf '50')"
  osascript -e "set volume output volume ${KOE_SMOKE_SPEAKER_VOLUME:-70}" >/dev/null 2>&1 || true
  curl -sS -X POST "http://127.0.0.1:${port}/call/start" >/dev/null
  sleep 1
  say -v "${KOE_SMOKE_SPEAKER_VOICE:-Samantha}" -r "${KOE_SMOKE_SPEAKER_RATE:-145}" \
    "Kocoro, please listen carefully. Please say exactly ready. This is a desktop speaker echo suppression test."
  sleep "${KOE_SMOKE_SPEAKER_SECONDS:-8}"
  curl -sS -X POST "http://127.0.0.1:${port}/call/end" >/dev/null
  sleep 1
  osascript -e "set volume output volume ${old_volume}" >/dev/null 2>&1 || true

  local fresh_log="$WORKDIR/desktop-speaker-echo.log"
  tail -n "+$((start_line+1))" "$koe_log" > "$fresh_log"
  sed -n '1,220p' "$fresh_log"
  rg -q 'call ready' "$fresh_log" || fail "Desktop speaker-echo smoke did not reach call ready"
  ! rg -q 'input_audio_buffer\.speech_started|response\.created' "$fresh_log" || fail "speaker echo triggered Realtime speech/response"
  rg -q 'SpeechStarts:0' "$fresh_log" || fail "speaker echo opened local mic gate"
  rg -q 'PlayUnderruns:0' "$fresh_log" || fail "Desktop speaker-echo VPIO playback underrun"
  rg -q 'PlayOverwrites:0' "$fresh_log" || fail "Desktop speaker-echo VPIO playback overwrite"
  rg -q 'PlayBuffered:0' "$fresh_log" || fail "Desktop speaker-echo left playback buffered"
}

run_offline_tests
run_vpio_hardware_tests
run_live_headless
run_desktop_empty_room
run_desktop_speaker_echo
log "PASS"
