# Encryption Validation Report (Phase 1)

**Date:** 2026-05-07
**Spec:** docs/superpowers/specs/2026-05-07-encryption-audit-and-hardening-design.md
**Stack:** docker-compose.encryption-test.yml (MinIO + engine, local key provider)

## Summary

| # | Check | Status |
|---|-------|--------|
| 1 | Ciphertext on disk | **FAIL** |
| 2 | Round-trip | PASS |
| 3 | GCM tamper rejection | **FAIL** |
| 4 | Restart behavior | PASS |
| 5 | Concurrent writes / nonce uniqueness | PASS (caveat) |
| 6 | Provider unreachable (fail-closed) | **FAIL** |
| 7 | Log / key material leakage | PASS (caveat) |
| 8 | Metrics surface | PASS (1 metric only) |
| 9 | Key rotation API | REVIEW (KMS-side only) |
| 10 | Startup key validation | **FAIL** |

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

## Check 2 — Round-trip

**Status:** PASS

Wrote 26 bytes; read back identical bytes.

## Check 3 — GCM tamper rejection

**Status:** FAIL

Tampered object returned HTTP 200 with 0 bytes — no auth tag check (consistent with Check 1: encryption unwired, so no GCM tag exists to reject).

## Check 4 — Restart behavior

**Status:** PASS

Object written before restart is readable after restart with same master key.

## Check 5 — Concurrent writes / nonce uniqueness

**Status:** PASS

50 concurrent writes produced 50 distinct first-64-byte headers (proxy for
unique nonces / envelope randomness).

**Caveat:** because Check 1 confirmed encryption is unwired, those headers
are first-64-bytes of *plaintext*, and each plaintext was crafted to be
unique (`concurrent-$i-$(date +%s%N)`). This check therefore says nothing
about nonce reuse — it must be re-run after the wiring fix to be
meaningful.

## Check 6 — Provider unreachable / invalid key (fail-closed)

**Status:** FAIL

Engine accepted writes (HTTP 200) with an invalid master key — fail-open.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:28:04] S3 request received                           [36maccept[0m="*/*" [36mauthType[0m=basic [36mcontentLength[0m=4 [36mcontentLengthHeader[0m=4 [36mhasAuth[0m=true [36mmethod[0m=PUT [36mpath[0m=/enc-test-bucket/check06/x [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] Starting authentication check                 [36mauth_type[0m=basic [36mis_public_path[0m=false [36mis_ui_path[0m=false [36mpath[0m=/enc-test-bucket/check06/x
[36mINFO[0m[2026/05/07 - 21:28:04] Incoming S3 request                           [36mall_headers[0m="map[Accept:[*/*] Authorization:[Basic YWRtaW46c2VjcmV0] Content-Length:[4] Content-Type:[application/x-www-form-urlencoded] User-Agent:[curl/8.7.1]]" [36mclientLabels[0m="[unknown]" [36mhas_amz_date[0m=false [36mhas_auth[0m=true [36mmethod[0m=PUT [36mpath[0m=/enc-test-bucket/check06/x [36mquery[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] PUT handler started                           [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36misAWSCLI[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=start [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] Chunked transfer detection                    [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentEncoding[0m= [36mcontentMD5[0m= [36mcontentSha256[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36misAWSCLI[0m=false [36misChunkedTransfer[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=start [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] Deciding read strategy                        [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36misAWSCLI[0m=false [36misChunkedTransfer[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=start [36mtransferEncoding[0m= [36museChunkedPath[0m=false [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] Using standard ReadFull strategy              [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36misAWSCLI[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=start [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] Small file processed with buffer pool and MD5 ETag  [36mactualSize[0m=4 [36mbucket[0m=enc-test-bucket [36mbufferPoolUsed[0m=true [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36metag[0m="\"098f6bcd4621d373cade4e832627b4f6\"" [36misAWSCLI[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=start [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] Processing upload request                     [36mbodyType[0m="*bytes.Reader" [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36misAWSCLI[0m=false [36misAvro[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=start [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] About to call S3 PutObject                    [36mbucket[0m=enc-test-bucket [36mhasBody[0m=true [36mkey[0m=check06/x [36mmetadata[0m="map[Content-Type:0x4000548e30]" [36msize[0m=4
[36mINFO[0m[2026/05/07 - 21:28:04] S3 PutObject completed                        [36mbucket[0m=enc-test-bucket [36mduration[0m=7.075334ms [36merror[0m="<nil>" [36metag[0m=0x40005ca280 [36mkey[0m=check06/x [36msize[0m=4
[36mINFO[0m[2026/05/07 - 21:28:04] Upload completed successfully, about to send response  [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36metag[0m="\"098f6bcd4621d373cade4e832627b4f6\"" [36misAWSCLI[0m=false [36misAzure[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36msize[0m=4 [36mstage[0m=before_response [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] PUT object handler completed successfully     [36mbucket[0m=enc-test-bucket [36mchecksumAlgorithm[0m= [36mcontentMD5[0m= [36mcontentType[0m=application/x-www-form-urlencoded [36metag[0m="\"098f6bcd4621d373cade4e832627b4f6\"" [36misAWSCLI[0m=false [36misChunkedWithoutSize[0m=false [36misIcebergData[0m=false [36misIcebergMeta[0m=false [36misJavaClient[0m=false [36misSparkUpload[0m=false [36mkey[0m=check06/x [36mmethod[0m=PUT [36mrequestID[0m=1778189284986005881 [36mresponseHeaders[0m="map[Access-Control-Allow-Headers:[Content-Type, Authorization, X-Amz-*] Access-Control-Allow-Methods:[GET, POST, PUT, DELETE, HEAD, OPTIONS] Access-Control-Allow-Origin:[*] Content-Length:[0] Content-Security-Policy:[default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.tailwindcss.com https://unpkg.com https://browser.sentry-cdn.com https://*.sentry.io https://cdnjs.cloudflare.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdn.tailwindcss.com https://cdnjs.cloudflare.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: blob:; connect-src 'self' https://*.sentry.io; frame-src 'self' https://*.sentry.io; frame-ancestors 'none';] Etag:[\"098f6bcd4621d373cade4e832627b4f6\"] Permissions-Policy:[accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()] Referrer-Policy:[strict-origin-when-cross-origin] Server:[AmazonS3] Strict-Transport-Security:[max-age=31536000; includeSubDomains] X-Amz-Request-Id:[1778189284986005881] X-Content-Type-Options:[nosniff] X-Frame-Options:[DENY] X-Xss-Protection:[1; mode=block]]" [36msize[0m=4 [36mstage[0m=handler_complete [36mtransferEncoding[0m= [36muserAgent[0m=curl/8.7.1
[36mINFO[0m[2026/05/07 - 21:28:04] S3 request completed                          [36mduration[0m=7.471458ms [36mmethod[0m=PUT [36mpath[0m=/enc-test-bucket/check06/x [36mstatus[0m=200
[FSE] 2026/05/07 - 21:28:04 | 200 |        7.63ms |    192.168.65.1 | PUT     "/enc-test-bucket/check06/x"
```

## Check 7 — Log / key material leakage

**Status:** PASS

No plaintext canary or master key bytes in logs. Sample long base64 strings (review manually for DEK material):

```
(none)
```

## Check 8 — Metrics surface

**Status:** PASS

Encryption-related metrics found:

```
foundation_storage_engine_kms_data_keys_active 0
```

## Check 9 — Key rotation API

**Status:** REVIEW

Rotation-related symbols found — manual review needed.

Source hits:
```
internal/kms/config.go:42:		EnableKeyRotation: cfg.EnableKeyRotation,
internal/kms/manager.go:33:	EnableKeyRotation bool              `yaml:"enable_key_rotation" json:"enable_key_rotation"`
internal/kms/manager.go:314:// GetKeyRotationStatus checks if key rotation is enabled
internal/kms/manager.go:315:func (m *Manager) GetKeyRotationStatus(ctx context.Context, keyID string) (bool, error) {
internal/kms/manager.go:324:	resp, err := m.client.GetKeyRotationStatus(ctx, &kms.GetKeyRotationStatusInput{
internal/kms/manager.go:328:		return false, fmt.Errorf("failed to get key rotation status: %w", err)
internal/kms/manager.go:331:	return resp.KeyRotationEnabled, nil
internal/kms/manager.go:334:// EnableKeyRotation enables automatic key rotation
internal/kms/manager.go:335:func (m *Manager) EnableKeyRotation(ctx context.Context, keyID string) error {
internal/kms/manager.go:344:	_, err := m.client.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{
internal/kms/manager.go:348:		return fmt.Errorf("failed to enable key rotation: %w", err)
```

Endpoint hits:
```
(none)
```

## Check 10 — Startup key validation

**Status:** FAIL

Engine reached healthy state with empty master key — startup validation gap.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:29:41] Starting S3 proxy server                      [36mcommit[0m=none [36mdate[0m=unknown [36mnum_cpu[0m=10 [36mversion[0m=dev
[36mINFO[0m[2026/05/07 - 21:29:41] Configuration loaded                          [36mauth_type[0m=basic [36mlisten_addr[0m=":8080" [36ms3_config[0m="map[ignore_headers:true region:us-east-1]" [36mstorage_provider[0m=s3
[36mINFO[0m[2026/05/07 - 21:29:41] Using custom S3 endpoint                      [36mendpoint[0m="http://minio:9000"
[36mINFO[0m[2026/05/07 - 21:29:41] Using static AWS credentials                 
[36mINFO[0m[2026/05/07 - 21:29:41] AWS credentials resolved                      [36mhasAccessKey[0m=true [36mprovider[0m=StaticProvider
[36mINFO[0m[2026/05/07 - 21:29:41] S3 backend created                            [36mendpoint[0m="http://minio:9000" [36mregion[0m=us-east-1 [36musePathStyle[0m=true
[36mINFO[0m[2026/05/07 - 21:29:41] Authentication provider initialized          
[36mINFO[0m[2026/05/07 - 21:29:41] API documentation available at /docs/ and /api/docs/ 
[36mINFO[0m[2026/05/07 - 21:29:41] Starting HTTP server with configured timeouts  [36midleTimeout[0m=2m0s [36mlisten[0m=":8080" [36mreadTimeout[0m=0s [36mwriteTimeout[0m=0s
[36mINFO[0m[2026/05/07 - 21:29:41] Server listening                              [36maddr[0m=":8080"
```

---

## Conclusions

### Headline finding

**Encryption is configured but not wired into the request path.** The
engine ships a complete envelope-encryption stack (`internal/encryption`,
`internal/kms` with AES-256-GCM and AWS / Azure / custom / local
providers) and faithfully constructs an `encryption.Manager` when
`ENCRYPTION_ENABLED=true`, but no production call site wraps the storage
backend with `EncryptedBackend`. Objects are written to MinIO as
plaintext regardless of configuration. Until this wiring is fixed, every
data-plane behavior of "encryption" is fictional.

### Confirmed working

- **Check 2 — Round-trip:** PUT/GET works end-to-end. Storage path is
  functionally correct; only encryption is missing.
- **Check 4 — Restart behavior:** State survives engine restart (this
  is trivially true today since nothing is encrypted, but the test
  harness is sound for re-running post-fix).
- **Check 7 — Log/key material leakage:** No plaintext canary or master
  key bytes observed in logs. **Caveat:** since no encryption ops are
  occurring, there is also no DEK material to leak. Re-run after wiring
  to draw a real conclusion.
- **Check 8 — Metrics surface:** A single encryption-related metric
  exists (`foundation_storage_engine_kms_data_keys_active`). Skeleton
  is in place but coverage is thin.

### Gaps identified

1. **Encryption not wired (Check 1, FAIL).** The dominant gap. Every
   other negative finding flows from this.
2. **No GCM tamper rejection (Check 3, FAIL).** Direct consequence of
   #1 — there is no auth tag on disk to verify.
3. **Fail-open with invalid master key (Check 6, FAIL).** Engine
   accepted writes (HTTP 200) when started with a malformed
   `ENCRYPTION_LOCAL_MASTER_KEY`. Master key validity is not enforced
   at startup, and the bad value never reaches a real encrypt operation
   because of #1.
4. **No startup validation (Check 10, FAIL).** Engine reaches healthy
   state with an empty master key. Should refuse to start when
   `ENCRYPTION_ENABLED=true` but the key provider cannot produce a
   working key.
5. **No data-plane key rotation (Check 9, REVIEW).** The codebase has
   `EnableKeyRotation` / `GetKeyRotationStatus` symbols, but they are
   thin wrappers around the AWS KMS API for rotating a CMK on the AWS
   side. There is no DEK rewrap, no per-object key version, and no
   admin endpoint to trigger or monitor rotation. Rotation is a Phase 2
   green-field design.
6. **Sparse encryption metrics (Check 8, PASS but thin).** Only one
   metric (`kms_data_keys_active`). No counters for encrypt/decrypt
   ops, DEK cache hit/miss, KMS calls, errors, or rotation events.

### Phase 2 hardening candidates

Listed in priority order; each ties back to a specific check above.

1. **Wire `EncryptedBackend` into the production backend chain**
   (root cause of Checks 1, 3). Whatever `cmd/warp-storage-engine`
   constructs as the storage backend should be wrapped when
   `cfg.Encryption.Enabled`. Add an integration test that asserts raw
   bytes on the underlying backend are not the plaintext canary.
2. **Mandatory startup key validation** (Checks 6, 10). When encryption
   is enabled, the engine must call the key provider once during
   startup, generate a test DEK, encrypt+decrypt a known plaintext, and
   refuse to begin serving traffic on any failure. Empty / malformed
   master keys must be rejected before the listener binds.
3. **Fail-closed semantics on key provider errors** (Check 6). If a
   per-request key derivation or DEK fetch fails at runtime, return
   5xx; never silently fall through to plaintext.
4. **Data-plane key rotation design** (Check 9). Per-object DEK
   versioning, master-key rotation that triggers DEK rewrap, an admin
   endpoint exposing rotation state and progress.
5. **Encryption metric set** (Check 8). At minimum: counters for
   `encrypt_ops_total{result}`, `decrypt_ops_total{result}`,
   `dek_cache_{hit,miss,evict}_total`, `kms_calls_total{op,result}`,
   `key_rotation_events_total`, plus latency histograms for KMS calls.
6. **Re-run all checks** (especially Checks 5 and 7) after #1 is fixed.
   Their PASS results are currently meaningless because no encryption
   activity is happening.

### Notes on what was NOT validated

- Real cloud-KMS behavior (latency, IAM denial, alias resolution,
  network partition). Cannot be exercised without AWS / Azure access.
- AES-GCM cryptographic correctness (nonce construction, AAD usage,
  key-derivation discipline). Out of scope; requires a separate
  crypto-focused review.
- Streaming / multipart upload path. Only single-shot PUT was used.
- The `pkg/vector` subsystem — its WAL and segment files are out of
  scope for this report and are not encrypted regardless of any fix
  applied above.

## Check 1 — Ciphertext on disk

**Status:** PASS

Plaintext canary not present in raw object. First 64 bytes:

```
    00000000: 37a0 c3d6 340c 2683 67ab 5eed 287e c831  7...4.&.g.^.(~.1
    00000010: 4a9e 838b a0d1 f798 704d 342b f01a c82f  J.......pM4+.../
    00000020: 0fe7 ca72 8da6 fb72 48a4 374a 2e14 2457  ...r...rH.7J..$W
    00000030: 3b7d 905a b752 37                        ;}.Z.R7
```

## Check 2 — Round-trip

**Status:** FAIL

Mismatch.

Expected:
```
hello-roundtrip-1778190442
```

Got:
```
lɔ�D'
Wu)wi֊�6���
eG��I�M}��a-*d6R�|�
```

## Check 3 — GCM tamper rejection

**Status:** FAIL

Tampered object returned HTTP 200 with 0 bytes — no auth tag check (consistent with Check 1: encryption unwired, so no GCM tag exists to reject).

## Check 10 — Startup key validation

**Status:** PASS

Engine did not become healthy with empty master key. State: exited exit=1.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:47:25] Using custom S3 endpoint                      [36mendpoint[0m="http://minio:9000"
[36mINFO[0m[2026/05/07 - 21:47:25] Using static AWS credentials                 
[36mINFO[0m[2026/05/07 - 21:47:25] AWS credentials resolved                      [36mhasAccessKey[0m=true [36mprovider[0m=StaticProvider
[36mINFO[0m[2026/05/07 - 21:47:25] S3 backend created                            [36mendpoint[0m="http://minio:9000" [36mregion[0m=us-east-1 [36musePathStyle[0m=true
Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: local key provider requires master key
Usage:
  warp-storage-engine [flags]

Flags:
  -c, --config string      config file path
  -h, --help               help for warp-storage-engine
      --listen string      listen address (default ":8080")
      --log-level string   log level (debug, info, warn, error) (default "info")

Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: local key provider requires master key
```

## Check 1 — Ciphertext on disk

**Status:** PASS

Plaintext canary not present in raw object. First 64 bytes:

```
    00000000: deaa 39af ecd3 8ffa 22db 4bbc 8bce e380  ..9.....".K.....
    00000010: 3c10 1c56 5d25 c54d 5411 d654 3cf3 9ec3  <..V]%.MT..T<...
    00000020: 2f88 2f25 ea82 d490 aa03 b8ef b297 881b  /./%............
    00000030: cc25 c43f acdf de                        .%.?...
```

## Check 2 — Round-trip

**Status:** PASS

Wrote 26 bytes; read back identical bytes.

## Check 3 — GCM tamper rejection

**Status:** FAIL

Tampered object returned HTTP 200 with 0 bytes — no auth tag check (consistent with Check 1: encryption unwired, so no GCM tag exists to reject).

## Check 4 — Restart behavior

**Status:** PASS

Object written before restart is readable after restart with same master key.

## Check 5 — Concurrent writes / nonce uniqueness

**Status:** PASS

50 concurrent writes produced 50 distinct first-64-byte headers (proxy for unique nonces / envelope randomness).

## Check 6 — Provider unreachable / invalid key (fail-closed)

**Status:** PASS

Engine refused to operate with invalid master key. health=000000 write=000000.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:50:56] Using custom S3 endpoint                      [36mendpoint[0m="http://minio:9000"
[36mINFO[0m[2026/05/07 - 21:50:56] Using static AWS credentials                 
[36mINFO[0m[2026/05/07 - 21:50:56] AWS credentials resolved                      [36mhasAccessKey[0m=true [36mprovider[0m=StaticProvider
[36mINFO[0m[2026/05/07 - 21:50:56] S3 backend created                            [36mendpoint[0m="http://minio:9000" [36mregion[0m=us-east-1 [36musePathStyle[0m=true
Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: failed to decode master key: illegal base64 data at input byte 3
Usage:
  warp-storage-engine [flags]

Flags:
  -c, --config string      config file path
  -h, --help               help for warp-storage-engine
      --listen string      listen address (default ":8080")
      --log-level string   log level (debug, info, warn, error) (default "info")

Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: failed to decode master key: illegal base64 data at input byte 3
```

## Check 7 — Log / key material leakage

**Status:** PASS

No plaintext canary or master key bytes in logs. Sample long base64 strings (review manually for DEK material):

```
(none)
```

## Check 8 — Metrics surface

**Status:** PASS

Encryption-related metrics found:

```
foundation_storage_engine_kms_data_keys_active 0
```

## Check 9 — Key rotation API

**Status:** REVIEW

Rotation-related symbols found — manual review needed.

Source hits:
```
internal/kms/config.go:42:		EnableKeyRotation: cfg.EnableKeyRotation,
internal/kms/manager.go:33:	EnableKeyRotation bool              `yaml:"enable_key_rotation" json:"enable_key_rotation"`
internal/kms/manager.go:314:// GetKeyRotationStatus checks if key rotation is enabled
internal/kms/manager.go:315:func (m *Manager) GetKeyRotationStatus(ctx context.Context, keyID string) (bool, error) {
internal/kms/manager.go:324:	resp, err := m.client.GetKeyRotationStatus(ctx, &kms.GetKeyRotationStatusInput{
internal/kms/manager.go:328:		return false, fmt.Errorf("failed to get key rotation status: %w", err)
internal/kms/manager.go:331:	return resp.KeyRotationEnabled, nil
internal/kms/manager.go:334:// EnableKeyRotation enables automatic key rotation
internal/kms/manager.go:335:func (m *Manager) EnableKeyRotation(ctx context.Context, keyID string) error {
internal/kms/manager.go:344:	_, err := m.client.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{
internal/kms/manager.go:348:		return fmt.Errorf("failed to enable key rotation: %w", err)
```

Endpoint hits:
```
(none)
```

## Check 10 — Startup key validation

**Status:** PASS

Engine did not become healthy with empty master key. State: exited exit=1.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:51:02] Using custom S3 endpoint                      [36mendpoint[0m="http://minio:9000"
[36mINFO[0m[2026/05/07 - 21:51:02] Using static AWS credentials                 
[36mINFO[0m[2026/05/07 - 21:51:02] AWS credentials resolved                      [36mhasAccessKey[0m=true [36mprovider[0m=StaticProvider
[36mINFO[0m[2026/05/07 - 21:51:02] S3 backend created                            [36mendpoint[0m="http://minio:9000" [36mregion[0m=us-east-1 [36musePathStyle[0m=true
Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: local key provider requires master key
Usage:
  warp-storage-engine [flags]

Flags:
  -c, --config string      config file path
  -h, --help               help for warp-storage-engine
      --listen string      listen address (default ":8080")
      --log-level string   log level (debug, info, warn, error) (default "info")

Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: local key provider requires master key
```

## Check 3 — GCM tamper rejection

**Status:** FAIL

Tampered object returned HTTP 200 with 5480 bytes — no auth tag check (consistent with Check 1: encryption unwired, so no GCM tag exists to reject).

## Check 3 — GCM tamper rejection

**Status:** PASS

Tampered ciphertext rejected with HTTP 404. AES-GCM auth tag verification works.

## Check 1 — Ciphertext on disk

**Status:** PASS

Plaintext canary not present in raw object. First 64 bytes:

```
    00000000: 01f7 9520 d2b3 5ca0 7b5c d308 4faa a310  ... ..\.{\..O...
    00000010: a4ce 6b58 1e02 764d 69da 35b0 66f9 f6fe  ..kX..vMi.5.f...
    00000020: 0fa3 68ff a53a b140 f35a 4ce4 8a9f 3752  ..h..:.@.ZL...7R
    00000030: ea79 ea0e eb59                           .y...Y
```

## Check 2 — Round-trip

**Status:** PASS

Wrote 26 bytes; read back identical bytes.

## Check 3 — GCM tamper rejection

**Status:** PASS

Tampered ciphertext rejected with HTTP 404. AES-GCM auth tag verification works.

## Check 4 — Restart behavior

**Status:** PASS

Object written before restart is readable after restart with same master key.

## Check 5 — Concurrent writes / nonce uniqueness

**Status:** PASS

50 concurrent writes produced 50 distinct first-64-byte headers (proxy for unique nonces / envelope randomness).

## Check 6 — Provider unreachable / invalid key (fail-closed)

**Status:** PASS

Engine refused to operate with invalid master key. health=000000 write=000000.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:54:28] Using custom S3 endpoint                      [36mendpoint[0m="http://minio:9000"
[36mINFO[0m[2026/05/07 - 21:54:28] Using static AWS credentials                 
[36mINFO[0m[2026/05/07 - 21:54:28] AWS credentials resolved                      [36mhasAccessKey[0m=true [36mprovider[0m=StaticProvider
[36mINFO[0m[2026/05/07 - 21:54:28] S3 backend created                            [36mendpoint[0m="http://minio:9000" [36mregion[0m=us-east-1 [36musePathStyle[0m=true
Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: failed to decode master key: illegal base64 data at input byte 3
Usage:
  warp-storage-engine [flags]

Flags:
  -c, --config string      config file path
  -h, --help               help for warp-storage-engine
      --listen string      listen address (default ":8080")
      --log-level string   log level (debug, info, warn, error) (default "info")

Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: failed to decode master key: illegal base64 data at input byte 3
```

## Check 7 — Log / key material leakage

**Status:** PASS

No plaintext canary or master key bytes in logs. Sample long base64 strings (review manually for DEK material):

```
(none)
```

## Check 8 — Metrics surface

**Status:** PASS

Encryption-related metrics found:

```
foundation_storage_engine_kms_data_keys_active 0
```

## Check 9 — Key rotation API

**Status:** REVIEW

Rotation-related symbols found — manual review needed.

Source hits:
```
internal/kms/config.go:42:		EnableKeyRotation: cfg.EnableKeyRotation,
internal/kms/manager.go:33:	EnableKeyRotation bool              `yaml:"enable_key_rotation" json:"enable_key_rotation"`
internal/kms/manager.go:314:// GetKeyRotationStatus checks if key rotation is enabled
internal/kms/manager.go:315:func (m *Manager) GetKeyRotationStatus(ctx context.Context, keyID string) (bool, error) {
internal/kms/manager.go:324:	resp, err := m.client.GetKeyRotationStatus(ctx, &kms.GetKeyRotationStatusInput{
internal/kms/manager.go:328:		return false, fmt.Errorf("failed to get key rotation status: %w", err)
internal/kms/manager.go:331:	return resp.KeyRotationEnabled, nil
internal/kms/manager.go:334:// EnableKeyRotation enables automatic key rotation
internal/kms/manager.go:335:func (m *Manager) EnableKeyRotation(ctx context.Context, keyID string) error {
internal/kms/manager.go:344:	_, err := m.client.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{
internal/kms/manager.go:348:		return fmt.Errorf("failed to enable key rotation: %w", err)
```

Endpoint hits:
```
(none)
```

## Check 10 — Startup key validation

**Status:** PASS

Engine did not become healthy with empty master key. State: exited exit=1.

Log tail:
```
[36mINFO[0m[2026/05/07 - 21:54:34] Using custom S3 endpoint                      [36mendpoint[0m="http://minio:9000"
[36mINFO[0m[2026/05/07 - 21:54:34] Using static AWS credentials                 
[36mINFO[0m[2026/05/07 - 21:54:34] AWS credentials resolved                      [36mhasAccessKey[0m=true [36mprovider[0m=StaticProvider
[36mINFO[0m[2026/05/07 - 21:54:34] S3 backend created                            [36mendpoint[0m="http://minio:9000" [36mregion[0m=us-east-1 [36musePathStyle[0m=true
Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: local key provider requires master key
Usage:
  warp-storage-engine [flags]

Flags:
  -c, --config string      config file path
  -h, --help               help for warp-storage-engine
      --listen string      listen address (default ":8080")
      --log-level string   log level (debug, info, warn, error) (default "info")

Error: failed to create proxy server: failed to create encryption manager: failed to create key provider: local key provider requires master key
```

---

## Re-run after fix (2026-05-08)

After applying the fixes described below, all 9 testable checks now PASS
(Check 9 remains REVIEW — it documents a Phase 2 *design* gap, not a
bug).

| # | Check | Before | After |
|---|-------|--------|-------|
| 1 | Ciphertext on disk | FAIL | PASS |
| 2 | Round-trip | PASS | PASS |
| 3 | GCM tamper rejection | FAIL | PASS |
| 4 | Restart behavior | PASS | PASS |
| 5 | Concurrent writes / nonce uniqueness | PASS (caveat) | PASS |
| 6 | Provider unreachable (fail-closed) | FAIL | PASS |
| 7 | Log / key material leakage | PASS (caveat) | PASS |
| 8 | Metrics surface | PASS (1 metric) | PASS (1 metric) |
| 9 | Key rotation API | REVIEW | REVIEW (unchanged) |
| 10 | Startup key validation | FAIL | PASS |

### Fixes applied

**1. Wire `EncryptedBackend` into the production storage chain**
(`internal/proxy/server.go`). After cache wrap, build `encryption.Manager`
from `cfg.Encryption` when `Enabled=true` and wrap the storage backend
with `storage.NewEncryptedBackend(...)`. Encryption is the outermost
layer so cache (when present) holds ciphertext, not plaintext.
*Resolves Checks 1, 3.*

**2. Add startup self-test** (`validateEncryptionAtStartup` in
`internal/proxy/server.go`). When encryption is enabled, perform a small
encrypt+decrypt round-trip with a known canary against the configured
key provider. Refuse to start the server on any failure (encrypt error,
decrypt error, byte mismatch, missing metadata). This makes invalid /
empty master keys, unreachable key providers, and broken DEK derivation
all visible as a hard startup failure instead of silent fail-open.
*Resolves Checks 6, 10.*

**3. Add missing `Backend` interface methods to `EncryptedBackend`**
(`internal/storage/encrypted.go`): `RestoreObject` and
`ListDeletedObjects`, both delegating to the wrapped backend (with
metadata cleaning on the list path). Without these the type did not
satisfy the `storage.Backend` interface and could not be used in the
backend chain.

**4. Fix S3 metadata round-trip prefix bug** (`internal/storage/s3.go`).
The S3 backend was passing user metadata keys verbatim to the AWS SDK,
which prepends `X-Amz-Meta-` automatically. Keys that already had the
prefix (e.g., `x-amz-meta-encryption-algorithm` from the encryption
metadata constants) ended up doubled on the wire
(`X-Amz-Meta-X-Amz-Meta-Encryption-Algorithm`). On read, the SDK
stripped only one prefix, breaking case-sensitive lookups in
`EncryptedBackend.GetObject` (`obj.Metadata[types.MetadataKeyAlgorithm]`)
so encryption metadata was not detected and decryption never ran.
Added `stripUserMetadataPrefix` (write side) and
`normalizeUserMetadataKey` (read side) helpers and applied them in all
four metadata-handling sites (`PutObject`, `CopyObject`,
`InitiateMultipartUpload`, `GetObject`, `HeadObject`).
*Without this fix, Check 1 would pass (writes are encrypted) but
Check 2 would fail (reads return ciphertext).*

### Verification

- All 10 validation scripts re-run end-to-end against the rebuilt
  engine image. 9 PASS, 1 REVIEW (Check 9, unchanged design-level gap).
- `go test -run "Encrypt|Encryption" ./internal/storage/...` passes.
- `go test ./internal/proxy/...` passes.
- `go test ./internal/encryption/...` passes (all sub-packages).
- The non-encryption AWS-network tests in `internal/storage` time out
  without AWS access; this is pre-existing and unrelated to the fix.

### Phase 2 items still open

- **Key rotation (Check 9).** Existing `EnableKeyRotation` /
  `GetKeyRotationStatus` only wrap AWS KMS CMK rotation. Data-plane
  rotation (per-object DEK versioning, master-key rotation triggering
  DEK rewrap, admin endpoint) is not implemented.
- **Encryption metric set (Check 8).** Only
  `foundation_storage_engine_kms_data_keys_active` exists. Add
  counters for encrypt/decrypt ops, DEK cache hit/miss, KMS calls,
  errors, latency histograms.
- **`pkg/vector` encryption.** WAL and segment files in the new vector
  subsystem are not wired through `EncryptedBackend` and are written
  as plaintext regardless.
