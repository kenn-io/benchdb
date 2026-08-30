#!/usr/bin/env bash
# Materialize dashboard screenshots from the orphan docs-screenshots branch
# without adding binary history to the application branch.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

destination="${BENCHDB_DOCS_SCREENSHOT_OUT_DIR:-$root/docs/site/assets/screenshots}"
remote="${BENCHDB_DOCS_SCREENSHOT_REMOTE:-https://github.com/kenn-io/benchdb.git}"
source_dir="${BENCHDB_DOCS_SCREENSHOT_SOURCE_DIR:-}"
stage="$(mktemp -d "${TMPDIR:-/tmp}/benchdb-docs-screenshots.XXXXXX")"
trap 'find "$stage" -depth -delete' EXIT

screenshots_are_valid() {
	local directory="$1"
	[[ -d "$directory" ]] && \
		BENCHDB_DOCS_SCREENSHOT_OUT_DIR="$directory" \
		bash "$root/scripts/check_docs_screenshots.sh" >/dev/null 2>&1
}

if [[ -n "$source_dir" ]]; then
	if [[ ! -d "$source_dir" ]]; then
		echo "error: BENCHDB_DOCS_SCREENSHOT_SOURCE_DIR is not a directory: $source_dir" >&2
		exit 1
	fi
	mkdir -p "$stage/latest"
	cp "$source_dir"/dashboard-*.png "$stage/latest/"
	cp "$source_dir/dashboard-screenshots-evidence.json" "$stage/latest/"
	echo "staged dashboard screenshots from BENCHDB_DOCS_SCREENSHOT_SOURCE_DIR"
elif screenshots_are_valid "$destination"; then
	echo "using verified dashboard screenshots in $destination"
	exit 0
elif git fetch --quiet --depth=1 "$remote" docs-screenshots; then
	git archive FETCH_HEAD latest | tar -x -C "$stage"
else
	echo "error: could not fetch docs-screenshots and no verified local screenshots exist" >&2
	exit 1
fi

if ! screenshots_are_valid "$stage/latest"; then
	echo "error: staged dashboard screenshots do not match the current documentation inventory" >&2
	exit 1
fi

mkdir -p "$destination"
find "$destination" -maxdepth 1 -type f \( -name 'dashboard-*.png' -o -name 'dashboard-screenshots-evidence.json' \) -delete
cp "$stage/latest"/dashboard-*.png "$destination/"
cp "$stage/latest/dashboard-screenshots-evidence.json" "$destination/"

BENCHDB_DOCS_SCREENSHOT_OUT_DIR="$destination" bash "$root/scripts/check_docs_screenshots.sh"
echo "synced dashboard screenshots from docs-screenshots"
