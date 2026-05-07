#!/usr/bin/env bash
# scripts/encryption-validation/run-all.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
bash "${SCRIPT_DIR}/00-setup.sh"
for n in 01 02 03 04 05 06 07 08 09 10; do
  echo "=== running check ${n} ==="
  bash "${SCRIPT_DIR}/${n}-"*.sh
done
echo "all checks complete"
