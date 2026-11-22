#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The checker compares working-tree source, not the caller's staged index.
# Clear hook coordinates before Git discovers repositories from nested paths.
git_env=$(git rev-parse --local-env-vars)
unset $git_env
cd "$ROOT/internal/apicompat"
GOWORK=off go run .
