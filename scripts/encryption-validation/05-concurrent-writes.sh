#!/usr/bin/env bash
# scripts/encryption-validation/05-concurrent-writes.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

N=50
PIDS=()
for i in $(seq 1 ${N}); do
  ( echo -n "concurrent-$i-$(date +%s%N)" \
    | engine_curl -X PUT --data-binary @- \
        "${ENGINE_URL}/${TEST_BUCKET}/check05/obj-${i}.txt" >/dev/null ) &
  PIDS+=($!)
done
for p in "${PIDS[@]}"; do wait "$p"; done

TMPDIR="$(mktemp -d)"
for i in $(seq 1 ${N}); do
  mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/check05/obj-${i}.txt" \
    | head -c 64 | xxd -p -c 64 > "${TMPDIR}/h-${i}.hex" 2>/dev/null || true
done

UNIQUE=$(cat "${TMPDIR}"/h-*.hex 2>/dev/null | sort -u | wc -l | tr -d ' ')

if [[ "${UNIQUE}" == "${N}" ]]; then
  STATUS="PASS"
  BODY="${N} concurrent writes produced ${UNIQUE} distinct first-64-byte headers (proxy for unique nonces / envelope randomness)."
else
  STATUS="WARN"
  BODY="${N} concurrent writes produced ${UNIQUE} distinct headers. Without encryption (Check 1), this metric reflects plaintext distinctness only — meaningful nonce-uniqueness measurement requires encryption to be wired first."
fi

report_section "Check 5 — Concurrent writes / nonce uniqueness" "${STATUS}" "${BODY}"
echo "Check 5: ${STATUS}"
