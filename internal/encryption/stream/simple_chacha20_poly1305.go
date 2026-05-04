package stream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/prysmsh/warp-storage-engine/internal/encryption/types"
)

// SimpleChaCha20Poly1305Encryptor implements ChaCha20-Poly1305 encryption with
// the same in-memory approach as SimpleAESGCMEncryptor. The key is a 32-byte
// DEK and the nonce is 12 bytes (RFC 7539).
type SimpleChaCha20Poly1305Encryptor struct {
	keyProvider   types.KeyProvider
	maxMemorySize int64
}

// NewSimpleChaCha20Poly1305Encryptor creates a new ChaCha20-Poly1305 encryptor.
func NewSimpleChaCha20Poly1305Encryptor(keyProvider types.KeyProvider) *SimpleChaCha20Poly1305Encryptor {
	return &SimpleChaCha20Poly1305Encryptor{
		keyProvider:   keyProvider,
		maxMemorySize: 100 * 1024 * 1024,
	}
}

// Encrypt wraps a reader with ChaCha20-Poly1305 encryption.
func (e *SimpleChaCha20Poly1305Encryptor) Encrypt(ctx context.Context, reader io.Reader, size int64) (io.Reader, map[string]string, error) {
	dek, encryptedDEK, err := e.keyProvider.GenerateDEK(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate DEK: %w", err)
	}

	aead, err := chacha20poly1305.New(dek)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create ChaCha20-Poly1305 AEAD: %w", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, randErr := rand.Read(nonce); randErr != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", randErr)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read data: %w", err)
	}

	plaintextSize := int64(len(data))
	if size < 0 || size > e.maxMemorySize {
		// Match the AES-GCM behavior: large/unknown sizes still get processed
		// in memory until proper streaming support lands.
		size = plaintextSize
	}

	encrypted := aead.Seal(nil, nonce, data, nil)

	metadata := map[string]string{
		types.MetadataKeyAlgorithm:     string(types.AlgorithmChaCha20Poly1305),
		types.MetadataKeyID:            e.keyProvider.Name(),
		types.MetadataKeyEncryptedDEK:  encryptedDEK,
		types.MetadataKeyNonce:         base64.StdEncoding.EncodeToString(nonce),
		types.MetadataKeyEncryptedSize: strconv.FormatInt(plaintextSize, 10),
	}

	return bytes.NewReader(encrypted), metadata, nil
}

// Decrypt wraps a reader with ChaCha20-Poly1305 decryption.
func (e *SimpleChaCha20Poly1305Encryptor) Decrypt(ctx context.Context, reader io.Reader, metadata map[string]string) (io.Reader, error) {
	encryptedDEK := metadata[types.MetadataKeyEncryptedDEK]
	if encryptedDEK == "" {
		return nil, fmt.Errorf("missing encrypted DEK in metadata")
	}

	nonceStr := metadata[types.MetadataKeyNonce]
	if nonceStr == "" {
		return nil, fmt.Errorf("missing nonce in metadata")
	}

	nonce, err := base64.StdEncoding.DecodeString(nonceStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode nonce: %w", err)
	}

	dek, err := e.keyProvider.DecryptDEK(ctx, encryptedDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}

	aead, err := chacha20poly1305.New(dek)
	if err != nil {
		return nil, fmt.Errorf("failed to create ChaCha20-Poly1305 AEAD: %w", err)
	}

	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size: got %d, want %d", len(nonce), aead.NonceSize())
	}

	encrypted, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read encrypted data: %w", err)
	}

	decrypted, err := aead.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt data: %w", err)
	}

	return bytes.NewReader(decrypted), nil
}

// Algorithm returns the encryption algorithm.
func (e *SimpleChaCha20Poly1305Encryptor) Algorithm() types.Algorithm {
	return types.AlgorithmChaCha20Poly1305
}
