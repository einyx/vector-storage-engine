#!/usr/bin/env bash
# scripts/encryption-validation/00-setup.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

# Generate a fresh 32-byte base64 master key for this run
ENCRYPTION_LOCAL_MASTER_KEY="$(openssl rand -base64 32)"
export ENCRYPTION_LOCAL_MASTER_KEY
echo "ENCRYPTION_LOCAL_MASTER_KEY=${ENCRYPTION_LOCAL_MASTER_KEY}" > .env.encryption-test

# Bring the stack up
docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test up -d --build

# Wait for engine health
for i in $(seq 1 30); do
  if curl -sf "${ENGINE_URL}/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

# Create test bucket directly in MinIO (bypassing the engine, so we control the layout)
mc_cmd mb --ignore-existing "${MINIO_ALIAS}/${TEST_BUCKET}"
echo "setup complete"
