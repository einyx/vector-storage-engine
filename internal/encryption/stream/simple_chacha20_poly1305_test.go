package stream

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"strings"
	"testing"

	"github.com/prysmsh/warp-storage-engine/internal/encryption/types"
)

func newChaChaKeyProvider(t *testing.T) *mockKeyProvider {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return &mockKeyProvider{name: "chacha-test-key", dek: key}
}

func TestNewSimpleChaCha20Poly1305Encryptor(t *testing.T) {
	kp := newChaChaKeyProvider(t)
	e := NewSimpleChaCha20Poly1305Encryptor(kp)
	if e == nil {
		t.Fatal("constructor returned nil")
	}
	if e.Algorithm() != types.AlgorithmChaCha20Poly1305 {
		t.Errorf("algorithm = %s, want %s", e.Algorithm(), types.AlgorithmChaCha20Poly1305)
	}
	if e.keyProvider != kp {
		t.Error("key provider not stored")
	}
}

func TestSimpleChaCha20Poly1305_Roundtrip(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"small", []byte("hello, chacha")},
		{"binary", bytes.Repeat([]byte{0x00, 0xff, 0x42}, 1024)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kp := newChaChaKeyProvider(t)
			e := NewSimpleChaCha20Poly1305Encryptor(kp)

			encReader, metadata, err := e.Encrypt(context.Background(), bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}

			if metadata[types.MetadataKeyAlgorithm] != string(types.AlgorithmChaCha20Poly1305) {
				t.Errorf("algorithm metadata = %q", metadata[types.MetadataKeyAlgorithm])
			}
			for _, key := range []string{types.MetadataKeyEncryptedDEK, types.MetadataKeyNonce, types.MetadataKeyID} {
				if metadata[key] == "" {
					t.Errorf("missing metadata key %q", key)
				}
			}

			ciphertext, err := io.ReadAll(encReader)
			if err != nil {
				t.Fatalf("read ciphertext: %v", err)
			}
			if len(tc.data) > 0 && bytes.Equal(ciphertext, tc.data) {
				t.Error("ciphertext equals plaintext")
			}

			decReader, err := e.Decrypt(context.Background(), bytes.NewReader(ciphertext), metadata)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			plaintext, err := io.ReadAll(decReader)
			if err != nil {
				t.Fatalf("read plaintext: %v", err)
			}
			if !bytes.Equal(plaintext, tc.data) {
				t.Errorf("roundtrip mismatch: got %q, want %q", plaintext, tc.data)
			}
		})
	}
}

func TestSimpleChaCha20Poly1305_TamperedCiphertextFails(t *testing.T) {
	kp := newChaChaKeyProvider(t)
	e := NewSimpleChaCha20Poly1305Encryptor(kp)

	encReader, metadata, err := e.Encrypt(context.Background(), strings.NewReader("authenticated payload"), -1)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	ciphertext[0] ^= 0x01
	if _, err := e.Decrypt(context.Background(), bytes.NewReader(ciphertext), metadata); err == nil {
		t.Fatal("expected decrypt to fail on tampered ciphertext")
	}
}

func TestSimpleChaCha20Poly1305_MissingMetadata(t *testing.T) {
	kp := newChaChaKeyProvider(t)
	e := NewSimpleChaCha20Poly1305Encryptor(kp)

	if _, err := e.Decrypt(context.Background(), strings.NewReader(""), map[string]string{}); err == nil {
		t.Error("expected error for missing DEK metadata")
	}
	if _, err := e.Decrypt(context.Background(), strings.NewReader(""), map[string]string{
		types.MetadataKeyEncryptedDEK: "x",
	}); err == nil {
		t.Error("expected error for missing nonce metadata")
	}
}
