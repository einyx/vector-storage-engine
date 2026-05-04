package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/prysmsh/warp-storage-engine/internal/config"
	"github.com/prysmsh/warp-storage-engine/internal/storage"
)

func TestParseRangeHeader(t *testing.T) {
	const size = int64(1000)

	tests := []struct {
		name      string
		header    string
		size      int64
		wantStart int64
		wantLen   int64
		wantErr   error
	}{
		{name: "standard range", header: "bytes=0-499", size: size, wantStart: 0, wantLen: 500},
		{name: "middle range", header: "bytes=500-999", size: size, wantStart: 500, wantLen: 500},
		{name: "single byte", header: "bytes=0-0", size: size, wantStart: 0, wantLen: 1},
		{name: "open-ended", header: "bytes=500-", size: size, wantStart: 500, wantLen: 500},
		{name: "open-ended from zero", header: "bytes=0-", size: size, wantStart: 0, wantLen: 1000},
		{name: "suffix range", header: "bytes=-100", size: size, wantStart: 900, wantLen: 100},
		{name: "suffix exceeds size", header: "bytes=-2000", size: size, wantStart: 0, wantLen: 1000},
		{name: "end exceeds size clamps", header: "bytes=500-5000", size: size, wantStart: 500, wantLen: 500},
		{name: "whitespace tolerated", header: "bytes= 100 - 199 ", size: size, wantStart: 100, wantLen: 100},

		{name: "missing prefix", header: "0-100", size: size, wantErr: errRangeInvalid},
		{name: "wrong unit", header: "items=0-100", size: size, wantErr: errRangeInvalid},
		{name: "no dash", header: "bytes=100", size: size, wantErr: errRangeInvalid},
		{name: "empty spec", header: "bytes=", size: size, wantErr: errRangeInvalid},
		{name: "multi-range rejected", header: "bytes=0-100,200-300", size: size, wantErr: errRangeInvalid},
		{name: "negative start", header: "bytes=--5", size: size, wantErr: errRangeInvalid},
		{name: "non-numeric", header: "bytes=abc-xyz", size: size, wantErr: errRangeInvalid},
		{name: "end before start", header: "bytes=500-100", size: size, wantErr: errRangeInvalid},
		{name: "empty suffix", header: "bytes=-", size: size, wantErr: errRangeInvalid},

		{name: "start past end", header: "bytes=2000-3000", size: size, wantErr: errRangeUnsatisfiable},
		{name: "start equals size", header: "bytes=1000-1100", size: size, wantErr: errRangeUnsatisfiable},
		{name: "zero suffix", header: "bytes=-0", size: size, wantErr: errRangeUnsatisfiable},
		{name: "empty resource", header: "bytes=0-0", size: 0, wantErr: errRangeUnsatisfiable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRangeHeader(tt.header, tt.size)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.start != tt.wantStart || got.length != tt.wantLen {
				t.Fatalf("got {start:%d length:%d}, want {start:%d length:%d}", got.start, got.length, tt.wantStart, tt.wantLen)
			}
		})
	}
}

// rangeMockStorage serves a fixed payload whose size matches HeadObject so
// that range arithmetic can be verified end-to-end.
type rangeMockStorage struct {
	mockStorage
	payload string
}

func (m *rangeMockStorage) GetObject(_ context.Context, _, _ string) (*storage.Object, error) {
	return &storage.Object{
		Body:         io.NopCloser(strings.NewReader(m.payload)),
		ContentType:  "application/octet-stream",
		Size:         int64(len(m.payload)),
		ETag:         "range-etag",
		LastModified: time.Unix(0, 0).UTC(),
	}, nil
}

func (m *rangeMockStorage) HeadObject(_ context.Context, _, _ string) (*storage.ObjectInfo, error) {
	return &storage.ObjectInfo{
		Size:         int64(len(m.payload)),
		ETag:         "range-etag",
		LastModified: time.Unix(0, 0).UTC(),
		ContentType:  "application/octet-stream",
	}, nil
}

func newRangeHandler(payload string) *Handler {
	return NewHandler(&rangeMockStorage{payload: payload}, &mockAuth{}, config.S3Config{}, config.ChunkingConfig{})
}

func TestGetRangeObject_PartialContent(t *testing.T) {
	payload := "abcdefghijklmnopqrstuvwxyz" // 26 bytes
	h := newRangeHandler(payload)

	req := httptest.NewRequest("GET", "/warehouse/obj.bin", nil)
	req.Header.Set("Range", "bytes=5-9")
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "obj.bin"})
	rr := httptest.NewRecorder()

	h.getRangeObject(rr, req, "warehouse", "obj.bin", "bytes=5-9")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPartialContent)
	}
	if got, want := rr.Body.String(), "fghij"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got, want := rr.Header().Get("Content-Range"), "bytes 5-9/26"; got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
	if got, want := rr.Header().Get("Content-Length"), "5"; got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
	if got, want := rr.Header().Get("Accept-Ranges"), "bytes"; got != want {
		t.Fatalf("Accept-Ranges = %q, want %q", got, want)
	}
}

func TestGetRangeObject_SuffixRange(t *testing.T) {
	payload := "abcdefghijklmnopqrstuvwxyz"
	h := newRangeHandler(payload)

	req := httptest.NewRequest("GET", "/warehouse/obj.bin", nil)
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "obj.bin"})
	rr := httptest.NewRecorder()

	h.getRangeObject(rr, req, "warehouse", "obj.bin", "bytes=-3")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPartialContent)
	}
	if got, want := rr.Body.String(), "xyz"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got, want := rr.Header().Get("Content-Range"), "bytes 23-25/26"; got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
}

func TestGetRangeObject_OpenEnded(t *testing.T) {
	payload := "abcdefghij" // 10 bytes
	h := newRangeHandler(payload)

	req := httptest.NewRequest("GET", "/warehouse/obj.bin", nil)
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "obj.bin"})
	rr := httptest.NewRecorder()

	h.getRangeObject(rr, req, "warehouse", "obj.bin", "bytes=4-")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPartialContent)
	}
	if got, want := rr.Body.String(), "efghij"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got, want := rr.Header().Get("Content-Range"), "bytes 4-9/10"; got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
}

func TestGetRangeObject_Unsatisfiable(t *testing.T) {
	payload := "abcdefghij"
	h := newRangeHandler(payload)

	req := httptest.NewRequest("GET", "/warehouse/obj.bin", nil)
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "obj.bin"})
	rr := httptest.NewRecorder()

	h.getRangeObject(rr, req, "warehouse", "obj.bin", "bytes=100-200")

	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusRequestedRangeNotSatisfiable)
	}
	if got, want := rr.Header().Get("Content-Range"), "bytes */10"; got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
}

func TestGetRangeObject_MalformedFallsBackToFullObject(t *testing.T) {
	payload := "abcdefghij"
	h := newRangeHandler(payload)

	req := httptest.NewRequest("GET", "/warehouse/obj.bin", nil)
	req = mux.SetURLVars(req, map[string]string{"bucket": "warehouse", "key": "obj.bin"})
	rr := httptest.NewRecorder()

	h.getRangeObject(rr, req, "warehouse", "obj.bin", "not-a-range")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got, want := rr.Body.String(), payload; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if rr.Header().Get("Content-Range") != "" {
		t.Fatalf("Content-Range should not be set on fallback, got %q", rr.Header().Get("Content-Range"))
	}
}
