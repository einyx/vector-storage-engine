package stream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/prysmsh/warp-storage-engine/internal/encryption/types"
)

const (
	// Plaintext per AES-GCM chunk when streaming (large objects / unknown size).
	aesChunkPlaintextMax = 256 << 10
	maxSealedChunkBytes  = aesChunkPlaintextMax + 512 // ciphertext + tag + slack
)

// SimpleAESGCMEncryptor implements AES-256-GCM encryption with a simpler approach
// It encrypts the entire data in memory for objects up to a certain size
type SimpleAESGCMEncryptor struct {
	keyProvider   types.KeyProvider
	maxMemorySize int64 // Maximum size to encrypt in memory (default: 100MB)
}

// NewSimpleAESGCMEncryptor creates a new simple AES-GCM encryptor
func NewSimpleAESGCMEncryptor(keyProvider types.KeyProvider) *SimpleAESGCMEncryptor {
	return &SimpleAESGCMEncryptor{
		keyProvider:   keyProvider,
		maxMemorySize: 100 * 1024 * 1024, // 100MB
	}
}

func writeAESChunk(w io.Writer, gcm cipher.AEAD, plain []byte) error {
	if len(plain) == 0 {
		return nil
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	if len(sealed) > maxSealedChunkBytes {
		return fmt.Errorf("sealed chunk exceeds limit")
	}
	hdr := make([]byte, 2+len(nonce)+4)
	binary.BigEndian.PutUint16(hdr[0:2], uint16(len(nonce)))
	copy(hdr[2:2+len(nonce)], nonce)
	binary.BigEndian.PutUint32(hdr[2+len(nonce):6+len(nonce)], uint32(len(sealed)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(sealed)
	return err
}

func encryptChunksToPipe(reader io.Reader, gcm cipher.AEAD, pw *io.PipeWriter) {
	go func() {
		defer pw.Close()
		buf := make([]byte, aesChunkPlaintextMax)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				if werr := writeAESChunk(pw, gcm, buf[:n]); werr != nil {
					_ = pw.CloseWithError(werr)
					return
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()
}

type aesChunkDecryptReader struct {
	r        io.Reader
	gcm      cipher.AEAD
	leftover []byte
}

func readOneOpenedChunk(gcm cipher.AEAD, r io.Reader) ([]byte, error) {
	var nlenBuf [2]byte
	if _, err := io.ReadFull(r, nlenBuf[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	nlen := int(binary.BigEndian.Uint16(nlenBuf[:]))
	if nlen != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid chunk nonce length %d", nlen)
	}
	nonce := make([]byte, nlen)
	if _, err := io.ReadFull(r, nonce); err != nil {
		return nil, err
	}
	var clenBuf [4]byte
	if _, err := io.ReadFull(r, clenBuf[:]); err != nil {
		return nil, err
	}
	clen := int(binary.BigEndian.Uint32(clenBuf[:]))
	if clen <= 0 || clen > maxSealedChunkBytes {
		return nil, fmt.Errorf("invalid sealed chunk size %d", clen)
	}
	sealed := make([]byte, clen)
	if _, err := io.ReadFull(r, sealed); err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, sealed, nil)
}

func (d *aesChunkDecryptReader) Read(p []byte) (int, error) {
	if len(d.leftover) > 0 {
		n := copy(p, d.leftover)
		d.leftover = d.leftover[n:]
		return n, nil
	}
	plain, err := readOneOpenedChunk(d.gcm, d.r)
	if err != nil {
		return 0, err
	}
	d.leftover = plain
	return d.Read(p)
}

// Encrypt wraps a reader with AES-GCM encryption
func (e *SimpleAESGCMEncryptor) Encrypt(ctx context.Context, reader io.Reader, size int64) (io.Reader, map[string]string, error) {
	dek, encryptedDEK, err := e.keyProvider.GenerateDEK(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate DEK: %w", err)
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	useStreaming := size < 0 || size > e.maxMemorySize
	if useStreaming {
		sizeStr := "-1"
		if size >= 0 {
			sizeStr = strconv.FormatInt(size, 10)
		}
		meta := map[string]string{
			types.MetadataKeyAlgorithm:       string(types.AlgorithmAES256GCM),
			types.MetadataKeyID:              e.keyProvider.Name(),
			types.MetadataKeyEncryptedDEK:    encryptedDEK,
			types.MetadataKeyEncryptedSize:   sizeStr,
			types.MetadataKeyStreamingFormat: types.StreamingFormatAESGCMChunkedV1,
		}

		pr, pw := io.Pipe()
		encryptChunksToPipe(reader, gcm, pw)
		return pr, meta, nil
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, randErr := rand.Read(nonce); randErr != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", randErr)
	}

	data, readErr := io.ReadAll(reader)
	if readErr != nil {
		return nil, nil, fmt.Errorf("failed to read data: %w", readErr)
	}

	encrypted := gcm.Seal(nil, nonce, data, nil)
	metadata := map[string]string{
		types.MetadataKeyAlgorithm:     string(types.AlgorithmAES256GCM),
		types.MetadataKeyID:            e.keyProvider.Name(),
		types.MetadataKeyEncryptedDEK:  encryptedDEK,
		types.MetadataKeyNonce:         base64.StdEncoding.EncodeToString(nonce),
		types.MetadataKeyEncryptedSize: strconv.FormatInt(int64(len(data)), 10),
	}

	return bytes.NewReader(encrypted), metadata, nil
}

// Decrypt wraps a reader with AES-GCM decryption
func (e *SimpleAESGCMEncryptor) Decrypt(ctx context.Context, reader io.Reader, metadata map[string]string) (io.Reader, error) {
	encryptedDEK := metadata[types.MetadataKeyEncryptedDEK]
	if encryptedDEK == "" {
		return nil, fmt.Errorf("missing encrypted DEK in metadata")
	}

	streaming := metadata[types.MetadataKeyStreamingFormat]
	var nonceStr string
	if streaming != types.StreamingFormatAESGCMChunkedV1 {
		nonceStr = metadata[types.MetadataKeyNonce]
		if nonceStr == "" {
			return nil, fmt.Errorf("missing nonce in metadata")
		}
	}

	dek, err := e.keyProvider.DecryptDEK(ctx, encryptedDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	if streaming == types.StreamingFormatAESGCMChunkedV1 {
		return &aesChunkDecryptReader{r: reader, gcm: gcm}, nil
	}

	nonce, err := base64.StdEncoding.DecodeString(nonceStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode nonce: %w", err)
	}

	var originalSize int64 = -1
	if sizeStr := metadata[types.MetadataKeyEncryptedSize]; sizeStr != "" {
		if parsedSize, parseErr := strconv.ParseInt(sizeStr, 10, 64); parseErr == nil {
			originalSize = parsedSize
		}
	}

	if originalSize >= 0 && originalSize <= e.maxMemorySize {
		encrypted, readErr := io.ReadAll(reader)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read encrypted data: %w", readErr)
		}

		decrypted, decryptErr := gcm.Open(nil, nonce, encrypted, nil)
		if decryptErr != nil {
			return nil, fmt.Errorf("failed to decrypt data: %w", decryptErr)
		}

		return bytes.NewReader(decrypted), nil
	}

	encrypted, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read encrypted data: %w", err)
	}

	decrypted, err := gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt data: %w", err)
	}

	return bytes.NewReader(decrypted), nil
}

// Algorithm returns the encryption algorithm
func (e *SimpleAESGCMEncryptor) Algorithm() types.Algorithm {
	return types.AlgorithmAES256GCM
}
