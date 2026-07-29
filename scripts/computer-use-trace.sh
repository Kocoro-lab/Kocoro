#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 || -z "$1" ]] ||
  [[ $# -eq 2 && "$2" != "--summary" ]]; then
  echo "usage: $0 <session-id> [--summary]" >&2
  exit 2
fi

audit_log="${SHANNON_AUDIT_LOG:-$HOME/.shannon/logs/audit.log}"
if [[ ! -f "$audit_log" ]]; then
  echo "audit log not found: $audit_log" >&2
  exit 1
fi

if [[ $# -eq 2 ]]; then
  jq -s --arg session_id "$1" '
    [
      .[]
      | select(
          .session_id == $session_id and
          .event == "computer_use_trace_v1"
        )
      | .input_summary
      | fromjson
    ] as $events
    | if ($events | length) == 0 then
        error("no computer-use trace rows for session")
      else
        (
          [
            range(0; ($events | length))
            | select(
                $events[.].phase == "task" and
                $events[.].status == "started"
              )
          ]
        ) as $task_starts
        | if ($task_starts | length) == 0 then
            error("no computer-use task boundary for session")
          else
            ($task_starts | length) as $task_count
            | $task_starts[-1] as $task_start
            | $events[$task_start:] as $task_events
            | (
                $task_events
                | map(select(.phase == "task" and .status != "started"))
                | last
              ) as $task
            | (
                $task_events
                | map(select(.phase == "private_executor"))
                | last
              ) as $private
            | (
                $task_events
                | map(select(.phase == "action"))
              ) as $actions
            | (
                $actions
                | map(
                    select(
                      (.action_type // "") != "screenshot" and
                      (.action_type // "") != "wait"
                    )
                  )
              ) as $mutating_actions
            | {
                schema_version: 1,
                session_id: $session_id,
                task_count: $task_count,
                task_ordinal: $task_count,
                task_status: ($task.status // "unknown"),
                duration_ms: ($task.duration_ms // 0),
                model_calls: ($private.model_calls // 0),
                model_timeouts: ($private.model_timeouts // 0),
                provider_batches: (
                  $private.batch_count //
                  (
                    [
                      $task_events[]
                      | select(.phase == "batch")
                      | .batch_index
                    ]
                    | max // 0
                  )
                ),
                actions: ($actions | length),
                action_types: (
                  $actions
                  | map(.action_type // "unknown")
                  | group_by(.)
                  | map({key: .[0], value: length})
                  | from_entries
                ),
                attempted_mutating_actions: ($mutating_actions | length),
                committed_mutating_actions: (
                  [
                    $mutating_actions[]
                    | select(
                        .commit_state == "committed_verified" or
                        .commit_state == "committed_unverified"
                      )
                  ]
                  | length
                ),
                unknown_commit_actions: (
                  [
                    $mutating_actions[]
                    | select(.commit_state == "commit_status_unknown")
                  ]
                  | length
                ),
                not_committed_mutating_actions: (
                  [
                    $mutating_actions[]
                    | select(.commit_state == "not_committed")
                  ]
                  | length
                ),
                initial_observation_attempts: (
                  [
                    $task_events[]
                    | select(.phase == "initial_observation")
                  ]
                  | length
                ),
                final_observation_attempts: (
                  [
                    $task_events[]
                    | select(.phase == "final_observation")
                  ]
                  | length
                ),
                failed_batches: (
                  [
                    $task_events[]
                    | select(.phase == "batch" and .status == "failed")
                  ]
                  | length
                ),
                failures: (
                  [
                    $task_events[]
                    | select((.failure_code // "") != "")
                    | {phase, code: .failure_code}
                  ]
                  | unique_by([.phase, .code])
                )
              }
          end
      end
  ' "$audit_log"
  exit 0
fi

jq --arg session_id "$1" '
  select(
    .session_id == $session_id and
    .event == "computer_use_trace_v1"
  )
  | .input_summary
  | fromjson
' "$audit_log"
