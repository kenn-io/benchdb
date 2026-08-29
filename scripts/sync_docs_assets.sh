#!/usr/bin/env bash
# Materialize product screenshots from the orphan docs-assets branch without
# adding binary history to the application branch.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

asset="benchmark-series.png"
destination="website/assets/$asset"
marker="website/assets/.docs-assets.synced"
remote="${BENCHDB_DOCS_ASSET_REMOTE:-https://github.com/kenn-io/benchdb.git}"
stage="$(mktemp -d "${TMPDIR:-/tmp}/benchdb-docs-assets.XXXXXX")"
trap 'rm -rf "$stage"' EXIT

cached_asset_is_valid() {
  [[ -f "$destination" && -f "$marker" ]] && shasum -a 256 -c "$marker" >/dev/null 2>&1
}

if git fetch --quiet --depth=1 "$remote" docs-assets; then
  git show "FETCH_HEAD:$asset" > "$stage/$asset"
  mkdir -p "$(dirname "$destination")"
  mv -f "$stage/$asset" "$destination"
  shasum -a 256 "$destination" > "$stage/marker"
  mv -f "$stage/marker" "$marker"
  echo "synced $destination from docs-assets"
elif cached_asset_is_valid; then
  echo "using verified cached $destination"
else
  echo "error: could not fetch docs-assets and no verified cached screenshot exists" >&2
  exit 1
fi
