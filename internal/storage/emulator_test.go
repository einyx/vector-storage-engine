//go:build integration
// +build integration

package storage

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prysmsh/warp-storage-engine/internal/config"
)

// Emulator endpoints, provided by docker-compose.test.yml (make test-emulators).
const (
	defaultS3MockEndpoint  = "http://127.0.0.1:9090"
	defaultAzuriteEndpoint = "http://127.0.0.1:10000/devstoreaccount1"
	azuriteAccountName     = "devstoreaccount1"
	azuriteAccountKey      = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

func emulatorContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// emulatorReachable reports whether something is listening on the endpoint host.
func emulatorReachable(t *testing.T, endpoint string) bool {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(host, "80")
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// TestS3MockBackend exercises the S3 backend against the S3Mock emulator.
func TestS3MockBackend(t *testing.T) {
	endpoint := os.Getenv("S3MOCK_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultS3MockEndpoint
	}
	if !emulatorReachable(t, endpoint) {
		t.Skipf("S3Mock not reachable at %s, skipping integration test", endpoint)
	}

	backend, err := NewS3Backend(&config.S3StorageConfig{
		Endpoint:     endpoint,
		Region:       "us-east-1",
		AccessKey:    "test",
		SecretKey:    "test",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}

	ctx := emulatorContext(t)
	bucket := "warp-test-bucket"
	key := "hello/world.txt"
	payload := "hello from s3mock"

	exists, err := backend.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketExists: %v", err)
	}
	if !exists {
		if err := backend.CreateBucket(ctx, bucket); err != nil {
			t.Fatalf("CreateBucket: %v", err)
		}
	}

	if err := backend.PutObject(ctx, bucket, key, strings.NewReader(payload), int64(len(payload)), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	obj, err := backend.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = obj.Body.Close() }()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("GetObject body = %q, want %q", got, payload)
	}

	list, err := backend.ListObjects(ctx, bucket, "", "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	found := false
	for _, o := range list.Contents {
		if o.Key == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListObjects did not return %q, got %+v", key, list.Contents)
	}

	head, err := backend.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.Size != int64(len(payload)) {
		t.Fatalf("HeadObject size = %d, want %d", head.Size, len(payload))
	}

	if err := backend.DeleteObject(ctx, bucket, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := backend.GetObject(ctx, bucket, key); err == nil {
		t.Fatal("GetObject after delete: expected error, got nil")
	}
}

// TestAzuriteBackend exercises the Azure backend against the Azurite emulator.
func TestAzuriteBackend(t *testing.T) {
	endpoint := os.Getenv("AZURITE_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultAzuriteEndpoint
	}
	if !emulatorReachable(t, endpoint) {
		t.Skipf("Azurite not reachable at %s, skipping integration test", endpoint)
	}

	backend, err := NewAzureBackend(&config.AzureStorageConfig{
		AccountName:   azuriteAccountName,
		AccountKey:    azuriteAccountKey,
		ContainerName: "warp-test-container",
		Endpoint:      endpoint,
	})
	if err != nil {
		t.Fatalf("NewAzureBackend: %v", err)
	}

	ctx := emulatorContext(t)
	bucket := "warp-test-container"
	key := "hello/world.txt"
	payload := "hello from azurite"

	exists, err := backend.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketExists: %v", err)
	}
	if !exists {
		if err := backend.CreateBucket(ctx, bucket); err != nil {
			t.Fatalf("CreateBucket: %v", err)
		}
	}

	if err := backend.PutObject(ctx, bucket, key, strings.NewReader(payload), int64(len(payload)), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	obj, err := backend.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = obj.Body.Close() }()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("GetObject body = %q, want %q", got, payload)
	}

	list, err := backend.ListObjects(ctx, bucket, "", "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	found := false
	for _, o := range list.Contents {
		if o.Key == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListObjects did not return %q, got %+v", key, list.Contents)
	}

	if err := backend.DeleteObject(ctx, bucket, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := backend.GetObject(ctx, bucket, key); err == nil {
		t.Fatal("GetObject after delete: expected error, got nil")
	}
}
