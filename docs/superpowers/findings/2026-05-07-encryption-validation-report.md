# Encryption Validation Report (Phase 1)

**Date:** 2026-05-07
**Spec:** docs/superpowers/specs/2026-05-07-encryption-audit-and-hardening-design.md
**Stack:** docker-compose.encryption-test.yml (MinIO + engine, local key provider)

## Summary

| # | Check | Status |
|---|-------|--------|
| 1 | Ciphertext on disk | **FAIL** |
| 2 | Round-trip | pending |
| 3 | GCM tamper rejection | pending |
| 4 | Restart behavior | pending |
| 5 | Concurrent writes / nonce uniqueness | pending |
| 6 | Provider unreachable (fail-closed) | pending |
| 7 | Log / key material leakage | pending |
| 8 | Metrics surface | pending |
| 9 | Key rotation API | pending |
| 10 | Startup key validation | pending |

## Check 1 — Ciphertext on disk

**Status:** FAIL

Plaintext canary `WARP-ENCRYPTION-CANARY-1778188971-1751` found in raw MinIO
object — encryption did NOT happen.

### Root cause (preliminary)

`internal/storage/encrypted.go` defines `EncryptedBackend` and
`NewEncryptedBackend(...)`, but a code-wide search shows it is only ever
constructed from `_test.go` files. There is no production call site that
wraps the underlying S3 backend with `EncryptedBackend`, so even though
`ENCRYPTION_ENABLED=true` is honored to the extent of building an
`encryption.Manager`, no request path actually consults it.

```
$ grep -rn "NewEncryptedBackend" --include="*.go" cmd internal | grep -v _test.go
(no results)
```

This is the dominant Phase 2 finding: encryption is unwired in the request
path. All subsequent checks in this report observe behavior of a system
that is effectively running with encryption disabled, regardless of
configuration.
