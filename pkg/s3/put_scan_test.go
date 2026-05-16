package s3

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"github.com/prysmsh/warp-storage-engine/internal/config"
	"github.com/prysmsh/warp-storage-engine/internal/virustotal"
)

type stubMalwareScanner struct {
	threat   bool
	scanErr  error
	maxBytes int64
}

func (s *stubMalwareScanner) IsEnabled() bool { return true }

func (s *stubMalwareScanner) CanBufferScanForUpload(size int64) bool {
	if size < 0 {
		return false
	}
	if s.maxBytes > 0 && size > s.maxBytes {
		return false
	}
	return true
}

func (s *stubMalwareScanner) ScanReader(ctx context.Context, reader io.Reader, filename string, size int64) (*virustotal.ScanResult, error) {
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	if s.threat {
		return &virustotal.ScanResult{Malicious: 3}, nil
	}
	return nil, nil
}

func (s *stubMalwareScanner) ShouldBlockUpload(result *virustotal.ScanResult) bool {
	if result == nil {
		return false
	}
	return s.threat && result.Malicious > 0
}

func TestPutObject_MalwareScanAllowsCleanUpload(t *testing.T) {
	st := &stubMalwareScanner{threat: false, maxBytes: 1024}
	h := NewHandler(&mockStorage{}, &mockAuth{}, config.S3Config{}, config.ChunkingConfig{})
	h.SetScanner(st)

	body := []byte("clean payload")
	req := httptest.NewRequest(http.MethodPut, "/warehouse/clean.txt", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "clean.txt"})

	rr := httptest.NewRecorder()
	h.putObject(rr, req, "warehouse", "clean.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPutObject_MalwareScanBlocksThreat(t *testing.T) {
	st := &stubMalwareScanner{threat: true, maxBytes: 1024}
	h := NewHandler(&mockStorage{}, &mockAuth{}, config.S3Config{}, config.ChunkingConfig{})
	h.SetScanner(st)

	body := []byte("bad payload")
	req := httptest.NewRequest(http.MethodPut, "/warehouse/bad.txt", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "bad.txt"})

	rr := httptest.NewRecorder()
	h.putObject(rr, req, "warehouse", "bad.txt")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPutObject_MalwareScanBackendError(t *testing.T) {
	st := &stubMalwareScanner{scanErr: io.ErrUnexpectedEOF, maxBytes: 1024}
	h := NewHandler(&mockStorage{}, &mockAuth{}, config.S3Config{}, config.ChunkingConfig{})
	h.SetScanner(st)

	body := []byte("x")
	req := httptest.NewRequest(http.MethodPut, "/warehouse/x.txt", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "x.txt"})

	rr := httptest.NewRecorder()
	h.putObject(rr, req, "warehouse", "x.txt")

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
}
