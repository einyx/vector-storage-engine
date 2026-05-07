package kms

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: &Config{
				Enabled:      true,
				DefaultKeyID: "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
				KeySpec:      types.DataKeySpecAes256,
				Region:       "us-east-1",
				EncryptionContext: map[string]string{
					"app": "warp-storage-engine",
				},
				DataKeyCacheTTL: 5 * time.Minute,
			},
			wantErr: false,
		},
		{
			name: "disabled config",
			config: &Config{
				Enabled: false,
			},
			wantErr: false,
		},
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(context.Background(), tt.config)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetEncryptionHeaders(t *testing.T) {
	// Create a mock KMS client to simulate enabled state
	mockClient := &kms.Client{}

	manager := &Manager{
		config: &Config{
			Enabled:      true,
			DefaultKeyID: "alias/default-key",
			EncryptionContext: map[string]string{
				"app": "warp-storage-engine",
			},
		},
		client: mockClient, // Set non-nil client to pass IsEnabled check
	}

	tests := []struct {
		name         string
		bucket       string
		bucketConfig *BucketKMSConfig
		wantHeaders  map[string]string
	}{
		{
			name:   "default key",
			bucket: "test-bucket",
			wantHeaders: map[string]string{
				"x-amz-server-side-encryption":                "aws:kms",
				"x-amz-server-side-encryption-aws-kms-key-id": "alias/default-key",
				"x-amz-server-side-encryption-context":        "app=warp-storage-engine",
			},
		},
		{
			name:   "bucket-specific key",
			bucket: "secure-bucket",
			bucketConfig: &BucketKMSConfig{
				KeyID: "alias/secure-bucket-key",
				EncryptionContext: map[string]string{
					"bucket": "secure-bucket",
					"env":    "prod",
				},
				OverrideDefault: true,
			},
			wantHeaders: map[string]string{
				"x-amz-server-side-encryption":                "aws:kms",
				"x-amz-server-side-encryption-aws-kms-key-id": "alias/secure-bucket-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := manager.GetEncryptionHeaders(tt.bucket, tt.bucketConfig)

			// Check required headers
			assert.Equal(t, tt.wantHeaders["x-amz-server-side-encryption"], headers["x-amz-server-side-encryption"])
			assert.Equal(t, tt.wantHeaders["x-amz-server-side-encryption-aws-kms-key-id"], headers["x-amz-server-side-encryption-aws-kms-key-id"])

			// Check encryption context if present
			if _, ok := tt.wantHeaders["x-amz-server-side-encryption-context"]; ok {
				assert.NotEmpty(t, headers["x-amz-server-side-encryption-context"])
			}
		})
	}
}

func TestGetEncryptionHeaders_NotEnabled(t *testing.T) {
	mgr, _ := New(context.Background(), &Config{Enabled: false})
	headers := mgr.GetEncryptionHeaders("bucket", nil)
	assert.Nil(t, headers)
}

func TestGetEncryptionHeaders_EmptyDefaultKeyID(t *testing.T) {
	manager := &Manager{
		config: &Config{
			Enabled:      true,
			DefaultKeyID: "", // no default key
			EncryptionContext: map[string]string{"app": "test"},
		},
		client: &kms.Client{},
	}
	headers := manager.GetEncryptionHeaders("b", nil)
	assert.NotNil(t, headers)
	assert.Equal(t, "aws:kms", headers["x-amz-server-side-encryption"])
	// No key id header when DefaultKeyID empty
	_, hasKey := headers["x-amz-server-side-encryption-aws-kms-key-id"]
	assert.False(t, hasKey)
}

func TestDataKeyCache(t *testing.T) {
	cache := NewDataKeyCache(100 * time.Millisecond)
	defer cache.Close()

	dataKey := &DataKey{
		KeyID:          "test-key",
		PlaintextKey:   []byte("test-plaintext-key"),
		CiphertextBlob: []byte("test-ciphertext"),
		EncryptionContext: map[string]string{
			"test": "context",
		},
		CreatedAt: time.Now(),
	}

	// Test Put and Get
	cacheKey := buildDataKeyCacheKey("test-key", dataKey.EncryptionContext)
	cache.Put(cacheKey, dataKey)

	retrieved := cache.Get(cacheKey)
	require.NotNil(t, retrieved)
	assert.Equal(t, dataKey.KeyID, retrieved.KeyID)

	// Test expiration
	time.Sleep(150 * time.Millisecond)
	expired := cache.Get(cacheKey)
	assert.Nil(t, expired)

	// Test cleanup
	cache.Clear() // Clear cache first to ensure clean state
	cache.Put("key1", dataKey)
	cache.Put("key2", dataKey)
	assert.Equal(t, 2, cache.Size())

	cache.Clear()
	assert.Equal(t, 0, cache.Size())
}

func TestSerializeEncryptionContext(t *testing.T) {
	tests := []struct {
		name     string
		context  map[string]string
		expected string
		notEmpty bool
	}{
		{
			name:     "empty context",
			context:  map[string]string{},
			expected: "",
		},
		{
			name: "single key",
			context: map[string]string{
				"bucket": "test",
			},
			notEmpty: true,
		},
		{
			name: "multiple keys",
			context: map[string]string{
				"bucket": "test",
				"env":    "prod",
			},
			notEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := serializeEncryptionContext(tt.context)
			if tt.expected != "" {
				assert.Equal(t, tt.expected, result)
			} else if tt.notEmpty {
				assert.NotEmpty(t, result)
			} else {
				assert.Empty(t, result)
			}
		})
	}
}

func TestKMSError(t *testing.T) {
	err := &KMSError{
		Op:        "GenerateDataKey",
		KeyID:     "test-key",
		Err:       ErrInsufficientPermissions,
		Retryable: false,
	}

	assert.Contains(t, err.Error(), "GenerateDataKey")
	assert.Contains(t, err.Error(), "test-key")
	assert.Equal(t, ErrInsufficientPermissions, err.Unwrap())
	assert.False(t, err.IsRetryable())

	// Test IsKMSError
	assert.True(t, IsKMSError(err))
	assert.False(t, IsKMSError(ErrKMSNotEnabled))

	// Test WrapError
	wrapped := WrapError("Decrypt", "key-123", ErrKeyNotFound, true)
	assert.True(t, IsKMSError(wrapped))
	assert.True(t, IsRetryableError(wrapped))

	// WrapError with nil returns nil
	assert.Nil(t, WrapError("Op", "key", nil, false))

	// KMSError Error() with empty KeyID
	errNoKey := &KMSError{Op: "Validate", KeyID: "", Err: ErrKeyNotFound}
	assert.Contains(t, errNoKey.Error(), "kms Validate failed")

	// IsRetryableError with non-KMS error
	assert.False(t, IsRetryableError(ErrKMSNotEnabled))
}

func TestManager_IsEnabled(t *testing.T) {
	assert.False(t, (*Manager)(nil).IsEnabled())
	m := &Manager{config: &Config{Enabled: false}}
	assert.False(t, m.IsEnabled())
	m.config.Enabled = true
	assert.False(t, m.IsEnabled()) // no client
	m.client = &kms.Client{}
	assert.True(t, m.IsEnabled())
}

func TestManager_Close(t *testing.T) {
	// nil receiver
	(*Manager)(nil).Close()
	// with cache
	m := &Manager{config: &Config{Enabled: false}, dataKeyCache: NewDataKeyCache(time.Minute)}
	m.Close()
}

func TestManager_ValidateKey_NotEnabled(t *testing.T) {
	m, _ := New(context.Background(), &Config{Enabled: false})
	_, err := m.ValidateKey(context.Background(), "alias/my-key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kms is not enabled")
}

func TestManager_ValidateKey_EmptyKeyID(t *testing.T) {
	m := &Manager{config: &Config{Enabled: true}, client: &kms.Client{}}
	_, err := m.ValidateKey(context.Background(), "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "keyID cannot be empty")
}

func TestNewProvider(t *testing.T) {
	ctx := context.Background()
	// Local provider
	p, err := NewProvider(ctx, &ProviderConfig{Type: ProviderTypeLocal, Config: map[string]interface{}{
		"master_key": hex.EncodeToString(make([]byte, 32)),
	}}, nil)
	assert.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, string(ProviderTypeLocal), p.Name())

	// Unsupported type
	_, err = NewProvider(ctx, &ProviderConfig{Type: ProviderType("invalid")}, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported provider type")
}

func TestDataKeyCache_Delete_NilEmpty(t *testing.T) {
	cache := NewDataKeyCache(time.Minute)
	defer cache.Close()
	key := &DataKey{KeyID: "k", PlaintextKey: []byte("x")}
	cache.Put("k1", key)
	cache.Delete("k1")
	assert.Nil(t, cache.Get("k1"))
	cache.Delete("")
	cache.Delete("nonexistent")
}

func TestBuildDataKeyCacheKey(t *testing.T) {
	assert.Empty(t, buildDataKeyCacheKey("", nil))
	k := buildDataKeyCacheKey("key1", map[string]string{"a": "b"})
	assert.NotEmpty(t, k)
	assert.Len(t, k, 64) // sha256 hex
}
