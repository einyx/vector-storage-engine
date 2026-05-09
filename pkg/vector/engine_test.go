//go:build vector

package vector

import (
	"context"
	"io"
	"math"
	mrand "math/rand"
	"strings"
	"testing"
	"time"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:           "warp-vectors",
		SegmentSizeBytes: 4096,
		WALFlushCount:    100,
		HNSWm:           4,
		HNSWefConstruct:  20,
		HNSWefSearch:     10,
		CacheMemoryBytes: 1024 * 1024,
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestEngine_CreateCollection(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	err := e.CreateCollection(ctx, Collection{Name: "test", Dimensions: 3, Distance: Cosine})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	col, err := e.GetCollection(ctx, "test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if col.Name != "test" || col.Dimensions != 3 {
		t.Fatalf("unexpected collection: %+v", col)
	}
}

func TestEngine_CreateDuplicateCollection(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "dup", Dimensions: 3, Distance: Cosine})
	err := e.CreateCollection(ctx, Collection{Name: "dup", Dimensions: 3, Distance: Cosine})
	if err == nil {
		t.Fatal("expected error on duplicate collection")
	}
}

func TestEngine_InsertAndSearch(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "test", Dimensions: 3, Distance: Cosine})
	points := []Point{
		{ID: 1, Vector: []float32{1, 0, 0}},
		{ID: 2, Vector: []float32{0, 1, 0}},
		{ID: 3, Vector: []float32{0.9, 0.1, 0}},
	}
	if err := e.Insert(ctx, "test", points); err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := e.Search(ctx, SearchRequest{Collection: "test", Vector: []float32{1, 0, 0}, TopK: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Points) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Points))
	}
	if resp.Points[0].ID != 1 {
		t.Fatalf("closest should be ID 1, got %d", resp.Points[0].ID)
	}
}

func TestEngine_SearchWithFilter(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{
		Name: "test", Dimensions: 3, Distance: Cosine,
		Schema: []FieldSchema{{Name: "color", Type: FieldString, Indexed: true}},
	})
	e.Insert(ctx, "test", []Point{
		{ID: 1, Vector: []float32{1, 0, 0}, Payload: map[string]any{"color": "red"}},
		{ID: 2, Vector: []float32{0.9, 0.1, 0}, Payload: map[string]any{"color": "blue"}},
		{ID: 3, Vector: []float32{0.8, 0.2, 0}, Payload: map[string]any{"color": "red"}},
	})

	resp, err := e.Search(ctx, SearchRequest{
		Collection: "test", Vector: []float32{1, 0, 0}, TopK: 10,
		Filter: &Filter{Field: "color", Op: OpEq, Value: "red"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Points) != 2 {
		t.Fatalf("expected 2 red results, got %d", len(resp.Points))
	}
	for _, p := range resp.Points {
		if p.Payload["color"] != "red" {
			t.Fatalf("expected color=red, got %v", p.Payload["color"])
		}
	}
}

func TestEngine_Get(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "test", Dimensions: 3, Distance: Cosine})
	e.Insert(ctx, "test", []Point{{ID: 42, Vector: []float32{1, 0, 0}, Payload: map[string]any{"x": "y"}}})

	points, err := e.Get(ctx, "test", []PointID{42})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(points) != 1 || points[0].ID != 42 {
		t.Fatalf("unexpected: %+v", points)
	}
}

func TestEngine_Delete(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "test", Dimensions: 3, Distance: Cosine})
	e.Insert(ctx, "test", []Point{
		{ID: 1, Vector: []float32{1, 0, 0}},
		{ID: 2, Vector: []float32{0, 1, 0}},
	})

	if err := e.Delete(ctx, "test", []PointID{1}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	points, err := e.Get(ctx, "test", []PointID{1})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("expected point 1 to be deleted, got %+v", points)
	}
}

func TestEngine_ListCollections(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "a", Dimensions: 3, Distance: Cosine})
	e.CreateCollection(ctx, Collection{Name: "b", Dimensions: 8, Distance: L2})

	cols, err := e.ListCollections(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("expected 2, got %d", len(cols))
	}
}

func TestEngine_DeleteCollection(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "test", Dimensions: 3, Distance: Cosine})
	if err := e.DeleteCollection(ctx, "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := e.GetCollection(ctx, "test")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestEngine_LargeInsertAndSearch(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	dim := 32

	e.CreateCollection(ctx, Collection{Name: "big", Dimensions: dim, Distance: Cosine})

	rng := mrand.New(mrand.NewSource(42))
	batch := make([]Point, 200)
	for i := range batch {
		batch[i] = Point{ID: uint64(i + 1), Vector: randomVector(dim, rng)}
	}
	if err := e.Insert(ctx, "big", batch); err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := e.Search(ctx, SearchRequest{Collection: "big", Vector: randomVector(dim, rng), TopK: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Points) != 10 {
		t.Fatalf("expected 10 results, got %d", len(resp.Points))
	}
	for i := 1; i < len(resp.Points); i++ {
		if resp.Points[i].Score < resp.Points[i-1].Score {
			t.Fatalf("results not sorted at %d", i)
		}
	}
}

func TestEngine_SearchNonexistentCollection(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	_, err := e.Search(ctx, SearchRequest{Collection: "nope", Vector: []float32{1}, TopK: 1})
	if err == nil {
		t.Fatal("expected error for nonexistent collection")
	}
}

// slowBackend wraps segTestBackend and adds a configurable delay to
// vectors.bin / hnsw.bin / payload.bin PUTs to simulate slow sealing.
type slowBackend struct {
	*segTestBackend
	putDelay time.Duration
}

func (s *slowBackend) PutObject(ctx context.Context, bucket, k string, r io.Reader, n int64, m map[string]string) error {
	if s.putDelay > 0 && (strings.HasSuffix(k, "/vectors.bin") || strings.HasSuffix(k, "/hnsw.bin") || strings.HasSuffix(k, "/payload.bin")) {
		time.Sleep(s.putDelay)
	}
	return s.segTestBackend.PutObject(ctx, bucket, k, r, n, m)
}

func TestEngine_AsyncSealDoesNotBlockReads(t *testing.T) {
	be := &slowBackend{segTestBackend: newSegTestBackend(), putDelay: 200 * time.Millisecond}
	cfg := VectorConfig{
		Bucket:           "warp-vectors",
		SegmentSizeBytes: 64, // tiny — first batch overflows
		WALFlushCount:    1,
		HNSWm:            4,
		HNSWefConstruct:  20,
		HNSWefSearch:     10,
		CacheMemoryBytes: 1024 * 1024,
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()
	if err := e.CreateCollection(ctx, Collection{Name: "async", Dimensions: 4, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Trigger one seal — it'll take ~600ms (3 slow PUTs) running in background.
	rng := mrand.New(mrand.NewSource(1))
	points := make([]Point, 10)
	for i := range points {
		points[i] = Point{ID: uint64(i + 1), Vector: randomVector(4, rng)}
	}
	t0 := time.Now()
	if err := e.Insert(ctx, "async", points); err != nil {
		t.Fatalf("insert: %v", err)
	}
	insertElapsed := time.Since(t0)

	// Insert should return well before the slow seal finishes (~600ms).
	if insertElapsed > 150*time.Millisecond {
		t.Fatalf("Insert blocked on seal: took %v", insertElapsed)
	}

	// Search should also not block on the in-flight seal.
	t1 := time.Now()
	if _, err := e.Search(ctx, SearchRequest{Collection: "async", Vector: []float32{0, 0, 0, 0}, TopK: 5}); err != nil {
		t.Fatalf("search: %v", err)
	}
	searchElapsed := time.Since(t1)
	if searchElapsed > 100*time.Millisecond {
		t.Fatalf("Search blocked on in-flight seal: took %v", searchElapsed)
	}

	// After draining, the sealed segment is visible.
	if err := e.SyncSealing("async"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	e.mu.RLock()
	cs := e.collections["async"]
	e.mu.RUnlock()
	if len(cs.sealedAcrossShards()) == 0 {
		t.Fatal("expected sealed segment after sync")
	}
}

func TestEngine_Int8QuantizationRoundTripAndRecall(t *testing.T) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:                "warp-vectors",
		SegmentSizeBytes:      4096,
		WALFlushCount:         1,
		HNSWm:                 8,
		HNSWefConstruct:       40,
		HNSWefSearch:          40,
		CacheMemoryBytes:      1024 * 1024,
		CompactionMaxSegments: 0, // disable compaction so we observe the raw sealed segment
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()

	const dim = 16
	if err := e.CreateCollection(ctx, Collection{
		Name: "q", Dimensions: dim, Distance: L2, Quantization: QuantInt8,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rng := mrand.New(mrand.NewSource(99))
	const N = 500
	pts := make([]Point, N)
	for i := range pts {
		pts[i] = Point{ID: uint64(i + 1), Vector: randomVector(dim, rng)}
	}
	if err := e.Insert(ctx, "q", pts); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := e.SyncSealing("q"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Confirm the on-disk format is the quantized one.
	e.mu.RLock()
	cs := e.collections["q"]
	e.mu.RUnlock()
	sealed := cs.sealedAcrossShards()
	if len(sealed) == 0 {
		t.Fatal("expected at least one sealed segment")
	}
	if sealed[0].Meta.Quantization != QuantInt8 {
		t.Fatalf("expected QuantInt8 segment, got %q", sealed[0].Meta.Quantization)
	}

	// Recall sanity: top-1 brute-force should agree with the engine on most
	// queries despite quantization (int8 with per-dim scale on uniform random
	// data → small dequant error).
	matches := 0
	const Q = 30
	for q := 0; q < Q; q++ {
		query := randomVector(dim, rng)
		// Engine's top-1.
		resp, err := e.Search(ctx, SearchRequest{Collection: "q", Vector: query, TopK: 1})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(resp.Points) != 1 {
			t.Fatalf("expected 1 result, got %d", len(resp.Points))
		}
		// Brute-force top-1 over the original (pre-quant) vectors.
		var bestID PointID
		bestDist := float32(math.MaxFloat32)
		for _, p := range pts {
			d := l2DistanceTest(query, p.Vector)
			if d < bestDist {
				bestDist = d
				bestID = p.ID
			}
		}
		if resp.Points[0].ID == bestID {
			matches++
		}
	}
	// On uniform random vectors with 8-bit per-dim quantization plus HNSW's
	// inherent approximate recall, expect well above 70% top-1 agreement.
	if matches*100 < 70*Q {
		t.Fatalf("recall too low: %d/%d top-1 matches", matches, Q)
	}
}

func l2DistanceTest(a, b []float32) float32 {
	var sum float32
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

func TestEngine_CompactionMergesSmallestPair(t *testing.T) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:                "warp-vectors",
		SegmentSizeBytes:      1, // every batch overflows
		WALFlushCount:         1,
		HNSWm:                 4,
		HNSWefConstruct:       20,
		HNSWefSearch:          10,
		CacheMemoryBytes:      1024 * 1024,
		CompactionMaxSegments: 2, // compact when > 2 sealed
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()
	if err := e.CreateCollection(ctx, Collection{Name: "cmp", Dimensions: 3, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Three batches → three sealed segments → trigger compaction (2 → 1) so
	// the steady-state count is 2.
	for i := 1; i <= 3; i++ {
		if err := e.Insert(ctx, "cmp", []Point{{ID: uint64(i), Vector: []float32{float32(i), 0, 0}}}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := e.SyncSealing("cmp"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	e.mu.RLock()
	cs := e.collections["cmp"]
	e.mu.RUnlock()
	if got := len(cs.sealedAcrossShards()); got != 2 {
		t.Fatalf("expected 2 sealed segments after compaction, got %d", got)
	}

	// All three points must still be findable.
	for i := uint64(1); i <= 3; i++ {
		pts, err := e.Get(ctx, "cmp", []PointID{i})
		if err != nil || len(pts) != 1 {
			t.Fatalf("Get %d: pts=%v err=%v", i, pts, err)
		}
	}
}

func TestEngine_CompactionDropsTombstones(t *testing.T) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:                "warp-vectors",
		SegmentSizeBytes:      1,
		WALFlushCount:         1,
		HNSWm:                 4,
		HNSWefConstruct:       20,
		HNSWefSearch:          10,
		CacheMemoryBytes:      1024 * 1024,
		CompactionMaxSegments: 2,
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()
	if err := e.CreateCollection(ctx, Collection{Name: "tomb", Dimensions: 3, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if err := e.Insert(ctx, "tomb", []Point{{ID: uint64(i), Vector: []float32{float32(i), 0, 0}}}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Tombstone point 1 — sits in the oldest sealed segment, so compaction
	// should physically drop it.
	if err := e.Delete(ctx, "tomb", []PointID{1}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.SyncSealing("tomb"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Inspect the merged segment: it should not contain ID 1 at all.
	e.mu.RLock()
	cs := e.collections["tomb"]
	e.mu.RUnlock()
	for _, ss := range cs.sealedAcrossShards() {
		if _, ok := ss.Get(1); ok {
			t.Fatalf("expected tombstoned point 1 to be physically removed from segment %s", ss.Meta.ID)
		}
	}
}

func TestEngine_AsyncSealBackpressure(t *testing.T) {
	// With queue capacity 1, a third overflowing Insert must block until the
	// first seal drains. We assert by timing.
	be := &slowBackend{segTestBackend: newSegTestBackend(), putDelay: 100 * time.Millisecond}
	cfg := VectorConfig{
		Bucket:           "warp-vectors",
		SegmentSizeBytes: 1, // every batch overflows
		WALFlushCount:    1,
		HNSWm:            4,
		HNSWefConstruct:  20,
		HNSWefSearch:     10,
		CacheMemoryBytes: 1024 * 1024,
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()
	if err := e.CreateCollection(ctx, Collection{Name: "bp", Dimensions: 3, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// First two Inserts return fast (1 in-flight + 1 queued).
	for i := 1; i <= 2; i++ {
		if err := e.Insert(ctx, "bp", []Point{{ID: uint64(i), Vector: []float32{1, 0, 0}}}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Third must block waiting for the first seal to drain (~300ms).
	t0 := time.Now()
	if err := e.Insert(ctx, "bp", []Point{{ID: 3, Vector: []float32{0, 1, 0}}}); err != nil {
		t.Fatalf("insert 3: %v", err)
	}
	if d := time.Since(t0); d < 50*time.Millisecond {
		t.Fatalf("expected backpressure to delay third insert, took %v", d)
	}
}

func TestEngine_SearchDedupsAcrossSegments(t *testing.T) {
	// Same PointID written into two different sealed segments must appear once.
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:           "warp-vectors",
		SegmentSizeBytes: 1, // seal after every batch
		WALFlushCount:    1,
		HNSWm:            4,
		HNSWefConstruct:  20,
		HNSWefSearch:     10,
		CacheMemoryBytes: 1024 * 1024,
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()
	if err := e.CreateCollection(ctx, Collection{Name: "dup", Dimensions: 3, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// First batch: ID 1 with vector v1 — gets sealed.
	if err := e.Insert(ctx, "dup", []Point{{ID: 1, Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	// Second batch: ID 1 again with a different vector — sealed into a separate segment.
	if err := e.Insert(ctx, "dup", []Point{{ID: 1, Vector: []float32{0, 1, 0}}}); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	// Third batch: a different ID so we can confirm topK still works.
	if err := e.Insert(ctx, "dup", []Point{{ID: 2, Vector: []float32{0, 0, 1}}}); err != nil {
		t.Fatalf("insert 3: %v", err)
	}
	if err := e.SyncSealing("dup"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	resp, err := e.Search(ctx, SearchRequest{Collection: "dup", Vector: []float32{0, 1, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := make(map[PointID]int)
	for _, p := range resp.Points {
		seen[p.ID]++
	}
	if seen[1] != 1 {
		t.Fatalf("expected ID 1 to appear exactly once, got %d (results=%+v)", seen[1], resp.Points)
	}
	// Newest-wins: ID 1's surviving vector should be the second one ({0,1,0}),
	// so distance against query {0,1,0} should be 0.
	for _, p := range resp.Points {
		if p.ID == 1 && p.Score > 1e-5 {
			t.Fatalf("expected newest-wins (vector {0,1,0}, score≈0), got score=%g vec=%v", p.Score, p.Vector)
		}
	}
}

func TestEngine_MultiShardRoutingAndSearch(t *testing.T) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:           "warp-vectors",
		SegmentSizeBytes: 64 * 1024 * 1024,
		WALFlushCount:    10,
		HNSWm:            4,
		HNSWefConstruct:  20,
		HNSWefSearch:     20,
		CacheMemoryBytes: 1024 * 1024,
	}
	e, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()

	const dim = 8
	const shards = 4
	if err := e.CreateCollection(ctx, Collection{
		Name: "ms", Dimensions: dim, Distance: L2, ShardCount: shards,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rng := mrand.New(mrand.NewSource(3))
	const N = 200
	pts := make([]Point, N)
	for i := range pts {
		pts[i] = Point{ID: uint64(i + 1), Vector: randomVector(dim, rng)}
	}
	if err := e.Insert(ctx, "ms", pts); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Each shard should have ~N/shards points (id % shards routing).
	e.mu.RLock()
	cs := e.collections["ms"]
	e.mu.RUnlock()
	if len(cs.shards) != shards {
		t.Fatalf("expected %d shards, got %d", shards, len(cs.shards))
	}
	total := 0
	for i, sh := range cs.shards {
		sh.mu.RLock()
		n := sh.growing.Count()
		sh.mu.RUnlock()
		// Each shard receives floor or ceil of N/shards (50 ± 1 here).
		if n < N/shards-5 || n > N/shards+5 {
			t.Fatalf("shard %d unexpected size %d (~%d expected)", i, n, N/shards)
		}
		total += n
	}
	if total != N {
		t.Fatalf("expected total %d points across shards, got %d", N, total)
	}

	// Search must aggregate hits from every shard.
	resp, err := e.Search(ctx, SearchRequest{Collection: "ms", Vector: randomVector(dim, rng), TopK: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Points) != 10 {
		t.Fatalf("expected 10 results, got %d", len(resp.Points))
	}

	// Get must route by ID and return the right point regardless of shard.
	for i := uint64(1); i <= N; i++ {
		got, err := e.Get(ctx, "ms", []PointID{i})
		if err != nil || len(got) != 1 || got[0].ID != i {
			t.Fatalf("Get %d: got=%v err=%v", i, got, err)
		}
	}
}

func TestEngine_RecoverTombstones(t *testing.T) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:                "warp-vectors",
		SegmentSizeBytes:      1, // every batch overflows → trigger seals
		WALFlushCount:         1,
		HNSWm:                 4,
		HNSWefConstruct:       20,
		HNSWefSearch:          10,
		CacheMemoryBytes:      1024 * 1024,
		CompactionMaxSegments: 0, // disable so the deleted point stays in its sealed segment
	}
	e1, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx := context.Background()
	if err := e1.CreateCollection(ctx, Collection{Name: "tomb", Dimensions: 3, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Insert + seal point 1.
	if err := e1.Insert(ctx, "tomb", []Point{{ID: 1, Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if err := e1.SyncSealing("tomb"); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	// 2. Delete it (tombstone goes into WAL).
	if err := e1.Delete(ctx, "tomb", []PointID{1}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 3. Insert + seal point 2 — this triggers WAL truncation that previously
	//    swept away the tombstone.
	if err := e1.Insert(ctx, "tomb", []Point{{ID: 2, Vector: []float32{0, 1, 0}}}); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	if err := e1.SyncSealing("tomb"); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	e1.Close()

	// Restart and confirm point 1 stays deleted.
	e2, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine 2: %v", err)
	}
	t.Cleanup(func() { e2.Close() })
	if err := e2.LoadCollectionsFromBackend(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}

	pts, err := e2.Get(ctx, "tomb", []PointID{1})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(pts) != 0 {
		t.Fatalf("expected deleted point 1 to stay deleted across restart, got %+v", pts)
	}

	// Sanity: point 2 still exists.
	pts2, err := e2.Get(ctx, "tomb", []PointID{2})
	if err != nil || len(pts2) != 1 {
		t.Fatalf("expected point 2 alive: pts=%v err=%v", pts2, err)
	}
}

func TestEngine_RecoverSealedSegments(t *testing.T) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:           "warp-vectors",
		SegmentSizeBytes: 256, // tiny — forces sealing after a few inserts
		WALFlushCount:    1,
		HNSWm:            4,
		HNSWefConstruct:  20,
		HNSWefSearch:     10,
		CacheMemoryBytes: 1024 * 1024,
	}
	e1, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx := context.Background()
	if err := e1.CreateCollection(ctx, Collection{Name: "rec", Dimensions: 4, Distance: L2}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rng := mrand.New(mrand.NewSource(7))
	// Insert in 3 batches; each batch should overflow segment_size and trigger seal.
	for batch := 0; batch < 3; batch++ {
		points := make([]Point, 20)
		for i := range points {
			points[i] = Point{ID: uint64(batch*100 + i + 1), Vector: randomVector(4, rng)}
		}
		if err := e1.Insert(ctx, "rec", points); err != nil {
			t.Fatalf("insert batch %d: %v", batch, err)
		}
	}

	if err := e1.SyncSealing("rec"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Confirm sealing actually happened in e1.
	e1.mu.RLock()
	cs1 := e1.collections["rec"]
	e1.mu.RUnlock()
	sealedCount := len(cs1.sealedAcrossShards())
	if sealedCount == 0 {
		t.Fatal("test setup: expected at least one sealed segment in e1")
	}
	e1.Close()

	// Boot a fresh engine on the same backend.
	e2, err := NewEngine(be, cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e2.Close() })
	if err := e2.LoadCollectionsFromBackend(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}

	e2.mu.RLock()
	cs2, ok := e2.collections["rec"]
	e2.mu.RUnlock()
	if !ok {
		t.Fatal("collection not recovered")
	}
	if got := len(cs2.sealedAcrossShards()); got != sealedCount {
		t.Fatalf("expected %d sealed segments after recovery, got %d", sealedCount, got)
	}

	// A point that lived in a sealed segment must still be searchable.
	resp, err := e2.Search(ctx, SearchRequest{Collection: "rec", Vector: []float32{0, 0, 0, 0}, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Points) == 0 {
		t.Fatal("expected results from recovered sealed segments")
	}
}

func TestEngine_InsertWrongDimensions(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	e.CreateCollection(ctx, Collection{Name: "test", Dimensions: 3, Distance: Cosine})
	err := e.Insert(ctx, "test", []Point{{ID: 1, Vector: []float32{1, 0}}})
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}
