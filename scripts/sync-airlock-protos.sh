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
	cp "$SRC_DIR/deployments.pb.go" "$dst/deployments.pb.go"
	cp "$SRC_DIR/bindings.pb.go" "$dst/bindings.pb.go"
	cp "$SRC_DIR/testing.pb.go" "$dst/testing.pb.go"
	cp "$SRC_DIR/resources.pb.go" "$dst/resources.pb.go"
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
		diff -u "$tmp/deployments.pb.go" "$DST_DIR/deployments.pb.go"
		diff -u "$tmp/bindings.pb.go" "$DST_DIR/bindings.pb.go"
		diff -u "$tmp/testing.pb.go" "$DST_DIR/testing.pb.go"
		diff -u "$tmp/resources.pb.go" "$DST_DIR/resources.pb.go"
		;;
	-h|--help|help)
		usage
		;;
	*)
		usage
		exit 2
		;;
esac
