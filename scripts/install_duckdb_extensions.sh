#!/usr/bin/env bash
set -euo pipefail

# Keep this version aligned with duckdb-go/v2 in go.mod. Each signed extension
# must match both the embedded DuckDB version and the target platform. BenchDB
# loads these files with runtime downloads disabled: postgres_scanner for every
# archive, and httpfs and aws for S3 and GCS archives.
version=v1.5.5
extensions=(postgres_scanner httpfs aws)
destination="${1:?usage: install_duckdb_extensions.sh DIRECTORY}"
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) platform=linux_amd64 ;;
  Linux/aarch64 | Linux/arm64) platform=linux_arm64 ;;
  Darwin/x86_64) platform=osx_amd64 ;;
  Darwin/arm64) platform=osx_arm64 ;;
  *) echo "DuckDB extension packaging requires Linux or macOS on amd64 or arm64" >&2; exit 1 ;;
esac

directory="$destination/$version/$platform"
mkdir -p "$directory"
download=""
trap 'rm -f "$download"' EXIT
for name in "${extensions[@]}"; do
  extension="$directory/$name.duckdb_extension"
  if [[ -s "$extension" ]]; then
    continue
  fi
  download="$(mktemp "$directory/.$name.XXXXXX")"
  curl --fail --silent --show-error --location --retry 3 \
    "https://extensions.duckdb.org/$version/$platform/$name.duckdb_extension.gz" \
    | gzip -d > "$download"
  chmod 0644 "$download"
  mv "$download" "$extension"
  download=""
done
