#!/usr/bin/env bash
# Fail when the committed baseline agent notices differ from hermetic generation.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/scaffold/templates/THIRD_PARTY_NOTICES.generated.md"
backup=$(mktemp)
cp "$OUT" "$backup"
restore() {
	cp "$backup" "$OUT"
	rm -f "$backup"
}
trap restore EXIT

if ! "$ROOT/scripts/gen-notices.sh" >/dev/null; then
	echo "ERROR: agentsdk notice generation failed" >&2
	exit 1
fi
if ! cmp -s "$backup" "$OUT"; then
	echo "ERROR: scaffold/templates/THIRD_PARTY_NOTICES.generated.md is out of date." >&2
	echo "Regenerate and commit: ./scripts/gen-notices.sh" >&2
	exit 1
fi

echo "agentsdk notices: OK"
