// Package encryption provides encryption management for S3 objects
package encryption

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/prysmsh/warp-storage-engine/internal/encryption/types"
	"github.com/prysmsh/warp-storage-engine/internal/metrics"
)

// Manager coordinates encryption operations
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

// IsEnabled returns whether encryption is enabled
func (m *Manager) IsEnabled() bool {
	return m.enabled
}

// Encrypt wraps a reader with encryption if enabled
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

// Decrypt wraps a reader with decryption if needed
func (m *Manager) Decrypt(ctx context.Context, reader io.Reader, metadata map[string]string) (io.Reader, error) {
	// Check if object is encrypted
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

// ShouldEncrypt determines if an object should be encrypted based on bucket policies
func (m *Manager) ShouldEncrypt(_ string) bool {
	if !m.enabled {
		return false
	}

	// TODO: Implement bucket-specific policies
	// For now, encrypt everything when enabled
	return true
}

// GetKeyProvider returns the current key provider
func (m *Manager) GetKeyProvider() types.KeyProvider {
	return m.keyProvider
}

// GetEncryptor returns the current encryptor
func (m *Manager) GetEncryptor() types.Encryptor {
	return m.encryptor
}
