#!/bin/sh
# Copies the license files of every third-party module compiled into the plugin
# into a directory, for inclusion in release archives.
#
# Usage: scripts/third_party_licenses.sh [output-dir]   (default: third_party_licenses)
set -e

OUT=${1:-third_party_licenses}
mkdir -p "$OUT"
go mod download

GOOS=linux go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' . | sort -u |
while read -r mod dir; do
  dest="$OUT/$mod"
  mkdir -p "$dest"
  found=0
  for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do
    if [ -f "$f" ]; then
      cp "$f" "$dest/"
      found=1
    fi
  done
  if [ "$found" = 0 ]; then
    echo "no license file found for $mod" >&2
    exit 1
  fi
done

echo "collected licenses into $OUT"
