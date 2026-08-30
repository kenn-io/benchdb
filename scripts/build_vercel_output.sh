#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
site="$root/site"
output="$root/.vercel/output"

for required in index.html guide/index.html docs/index.html docs.md llms.txt; do
	if [[ ! -s "$site/$required" ]]; then
		echo "error: built website is missing $required" >&2
		exit 1
	fi
done

rm -rf "$output"
mkdir -p "$output/static"
cp -R "$site/." "$output/static/"
printf '{"version":3}\n' > "$output/config.json"

echo "Vercel static output built at $output"
