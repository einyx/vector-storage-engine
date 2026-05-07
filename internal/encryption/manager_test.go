package encryption

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/prysmsh/warp-storage-engine/internal/config"
	"github.com/prysmsh/warp-storage-engine/internal/encryption/types"
	"github.com/prysmsh/warp-storage-engine/internal/metrics"
)

func TestNewManager(t *testing.T) {
	m := NewManager(nil, nil, false, nil)
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.IsEnabled() {
		t.Error("expected disabled")
	}
	if m.GetKeyProvider() != nil {
		t.Error("GetKeyProvider should be nil")
	}
	if m.GetEncryptor() != nil {
		t.Error("GetEncryptor should be nil")
	}
}

func TestManager_Encrypt_Disabled(t *testing.T) {
	m := NewManager(nil, nil, false, nil)
	r := bytes.NewReader([]byte("data"))
	out, meta, err := m.Encrypt(context.Background(), r, 4)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if out != r {
		t.Error("Encrypt(disabled) should return original reader")
	}
	if meta != nil {
		t.Error("Encrypt(disabled) should return nil metadata")
	}
}

func TestManager_Decrypt_NotEncrypted(t *testing.T) {
	m := NewManager(nil, nil, true, nil)
	r := bytes.NewReader([]byte("data"))
	out, err := m.Decrypt(context.Background(), r, nil)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if out != r {
		t.Error("Decrypt(no metadata) should return original reader")
	}
	out2, err := m.Decrypt(context.Background(), r, map[string]string{})
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if out2 != r {
		t.Error("Decrypt(empty metadata) should return original reader")
	}
}

func TestManager_Decrypt_DisabledButEncrypted(t *testing.T) {
	m := NewManager(nil, nil, false, nil)
	r := bytes.NewReader([]byte("x"))
	meta := map[string]string{types.MetadataKeyAlgorithm: "AES-256-GCM"}
	_, err := m.Decrypt(context.Background(), r, meta)
	if err == nil {
		t.Fatal("Decrypt(disabled, encrypted) expected error")
	}
}

func TestManager_ShouldEncrypt(t *testing.T) {
	m := NewManager(nil, nil, false, nil)
	if m.ShouldEncrypt("bucket") {
		t.Error("ShouldEncrypt(disabled) should be false")
	}
	m2 := NewManager(nil, nil, true, nil)
	if !m2.ShouldEncrypt("bucket") {
		t.Error("ShouldEncrypt(enabled) should be true")
	}
}

func TestManager_GetKeyProvider_GetEncryptor(t *testing.T) {
	m := NewManager(nil, nil, true, nil)
	if m.GetKeyProvider() != nil {
		t.Error("expected nil key provider")
	}
	if m.GetEncryptor() != nil {
		t.Error("expected nil encryptor")
	}
}

func buildTestManager(t *testing.T, m *metrics.Metrics) *Manager {
	t.Helper()
	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cfg := &config.EncryptionConfig{
		Enabled:     true,
		Algorithm:   "AES-256-GCM",
		KeyProvider: "local",
		Local:       &config.LocalKeyConfig{MasterKey: masterKey},
	}
	mgr, err := NewFromConfig(context.Background(), cfg, m)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	return mgr
}

func TestManager_ObjectMetrics_Encrypt(t *testing.T) {
	m := metrics.NewMetrics("test_obj_enc_mgr_e")
	mgr := buildTestManager(t, m)

	plaintext := []byte("hello-metrics")
	encReader, _, err := mgr.Encrypt(context.Background(), bytes.NewReader(plaintext), int64(len(plaintext)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := io.ReadAll(encReader); err != nil {
		t.Fatalf("read: %v", err)
	}

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
