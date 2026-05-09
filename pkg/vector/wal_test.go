//go:build vector

package vector

import (
	"context"
	"testing"
	"time"
)

func TestWAL_AppendAndReplay(t *testing.T) {
	be := newSegTestBackend()
	w := NewWAL(be, "warp-vectors", "test-col", 0)

	entries := []WALEntry{
		{Type: WALInsert, Point: Point{ID: 1, Vector: []float32{1, 0, 0}}},
		{Type: WALInsert, Point: Point{ID: 2, Vector: []float32{0, 1, 0}}},
		{Type: WALDelete, PointID: 1},
	}
	for _, e := range entries {
		if err := w.Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	ctx := context.Background()
	if _, err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	replayed, err := ReplayWAL(ctx, be, "warp-vectors", "test-col", 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(replayed))
	}
	if replayed[0].Type != WALInsert || replayed[0].Point.ID != 1 {
		t.Fatalf("entry 0 mismatch: %+v", replayed[0])
	}
	if replayed[2].Type != WALDelete || replayed[2].PointID != 1 {
		t.Fatalf("entry 2 mismatch: %+v", replayed[2])
	}
}

func TestWAL_FlushOnCount(t *testing.T) {
	be := newSegTestBackend()
	w := NewWAL(be, "warp-vectors", "test-col", 0)
	w.SetFlushThresholds(3, 1*time.Hour)

	for i := uint64(1); i <= 3; i++ {
		if err := w.Append(WALEntry{Type: WALInsert, Point: Point{ID: i, Vector: []float32{1, 0, 0}}}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	ctx := context.Background()
	replayed, err := ReplayWAL(ctx, be, "warp-vectors", "test-col", 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed) != 3 {
		t.Fatalf("expected 3 entries after auto-flush, got %d", len(replayed))
	}
}

func TestWAL_Truncate(t *testing.T) {
	be := newSegTestBackend()
	w := NewWAL(be, "warp-vectors", "test-col", 0)

	for i := uint64(1); i <= 5; i++ {
		w.Append(WALEntry{Type: WALInsert, Point: Point{ID: i, Vector: []float32{1, 0, 0}}})
	}
	ctx := context.Background()
	w.Flush(ctx)

	if err := w.Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	replayed, err := ReplayWAL(ctx, be, "warp-vectors", "test-col", 0)
	if err != nil {
		t.Fatalf("replay after truncate: %v", err)
	}
	if len(replayed) != 0 {
		t.Fatalf("expected 0 entries after truncate, got %d", len(replayed))
	}
}
