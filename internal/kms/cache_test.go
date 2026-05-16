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
