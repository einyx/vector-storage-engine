package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/prysmsh/warp-storage-engine/internal/config"
)

func TestFileSystemBackend_GetObject_InvalidBucket(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()
	ctx := context.Background()
	_, err := backend.GetObject(ctx, "..", "key")
	if err == nil {
		t.Error("GetObject with traversal bucket should fail")
	}
	_, err = backend.GetObject(ctx, "", "key")
	if err == nil {
		t.Error("GetObject with empty bucket should fail")
	}
}

func TestNewFileSystemBackend(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.FileSystemConfig
		wantErr     bool
		errContains string
	}{
		{
			name: "valid config",
			cfg: &config.FileSystemConfig{
				BaseDir: "/tmp/test",
			},
			wantErr: false,
		},
		{
			name:        "missing base dir",
			cfg:         &config.FileSystemConfig{},
			wantErr:     true,
			errContains: "base directory is required",
		},
		{
			name: "empty base dir",
			cfg: &config.FileSystemConfig{
				BaseDir: "",
			},
			wantErr:     true,
			errContains: "base directory is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp directory for valid configs
			if !tt.wantErr && tt.cfg.BaseDir != "" {
				tempDir, err := os.MkdirTemp("", "fs-test-*")
				if err != nil {
					t.Fatalf("Failed to create temp dir: %v", err)
				}
				defer func() {
					if err := os.RemoveAll(tempDir); err != nil {
						t.Logf("Failed to clean up temp dir: %v", err)
					}
				}()
				tt.cfg.BaseDir = tempDir
			}

			_, err := NewFileSystemBackend(tt.cfg)

			if tt.wantErr {
				if err == nil {
					t.Errorf("NewFileSystemBackend() expected error but got none")
				} else if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("NewFileSystemBackend() error = %v, want error containing %v", err, tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("NewFileSystemBackend() unexpected error = %v", err)
				}
			}
		})
	}
}

// setupFileSystemBackend creates a temporary filesystem backend for testing
func setupFileSystemBackend(t *testing.T) (*FileSystemBackend, string) {
	tempDir, err := os.MkdirTemp("", "warp-storage-engine-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	backend, err := NewFileSystemBackend(&config.FileSystemConfig{
		BaseDir: tempDir,
	})
	if err != nil {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
		t.Fatalf("Failed to create filesystem backend: %v", err)
	}

	return backend, tempDir
}

func TestFileSystemBackend_CreateAndCheckBucket(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "test-bucket"

	// Create bucket
	err := backend.CreateBucket(ctx, bucket)
	if err != nil {
		t.Fatalf("CreateBucket() error = %v", err)
	}

	// Check exists
	exists, err := backend.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketExists() error = %v", err)
	}
	if !exists {
		t.Error("BucketExists() = false, want true")
	}
}

func TestFileSystemBackend_ListBuckets(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()

	// Create a few buckets
	buckets := []string{"bucket1", "bucket2", "bucket3"}
	for _, b := range buckets {
		if err := backend.CreateBucket(ctx, b); err != nil {
			t.Fatalf("CreateBucket(%s) error = %v", b, err)
		}
	}

	// List buckets
	result, err := backend.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets() error = %v", err)
	}

	// Should have at least the buckets we created
	if len(result) < len(buckets) {
		t.Errorf("ListBuckets() returned %d buckets, want at least %d", len(result), len(buckets))
	}
}

func TestFileSystemBackend_PutAndGetObject(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "put-get-bucket"
	key := "test-key.txt"
	content := []byte("test content")

	// Create bucket
	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Put object
	err := backend.PutObject(ctx, bucket, key, bytes.NewReader(content), int64(len(content)), nil)
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	// Get object
	obj, err := backend.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	defer func() {
		if closeErr := obj.Body.Close(); closeErr != nil {
			t.Logf("Failed to close body: %v", closeErr)
		}
	}()

	if obj.Size != int64(len(content)) {
		t.Errorf("GetObject() size = %v, want %v", obj.Size, len(content))
	}

	data, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("Failed to read object: %v", err)
	}

	if !bytes.Equal(data, content) {
		t.Errorf("GetObject() content = %v, want %v", data, content)
	}
}

func TestFileSystemBackend_ObjectCanAlsoBePrefix(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "vault-bucket"
	parentKey := "logical/root/metadata/parent"
	childKey := parentKey + "/child"
	parentContent := []byte("parent object")
	childContent := []byte("child object")

	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	if err := backend.PutObject(ctx, bucket, parentKey, bytes.NewReader(parentContent), int64(len(parentContent)), nil); err != nil {
		t.Fatalf("PutObject(parent) error = %v", err)
	}

	if err := backend.PutObject(ctx, bucket, childKey, bytes.NewReader(childContent), int64(len(childContent)), nil); err != nil {
		t.Fatalf("PutObject(child) error = %v", err)
	}

	parentObj, err := backend.GetObject(ctx, bucket, parentKey)
	if err != nil {
		t.Fatalf("GetObject(parent) error = %v", err)
	}
	defer func() {
		if closeErr := parentObj.Body.Close(); closeErr != nil {
			t.Logf("Failed to close parent body: %v", closeErr)
		}
	}()

	gotParent, err := io.ReadAll(parentObj.Body)
	if err != nil {
		t.Fatalf("Failed to read parent object: %v", err)
	}
	if !bytes.Equal(gotParent, parentContent) {
		t.Fatalf("parent content = %q, want %q", gotParent, parentContent)
	}

	childObj, err := backend.GetObject(ctx, bucket, childKey)
	if err != nil {
		t.Fatalf("GetObject(child) error = %v", err)
	}
	defer func() {
		if closeErr := childObj.Body.Close(); closeErr != nil {
			t.Logf("Failed to close child body: %v", closeErr)
		}
	}()

	gotChild, err := io.ReadAll(childObj.Body)
	if err != nil {
		t.Fatalf("Failed to read child object: %v", err)
	}
	if !bytes.Equal(gotChild, childContent) {
		t.Fatalf("child content = %q, want %q", gotChild, childContent)
	}

	head, err := backend.HeadObject(ctx, bucket, parentKey)
	if err != nil {
		t.Fatalf("HeadObject(parent) error = %v", err)
	}
	if head.Size != int64(len(parentContent)) {
		t.Fatalf("HeadObject(parent) size = %d, want %d", head.Size, len(parentContent))
	}
}

func TestFileSystemBackend_DeleteObject(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "delete-bucket"
	key := "delete-test.txt"
	content := []byte("delete me")

	// Create bucket
	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Put object
	err := backend.PutObject(ctx, bucket, key, bytes.NewReader(content), int64(len(content)), nil)
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	// Delete object
	err = backend.DeleteObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("DeleteObject() error = %v", err)
	}

	// Verify deleted
	_, err = backend.GetObject(ctx, bucket, key)
	if err == nil {
		t.Error("GetObject() expected error after delete")
	}
}

func TestFileSystemBackend_HeadObject(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "head-bucket"
	key := "head-test.txt"
	content := []byte("head test content")

	// Create bucket
	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Put object
	err := backend.PutObject(ctx, bucket, key, bytes.NewReader(content), int64(len(content)), nil)
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	// Head object
	info, err := backend.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("HeadObject() error = %v", err)
	}

	if info.Size != int64(len(content)) {
		t.Errorf("HeadObject() size = %v, want %v", info.Size, len(content))
	}

	if info.Key != key {
		t.Errorf("HeadObject() key = %v, want %v", info.Key, key)
	}
}

func TestFileSystemBackend_ListObjects(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "list-bucket"

	// Create bucket
	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Put multiple objects
	objects := []string{
		"dir1/file1.txt",
		"dir1/file2.txt",
		"dir2/file3.txt",
		"file4.txt",
	}

	for _, key := range objects {
		content := []byte("content for " + key)
		err := backend.PutObject(ctx, bucket, key, bytes.NewReader(content), int64(len(content)), nil)
		if err != nil {
			t.Fatalf("PutObject() error = %v", err)
		}
	}

	// List all objects (note: ListObjects uses delimiter by default)
	result, err := backend.ListObjects(ctx, bucket, "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects() error = %v", err)
	}

	// Since ListObjects uses delimiter by default, we expect only root-level objects
	// which is just "file4.txt"
	if len(result.Contents) != 1 {
		t.Errorf("ListObjects() objects = %v, want 1", len(result.Contents))
	}

	// Should have 2 common prefixes (dir1/ and dir2/)
	if len(result.CommonPrefixes) != 2 {
		t.Errorf("ListObjects() prefixes = %v, want 2", len(result.CommonPrefixes))
	}
}

func TestFileSystemBackend_ListObjectsWithDelimiter(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "list-delim-bucket"

	// Create bucket
	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Put objects
	objects := []string{
		"dir1/file1.txt",
		"dir1/file2.txt",
		"dir2/file3.txt",
		"file4.txt",
	}

	for _, key := range objects {
		content := []byte("content")
		err := backend.PutObject(ctx, bucket, key, bytes.NewReader(content), int64(len(content)), nil)
		if err != nil {
			t.Fatalf("PutObject() error = %v", err)
		}
	}

	// List with delimiter
	result, err := backend.ListObjectsWithDelimiter(ctx, bucket, "", "", "/", 1000)
	if err != nil {
		t.Fatalf("ListObjectsWithDelimiter() error = %v", err)
	}

	// Should have 1 object at root and 2 prefixes
	if len(result.Contents) != 1 {
		t.Errorf("ListObjectsWithDelimiter() objects = %v, want 1", len(result.Contents))
	}

	if len(result.CommonPrefixes) != 2 {
		t.Errorf("ListObjectsWithDelimiter() prefixes = %v, want 2", len(result.CommonPrefixes))
	}
}

func TestFileSystemBackend_DeleteBucket(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	ctx := context.Background()
	bucket := "delete-bucket-test"

	// Create bucket
	err := backend.CreateBucket(ctx, bucket)
	if err != nil {
		t.Fatalf("CreateBucket() error = %v", err)
	}

	// Delete bucket
	err = backend.DeleteBucket(ctx, bucket)
	if err != nil {
		t.Fatalf("DeleteBucket() error = %v", err)
	}

	// Verify deleted
	exists, err := backend.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketExists() error = %v", err)
	}
	if exists {
		t.Error("BucketExists() = true after delete")
	}
}

func TestFileSystemBackend_GetObjectACL(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	_ = backend.CreateBucket(ctx, "acl-bucket")
	acl, err := backend.GetObjectACL(ctx, "acl-bucket", "any-key")
	if err != nil {
		t.Fatalf("GetObjectACL: %v", err)
	}
	if acl.Owner.ID != "filesystem" || len(acl.Grants) != 1 {
		t.Errorf("GetObjectACL: owner=%q grants=%d", acl.Owner.ID, len(acl.Grants))
	}
}

func TestFileSystemBackend_PutObjectACL(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	_ = backend.CreateBucket(ctx, "acl-bucket")
	err := backend.PutObjectACL(ctx, "acl-bucket", "key", &ACL{})
	if err != nil {
		t.Fatalf("PutObjectACL: %v", err)
	}
}

func TestFileSystemBackend_HeadObject_NotFound(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	_ = backend.CreateBucket(ctx, "head-bucket")
	_, err := backend.HeadObject(ctx, "head-bucket", "nonexistent-key")
	if err == nil {
		t.Error("HeadObject(nonexistent) expected error")
	}
}

func TestFileSystemBackend_ListDeletedObjects(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	_ = backend.CreateBucket(ctx, "b")
	_, err := backend.ListDeletedObjects(ctx, "b", "", "", 100)
	if err == nil {
		t.Error("ListDeletedObjects expected error (not implemented)")
	}
	if err != nil && !contains(err.Error(), "not implemented") {
		t.Errorf("ListDeletedObjects: %v", err)
	}
}

func TestFileSystemBackend_RestoreObject(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	_ = backend.CreateBucket(ctx, "b")
	err := backend.RestoreObject(ctx, "b", "key", "")
	if err == nil {
		t.Error("RestoreObject expected error (not implemented)")
	}
	if err != nil && !contains(err.Error(), "not implemented") {
		t.Errorf("RestoreObject: %v", err)
	}
}

func TestFileSystemBackend_ListObjects_EmptyBucket(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	bucket := "empty-bucket"
	_ = backend.CreateBucket(ctx, bucket)
	result, err := backend.ListObjects(ctx, bucket, "", "", 100)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.Contents) != 0 || len(result.CommonPrefixes) != 0 {
		t.Errorf("ListObjects(empty): contents=%d prefixes=%d", len(result.Contents), len(result.CommonPrefixes))
	}
}

func TestFileSystemBackend_ListParts(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	bucket := "listparts-bucket"
	key := "obj"
	_ = backend.CreateBucket(ctx, bucket)
	uploadID, err := backend.InitiateMultipartUpload(ctx, bucket, key, nil)
	if err != nil {
		t.Fatalf("InitiateMultipartUpload: %v", err)
	}
	defer backend.AbortMultipartUpload(ctx, bucket, key, uploadID)
	_, err = backend.UploadPart(ctx, bucket, key, uploadID, 1, bytes.NewReader([]byte("part1")), 5)
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	result, err := backend.ListParts(ctx, bucket, key, uploadID, 10, 0)
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if result.Bucket != bucket || result.Key != key || result.UploadID != uploadID {
		t.Errorf("ListParts result: bucket=%q key=%q uploadID=%q", result.Bucket, result.Key, result.UploadID)
	}
	if len(result.Parts) != 1 {
		t.Errorf("ListParts: want 1 part, got %d", len(result.Parts))
	}
	if len(result.Parts) > 0 && result.Parts[0].PartNumber != 1 {
		t.Errorf("ListParts: part number = %d", result.Parts[0].PartNumber)
	}
}

func TestFileSystemBackend_ListParts_EmptyUpload(t *testing.T) {
	backend, tempDir := setupFileSystemBackend(t)
	defer func() { _ = os.RemoveAll(tempDir) }()
	ctx := context.Background()
	bucket := "listparts-empty"
	key := "obj"
	_ = backend.CreateBucket(ctx, bucket)
	uploadID, err := backend.InitiateMultipartUpload(ctx, bucket, key, nil)
	if err != nil {
		t.Fatalf("InitiateMultipartUpload: %v", err)
	}
	defer backend.AbortMultipartUpload(ctx, bucket, key, uploadID)
	result, err := backend.ListParts(ctx, bucket, key, uploadID, 10, 0)
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(result.Parts) != 0 {
		t.Errorf("ListParts(no parts): got %d parts", len(result.Parts))
	}
}

func TestFileSystemBackend_Multipart(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "warp-storage-engine-multipart-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if cleanErr := os.RemoveAll(tempDir); cleanErr != nil {
			t.Logf("Failed to clean up temp dir: %v", cleanErr)
		}
	}()

	backend, err := NewFileSystemBackend(&config.FileSystemConfig{
		BaseDir: tempDir,
	})
	if err != nil {
		t.Fatalf("Failed to create filesystem backend: %v", err)
	}

	bucket := "multipart-bucket"
	key := "multipart.txt"

	// Create bucket
	if err := backend.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	t.Run("InitiateMultipartUpload", func(t *testing.T) {
		uploadID, err := backend.InitiateMultipartUpload(ctx, bucket, key, nil)
		if err != nil {
			t.Fatalf("InitiateMultipartUpload() error = %v", err)
		}

		if uploadID == "" {
			t.Error("InitiateMultipartUpload() returned empty upload ID")
		}

		// Abort for cleanup
		if err := backend.AbortMultipartUpload(ctx, bucket, key, uploadID); err != nil {
			t.Logf("Failed to abort multipart upload: %v", err)
		}
	})

	t.Run("Complete multipart upload flow", func(t *testing.T) {
		// Initiate
		uploadID, err := backend.InitiateMultipartUpload(ctx, bucket, key, nil)
		if err != nil {
			t.Fatalf("InitiateMultipartUpload() error = %v", err)
		}

		// Upload parts
		part1 := []byte("part 1 content")
		part2 := []byte("part 2 content")

		etag1, err := backend.UploadPart(ctx, bucket, key, uploadID, 1, bytes.NewReader(part1), int64(len(part1)))
		if err != nil {
			t.Fatalf("UploadPart(1) error = %v", err)
		}

		etag2, err := backend.UploadPart(ctx, bucket, key, uploadID, 2, bytes.NewReader(part2), int64(len(part2)))
		if err != nil {
			t.Fatalf("UploadPart(2) error = %v", err)
		}

		// Complete upload
		parts := []CompletedPart{
			{PartNumber: 1, ETag: etag1},
			{PartNumber: 2, ETag: etag2},
		}

		err = backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts)
		if err != nil {
			t.Fatalf("CompleteMultipartUpload() error = %v", err)
		}

		// Verify object exists
		obj, err := backend.GetObject(ctx, bucket, key)
		if err != nil {
			t.Fatalf("GetObject() after multipart error = %v", err)
		}
		defer func() {
			if closeErr := obj.Body.Close(); closeErr != nil {
				t.Logf("Failed to close body: %v", closeErr)
			}
		}()

		data, _ := io.ReadAll(obj.Body)
		expected := append(part1, part2...)
		if !bytes.Equal(data, expected) {
			t.Errorf("Multipart object content = %v, want %v", data, expected)
		}
	})
}
