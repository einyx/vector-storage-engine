#!/usr/bin/env bash
# scripts/encryption-validation/04-restart.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

PLAINTEXT="restart-canary-$(date +%s)"
KEY="check04/restart.txt"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test \
  restart warp-storage-engine >/dev/null

for i in $(seq 1 30); do
  if curl -sf "${ENGINE_URL}/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

GOT="$(engine_curl "${ENGINE_URL}/${TEST_BUCKET}/${KEY}")"

if [[ "${GOT}" == "${PLAINTEXT}" ]]; then
  STATUS="PASS"
  BODY="Object written before restart is readable after restart with same master key."
else
  STATUS="FAIL"
  BODY="Post-restart read mismatch. Got: ${GOT}"
fi

report_section "Check 4 — Restart behavior" "${STATUS}" "${BODY}"
echo "Check 4: ${STATUS}"
