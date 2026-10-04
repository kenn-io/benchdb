#!/usr/bin/env bash
# Emit image metadata only for an existing stable tag merged into origin/main.
set -euo pipefail
if [[ $# -ne 1 || ! $1 =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "release tag must be vMAJOR.MINOR.PATCH without leading zeroes or prerelease suffixes" >&2
  exit 1
fi
version=$1
if ! revision=$(git rev-parse --verify "refs/tags/${version}^{commit}" 2>/dev/null); then
  echo "release tag does not identify an existing commit" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$revision" refs/remotes/origin/main; then
  echo "release tag commit must be merged into origin/main" >&2
  exit 1
fi
printf 'version=%s\nrevision=%s\nimage=ghcr.io/kenn-io/benchdb\n' "$version" "$revision"
