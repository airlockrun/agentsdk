#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AIRLOCK_DIR="${AIRLOCK_DIR:-$(cd "$ROOT_DIR/../airlock" && pwd)}"
SRC_DIR="$AIRLOCK_DIR/gen/airlock/v1"
DST_DIR="$ROOT_DIR/internal/airlockv1"

usage() {
	printf 'usage: %s [sync|check]\n' "$(basename "$0")" >&2
}

copy_files() {
	local dst="$1"
	mkdir -p "$dst"
	cp "$SRC_DIR/api.pb.go" "$dst/api.pb.go"
	cp "$SRC_DIR/types.pb.go" "$dst/types.pb.go"
}

mode="${1:-sync}"
case "$mode" in
	sync)
		copy_files "$DST_DIR"
		;;
	check)
		tmp="$(mktemp -d)"
		trap 'rm -rf "$tmp"' EXIT
		copy_files "$tmp"
		diff -u "$tmp/api.pb.go" "$DST_DIR/api.pb.go"
		diff -u "$tmp/types.pb.go" "$DST_DIR/types.pb.go"
		;;
	-h|--help|help)
		usage
		;;
	*)
		usage
		exit 2
		;;
esac
