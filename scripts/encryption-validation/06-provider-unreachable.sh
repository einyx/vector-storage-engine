#!/usr/bin/env bash
# scripts/encryption-validation/06-provider-unreachable.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test stop warp-storage-engine >/dev/null

BAD_ENV="$(mktemp)"
grep -v '^ENCRYPTION_LOCAL_MASTER_KEY=' .env.encryption-test > "${BAD_ENV}" || true
echo "ENCRYPTION_LOCAL_MASTER_KEY=not-a-valid-base64-key!!!" >> "${BAD_ENV}"

set +e
docker compose -f docker-compose.encryption-test.yml --env-file "${BAD_ENV}" up -d --force-recreate warp-storage-engine >/dev/null 2>&1
sleep 5

HEALTH="$(curl -sf -o /dev/null -w '%{http_code}' "${ENGINE_URL}/health" || echo 000)"
WRITE_CODE="$(echo -n test | engine_curl -X PUT --data-binary @- -o /dev/null -w '%{http_code}' "${ENGINE_URL}/${TEST_BUCKET}/check06/x" || echo 000)"
LOGS="$(docker logs --tail 30 enc-test-engine 2>&1)"
set -e

docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test up -d --force-recreate warp-storage-engine >/dev/null 2>&1
for i in $(seq 1 30); do
  if curl -sf "${ENGINE_URL}/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

if [[ "${HEALTH}" != "200" ]] || [[ "${WRITE_CODE}" =~ ^[45] ]]; then
  STATUS="PASS"
  BODY=$'Engine refused to operate with invalid master key. health='"${HEALTH}"$' write='"${WRITE_CODE}"$'.\n\nLog tail:\n```\n'"$(echo "${LOGS}" | tail -15)"$'\n```'
else
  STATUS="FAIL"
  BODY=$'Engine accepted writes (HTTP '"${WRITE_CODE}"$') with an invalid master key — fail-open.\n\nLog tail:\n```\n'"$(echo "${LOGS}" | tail -15)"$'\n```'
fi

report_section "Check 6 — Provider unreachable / invalid key (fail-closed)" "${STATUS}" "${BODY}"
echo "Check 6: ${STATUS}"
