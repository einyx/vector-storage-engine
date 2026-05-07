# Encryption Validation (Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a local-only test stack (no AWS) and produce an evidence-backed validation report covering 10 operational behaviors of the encryption subsystem.

**Architecture:** A new `docker-compose.encryption-test.yml` runs MinIO + the engine configured for the `local` key provider with a generated 32-byte master key. A scripted check suite (`scripts/encryption-validation/`) drives the engine via S3-compatible API and inspects raw MinIO storage. Each check appends to a markdown findings report. No code changes to `internal/encryption` or `internal/kms` in this plan — observation only.

**Tech Stack:** Go (existing engine), Docker Compose, MinIO + `mc` CLI, `curl`/`aws s3 cli`, bash, `xxd`.

---

## File Structure

- Create: `docker-compose.encryption-test.yml` — test stack (MinIO + engine, local key provider)
- Create: `config.encryption-test.yaml` — engine config for the test stack
- Create: `scripts/encryption-validation/` — directory holding one script per check
  - `00-setup.sh` — generate master key, bring stack up, create test bucket
  - `01-ciphertext-on-disk.sh`
  - `02-roundtrip.sh`
  - `03-tamper-rejection.sh`
  - `04-restart.sh`
  - `05-concurrent-writes.sh`
  - `06-provider-unreachable.sh`
  - `07-log-leakage.sh`
  - `08-metrics.sh`
  - `09-rotation.sh`
  - `10-startup-validation.sh`
  - `lib.sh` — shared helpers (assertions, append-to-report)
  - `run-all.sh` — runs every check in order
- Create: `docs/superpowers/findings/2026-05-07-encryption-validation-report.md` — populated incrementally by checks

---

## Task 1: Bootstrap the test stack

**Files:**
- Create: `docker-compose.encryption-test.yml`
- Create: `config.encryption-test.yaml`
- Create: `scripts/encryption-validation/lib.sh`
- Create: `scripts/encryption-validation/00-setup.sh`
- Create: `docs/superpowers/findings/2026-05-07-encryption-validation-report.md`

- [ ] **Step 1: Create the compose file**

```yaml
# docker-compose.encryption-test.yml
services:
  minio:
    image: minio/minio:latest
    container_name: enc-test-minio
    ports:
      - "9100:9000"
      - "9101:9001"
    environment:
      - MINIO_ROOT_USER=minioadmin
      - MINIO_ROOT_PASSWORD=minioadmin
    command: server /data --console-address ":9001"
    volumes:
      - minio-data:/data

  warp-storage-engine:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: enc-test-engine
    depends_on:
      - minio
    ports:
      - "8090:8080"
    environment:
      - STORAGE_PROVIDER=s3
      - S3_ENDPOINT=http://minio:9000
      - S3_ACCESS_KEY=minioadmin
      - S3_SECRET_KEY=minioadmin
      - S3_REGION=us-east-1
      - S3_PATH_STYLE=true
      - AUTH_TYPE=basic
      - AUTH_IDENTITY=admin
      - AUTH_CREDENTIAL=secret
      - ENCRYPTION_ENABLED=true
      - ENCRYPTION_KEY_PROVIDER=local
      - ENCRYPTION_ALGORITHM=AES-256-GCM
      - ENCRYPTION_LOCAL_MASTER_KEY=${ENCRYPTION_LOCAL_MASTER_KEY}
      - LOG_LEVEL=debug
      - LOG_FORMAT=json
    volumes:
      - ./config.encryption-test.yaml:/etc/warp-storage-engine/warp-storage-engine.yaml:ro
    command:
      - "--config"
      - "/etc/warp-storage-engine/warp-storage-engine.yaml"

volumes:
  minio-data:
```

- [ ] **Step 2: Create the engine config**

```yaml
# config.encryption-test.yaml
server:
  listen: ":8080"

storage:
  provider: s3

encryption:
  enabled: true
  algorithm: AES-256-GCM
  key_provider: local
```

- [ ] **Step 3: Create `lib.sh` with shared helpers**

```bash
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
  docker run --rm \
    --network warp-storage-engine_default \
    -e MC_HOST_${MINIO_ALIAS}=http://minioadmin:minioadmin@minio:9000 \
    minio/mc:latest "$@"
}

engine_curl() {
  curl -sS -u admin:secret "$@"
}
```

- [ ] **Step 4: Create `00-setup.sh`**

```bash
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
```

- [ ] **Step 5: Create the report skeleton**

```markdown
# Encryption Validation Report (Phase 1)

**Date:** 2026-05-07
**Spec:** docs/superpowers/specs/2026-05-07-encryption-audit-and-hardening-design.md
**Stack:** docker-compose.encryption-test.yml (MinIO + engine, local key provider)

## Summary

| # | Check | Status |
|---|-------|--------|
| 1 | Ciphertext on disk | pending |
| 2 | Round-trip | pending |
| 3 | GCM tamper rejection | pending |
| 4 | Restart behavior | pending |
| 5 | Concurrent writes / nonce uniqueness | pending |
| 6 | Provider unreachable (fail-closed) | pending |
| 7 | Log / key material leakage | pending |
| 8 | Metrics surface | pending |
| 9 | Key rotation API | pending |
| 10 | Startup key validation | pending |
```

- [ ] **Step 6: Make scripts executable and run setup**

```bash
chmod +x scripts/encryption-validation/*.sh
bash scripts/encryption-validation/00-setup.sh
```

Expected: `setup complete` and `docker ps` shows `enc-test-minio` and `enc-test-engine` healthy.

- [ ] **Step 7: Commit**

```bash
git add docker-compose.encryption-test.yml config.encryption-test.yaml scripts/encryption-validation/ docs/superpowers/findings/2026-05-07-encryption-validation-report.md .gitignore
echo ".env.encryption-test" >> .gitignore
git add .gitignore
git commit -m "test(encryption): bootstrap local validation stack"
```

---

## Task 2: Check 1 — Ciphertext on disk

**Files:**
- Create: `scripts/encryption-validation/01-ciphertext-on-disk.sh`

- [ ] **Step 1: Write the check script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/01-ciphertext-on-disk.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

PLAINTEXT="WARP-ENCRYPTION-CANARY-$(date +%s)-$RANDOM"
KEY="check01/canary.txt"

# Write through the engine
echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

# Read raw bytes directly from MinIO (bypassing engine)
RAW_FILE="$(mktemp)"
mc_cmd cp "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" "/tmp/raw.bin" >/dev/null
docker cp enc-test-minio:/tmp/raw.bin "${RAW_FILE}" 2>/dev/null || \
  mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" > "${RAW_FILE}"

# Assert plaintext does NOT appear in raw bytes
if grep -q "${PLAINTEXT}" "${RAW_FILE}"; then
  STATUS="FAIL"
  BODY="Plaintext canary \`${PLAINTEXT}\` found in raw MinIO object — encryption did NOT happen."
else
  STATUS="PASS"
  HEX_HEAD="$(xxd -l 64 "${RAW_FILE}" | sed 's/^/    /')"
  BODY=$'Plaintext canary not present in raw object. First 64 bytes:\n\n```\n'"${HEX_HEAD}"$'\n```'
fi

report_section "Check 1 — Ciphertext on disk" "${STATUS}" "${BODY}"
echo "Check 1: ${STATUS}"
```

- [ ] **Step 2: Run it**

```bash
bash scripts/encryption-validation/01-ciphertext-on-disk.sh
```

Expected: prints `Check 1: PASS` or `Check 1: FAIL`. Either is acceptable — the goal is evidence, not pass.

- [ ] **Step 3: Update summary table**

Edit the summary table in the report to reflect the actual result.

- [ ] **Step 4: Commit**

```bash
git add scripts/encryption-validation/01-ciphertext-on-disk.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 1 - ciphertext on disk"
```

---

## Task 3: Check 2 — Round-trip

**Files:**
- Create: `scripts/encryption-validation/02-roundtrip.sh`

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/02-roundtrip.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

PLAINTEXT="hello-roundtrip-$(date +%s)"
KEY="check02/rt.txt"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

GOT="$(engine_curl "${ENGINE_URL}/${TEST_BUCKET}/${KEY}")"

if [[ "${GOT}" == "${PLAINTEXT}" ]]; then
  STATUS="PASS"
  BODY="Wrote ${#PLAINTEXT} bytes; read back identical bytes."
else
  STATUS="FAIL"
  BODY=$'Mismatch.\n\nExpected:\n```\n'"${PLAINTEXT}"$'\n```\n\nGot:\n```\n'"${GOT}"$'\n```'
fi

report_section "Check 2 — Round-trip" "${STATUS}" "${BODY}"
echo "Check 2: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/02-roundtrip.sh
git add scripts/encryption-validation/02-roundtrip.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 2 - round-trip"
```

---

## Task 4: Check 3 — GCM tamper rejection

**Files:**
- Create: `scripts/encryption-validation/03-tamper-rejection.sh`

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/03-tamper-rejection.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

KEY="check03/tamper.bin"
PLAINTEXT="$(head -c 4096 /dev/urandom | base64)"

echo -n "${PLAINTEXT}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}"

# Pull raw, flip a byte in the middle, push raw back
RAW="$(mktemp)"
mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" > "${RAW}"
SIZE=$(stat -f%z "${RAW}" 2>/dev/null || stat -c%s "${RAW}")
OFFSET=$(( SIZE / 2 ))
printf '\xFF' | dd of="${RAW}" bs=1 seek="${OFFSET}" count=1 conv=notrunc 2>/dev/null
mc_cmd cp "${RAW}" "${MINIO_ALIAS}/${TEST_BUCKET}/${KEY}" >/dev/null

# Read through engine; expect failure
HTTP_CODE="$(engine_curl -o /tmp/tampered_response -w '%{http_code}' "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" || true)"

if [[ "${HTTP_CODE}" =~ ^[45] ]]; then
  STATUS="PASS"
  BODY="Tampered ciphertext rejected with HTTP ${HTTP_CODE}."
else
  STATUS="FAIL"
  BODY="Tampered ciphertext returned HTTP ${HTTP_CODE} — auth tag did not reject. Response head: $(head -c 200 /tmp/tampered_response | xxd)"
fi

report_section "Check 3 — GCM tamper rejection" "${STATUS}" "${BODY}"
echo "Check 3: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/03-tamper-rejection.sh
git add scripts/encryption-validation/03-tamper-rejection.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 3 - tamper rejection"
```

---

## Task 5: Check 4 — Restart behavior

**Files:**
- Create: `scripts/encryption-validation/04-restart.sh`

- [ ] **Step 1: Write the script**

```bash
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
  restart warp-storage-engine

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
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/04-restart.sh
git add scripts/encryption-validation/04-restart.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 4 - restart behavior"
```

---

## Task 6: Check 5 — Concurrent writes / nonce uniqueness

**Files:**
- Create: `scripts/encryption-validation/05-concurrent-writes.sh`

- [ ] **Step 1: Write the script**

```bash
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

# Pull raw bytes for each, sample first 32 bytes (likely contains a header/nonce),
# count unique. Real nonce extraction depends on envelope layout — we approximate.
TMPDIR="$(mktemp -d)"
for i in $(seq 1 ${N}); do
  mc_cmd cat "${MINIO_ALIAS}/${TEST_BUCKET}/check05/obj-${i}.txt" \
    | head -c 64 | xxd -p -c 64 > "${TMPDIR}/h-${i}.hex" || true
done

UNIQUE=$(cat "${TMPDIR}"/h-*.hex | sort -u | wc -l | tr -d ' ')

if [[ "${UNIQUE}" == "${N}" ]]; then
  STATUS="PASS"
  BODY="${N} concurrent writes produced ${UNIQUE} distinct first-64-byte headers (proxy for unique nonces / envelope randomness)."
else
  STATUS="WARN"
  BODY="${N} concurrent writes produced only ${UNIQUE} distinct headers. May indicate nonce reuse OR identical envelope metadata layout — manual review of envelope format required."
fi

report_section "Check 5 — Concurrent writes / nonce uniqueness" "${STATUS}" "${BODY}"
echo "Check 5: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/05-concurrent-writes.sh
git add scripts/encryption-validation/05-concurrent-writes.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 5 - concurrent writes"
```

---

## Task 7: Check 6 — Provider unreachable (fail-closed)

**Files:**
- Create: `scripts/encryption-validation/06-provider-unreachable.sh`

Note: the `local` provider has no network dependency, so the closest analog
is to start the engine with a malformed/missing master key and confirm it
refuses to accept writes rather than silently writing plaintext. A second
sub-check uses an empty master key.

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/06-provider-unreachable.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

# Stop the running engine; restart with a deliberately bad master key
docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test stop warp-storage-engine

# Override master key to something invalid (not base64 / wrong length)
BAD_ENV="$(mktemp)"
cp .env.encryption-test "${BAD_ENV}"
echo "ENCRYPTION_LOCAL_MASTER_KEY=not-a-valid-base64-key!!!" >> "${BAD_ENV}"

set +e
docker compose -f docker-compose.encryption-test.yml --env-file "${BAD_ENV}" up -d warp-storage-engine
sleep 5

# Engine should either fail to start, or refuse writes
HEALTH="$(curl -sf -o /dev/null -w '%{http_code}' "${ENGINE_URL}/health" || echo 000)"
WRITE_CODE="$(echo -n test | engine_curl -X PUT --data-binary @- -o /dev/null -w '%{http_code}' "${ENGINE_URL}/${TEST_BUCKET}/check06/x" || echo 000)"
LOGS="$(docker logs --tail 50 enc-test-engine 2>&1)"
set -e

# Restore good config for subsequent checks
docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test up -d warp-storage-engine
for i in $(seq 1 30); do
  if curl -sf "${ENGINE_URL}/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

if [[ "${HEALTH}" != "200" ]] || [[ "${WRITE_CODE}" =~ ^[45] ]]; then
  STATUS="PASS"
  BODY="Engine refused to operate with invalid master key. health=${HEALTH} write=${WRITE_CODE}. Log tail:\n\n\`\`\`\n$(echo "${LOGS}" | tail -20)\n\`\`\`"
else
  STATUS="FAIL"
  BODY="Engine accepted writes (HTTP ${WRITE_CODE}) with an invalid master key — possible fail-open. Log tail:\n\n\`\`\`\n$(echo "${LOGS}" | tail -20)\n\`\`\`"
fi

report_section "Check 6 — Provider unreachable / invalid key (fail-closed)" "${STATUS}" "${BODY}"
echo "Check 6: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/06-provider-unreachable.sh
git add scripts/encryption-validation/06-provider-unreachable.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 6 - provider unreachable"
```

---

## Task 8: Check 7 — Log / key material leakage

**Files:**
- Create: `scripts/encryption-validation/07-log-leakage.sh`

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/07-log-leakage.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

CANARY="LEAKCHECK-$(date +%s%N)"
KEY="check07/leak.txt"

# Source master key for grep
MASTER_KEY="$(grep '^ENCRYPTION_LOCAL_MASTER_KEY=' .env.encryption-test | cut -d= -f2-)"

echo -n "${CANARY}" | engine_curl -X PUT --data-binary @- \
  "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" >/dev/null
engine_curl "${ENGINE_URL}/${TEST_BUCKET}/${KEY}" >/dev/null

LOGS="$(docker logs enc-test-engine 2>&1)"

CANARY_HITS=$(echo "${LOGS}" | grep -c "${CANARY}" || true)
KEY_HITS=$(echo "${LOGS}" | grep -F -c "${MASTER_KEY}" || true)

# Heuristic: long base64 strings in logs (potential DEK leakage)
LONG_B64=$(echo "${LOGS}" | grep -oE '[A-Za-z0-9+/]{40,}=*' | sort -u | head -10 || true)

if [[ "${CANARY_HITS}" -eq 0 && "${KEY_HITS}" -eq 0 ]]; then
  STATUS="PASS"
  BODY="No plaintext canary or master key bytes in logs. Sample long base64 strings (review manually for DEK material):\n\n\`\`\`\n${LONG_B64:-(none)}\n\`\`\`"
else
  STATUS="FAIL"
  BODY="canary_hits=${CANARY_HITS} master_key_hits=${KEY_HITS} — secrets are appearing in logs."
fi

report_section "Check 7 — Log / key material leakage" "${STATUS}" "${BODY}"
echo "Check 7: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/07-log-leakage.sh
git add scripts/encryption-validation/07-log-leakage.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 7 - log leakage"
```

---

## Task 9: Check 8 — Metrics surface

**Files:**
- Create: `scripts/encryption-validation/08-metrics.sh`

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/08-metrics.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

# Generate some encryption activity
for i in $(seq 1 5); do
  echo -n "metric-$i" | engine_curl -X PUT --data-binary @- \
    "${ENGINE_URL}/${TEST_BUCKET}/check08/obj-$i" >/dev/null
  engine_curl "${ENGINE_URL}/${TEST_BUCKET}/check08/obj-$i" >/dev/null
done

METRICS="$(curl -sf "${ENGINE_URL}/metrics" || echo '')"
if [[ -z "${METRICS}" ]]; then
  STATUS="WARN"
  BODY="No /metrics endpoint reachable. Document for hardening: standardize Prometheus metrics exposure."
  report_section "Check 8 — Metrics surface" "${STATUS}" "${BODY}"
  echo "Check 8: ${STATUS}"
  exit 0
fi

ENC_METRICS=$(echo "${METRICS}" | grep -iE '^(# HELP |[a-z_]*)(encrypt|decrypt|kms|dek|key_provider)' | head -50)

if [[ -n "${ENC_METRICS}" ]]; then
  STATUS="PASS"
  BODY="Encryption-related metrics found:\n\n\`\`\`\n${ENC_METRICS}\n\`\`\`"
else
  STATUS="WARN"
  BODY="No encryption-related Prometheus metrics found. Hardening candidate: emit counters for encrypt/decrypt ops, DEK cache hit/miss, KMS calls, errors."
fi

report_section "Check 8 — Metrics surface" "${STATUS}" "${BODY}"
echo "Check 8: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/08-metrics.sh
git add scripts/encryption-validation/08-metrics.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 8 - metrics surface"
```

---

## Task 10: Check 9 — Key rotation API exploration

**Files:**
- Create: `scripts/encryption-validation/09-rotation.sh`

This check is partly a code search: there may be no rotation API. Document what exists.

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/09-rotation.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

# Search the codebase for any rotation surface
HITS="$(grep -rniE 'rotat(e|ion)|rewrap|reencrypt' internal/encryption internal/kms api 2>/dev/null \
  | grep -v _test.go | head -40 || true)"

# Look for an HTTP/admin endpoint
ENDPOINT_HITS="$(grep -rniE 'rotat|rewrap' cmd internal/transport internal/middleware 2>/dev/null \
  | grep -v _test.go | head -20 || true)"

if [[ -z "${HITS}${ENDPOINT_HITS}" ]]; then
  STATUS="GAP"
  BODY="No rotation, rewrap, or re-encrypt symbols found in source. Treat as Phase 2 hardening gap: design and implement key rotation flow."
else
  STATUS="REVIEW"
  BODY=$'Rotation-related symbols found — manual review needed to determine whether they implement online rotation, offline rewrap, or are placeholders.\n\nSource hits:\n```\n'"${HITS}"$'\n```\n\nEndpoint hits:\n```\n'"${ENDPOINT_HITS:-(none)}"$'\n```'
fi

report_section "Check 9 — Key rotation API" "${STATUS}" "${BODY}"
echo "Check 9: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/09-rotation.sh
git add scripts/encryption-validation/09-rotation.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 9 - rotation surface"
```

---

## Task 11: Check 10 — Startup key validation

**Files:**
- Create: `scripts/encryption-validation/10-startup-validation.sh`

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/10-startup-validation.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test stop warp-storage-engine

# Boot with NO master key set
NO_KEY_ENV="$(mktemp)"
grep -v '^ENCRYPTION_LOCAL_MASTER_KEY=' .env.encryption-test > "${NO_KEY_ENV}"
echo "ENCRYPTION_LOCAL_MASTER_KEY=" >> "${NO_KEY_ENV}"

set +e
docker compose -f docker-compose.encryption-test.yml --env-file "${NO_KEY_ENV}" up -d warp-storage-engine
sleep 5

CONTAINER_STATE="$(docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}}' enc-test-engine)"
HEALTH="$(curl -sf -o /dev/null -w '%{http_code}' "${ENGINE_URL}/health" || echo 000)"
LOGS="$(docker logs --tail 30 enc-test-engine 2>&1)"
set -e

# Restore good config
docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test up -d warp-storage-engine
for i in $(seq 1 30); do
  if curl -sf "${ENGINE_URL}/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

# Pass if engine refused to start OR refused to be healthy
if [[ "${HEALTH}" != "200" ]]; then
  STATUS="PASS"
  BODY=$'Engine did not become healthy with empty master key. State: '"${CONTAINER_STATE}"$'. Log tail:\n\n```\n'"$(echo "${LOGS}" | tail -20)"$'\n```'
else
  STATUS="FAIL"
  BODY="Engine reached healthy state with empty master key — startup validation gap."
fi

report_section "Check 10 — Startup key validation" "${STATUS}" "${BODY}"
echo "Check 10: ${STATUS}"
```

- [ ] **Step 2: Run, update summary, commit**

```bash
bash scripts/encryption-validation/10-startup-validation.sh
git add scripts/encryption-validation/10-startup-validation.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): check 10 - startup validation"
```

---

## Task 12: Compose `run-all.sh` and finalize report

**Files:**
- Create: `scripts/encryption-validation/run-all.sh`
- Modify: `docs/superpowers/findings/2026-05-07-encryption-validation-report.md`

- [ ] **Step 1: Write the runner**

```bash
#!/usr/bin/env bash
# scripts/encryption-validation/run-all.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
bash "${SCRIPT_DIR}/00-setup.sh"
for n in 01 02 03 04 05 06 07 08 09 10; do
  echo "=== running check ${n} ==="
  bash "${SCRIPT_DIR}/${n}-"*.sh
done
echo "all checks complete"
```

- [ ] **Step 2: Add the conclusions section to the report**

After the per-check sections, append:

```markdown
## Conclusions

### Confirmed working
(list checks that PASSed with one-line evidence reference)

### Gaps identified
(list checks that FAILed / WARNed / produced GAP, with the implication for Phase 2 hardening)

### Phase 2 hardening candidates
Bullet list of concrete topics derived from this run, each one trace-able to a check above. This becomes the input to the Phase 2 spec.
```

Fill in by hand from the actual check results.

- [ ] **Step 3: Tear down the test stack**

```bash
docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test down -v
```

- [ ] **Step 4: Final commit**

```bash
git add scripts/encryption-validation/run-all.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "docs(encryption): finalize Phase 1 validation report"
```

---

## Out of Scope (Phase 2)

Phase 2 will be a separate spec + plan written **after** this report exists, scoped by the actual gaps found. Likely areas:

- Key rotation / rewrap flow
- DEK cache TTL, eviction, memory zeroization
- KMS failure mode policy (fail-closed semantics, retry/backoff)
- Audit logging for key operations
- DEK scoping / blast radius reduction
- Mandatory startup validation
- Standard encryption metric set
