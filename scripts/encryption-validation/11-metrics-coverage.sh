#!/usr/bin/env bash
# scripts/encryption-validation/11-metrics-coverage.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

N=10
for i in $(seq 1 ${N}); do
  echo -n "metric-coverage-${i}-$(date +%s%N)" \
    | engine_curl -X PUT --data-binary @- \
        "${ENGINE_URL}/${TEST_BUCKET}/check11/obj-${i}" >/dev/null
done
for i in $(seq 1 ${N}); do
  engine_curl "${ENGINE_URL}/${TEST_BUCKET}/check11/obj-${i}" >/dev/null
done

METRICS="$(curl -sf "${ENGINE_URL}/metrics" || true)"
if [[ -z "${METRICS}" ]]; then
  STATUS="FAIL"
  BODY="No /metrics endpoint output."
  report_section "Check 11 — Metrics coverage" "${STATUS}" "${BODY}"
  echo "Check 11: ${STATUS}"
  exit 0
fi

extract_value() {
  local pattern="$1"
  echo "${METRICS}" | awk -v pat="${pattern}" '$0 ~ pat { print $NF; exit }'
}

OBJ_ENC="$(extract_value 'encryption_object_ops_total\{op="encrypt",status="success"\}')"
OBJ_DEC="$(extract_value 'encryption_object_ops_total\{op="decrypt",status="success"\}')"
OBJ_ENC_BYTES="$(extract_value 'encryption_object_bytes_total\{op="encrypt"\}')"
KMS_GEN="$(extract_value 'kms_operations_total\{operation="generate_data_key",status="success"\}')"
KMS_DEC="$(extract_value 'kms_operations_total\{operation="decrypt",status="success"\}')"
KMS_HIT="$(extract_value 'kms_cache_hits_total\{cache_type="dek"\}')"
KMS_MISS="$(extract_value 'kms_cache_misses_total\{cache_type="dek"\}')"

# Required (object-layer metrics — must advance regardless of provider):
FAILED=()
[[ -n "${OBJ_ENC}" && "${OBJ_ENC%.*}" -ge "${N}" ]] || FAILED+=("encryption_object_ops_total{encrypt,success} (${OBJ_ENC:-missing})")
[[ -n "${OBJ_DEC}" && "${OBJ_DEC%.*}" -ge "${N}" ]] || FAILED+=("encryption_object_ops_total{decrypt,success} (${OBJ_DEC:-missing})")
[[ -n "${OBJ_ENC_BYTES}" && "${OBJ_ENC_BYTES%.*}" -gt 0 ]] || FAILED+=("encryption_object_bytes_total{encrypt} (${OBJ_ENC_BYTES:-missing})")

# Informational (KMS-layer metrics — advance only when a KMS-backed provider is
# in use; the local key provider builds its key chain outside kms.NewProvider
# so these stay at zero in the local-only validation env). Tracked in the body
# but not enforced.
KMS_NOTE=""
if [[ -z "${KMS_GEN}${KMS_DEC}${KMS_HIT}${KMS_MISS}" ]]; then
  KMS_NOTE=" KMS-layer metrics absent (expected with the local key provider)."
fi

if [[ ${#FAILED[@]} -eq 0 ]]; then
  STATUS="PASS"
  BODY="Object-layer metrics advanced for ${N} encrypts + ${N} decrypts. Sample values: obj_enc=${OBJ_ENC}, obj_dec=${OBJ_DEC}, obj_bytes=${OBJ_ENC_BYTES}, kms_gen=${KMS_GEN:-0}, kms_dec=${KMS_DEC:-0}, hits=${KMS_HIT:-0}, misses=${KMS_MISS:-0}.${KMS_NOTE}"
else
  STATUS="FAIL"
  BODY="Missing or insufficient metrics:"$'\n'"$(printf -- '- %s\n' "${FAILED[@]}")"
fi

report_section "Check 11 — Metrics coverage" "${STATUS}" "${BODY}"
echo "Check 11: ${STATUS}"
