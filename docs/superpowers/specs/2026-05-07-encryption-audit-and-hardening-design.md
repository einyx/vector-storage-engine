# Encryption Audit & Operational Hardening — Design

**Date:** 2026-05-07
**Status:** Draft
**Scope:** `internal/encryption`, `internal/kms`

## Background

The storage engine has a full envelope-encryption stack (AES-256-GCM, DEK
caching, AWS / Azure / custom / local KMS providers) but it has never been
exercised end-to-end. The user has no AWS access, so all validation must
run against the `local` key provider, which goes through the same envelope
path as cloud KMS providers.

This work is operational hardening (not crypto-primitive review and not
compliance-driven). The premise: prove what actually works before
designing fixes for what doesn't.

## Goals

1. Establish a reproducible local validation harness for the encryption
   subsystem with no cloud dependencies.
2. Produce a findings report identifying real gaps (broken behavior,
   missing observability, unsafe failure modes).
3. From those findings, design a hardening change set scoped to operational
   concerns: key rotation, DEK cache lifecycle, KMS failure handling,
   audit logging, blast radius, startup validation.

## Non-Goals

- Reviewing AES-GCM, nonce, or AAD construction for cryptographic
  correctness (separate effort).
- FIPS / SOC2 / PCI compliance gap analysis.
- Wiring the new `pkg/vector` subsystem into encryption (separate effort).
- Exercising AWS / Azure provider behavior on real cloud KMS.

## Phase 1 — Local Baseline Validation

### Test stack

A `docker-compose.encryption-test.yml` (or reuse of `docker-compose.local.yml`
with overrides) running:

- MinIO (S3 backend)
- The engine, configured with:
  - `ENCRYPTION_ENABLED=true`
  - `ENCRYPTION_KEY_PROVIDER=local`
  - `ENCRYPTION_LOCAL_MASTER_KEY=<base64 32-byte key>`
  - `STORAGE_PROVIDER=s3` pointing at MinIO

### Validation checklist

Each item is a scripted check producing a pass/fail line in the report.

1. **Ciphertext on disk.** Write an object via the engine API. Read the
   raw object directly from MinIO. Verify it does not contain the plaintext
   bytes and is structurally consistent with the documented envelope format.
2. **Round-trip.** Read the object back through the engine API; bytes match.
3. **GCM auth tag rejection.** Flip a byte in the raw MinIO object. Read
   through the engine; expect a decryption error, no partial plaintext.
4. **Restart behavior.** Write, restart the engine, read. Verify DEK cache
   is rebuilt correctly and reads still succeed.
5. **Concurrent writes.** N concurrent writers; verify no nonce reuse
   (sample object headers, confirm uniqueness within a key scope).
6. **Key provider unreachable.** Simulate provider failure (for `local`,
   inject a faulty provider via config or a wrapper). Verify the engine
   fails *closed* — no plaintext writes, no plaintext reads — and surfaces
   a clear error.
7. **Logs.** Grep engine logs for plaintext samples and key material
   (master key, DEKs, base64 fragments). Expect zero hits.
8. **Metrics.** Enumerate Prometheus metrics for encryption ops
   (encrypt/decrypt count, DEK cache hit/miss, KMS calls, errors).
   Document what exists and what's missing.
9. **Key rotation, if exposed.** If a rotation API or admin command exists,
   exercise it and confirm old objects remain readable while new objects
   use the new key. If no such path exists, record as a Phase 2 gap.
10. **Startup key validation.** Boot with a wrong/missing master key;
    verify the engine refuses to start with a clear error rather than
    starting and failing later on first request.

### Deliverable

`docs/superpowers/findings/2026-05-07-encryption-validation-report.md`
containing one section per check, each with: command run, observed
output (trimmed), pass/fail, and a one-line note. A summary table at the
top.

## Phase 2 — Operational Hardening Spec

Driven by Phase 1 findings plus desk review of behaviors we cannot
exercise locally (real KMS latency, IAM denial, alias resolution).

Topics to address (final list determined by findings):

- **Key rotation flow.** Master key rotation, DEK rewrap, online vs.
  offline rotation, per-object key version metadata.
- **DEK cache lifecycle.** TTL bounds, eviction on rotation, memory
  zeroization on eviction, cache size caps.
- **KMS failure modes.** Fail-closed semantics, retry/backoff, circuit
  breaker behavior, distinguishing transient vs. permanent failure.
- **Audit logging.** Structured events for: KMS call, DEK generate,
  DEK decrypt, rotation, key validation. No key material in logs.
- **Blast radius of compromised DEK.** DEK scoping (per-object, per-bucket,
  per-tenant), how much data one leaked DEK exposes, recovery steps.
- **Startup validation.** Mandatory provider reachability + key
  decryptability check before serving traffic.
- **Metrics.** Standard set covering ops, errors, cache, latency.

Each topic gets: current state, gap, proposed change, test plan.

## Risks / Open Questions

- The `local` provider may not exercise all code paths the AWS provider
  hits (e.g., async DEK fetch, KMS-side caching). Phase 2 must mark
  any recommendation that depends on cloud-only behavior as
  desk-reviewed rather than validated.
- If Phase 1 finds the system fundamentally broken (e.g., plaintext
  on disk), hardening becomes secondary to a correctness fix and the
  Phase 2 scope shifts.

## Success Criteria

- Phase 1 report exists and every checklist item has a clear pass/fail
  with evidence.
- Phase 2 spec exists, every recommendation traces to either a Phase 1
  finding or a documented desk-review reason.
- Both phases land as committed docs and produce a follow-up
  implementation plan (via the writing-plans flow).
