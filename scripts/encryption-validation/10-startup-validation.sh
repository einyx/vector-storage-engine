#!/usr/bin/env bash
# scripts/encryption-validation/10-startup-validation.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test stop warp-storage-engine >/dev/null

NO_KEY_ENV="$(mktemp)"
grep -v '^ENCRYPTION_LOCAL_MASTER_KEY=' .env.encryption-test > "${NO_KEY_ENV}" || true
echo "ENCRYPTION_LOCAL_MASTER_KEY=" >> "${NO_KEY_ENV}"

set +e
docker compose -f docker-compose.encryption-test.yml --env-file "${NO_KEY_ENV}" up -d --force-recreate warp-storage-engine >/dev/null 2>&1
sleep 5

CONTAINER_STATE="$(docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}}' enc-test-engine 2>/dev/null || echo 'missing')"
HEALTH="$(curl -sf -o /dev/null -w '%{http_code}' "${ENGINE_URL}/health" || echo 000)"
LOGS="$(docker logs --tail 30 enc-test-engine 2>&1)"
set -e

docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test up -d --force-recreate warp-storage-engine >/dev/null 2>&1
for i in $(seq 1 30); do
  if curl -sf "${ENGINE_URL}/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

if [[ "${HEALTH}" != "200" ]]; then
  STATUS="PASS"
  BODY=$'Engine did not become healthy with empty master key. State: '"${CONTAINER_STATE}"$'.\n\nLog tail:\n```\n'"$(echo "${LOGS}" | tail -15)"$'\n```'
else
  STATUS="FAIL"
  BODY=$'Engine reached healthy state with empty master key — startup validation gap.\n\nLog tail:\n```\n'"$(echo "${LOGS}" | tail -15)"$'\n```'
fi

report_section "Check 10 — Startup key validation" "${STATUS}" "${BODY}"
echo "Check 10: ${STATUS}"
