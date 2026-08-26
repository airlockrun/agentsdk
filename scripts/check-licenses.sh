#!/usr/bin/env bash
# Fail closed when code or assets shipped by agentsdk use an unapproved license.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
violations=$(mktemp)
modules=$(mktemp)
trap 'rm -f "$violations" "$modules"' EXIT

classify() {
	local file="$1"
	grep -qiE "Affero General Public" "$file" && { echo "DENY AGPL"; return; }
	grep -qiE "Lesser General Public" "$file" && { echo "DENY LGPL"; return; }
	grep -qiE "GNU GENERAL PUBLIC LICENSE" "$file" && { echo "DENY GPL"; return; }
	grep -qiE "Mozilla Public License" "$file" && { echo "DENY MPL"; return; }
	grep -qiE "Eclipse Public License" "$file" && { echo "DENY EPL"; return; }
	grep -qiE "Common Development and Distribution|CDDL" "$file" && { echo "DENY CDDL"; return; }
	grep -qiE "Business Source License|\bBUSL\b" "$file" && { echo "DENY BSL"; return; }
	grep -qiE "Server Side Public License|\bSSPL\b" "$file" && { echo "DENY SSPL"; return; }
	grep -qiE "Commons Clause" "$file" && { echo "DENY Commons-Clause"; return; }
	grep -qiE "Elastic License" "$file" && { echo "DENY Elastic"; return; }
	grep -qiE "Apache License" "$file" && grep -qiE "Version 2\.0" "$file" && { echo "ALLOW Apache-2.0"; return; }
	grep -qiE "Permission to use, copy, modify, and(/or)? distribute" "$file" && { echo "ALLOW ISC"; return; }
	grep -qiE "Permission is hereby granted, free of charge" "$file" && { echo "ALLOW MIT"; return; }
	grep -qiE "Redistribution and use in source and binary forms" "$file" && { echo "ALLOW BSD"; return; }
	grep -qiE "free and unencumbered software released into the public domain" "$file" && { echo "ALLOW Unlicense"; return; }
	grep -qiE "Blue Oak Model License" "$file" && { echo "ALLOW BlueOak"; return; }
	echo "UNKNOWN"
}

(
	cd "$ROOT"
	GOWORK=off go list -deps -f '{{with .Module}}{{.Path}}|{{.Dir}}{{end}}' ./...
) | sort -u >"$modules"

while IFS='|' read -r path dir; do
	[ -z "$dir" ] && continue
	case "$path" in github.com/airlockrun/*) continue ;; esac
	license=""
	for candidate in "$dir"/LICENSE* "$dir"/COPYING* "$dir"/LICENCE*; do
		if [ -f "$candidate" ]; then
			license="$candidate"
			break
		fi
	done
	if [ -z "$license" ]; then
		echo "  $path - NO LICENSE FILE" >>"$violations"
		continue
	fi
	result=$(classify "$license")
	case "${result%% *}" in
	ALLOW) ;;
	*) echo "  $path - $result" >>"$violations" ;;
	esac
done <"$modules"

lucide_result=$(classify "$ROOT/lucide/UPSTREAM_LICENSE")
if [ "$lucide_result" != "ALLOW ISC" ]; then
	echo "  Lucide Icons bundled asset - $lucide_result, want ALLOW ISC" >>"$violations"
fi

if [ -s "$violations" ]; then
	echo "ERROR: disallowed or unrecognised agentsdk dependency licenses:" >&2
	while IFS= read -r line; do
		echo "$line" >&2
	done <"$violations"
	exit 1
fi

echo "agentsdk licenses: OK"
