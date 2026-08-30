#!/usr/bin/env bash
# Invoked by the GitHub runner through ACTIONS_RUNNER_HOOK_JOB_STARTED, as the
# runner user, before the first step of the assigned job. It only records that
# a job reached this worker so the idle watchdog in runner-entrypoint stands
# down. It receives no credential and prints nothing into the job log.
set -u
marker_dir=/run/runneryard
mkdir -p "$marker_dir" 2>/dev/null || true
: > "$marker_dir/job-started"
