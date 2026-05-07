# Encryption Metric Set — Design

**Date:** 2026-05-08
**Status:** Draft
**Scope:** `internal/encryption`, `internal/kms`, `internal/metrics`,
`internal/storage/encrypted.go`
**Phase 2 item:** Encryption metrics (Check 8 in the validation report)

## Background

Phase 1 validation found one encryption-related metric exposed at runtime
(`foundation_storage_engine_kms_data_keys_active`). Inspecting
`internal/metrics/metrics.go` reveals a fuller set of KMS metrics is
already defined — `KMSOperationsTotal`, `KMSOperationDuration`,
`KMSErrors`, `KMSCacheHits`, `KMSCacheMisses`, `KMSKeyValidations` — but
no production code path increments them. The metrics infrastructure
exists; the call sites are missing.

There are also no metrics at the **object** layer
(`encryption.Manager` / `EncryptedBackend`). KMS metrics measure key
operations (one DEK generation per object); object metrics measure data
operations (one per request). Both are needed to answer different
questions: "is the KMS provider slow?" vs. "how many objects am I
encrypting and at what rate?"

## Goals

1. Wire the existing KMS metric definitions to real call sites so they
   reflect production traffic.
2. Add a small set of object-layer metrics on
   `encryption.Manager` / `EncryptedBackend` for per-object encrypt and
   decrypt operations.
3. Keep cardinality bounded — no per-bucket, per-key-id, or per-tenant
   labels.

## Non-Goals

- Audit logging for key operations (separate concern).
- Tracing / spans (separate concern).
- Latency SLO definitions or alerting rules.
- Re-organizing `internal/metrics/metrics.go` beyond what this change
  requires.
- Changing existing metric names or labels (no breaking changes).

## Architecture

Two layers, both exporting through the same `*metrics.Metrics`
singleton constructed in `proxy.NewServer`:

```
HTTP request
   │
   ▼
EncryptedBackend.PutObject / GetObject ─────┐
   │                                        │
   ▼                                        │
encryption.Manager.Encrypt / Decrypt ◄──── object-layer metrics
   │
   ▼
KMS provider (Encrypt / Decrypt / GenerateDataKey / ValidateKey)
   │
   ▼
DataKeyCache.Get / Set ──────────────────── KMS-layer metrics
```

Both `encryption.Manager` and the KMS provider hold a reference to the
same `*metrics.Metrics`. The `metrics` package gains thin observation
methods (`ObserveKMSOperation`, `ObserveEncryptOp`, …) so call sites
don't reach into Prometheus types directly — this keeps the library
boundary clean and tests trivial (a no-op `*metrics.Metrics` works).

## KMS-layer wiring (existing definitions)

For each of the four `Provider` interface methods
(`Encrypt`, `Decrypt`, `GenerateDataKey`, `ValidateKey`), wrap the body
with start-time capture and a deferred observation. On success increment
`KMSOperationsTotal{operation, status="success"}` and observe duration.
On error increment both `KMSOperationsTotal{...,status="error"}` and
`KMSErrors{operation, error_type}`.

Implementation: introduce a small `instrumentedProvider` decorator in
`internal/kms/provider.go` that wraps a `Provider` and a `*metrics.Metrics`.
`kms.NewProvider` returns the decorator when metrics are non-nil. This
keeps each concrete provider (local, AWS, Azure, custom) free of
metric-emission code and avoids touching four files.

For the cache: `DataKeyCache.Get` already returns `(*DataKey, bool)`;
add metric increments at the call site (the manager) rather than inside
the cache, so cache stays a pure data structure.
`KMSDataKeysActive` already wired — leave it.

`error_type` label values: a small enumerated set —
`{not_found, unauthorized, invalid_key, network, internal}`.
Map common provider error types to these via a small classifier in
the decorator. No raw error strings as labels (cardinality risk).

## Object-layer additions (new definitions)

Add four metrics to `internal/metrics/metrics.go`:

| Name | Type | Labels | Meaning |
|------|------|--------|---------|
| `encryption_object_ops_total` | Counter | `op={encrypt,decrypt}, status={success,error}` | Object-level encrypt/decrypt counts |
| `encryption_object_bytes_total` | Counter | `op={encrypt,decrypt}` | Total bytes processed (plaintext bytes for both ops) |
| `encryption_object_duration_seconds` | Histogram | `op={encrypt,decrypt}` | Per-object encrypt/decrypt latency |
| `encryption_disabled_passthrough_total` | Counter | (none) | Times the manager fell through with encryption disabled (sanity check that disabling really disables) |

Histogram buckets: `prometheus.ExponentialBuckets(0.0001, 2, 12)`
(0.1ms → ~410ms), matching the storage-op buckets in the same file.

Wiring: in `encryption.Manager.Encrypt` and `Manager.Decrypt`, wrap the
encryptor call with `time.Now()` + a deferred observation. The byte
counter is incremented from the `size` parameter on encrypt and from
the metadata-recorded original size on decrypt.

## Cardinality budget

| Metric | Worst-case series |
|--------|-------------------|
| `kms_operations_total` | 4 ops × 2 statuses = 8 |
| `kms_operation_duration_seconds` | 4 ops |
| `kms_errors_total` | 4 ops × 5 error_types = 20 |
| `kms_cache_hits_total` | 1 (cache_type=dek) |
| `kms_cache_misses_total` | 1 |
| `kms_key_validations_total` | 2 statuses |
| `encryption_object_ops_total` | 2 × 2 = 4 |
| `encryption_object_bytes_total` | 2 |
| `encryption_object_duration_seconds` | 2 (× histogram buckets) |
| `encryption_disabled_passthrough_total` | 1 |

Total ≤ ~50 base series. Acceptable.

## Failure / edge cases

- **Metrics is nil.** Library code (kms, encryption) must accept a nil
  `*metrics.Metrics` and no-op. This makes unit tests trivial.
- **Encryption disabled.** `encryption.Manager.IsEnabled()==false` — the
  manager increments `encryption_disabled_passthrough_total` and skips
  the duration / bytes / ops counters. This gives ops a way to verify
  config drift hasn't accidentally turned encryption off.
- **Decrypt of non-encrypted object.** Existing code path returns the
  reader unchanged (Manager.Decrypt early-returns when metadata lacks
  the algorithm key). Same handling as "disabled" — increment the
  passthrough counter, skip per-op metrics.
- **Streaming encrypt where `size == -1`** (chunked uploads). Bytes
  counter is incremented after the reader is fully consumed; if the
  caller never finishes reading, the byte count for that op is lost.
  Document, don't fix — accuracy at scale is more important than
  per-op precision.

## Testing

- **Unit tests in `internal/metrics`** for the new observation methods
  (the existing test pattern at line 142 of `metrics_test.go` covers
  this style).
- **Unit tests in `internal/kms/provider_test.go`** for the
  `instrumentedProvider` decorator — verify each of the 4 provider
  methods, on both success and error paths, increments the expected
  counters and observes a duration. Use a `testutil.CollectAndCount`
  / `testutil.ToFloat64` pattern.
- **Unit tests in `internal/encryption/manager_test.go`** for the
  object-layer wiring — encrypt and decrypt of small payloads, verify
  ops counter, bytes counter, duration histogram all advance.
- **Integration check.** Add an 11th script
  `scripts/encryption-validation/11-metrics-coverage.sh` that runs N
  encrypt and N decrypt operations, scrapes `/metrics`, and asserts the
  expected counters advanced by N (and bytes counter advanced by ≥ N
  × min-payload). Append to the validation report.

## Risks / Open Questions

- **Decorator visibility.** `instrumentedProvider` lives in the `kms`
  package alongside the providers. If a consumer constructs a
  `Provider` directly (bypassing `NewProvider`), they get no metrics.
  This is acceptable — production code goes through `NewProvider`.
- **Hot path overhead.** `time.Now()` + a counter increment per encrypt
  op is ~100ns; negligible against the encrypt cost itself. No
  benchmarking gate needed.

## Success Criteria

- All 8 of the existing `KMS*` metric definitions show non-zero values
  after a workload runs.
- The 4 new `encryption_object_*` metrics exist and increment.
- The new `11-metrics-coverage.sh` validation script passes.
- No new test failures in `go test ./internal/encryption/...
  ./internal/kms/... ./internal/metrics/... ./internal/storage/...`.
- Total Prometheus base series added ≤ 50.
