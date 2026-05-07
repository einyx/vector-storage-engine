# Encryption Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the existing-but-unused KMS metrics, add a small set of object-layer encryption metrics, and verify with a new validation script.

**Architecture:** A nil-safe `instrumentedProvider` decorator in `internal/kms` wraps every concrete `Provider` and emits operation/duration/error metrics. The `DataKeyCache` accepts an optional `*metrics.Metrics` and emits hit/miss counters from inside `Get`. `encryption.Manager` gains a `*metrics.Metrics` field and emits 4 new object-layer counters/histograms (`encryption_object_*`). `*metrics.Metrics` is plumbed from `proxy.NewServer` through `encryption.NewFromConfig` and `kms.NewProvider`. All library code accepts a nil `*metrics.Metrics` and no-ops.

**Tech Stack:** Go, prometheus/client_golang, existing `internal/metrics` infrastructure.

---

## File Structure

- Modify: `internal/metrics/metrics.go` — add 4 object-layer metric definitions + helper methods
- Modify: `internal/metrics/metrics_test.go` — tests for the new helpers
- Create: `internal/kms/instrumented_provider.go` — decorator + error classifier
- Create: `internal/kms/instrumented_provider_test.go`
- Modify: `internal/kms/provider.go` — accept metrics in `NewProvider`, return decorator
- Modify: `internal/kms/cache.go` — accept metrics in `NewDataKeyCache`, emit hit/miss from `Get`
- Modify: `internal/kms/cache_test.go` *(file may not exist; create as needed)* — tests for cache metrics
- Modify: `internal/encryption/manager.go` — accept metrics, emit object-layer metrics
- Modify: `internal/encryption/manager_test.go` — tests for object-layer metrics
- Modify: `internal/encryption/factory.go` — accept and plumb metrics through `NewFromConfig`
- Modify: `internal/proxy/server.go` — pass `s.metrics` (lifted before encryption setup) into `encryption.NewFromConfig`
- Create: `scripts/encryption-validation/11-metrics-coverage.sh`
- Modify: `scripts/encryption-validation/run-all.sh` — append "11" to the loop
- Modify: `docs/superpowers/findings/2026-05-07-encryption-validation-report.md` — add Check 11 row + section

---

## Task 1: Add object-layer metric definitions and helpers

**Files:**
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/metrics/metrics_test.go`

- [ ] **Step 1: Write failing tests** in `internal/metrics/metrics_test.go` (append to existing file):

```go
func TestObjectEncryptionMetrics_Helpers(t *testing.T) {
	m := NewMetrics("test_obj_enc")

	m.IncEncryptionObjectOp("encrypt", "success")
	m.IncEncryptionObjectOp("decrypt", "error")
	m.IncEncryptionObjectBytes("encrypt", 1024)
	m.ObserveEncryptionObjectDuration("encrypt", 5*time.Millisecond)
	m.IncEncryptionDisabledPassthrough()

	if got := testutil.ToFloat64(m.EncryptionObjectOps.WithLabelValues("encrypt", "success")); got != 1 {
		t.Errorf("encrypt success = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EncryptionObjectOps.WithLabelValues("decrypt", "error")); got != 1 {
		t.Errorf("decrypt error = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EncryptionObjectBytes.WithLabelValues("encrypt")); got != 1024 {
		t.Errorf("encrypt bytes = %v, want 1024", got)
	}
	if got := testutil.CollectAndCount(m.EncryptionObjectDuration); got != 1 {
		t.Errorf("duration histogram count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EncryptionDisabledPassthrough); got != 1 {
		t.Errorf("passthrough = %v, want 1", got)
	}
}
```

Add the import if missing:
```go
"github.com/prometheus/client_golang/prometheus/testutil"
```

- [ ] **Step 2: Run test, expect FAIL** with "undefined: m.EncryptionObjectOps" etc.

```bash
go test ./internal/metrics/... -run TestObjectEncryptionMetrics_Helpers
```

- [ ] **Step 3: Add the 4 metric fields** to the `Metrics` struct in `internal/metrics/metrics.go`. Insert after the existing KMS metrics block (around line 57):

```go
	// Object-layer encryption metrics
	EncryptionObjectOps           *prometheus.CounterVec
	EncryptionObjectBytes         *prometheus.CounterVec
	EncryptionObjectDuration      *prometheus.HistogramVec
	EncryptionDisabledPassthrough prometheus.Counter
```

- [ ] **Step 4: Construct them in `NewMetrics`.** Insert after the existing KMS construction block (after `KMSKeyValidations` is initialized, around line 292):

```go
		EncryptionObjectOps: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "encryption_object_ops_total",
				Help:      "Total number of object-level encrypt/decrypt operations",
			},
			[]string{"op", "status"},
		),

		EncryptionObjectBytes: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "encryption_object_bytes_total",
				Help:      "Total plaintext bytes processed by object-level encrypt/decrypt",
			},
			[]string{"op"},
		),

		EncryptionObjectDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Name:      "encryption_object_duration_seconds",
				Help:      "Per-object encrypt/decrypt latency in seconds",
				Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 12),
			},
			[]string{"op"},
		),

		EncryptionDisabledPassthrough: promauto.NewCounter(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "encryption_disabled_passthrough_total",
				Help:      "Times the encryption manager fell through with encryption disabled or object not encrypted",
			},
		),
```

- [ ] **Step 5: Add helper methods.** Append to `internal/metrics/metrics.go` (near the other `IncKMS*` / `ObserveKMS*` helpers):

```go
// Object-layer encryption metric helpers

// IncEncryptionObjectOp increments the object-level encrypt/decrypt counter.
// op is "encrypt" or "decrypt"; status is "success" or "error".
func (m *Metrics) IncEncryptionObjectOp(op, status string) {
	if m == nil {
		return
	}
	m.EncryptionObjectOps.WithLabelValues(op, status).Inc()
}

// IncEncryptionObjectBytes adds to the byte counter for an op.
func (m *Metrics) IncEncryptionObjectBytes(op string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.EncryptionObjectBytes.WithLabelValues(op).Add(float64(n))
}

// ObserveEncryptionObjectDuration records duration for an encrypt or decrypt op.
func (m *Metrics) ObserveEncryptionObjectDuration(op string, d time.Duration) {
	if m == nil {
		return
	}
	m.EncryptionObjectDuration.WithLabelValues(op).Observe(d.Seconds())
}

// IncEncryptionDisabledPassthrough increments the passthrough sanity counter.
func (m *Metrics) IncEncryptionDisabledPassthrough() {
	if m == nil {
		return
	}
	m.EncryptionDisabledPassthrough.Inc()
}
```

Also add nil guards to the existing KMS helpers (`IncKMSOperation`, `ObserveKMSOperationDuration`, `IncKMSError`, `IncKMSCacheHit`, `IncKMSCacheMiss`, `SetKMSDataKeysActive`, `IncKMSKeyValidation`) at the top of each:

```go
	if m == nil {
		return
	}
```

- [ ] **Step 6: Run test, expect PASS.**

```bash
go test ./internal/metrics/... -run TestObjectEncryptionMetrics_Helpers
```

- [ ] **Step 7: Commit.**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): add object-layer encryption metrics + nil guards"
```

---

## Task 2: Add an `instrumentedProvider` decorator (and error classifier)

**Files:**
- Create: `internal/kms/instrumented_provider.go`
- Create: `internal/kms/instrumented_provider_test.go`

- [ ] **Step 1: Write failing tests** in `internal/kms/instrumented_provider_test.go`:

```go
package kms

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/prysmsh/warp-storage-engine/internal/metrics"
)

type stubProvider struct {
	name string
	err  error
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) GenerateDataKey(_ context.Context, _ string, _ string) (*ProviderDataKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &ProviderDataKey{Plaintext: []byte("k"), CiphertextBlob: []byte("c"), KeyID: "id", Provider: s.name}, nil
}
func (s *stubProvider) Decrypt(_ context.Context, _ []byte) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte("plain"), nil
}
func (s *stubProvider) ValidateKey(_ context.Context, _ string) error { return s.err }
func (s *stubProvider) GetKeyInfo(_ context.Context, _ string) (*KeyInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &KeyInfo{KeyID: "id"}, nil
}

func TestInstrumentedProvider_SuccessIncrementsMetrics(t *testing.T) {
	m := metrics.NewMetrics("test_instr_ok")
	p := newInstrumentedProvider(&stubProvider{name: "stub"}, m)

	if _, err := p.GenerateDataKey(context.Background(), "id", "AES_256"); err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if _, err := p.Decrypt(context.Background(), []byte("c")); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if err := p.ValidateKey(context.Background(), "id"); err != nil {
		t.Fatalf("ValidateKey: %v", err)
	}

	if got := testutil.ToFloat64(m.KMSOperationsTotal.WithLabelValues("generate_data_key", "success")); got != 1 {
		t.Errorf("generate_data_key success = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.KMSOperationsTotal.WithLabelValues("decrypt", "success")); got != 1 {
		t.Errorf("decrypt success = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.KMSKeyValidations.WithLabelValues("success")); got != 1 {
		t.Errorf("key_validation success = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(m.KMSOperationDuration); got < 1 {
		t.Errorf("duration histogram empty")
	}
}

func TestInstrumentedProvider_ErrorClassification(t *testing.T) {
	m := metrics.NewMetrics("test_instr_err")
	p := newInstrumentedProvider(&stubProvider{name: "stub", err: errors.New("connection refused")}, m)

	if _, err := p.GenerateDataKey(context.Background(), "id", "AES_256"); err == nil {
		t.Fatal("expected error")
	}

	if got := testutil.ToFloat64(m.KMSOperationsTotal.WithLabelValues("generate_data_key", "error")); got != 1 {
		t.Errorf("generate_data_key error = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.KMSErrors.WithLabelValues("generate_data_key", "network")); got != 1 {
		t.Errorf("network error = %v, want 1", got)
	}
}

func TestInstrumentedProvider_NilMetricsIsNoop(t *testing.T) {
	p := newInstrumentedProvider(&stubProvider{name: "stub"}, nil)
	if _, err := p.GenerateDataKey(context.Background(), "id", "AES_256"); err != nil {
		t.Fatalf("GenerateDataKey with nil metrics: %v", err)
	}
}

func TestClassifyKMSError(t *testing.T) {
	cases := map[string]string{
		"connection refused":             "network",
		"context deadline exceeded":      "network",
		"AccessDenied: not authorized":   "unauthorized",
		"NotFoundException: missing key": "not_found",
		"InvalidKeyId: bad":              "invalid_key",
		"some other failure":             "internal",
	}
	for msg, want := range cases {
		if got := classifyKMSError(errors.New(msg)); got != want {
			t.Errorf("classifyKMSError(%q) = %q, want %q", msg, got, want)
		}
	}
}
```

- [ ] **Step 2: Run tests, expect FAIL** ("undefined: newInstrumentedProvider").

```bash
go test ./internal/kms/... -run "TestInstrumentedProvider|TestClassifyKMSError"
```

- [ ] **Step 3: Implement** in `internal/kms/instrumented_provider.go`:

```go
package kms

import (
	"context"
	"strings"
	"time"

	"github.com/prysmsh/warp-storage-engine/internal/metrics"
)

// instrumentedProvider wraps a Provider and emits operation/duration/error
// metrics. Safe with a nil *metrics.Metrics (every helper is nil-guarded).
type instrumentedProvider struct {
	inner   Provider
	metrics *metrics.Metrics
}

func newInstrumentedProvider(inner Provider, m *metrics.Metrics) Provider {
	if inner == nil {
		return nil
	}
	return &instrumentedProvider{inner: inner, metrics: m}
}

func (p *instrumentedProvider) Name() string { return p.inner.Name() }

func (p *instrumentedProvider) GenerateDataKey(ctx context.Context, keyID, keySpec string) (*ProviderDataKey, error) {
	start := time.Now()
	dk, err := p.inner.GenerateDataKey(ctx, keyID, keySpec)
	p.observe("generate_data_key", time.Since(start), err)
	return dk, err
}

func (p *instrumentedProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	start := time.Now()
	pt, err := p.inner.Decrypt(ctx, ciphertext)
	p.observe("decrypt", time.Since(start), err)
	return pt, err
}

func (p *instrumentedProvider) ValidateKey(ctx context.Context, keyID string) error {
	start := time.Now()
	err := p.inner.ValidateKey(ctx, keyID)
	p.observe("validate_key", time.Since(start), err)
	if err == nil {
		p.metrics.IncKMSKeyValidation("success")
	} else {
		p.metrics.IncKMSKeyValidation("error")
	}
	return err
}

func (p *instrumentedProvider) GetKeyInfo(ctx context.Context, keyID string) (*KeyInfo, error) {
	start := time.Now()
	info, err := p.inner.GetKeyInfo(ctx, keyID)
	p.observe("get_key_info", time.Since(start), err)
	return info, err
}

func (p *instrumentedProvider) observe(op string, d time.Duration, err error) {
	if err != nil {
		p.metrics.IncKMSOperation(op, "error")
		p.metrics.IncKMSError(op, classifyKMSError(err))
	} else {
		p.metrics.IncKMSOperation(op, "success")
	}
	p.metrics.ObserveKMSOperationDuration(op, d)
}

// classifyKMSError maps a provider error to a small enum of error types so we
// don't blow out cardinality with raw error strings.
func classifyKMSError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "deadline") ||
		strings.Contains(s, "connection") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "i/o"):
		return "network"
	case strings.Contains(s, "accessdenied") ||
		strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "forbidden") ||
		strings.Contains(s, "not authorized"):
		return "unauthorized"
	case strings.Contains(s, "notfound") ||
		strings.Contains(s, "not found"):
		return "not_found"
	case strings.Contains(s, "invalidkey") ||
		strings.Contains(s, "invalid key"):
		return "invalid_key"
	default:
		return "internal"
	}
}
```

- [ ] **Step 4: Run tests, expect PASS.**

```bash
go test ./internal/kms/... -run "TestInstrumentedProvider|TestClassifyKMSError"
```

- [ ] **Step 5: Commit.**

```bash
git add internal/kms/instrumented_provider.go internal/kms/instrumented_provider_test.go
git commit -m "feat(kms): add instrumentedProvider decorator + error classifier"
```

---

## Task 3: Plumb metrics through `kms.NewProvider`

**Files:**
- Modify: `internal/kms/provider.go`

- [ ] **Step 1: Change `NewProvider` signature** to accept a metrics pointer and return the decorated provider. Replace the existing function body:

```go
// NewProvider creates a new provider based on the configuration. If m is non-nil,
// the returned provider is wrapped with instrumentation that emits KMS metrics.
func NewProvider(ctx context.Context, cfg *ProviderConfig, m *metrics.Metrics) (Provider, error) {
	var (
		inner Provider
		err   error
	)
	switch cfg.Type {
	case ProviderTypeAWSKMS:
		inner, err = NewAWSKMSProvider(ctx, cfg.Config)
	case ProviderTypeAzureKV:
		inner, err = NewAzureKeyVaultProvider(ctx, cfg.Config)
	case ProviderTypeCustom:
		inner, err = NewCustomProvider(ctx, cfg.Config)
	case ProviderTypeLocal:
		inner, err = NewLocalProvider(ctx, cfg.Config)
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", cfg.Type)
	}
	if err != nil {
		return nil, err
	}
	return newInstrumentedProvider(inner, m), nil
}
```

Add the import:

```go
"github.com/prysmsh/warp-storage-engine/internal/metrics"
```

- [ ] **Step 2: Update all callers** to pass a metrics argument. Find them:

```bash
grep -rn "kms.NewProvider(" --include="*.go"
```

Expected hits (paths and the change at each):

- `internal/encryption/factory.go:39` — currently:
  ```go
  kmsProvider, err := kms.NewProvider(ctx, &kms.ProviderConfig{...})
  ```
  Change to:
  ```go
  kmsProvider, err := kms.NewProvider(ctx, &kms.ProviderConfig{...}, m)
  ```
  where `m` is the metrics field on the encryption factory (Task 5 will add it; for now, pass `nil` and a follow-up task will replace).

  In this task: pass `nil` only.

- Any test file: pass `nil`.

- [ ] **Step 3: Build to verify the only call sites changed.**

```bash
go build ./...
```

If the build still fails on `kms.NewProvider`, list the offending call sites and update each to pass `nil` as the third argument. (No new behavior — these are getting wired in Task 5.)

- [ ] **Step 4: Run kms tests.**

```bash
go test ./internal/kms/...
```

Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/kms/provider.go internal/encryption/factory.go
git commit -m "feat(kms): plumb metrics through NewProvider"
```

(If the build required edits to other call sites, include those files in the commit.)

---

## Task 4: Wire DEK cache hit/miss metrics

**Files:**
- Modify: `internal/kms/cache.go`
- Modify: `internal/kms/cache_test.go` (create if absent)

- [ ] **Step 1: Write failing tests** in `internal/kms/cache_test.go` (append, or create file with this content if absent):

```go
package kms

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/prysmsh/warp-storage-engine/internal/metrics"
)

func TestDataKeyCache_MetricsHitMiss(t *testing.T) {
	m := metrics.NewMetrics("test_dek_cache")
	c := NewDataKeyCache(time.Minute, m)
	defer c.Close()

	if got := c.Get("missing"); got != nil {
		t.Fatal("expected miss")
	}
	c.Put("present", &DataKey{KeyID: "k"})
	if got := c.Get("present"); got == nil {
		t.Fatal("expected hit")
	}

	if got := testutil.ToFloat64(m.KMSCacheHits.WithLabelValues("dek")); got != 1 {
		t.Errorf("hits = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.KMSCacheMisses.WithLabelValues("dek")); got != 1 {
		t.Errorf("misses = %v, want 1", got)
	}
}

func TestDataKeyCache_NilMetricsIsNoop(t *testing.T) {
	c := NewDataKeyCache(time.Minute, nil)
	defer c.Close()

	c.Put("k", &DataKey{KeyID: "k"})
	if got := c.Get("k"); got == nil {
		t.Fatal("expected hit")
	}
	if got := c.Get("missing"); got != nil {
		t.Fatal("expected miss")
	}
}
```

- [ ] **Step 2: Run test, expect FAIL** ("too many arguments to NewDataKeyCache").

```bash
go test ./internal/kms/... -run TestDataKeyCache_
```

- [ ] **Step 3: Modify `DataKeyCache`** in `internal/kms/cache.go`:

Add a metrics field to the struct:

```go
type DataKeyCache struct {
	// ... existing fields ...
	metrics *metrics.Metrics
}
```

Add the import:

```go
"github.com/prysmsh/warp-storage-engine/internal/metrics"
```

Change `NewDataKeyCache`:

```go
// NewDataKeyCache creates a new data key cache. m may be nil.
func NewDataKeyCache(ttl time.Duration, m *metrics.Metrics) *DataKeyCache {
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	c := &DataKeyCache{
		cache:   make(map[string]*cacheEntry), // existing field name preserved
		ttl:     ttl,
		stop:    make(chan struct{}),
		metrics: m,
	}
	go c.cleanupLoop()
	return c
}
```

(If the existing struct uses different field names, preserve them; only add `metrics`.)

In `Get`, emit hit/miss:

```go
func (c *DataKeyCache) Get(key string) *DataKey {
	c.mu.RLock()
	entry, ok := c.cache[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		c.metrics.IncKMSCacheMiss("dek")
		return nil
	}
	c.metrics.IncKMSCacheHit("dek")
	return entry.dataKey
}
```

(Adapt the body to whatever the existing implementation does — only the two `c.metrics.Inc...` lines are new, and `c.metrics` is nil-safe via the helper.)

- [ ] **Step 4: Update all `NewDataKeyCache` call sites** to pass a metrics argument:

```bash
grep -rn "NewDataKeyCache(" --include="*.go"
```

For each site, pass `nil` unless we already have a metrics in scope (most don't yet — Task 5 plumbs it through). Tests in this file pass an explicit metrics; existing tests pass `nil`.

- [ ] **Step 5: Run test, expect PASS.**

```bash
go test ./internal/kms/...
```

- [ ] **Step 6: Commit.**

```bash
git add internal/kms/cache.go internal/kms/cache_test.go
# include any other files where call sites were updated
git commit -m "feat(kms): emit DEK cache hit/miss metrics from cache.Get"
```

---

## Task 5: Plumb metrics through `encryption.NewFromConfig` and on to `Manager`

**Files:**
- Modify: `internal/encryption/manager.go`
- Modify: `internal/encryption/factory.go`
- Modify: `internal/encryption/factory_test.go`
- Modify: `internal/proxy/server.go`

- [ ] **Step 1: Add a `metrics` field to `Manager`** in `internal/encryption/manager.go`:

```go
import (
	// existing imports
	"github.com/prysmsh/warp-storage-engine/internal/metrics"
	"github.com/prysmsh/warp-storage-engine/internal/encryption/types"
)

type Manager struct {
	keyProvider types.KeyProvider
	encryptor   types.Encryptor
	enabled     bool
	metrics     *metrics.Metrics
}

// NewManager creates a new encryption manager. m may be nil.
func NewManager(keyProvider types.KeyProvider, encryptor types.Encryptor, enabled bool, m *metrics.Metrics) *Manager {
	return &Manager{
		keyProvider: keyProvider,
		encryptor:   encryptor,
		enabled:     enabled,
		metrics:     m,
	}
}
```

- [ ] **Step 2: Change `NewFromConfig` signature** in `internal/encryption/factory.go`:

```go
func NewFromConfig(ctx context.Context, cfg *config.EncryptionConfig, m *metrics.Metrics) (*Manager, error) {
	if !cfg.Enabled {
		return &Manager{enabled: false, metrics: m}, nil
	}

	keyProvider, err := createKeyProvider(ctx, cfg, m)
	if err != nil {
		return nil, fmt.Errorf("failed to create key provider: %w", err)
	}

	encryptor, err := createEncryptor(cfg, keyProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	return NewManager(keyProvider, encryptor, true, m), nil
}
```

Update `createKeyProvider` and `createKMSProvider` to take and forward `m`:

```go
func createKeyProvider(ctx context.Context, cfg *config.EncryptionConfig, m *metrics.Metrics) (types.KeyProvider, error) {
	if providerName := cfg.KeyProvider; providerName != "" {
		if namedProvider, ok := cfg.KeyProviders[providerName]; ok {
			return createKMSProvider(ctx, &namedProvider, m)
		}
	}
	switch cfg.KeyProvider {
	// ... existing cases — replace each `createKMSProvider(ctx, providerCfg)` with
	// `createKMSProvider(ctx, providerCfg, m)`. The "local" case stays unchanged
	// because it builds a non-KMS local key provider directly.
	}
}

func createKMSProvider(ctx context.Context, cfg *config.ProviderConfig, m *metrics.Metrics) (types.KeyProvider, error) {
	kmsProvider, err := kms.NewProvider(ctx, &kms.ProviderConfig{
		Type:   kms.ProviderType(cfg.Type),
		Config: cfg.Config,
	}, m)
	if err != nil {
		return nil, err
	}
	return NewKMSKeyProviderAdapter(kmsProvider), nil
}
```

Add the import:

```go
"github.com/prysmsh/warp-storage-engine/internal/metrics"
```

- [ ] **Step 3: Update `internal/encryption/factory_test.go`** — every `NewFromConfig(...)` call gets `nil` as the third arg. Run:

```bash
grep -n "NewFromConfig(" internal/encryption/factory_test.go
```

Replace each with `..., nil)`.

- [ ] **Step 4: Update `internal/proxy/server.go`** — pass `s.metrics` (which is constructed later in the function today; we need it earlier). Restructure so the metrics is built before the encryption block:

Find the existing block:

```go
	// Wrap with encryption if enabled. ...
	if cfg.Encryption.Enabled {
		ctx := context.Background()
		encManager, err := encryption.NewFromConfig(ctx, &cfg.Encryption)
```

Change to:

```go
	// Build metrics early so KMS / encryption can record into the shared registry.
	metricsHandle := metrics.NewMetrics("foundation_storage_engine")

	if cfg.Encryption.Enabled {
		ctx := context.Background()
		encManager, err := encryption.NewFromConfig(ctx, &cfg.Encryption, metricsHandle)
```

Then later, when the `Server` struct is constructed, replace the
`metrics: metrics.NewMetrics(...)` field with `metrics: metricsHandle`.
(`NewMetrics` is a `sync.Once` singleton internally, so calling it twice
returns the same instance, but using the same handle keeps things
explicit.)

Add the import if missing:

```go
"github.com/prysmsh/warp-storage-engine/internal/metrics"
```

- [ ] **Step 5: Build everything.**

```bash
go build ./...
```

Expected: clean build.

- [ ] **Step 6: Run affected tests.**

```bash
go test ./internal/encryption/... ./internal/kms/... ./internal/proxy/...
```

- [ ] **Step 7: Commit.**

```bash
git add internal/encryption/manager.go internal/encryption/factory.go internal/encryption/factory_test.go internal/proxy/server.go
git commit -m "feat(encryption): plumb metrics through NewFromConfig and Manager"
```

---

## Task 6: Object-layer metrics in `encryption.Manager`

**Files:**
- Modify: `internal/encryption/manager.go`
- Modify: `internal/encryption/manager_test.go`

- [ ] **Step 1: Write failing tests** in `internal/encryption/manager_test.go` (append):

```go
func TestManager_ObjectMetrics_Encrypt(t *testing.T) {
	m := metrics.NewMetrics("test_obj_enc_mgr_e")
	mgr := buildTestManager(t, m) // helper using local provider; see below

	plaintext := []byte("hello-metrics")
	encReader, encMeta, err := mgr.Encrypt(context.Background(), bytes.NewReader(plaintext), int64(len(plaintext)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := io.ReadAll(encReader); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = encMeta

	if got := testutil.ToFloat64(m.EncryptionObjectOps.WithLabelValues("encrypt", "success")); got != 1 {
		t.Errorf("encrypt success = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EncryptionObjectBytes.WithLabelValues("encrypt")); got != float64(len(plaintext)) {
		t.Errorf("encrypt bytes = %v, want %d", got, len(plaintext))
	}
	if got := testutil.CollectAndCount(m.EncryptionObjectDuration); got < 1 {
		t.Errorf("duration not observed")
	}
}

func TestManager_ObjectMetrics_DisabledPassthrough(t *testing.T) {
	m := metrics.NewMetrics("test_obj_enc_mgr_d")
	mgr := NewManager(nil, nil, false, m)

	_, _, _ = mgr.Encrypt(context.Background(), bytes.NewReader([]byte("x")), 1)
	if got := testutil.ToFloat64(m.EncryptionDisabledPassthrough); got != 1 {
		t.Errorf("passthrough = %v, want 1", got)
	}
}
```

`buildTestManager` may need adding to the test file — make it construct a `Manager` from a local (in-memory master key) provider, mirroring the pattern in `factory_test.go` `TestNewFromConfig_LocalProvider`.

- [ ] **Step 2: Run, expect FAIL.**

```bash
go test ./internal/encryption/... -run "TestManager_ObjectMetrics"
```

- [ ] **Step 3: Wire metrics in `Manager.Encrypt`** in `internal/encryption/manager.go`:

```go
func (m *Manager) Encrypt(ctx context.Context, reader io.Reader, size int64) (io.Reader, map[string]string, error) {
	if !m.enabled {
		m.metrics.IncEncryptionDisabledPassthrough()
		return reader, nil, nil
	}

	start := time.Now()
	encReader, meta, err := m.encryptor.Encrypt(ctx, reader, size)
	m.metrics.ObserveEncryptionObjectDuration("encrypt", time.Since(start))
	if err != nil {
		m.metrics.IncEncryptionObjectOp("encrypt", "error")
		return encReader, meta, err
	}
	m.metrics.IncEncryptionObjectOp("encrypt", "success")
	if size > 0 {
		m.metrics.IncEncryptionObjectBytes("encrypt", size)
	}
	return encReader, meta, nil
}
```

And in `Manager.Decrypt`:

```go
func (m *Manager) Decrypt(ctx context.Context, reader io.Reader, metadata map[string]string) (io.Reader, error) {
	if metadata[types.MetadataKeyAlgorithm] == "" {
		m.metrics.IncEncryptionDisabledPassthrough()
		return reader, nil
	}
	if !m.enabled {
		return nil, fmt.Errorf("encryption is disabled but object is encrypted")
	}

	start := time.Now()
	decReader, err := m.encryptor.Decrypt(ctx, reader, metadata)
	m.metrics.ObserveEncryptionObjectDuration("decrypt", time.Since(start))
	if err != nil {
		m.metrics.IncEncryptionObjectOp("decrypt", "error")
		return decReader, err
	}
	m.metrics.IncEncryptionObjectOp("decrypt", "success")
	if encSize, ok := metadata[types.MetadataKeyEncryptedSize]; ok {
		if n, parseErr := strconv.ParseInt(encSize, 10, 64); parseErr == nil && n > 0 {
			m.metrics.IncEncryptionObjectBytes("decrypt", n)
		}
	}
	return decReader, nil
}
```

Add imports:

```go
"strconv"
"time"
```

- [ ] **Step 4: Run, expect PASS.**

```bash
go test ./internal/encryption/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/encryption/manager.go internal/encryption/manager_test.go
git commit -m "feat(encryption): emit object-layer metrics in Manager.Encrypt/Decrypt"
```

---

## Task 7: Validation script — Check 11 (metrics coverage)

**Files:**
- Create: `scripts/encryption-validation/11-metrics-coverage.sh`
- Modify: `scripts/encryption-validation/run-all.sh`
- Modify: `docs/superpowers/findings/2026-05-07-encryption-validation-report.md`

- [ ] **Step 1: Write the script.** Create `scripts/encryption-validation/11-metrics-coverage.sh`:

```bash
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

FAILED=()
[[ -n "${OBJ_ENC}" && "${OBJ_ENC%.*}" -ge "${N}" ]] || FAILED+=("encryption_object_ops_total{encrypt,success} (${OBJ_ENC:-missing})")
[[ -n "${OBJ_DEC}" && "${OBJ_DEC%.*}" -ge "${N}" ]] || FAILED+=("encryption_object_ops_total{decrypt,success} (${OBJ_DEC:-missing})")
[[ -n "${OBJ_ENC_BYTES}" && "${OBJ_ENC_BYTES%.*}" -gt 0 ]] || FAILED+=("encryption_object_bytes_total{encrypt} (${OBJ_ENC_BYTES:-missing})")
[[ -n "${KMS_GEN}" && "${KMS_GEN%.*}" -ge 1 ]] || FAILED+=("kms_operations_total{generate_data_key,success} (${KMS_GEN:-missing})")
[[ -n "${KMS_DEC}" && "${KMS_DEC%.*}" -ge 1 ]] || FAILED+=("kms_operations_total{decrypt,success} (${KMS_DEC:-missing})")
[[ -n "${KMS_HIT}${KMS_MISS}" ]] || FAILED+=("kms_cache_hits/misses (both missing)")

if [[ ${#FAILED[@]} -eq 0 ]]; then
  STATUS="PASS"
  BODY="All expected metrics advanced for ${N} encrypts + ${N} decrypts. Sample values: obj_enc=${OBJ_ENC}, obj_dec=${OBJ_DEC}, obj_bytes=${OBJ_ENC_BYTES}, kms_gen=${KMS_GEN}, kms_dec=${KMS_DEC}, hits=${KMS_HIT:-0}, misses=${KMS_MISS:-0}."
else
  STATUS="FAIL"
  BODY="Missing or insufficient metrics:"$'\n'"$(printf -- '- %s\n' "${FAILED[@]}")"
fi

report_section "Check 11 — Metrics coverage" "${STATUS}" "${BODY}"
echo "Check 11: ${STATUS}"
```

- [ ] **Step 2: Update `run-all.sh`** to include the new check. Replace the loop in `scripts/encryption-validation/run-all.sh`:

```bash
for n in 01 02 03 04 05 06 07 08 09 10 11; do
```

- [ ] **Step 3: Update the validation report**. In `docs/superpowers/findings/2026-05-07-encryption-validation-report.md` Summary table, append:

```markdown
| 11 | Metrics coverage | (run after Phase 2 metrics work) |
```

(The script appends its own Check 11 section when run; this row sets up the table.)

- [ ] **Step 4: Make executable + commit.**

```bash
chmod +x scripts/encryption-validation/11-metrics-coverage.sh
git add scripts/encryption-validation/11-metrics-coverage.sh scripts/encryption-validation/run-all.sh docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "test(encryption): add Check 11 - metrics coverage script"
```

---

## Task 8: End-to-end verification

**Files:** none (verification only).

- [ ] **Step 1: Bring up the test stack** (assumes Docker is running):

```bash
bash scripts/encryption-validation/00-setup.sh
```

- [ ] **Step 2: Run the full suite**.

```bash
bash scripts/encryption-validation/run-all.sh
```

Expected: Checks 1–8 PASS, Check 9 REVIEW, Check 10 PASS, Check 11 PASS.

- [ ] **Step 3: Spot-check the metrics endpoint** for new and previously-empty metrics:

```bash
curl -s http://localhost:8090/metrics | grep -E '^(encryption_object_|kms_(operations|cache_(hits|misses)|errors|key_validations)_total)' | head -30
```

Expected: non-zero values for encrypt + decrypt under the labels that were exercised.

- [ ] **Step 4: Tear down**.

```bash
docker compose -f docker-compose.encryption-test.yml --env-file .env.encryption-test down -v
```

- [ ] **Step 5: Append re-run summary to the report.** In
  `docs/superpowers/findings/2026-05-07-encryption-validation-report.md`,
  add a "Phase 2 — metrics" section noting Check 11 added and PASS, and
  Check 8 status updated from "PASS (1 metric only)" to "PASS (full
  metric set)".

- [ ] **Step 6: Commit final updates.**

```bash
git add docs/superpowers/findings/2026-05-07-encryption-validation-report.md
git commit -m "docs(encryption): record Phase 2 metrics validation results"
```

---

## Out of Scope

The two remaining Phase 2 items (`pkg/vector` encryption wiring; data-plane key rotation) get separate specs and plans. Once they exist, this metric set will already be in place to observe their behavior.
