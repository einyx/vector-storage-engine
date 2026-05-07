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
