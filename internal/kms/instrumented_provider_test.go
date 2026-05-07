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
