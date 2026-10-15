#!/usr/bin/env bash
# Runs one build step and prints one line for it. The step's own output is
# shown only when it fails, or as it runs when VERBOSE=1.
#
# Usage: run_step.sh [--summary] <label> <command> [args...]
#   --summary  print the step's last line of output instead of the label
set -uo pipefail

summary=0
if [[ "${1:-}" == "--summary" ]]; then
	summary=1
	shift
fi
label="$1"
shift

if [[ "${VERBOSE:-0}" == "1" ]]; then
	echo "==> $label"
	exec "$@"
fi

log="$(mktemp "${TMPDIR:-/tmp}/run-step.XXXXXX")"
trap 'rm -f "$log"' EXIT

if "$@" >"$log" 2>&1; then
	if [[ "$summary" == "1" ]]; then
		printf '  ok  %s\n' "$(tail -n 1 "$log")"
	else
		printf '  ok  %s\n' "$label"
	fi
	exit 0
else
	status=$?
	printf 'FAIL  %s\n' "$label" >&2
	cat "$log" >&2
	exit "$status"
fi
