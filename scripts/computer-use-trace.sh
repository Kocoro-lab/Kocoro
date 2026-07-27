#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 <session-id>" >&2
  exit 2
fi

audit_log="${SHANNON_AUDIT_LOG:-$HOME/.shannon/logs/audit.log}"
if [[ ! -f "$audit_log" ]]; then
  echo "audit log not found: $audit_log" >&2
  exit 1
fi

jq --arg session_id "$1" '
  select(
    .session_id == $session_id and
    .event == "computer_use_trace_v1"
  )
  | .input_summary
  | fromjson
' "$audit_log"
