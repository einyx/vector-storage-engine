#!/usr/bin/env bash
# scripts/encryption-validation/08-metrics.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

for i in $(seq 1 5); do
  echo -n "metric-$i" | engine_curl -X PUT --data-binary @- \
    "${ENGINE_URL}/${TEST_BUCKET}/check08/obj-$i" >/dev/null
  engine_curl "${ENGINE_URL}/${TEST_BUCKET}/check08/obj-$i" >/dev/null
done

METRICS="$(curl -sf "${ENGINE_URL}/metrics" || echo '')"
if [[ -z "${METRICS}" ]]; then
  STATUS="WARN"
  BODY="No /metrics endpoint reachable. Hardening candidate: standardize Prometheus metrics exposure."
  report_section "Check 8 — Metrics surface" "${STATUS}" "${BODY}"
  echo "Check 8: ${STATUS}"
  exit 0
fi

ENC_METRICS="$(echo "${METRICS}" | grep -iE '^(# HELP |[a-z_]*)(encrypt|decrypt|kms|dek|key_provider)' | head -50 || true)"

if [[ -n "${ENC_METRICS}" ]]; then
  STATUS="PASS"
  BODY=$'Encryption-related metrics found:\n\n```\n'"${ENC_METRICS}"$'\n```'
else
  STATUS="WARN"
  BODY="No encryption-related Prometheus metrics found. Hardening candidate: emit counters for encrypt/decrypt ops, DEK cache hit/miss, KMS calls, errors."
fi

report_section "Check 8 — Metrics surface" "${STATUS}" "${BODY}"
echo "Check 8: ${STATUS}"
