#!/usr/bin/env bash
# scripts/encryption-validation/02-roundtrip.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

PLAINTEXT="hello-roundtrip-$(date +%s)"
KEY="check02/rt.txt"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

GOT="$(engine_curl "${ENGINE_URL}/${TEST_BUCKET}/${KEY}")"

if [[ "${GOT}" == "${PLAINTEXT}" ]]; then
  STATUS="PASS"
  BODY="Wrote ${#PLAINTEXT} bytes; read back identical bytes."
else
  STATUS="FAIL"
  BODY=$'Mismatch.\n\nExpected:\n```\n'"${PLAINTEXT}"$'\n```\n\nGot:\n```\n'"${GOT}"$'\n```'
fi

report_section "Check 2 — Round-trip" "${STATUS}" "${BODY}"
echo "Check 2: ${STATUS}"
