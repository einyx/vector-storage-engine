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
