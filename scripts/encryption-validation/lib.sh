#!/usr/bin/env bash
# scripts/encryption-validation/lib.sh
set -euo pipefail

REPORT_FILE="${REPORT_FILE:-docs/superpowers/findings/2026-05-07-encryption-validation-report.md}"
ENGINE_URL="${ENGINE_URL:-http://localhost:8090}"
MINIO_URL="${MINIO_URL:-http://localhost:9100}"
MINIO_ALIAS="enctest"
TEST_BUCKET="enc-test-bucket"

report_section() {
  local title="$1"; local status="$2"; local body="$3"
  {
    echo ""
    echo "## ${title}"
    echo ""
    echo "**Status:** ${status}"
    echo ""
    echo "${body}"
  } >> "${REPORT_FILE}"
}

mc_cmd() {
  # Run mc inside the compose network so it can reach the minio service by name.
  docker run --rm -i \
    --network "${COMPOSE_NETWORK:-encryption-metrics_default}" \
    -e MC_HOST_${MINIO_ALIAS}=http://minioadmin:minioadmin@minio:9000 \
    minio/mc:latest "$@"
}

engine_curl() {
  curl -sS -u admin:secret "$@"
}
