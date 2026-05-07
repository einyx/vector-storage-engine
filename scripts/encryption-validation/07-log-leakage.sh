#!/usr/bin/env bash
# scripts/encryption-validation/07-log-leakage.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

CANARY="LEAKCHECK-$(date +%s%N)"
KEY="check07/leak.txt"

MASTER_KEY="$(grep '^ENCRYPTION_LOCAL_MASTER_KEY=' .env.encryption-test | cut -d= -f2-)"

echo -n "${CANARY}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" >/dev/null
engine_curl "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" >/dev/null

LOGS="$(docker logs enc-test-engine 2>&1)"

CANARY_HITS=$(echo "${LOGS}" | grep -c "${CANARY}" || true)
KEY_HITS=$(echo "${LOGS}" | grep -F -c "${MASTER_KEY}" || true)

LONG_B64="$(echo "${LOGS}" | grep -oE '[A-Za-z0-9+/]{40,}=*' | sort -u | head -10 || true)"

if [[ "${CANARY_HITS}" -eq 0 && "${KEY_HITS}" -eq 0 ]]; then
  STATUS="PASS"
  BODY=$'No plaintext canary or master key bytes in logs. Sample long base64 strings (review manually for DEK material):\n\n```\n'"${LONG_B64:-(none)}"$'\n```'
else
  STATUS="FAIL"
  BODY="canary_hits=${CANARY_HITS} master_key_hits=${KEY_HITS} — secrets are appearing in logs."
fi

report_section "Check 7 — Log / key material leakage" "${STATUS}" "${BODY}"
echo "Check 7: ${STATUS}"
