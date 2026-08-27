#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
project="${BENCHDB_DOCS_SCREENSHOT_PROJECT:-benchdb_docs_screenshots}"
host_port="${BENCHDB_DOCS_SCREENSHOT_HOST_PORT:-127.0.0.1:18180}"
base_url="${BENCHDB_DOCS_SCREENSHOT_BASE_URL:-http://127.0.0.1:${host_port##*:}}"
out_dir="${BENCHDB_DOCS_SCREENSHOT_OUT_DIR:-$root/docs/site/assets/screenshots}"
keep_stack="${BENCHDB_DOCS_SCREENSHOT_KEEP_STACK:-0}"
auth_disabled="${BENCHDB_DOCS_SCREENSHOT_AUTH_DISABLED:-false}"

compose=(
	docker compose
	-p "$project"
	-f "$root/docker-compose.server.yml"
	-f "$root/docker-compose.docs-screenshots.yml"
)

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		"${compose[@]}" logs --since 30m || true
	fi
	if [ "$keep_stack" != "1" ]; then
		"${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
	fi
	exit "$status"
}
trap cleanup EXIT

mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"
rm -f "$out_dir"/dashboard-*.png "$out_dir"/dashboard-screenshots-evidence.json
export BENCHDB_DOCS_SCREENSHOT_OUT_DIR="$out_dir"
export BENCHDB_DOCS_SCREENSHOT_PUBLIC_BASE_URL="${BENCHDB_DOCS_SCREENSHOT_PUBLIC_BASE_URL:-https://benchdb.example}"
export BENCHDB_DOCS_SCREENSHOT_UID="${BENCHDB_DOCS_SCREENSHOT_UID:-$(id -u)}"
export BENCHDB_DOCS_SCREENSHOT_GID="${BENCHDB_DOCS_SCREENSHOT_GID:-$(id -g)}"

"${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
DCOMP_BENCHDB_SERVER_HOST_PORT="$host_port" \
	DCOMP_BENCHDB_AUTH_DISABLED="$auth_disabled" \
	"${compose[@]}" up --build --wait --detach db server

curl -fsS "$base_url/api/ping" >/dev/null
curl -fsS "$base_url/series" >/dev/null
python3 -B "$root/scripts/docs_screenshot_preflight.py" "$base_url"

"${compose[@]}" build docs-screenshots-runner
"${compose[@]}" run --rm --no-deps docs-screenshots-runner

python3 -B "$root/scripts/docs_screenshot_evidence.py" write \
	"$root/web/docs-screenshots/screenshots.json" \
	"$out_dir" \
	"$out_dir/dashboard-screenshots-evidence.json"

bash "$root/scripts/check_docs_screenshots.sh"

echo "docs screenshots verified and written to $out_dir"
