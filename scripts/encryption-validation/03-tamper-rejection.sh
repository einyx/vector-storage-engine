#!/usr/bin/env bash
# scripts/encryption-validation/03-tamper-rejection.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

KEY="check03/tamper.bin"
PLAINTEXT="$(head -c 4096 /dev/urandom | base64)"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

RAW="$(mktemp)"
mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" > "${RAW}"
SIZE=$(stat -f%z "${RAW}" 2>/dev/null || stat -c%s "${RAW}")
OFFSET=$(( SIZE / 2 ))
printf '\xFF' | dd of="${RAW}" bs=1 seek="${OFFSET}" count=1 conv=notrunc 2>/dev/null
mc_cmd cp - "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" < "${RAW}" >/dev/null 2>&1 || \
  mc_cmd pipe "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" < "${RAW}" >/dev/null

HTTP_CODE="$(engine_curl -o /tmp/tampered_response -w '%{http_code}' "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" || true)"
GOT_BYTES="$(wc -c < /tmp/tampered_response | tr -d ' ')"

if [[ "${HTTP_CODE}" =~ ^[45] ]]; then
  STATUS="PASS"
  BODY="Tampered ciphertext rejected with HTTP ${HTTP_CODE}."
elif [[ "${HTTP_CODE}" == "200" ]]; then
  STATUS="FAIL"
  BODY="Tampered object returned HTTP 200 with ${GOT_BYTES} bytes — no auth tag check (consistent with Check 1: encryption unwired, so no GCM tag exists to reject)."
else
  STATUS="FAIL"
  BODY="Unexpected HTTP ${HTTP_CODE}."
fi

report_section "Check 3 — GCM tamper rejection" "${STATUS}" "${BODY}"
echo "Check 3: ${STATUS}"
