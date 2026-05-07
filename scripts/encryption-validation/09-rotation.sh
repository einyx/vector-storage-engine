#!/usr/bin/env bash
# scripts/encryption-validation/09-rotation.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

HITS="$(grep -rniE 'rotat(e|ion)|rewrap|reencrypt' internal/encryption internal/kms api 2>/dev/null \
  | grep -v _test.go | head -40 || true)"

ENDPOINT_HITS="$(grep -rniE 'rotat|rewrap' cmd internal/transport internal/middleware 2>/dev/null \
  | grep -v _test.go | head -20 || true)"

if [[ -z "${HITS}${ENDPOINT_HITS}" ]]; then
  STATUS="GAP"
  BODY="No rotation, rewrap, or re-encrypt symbols found in source. Phase 2 hardening gap: design and implement key rotation flow."
else
  STATUS="REVIEW"
  BODY=$'Rotation-related symbols found — manual review needed.\n\nSource hits:\n```\n'"${HITS:-(none)}"$'\n```\n\nEndpoint hits:\n```\n'"${ENDPOINT_HITS:-(none)}"$'\n```'
fi

report_section "Check 9 — Key rotation API" "${STATUS}" "${BODY}"
echo "Check 9: ${STATUS}"
