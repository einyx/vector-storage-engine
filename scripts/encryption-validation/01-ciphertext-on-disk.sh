#!/usr/bin/env bash
# scripts/encryption-validation/01-ciphertext-on-disk.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

PLAINTEXT="WARP-ENCRYPTION-CANARY-$(date +%s)-$RANDOM"
KEY="check01/canary.txt"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

RAW_FILE="$(mktemp)"
mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" > "${RAW_FILE}"

if grep -q "${PLAINTEXT}" "${RAW_FILE}"; then
  STATUS="FAIL"
  BODY="Plaintext canary \`${PLAINTEXT}\` found in raw MinIO object — encryption did NOT happen."
else
  STATUS="PASS"
  HEX_HEAD="$(xxd -l 64 "${RAW_FILE}" | sed 's/^/    /')"
  BODY=$'Plaintext canary not present in raw object. First 64 bytes:\n\n```\n'"${HEX_HEAD}"$'\n```'
fi

report_section "Check 1 — Ciphertext on disk" "${STATUS}" "${BODY}"
echo "Check 1: ${STATUS}"
