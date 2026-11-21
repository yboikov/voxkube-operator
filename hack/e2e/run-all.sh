#!/usr/bin/env bash
# Runs the full repeatable VoxKube E2E workflow (bootstrap -> install -> 12-stage suite).
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
LOG="${E2E_LOG:-/tmp/voxkube-e2e-last-run.log}"

{
  bash "${DIR}/00-bootstrap-vps.sh"
  bash "${DIR}/01-install-operator.sh"
  bash "${DIR}/02-run-e2e.sh"
} 2>&1 | tee "${LOG}"
