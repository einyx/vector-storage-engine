#!/usr/bin/env bash
# scripts/encryption-validation/03-tamper-rejection.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

KEY="check03/tamper.bin"
PLAINTEXT="$(head -c 4096 /dev/urandom | base64)"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

# Pull raw ciphertext + flip a byte at the midpoint.
RAW="$(mktemp)"
mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" > "${RAW}"
SIZE=$(stat -f%z "${RAW}" 2>/dev/null || stat -c%s "${RAW}")
OFFSET=$(( SIZE / 2 ))
printf '\xFF' | dd of="${RAW}" bs=1 seek="${OFFSET}" count=1 conv=notrunc 2>/dev/null

# Re-upload tampered bytes while preserving the encryption metadata. mc pipe
# replaces metadata entirely; mc cp --attr allows us to set it. Metadata names
# passed to --attr must not include the X-Amz-Meta- prefix (mc adds it).
ATTRS="$(mc_cmd stat --json "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" \
  | docker run --rm -i ghcr.io/jqlang/jq:latest -r '
      .metadata
      | to_entries
      | map(select(.key | test("^X-Amz-Meta-"; "i")))
      | map("\(.key | sub("^X-Amz-Meta-"; ""; "i"))=\(.value)")
      | join(";")
    ' 2>/dev/null || echo '')"

if [[ -z "${ATTRS}" ]]; then
  STATUS="WARN"
  BODY="Could not extract object metadata; tamper test inconclusive."
  report_section "Check 3 — GCM tamper rejection" "${STATUS}" "${BODY}"
  echo "Check 3: ${STATUS}"
  exit 0
fi

# mc cp from the host, with the raw file mounted into the mc container.
docker run --rm -i \
  --network "${COMPOSE_NETWORK:-encryption-metrics_default}" \
  -e MC_HOST_${MINIO_ALIAS}=http://minioadmin:minioadmin@minio:9000 \
  -v "${RAW}:/tmp/raw.bin:ro" \
  minio/mc:latest cp --attr "${ATTRS}" /tmp/raw.bin "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" >/dev/null

HTTP_CODE="$(engine_curl -o /tmp/tampered_response -w '%{http_code}' "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" || true)"
GOT_BYTES="$(wc -c < /tmp/tampered_response | tr -d ' ')"

if [[ "${HTTP_CODE}" =~ ^[45] ]]; then
  STATUS="PASS"
  BODY="Tampered ciphertext rejected with HTTP ${HTTP_CODE}. AES-GCM auth tag verification works."
elif [[ "${GOT_BYTES}" -gt 0 ]] && ! cmp -s /tmp/tampered_response <(echo -n "${PLAINTEXT}"); then
  # Engine returned 200 but bytes do NOT match the original plaintext —
  # decrypt produced corrupted output rather than failing closed.
  STATUS="FAIL"
  BODY="Tampered object returned HTTP 200 with ${GOT_BYTES} bytes that do NOT match the original plaintext. Decrypt is producing corrupted output instead of rejecting (AES-GCM auth tag is not being verified, or the engine is returning ciphertext as-is)."
else
  STATUS="FAIL"
  BODY="Unexpected: HTTP ${HTTP_CODE}, ${GOT_BYTES} bytes, content matches plaintext."
fi

report_section "Check 3 — GCM tamper rejection" "${STATUS}" "${BODY}"
echo "Check 3: ${STATUS}"
