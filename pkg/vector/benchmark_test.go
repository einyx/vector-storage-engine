//go:build vector

package vector

import (
	"context"
	"fmt"
	mrand "math/rand"
	"testing"
)

func benchRandVec(dim int, rng *mrand.Rand) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

func benchSetup(b *testing.B, pointCount, dim int) *Engine {
	b.Helper()
	be := newSegTestBackend()
	engine, err := NewEngine(be, VectorConfig{
		Bucket:           "bench",
		SegmentSizeBytes: 64 * 1024 * 1024,
		WALFlushCount:    1000,
		HNSWm:           16,
		HNSWefConstruct:  200,
		HNSWefSearch:     128,
		CacheMemoryBytes: 256 * 1024 * 1024,
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	engine.CreateCollection(ctx, Collection{Name: "bench", Dimensions: dim, Distance: Cosine})

	rng := mrand.New(mrand.NewSource(42))
	batch := make([]Point, 0, 100)
	for i := 0; i < pointCount; i++ {
		batch = append(batch, Point{
			ID:     uint64(i + 1),
			Vector: benchRandVec(dim, rng),
			Payload: map[string]any{
				"type": fmt.Sprintf("type_%d", i%10),
			},
		})
		if len(batch) == 100 {
			engine.Insert(ctx, "bench", batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		engine.Insert(ctx, "bench", batch)
	}
	return engine
}

// BenchmarkSearch_768d_500pts — AI WAF known-attacks collection size
func BenchmarkSearch_768d_500pts(b *testing.B) {
	engine := benchSetup(b, 500, 768)
	rng := mrand.New(mrand.NewSource(99))
	query := benchRandVec(768, rng)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Search(ctx, SearchRequest{Collection: "bench", Vector: query, TopK: 10})
	}
}

// BenchmarkSearch_768d_5000pts — AI WAF baseline collection size
func BenchmarkSearch_768d_5000pts(b *testing.B) {
	engine := benchSetup(b, 5000, 768)
	rng := mrand.New(mrand.NewSource(99))
	query := benchRandVec(768, rng)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Search(ctx, SearchRequest{Collection: "bench", Vector: query, TopK: 10})
	}
}

// BenchmarkInsert_768d — single point insert latency
func BenchmarkInsert_768d(b *testing.B) {
	be := newSegTestBackend()
	engine, _ := NewEngine(be, VectorConfig{
		Bucket:           "bench",
		SegmentSizeBytes: 64 * 1024 * 1024,
		WALFlushCount:    1000,
		HNSWm:           16,
		HNSWefConstruct:  200,
		CacheMemoryBytes: 256 * 1024 * 1024,
	})
	ctx := context.Background()
	engine.CreateCollection(ctx, Collection{Name: "bench", Dimensions: 768, Distance: Cosine})
	rng := mrand.New(mrand.NewSource(42))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Insert(ctx, "bench", []Point{{
			ID:     uint64(i + 1),
			Vector: benchRandVec(768, rng),
		}})
	}
}

// --- distance-kernel microbenchmarks ---

func benchTwoVecs(b *testing.B, dim int, fn DistanceFunc) {
	b.Helper()
	rng := mrand.New(mrand.NewSource(7))
	a := benchRandVec(dim, rng)
	c := benchRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	var sink float32
	for i := 0; i < b.N; i++ {
		sink += fn(a, c)
	}
	if sink == 0 {
		b.Fatal("sink unused")
	}
}

func BenchmarkDist_L2_768(b *testing.B)     { benchTwoVecs(b, 768, L2Distance) }
func BenchmarkDist_Cos_768(b *testing.B)    { benchTwoVecs(b, 768, CosineDistance) }
func BenchmarkDist_IP_768(b *testing.B)     { benchTwoVecs(b, 768, InnerProductDistance) }
func BenchmarkDist_L2_1536(b *testing.B)    { benchTwoVecs(b, 1536, L2Distance) }
func BenchmarkDist_Cos_1536(b *testing.B)   { benchTwoVecs(b, 1536, CosineDistance) }

// --- HNSW-isolated search benchmarks ---

func benchHNSWBuild(b *testing.B, dim, n int, quant bool) *HNSWIndex {
	b.Helper()
	rng := mrand.New(mrand.NewSource(11))
	pts := make([][]float32, n)
	for i := range pts {
		pts[i] = benchRandVec(dim, rng)
	}
	var idx *HNSWIndex
	if quant {
		samples := make([]Point, len(pts))
		for i, v := range pts {
			samples[i] = Point{ID: uint64(i + 1), Vector: v}
		}
		qp := computeQuantParams(samples, dim)
		idx = NewHNSWIndexQuantized(dim, 16, 200, L2Distance, qp)
	} else {
		idx = NewHNSWIndex(dim, 16, 200, L2Distance)
	}
	for i, v := range pts {
		idx.Insert(uint64(i+1), v)
	}
	return idx
}

func benchHNSWSearch(b *testing.B, dim, n int, quant bool) {
	b.Helper()
	idx := benchHNSWBuild(b, dim, n, quant)
	rng := mrand.New(mrand.NewSource(99))
	q := benchRandVec(dim, rng)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		idx.Search(q, 10, 128, nil)
	}
}

func BenchmarkHNSW_Search_768d_5000(b *testing.B)         { benchHNSWSearch(b, 768, 5000, false) }
func BenchmarkHNSW_Search_768d_5000_Quant(b *testing.B)   { benchHNSWSearch(b, 768, 5000, true) }
func BenchmarkHNSW_Search_1536d_5000(b *testing.B)        { benchHNSWSearch(b, 1536, 5000, false) }
func BenchmarkHNSW_Search_1536d_5000_Quant(b *testing.B)  { benchHNSWSearch(b, 1536, 5000, true) }

// --- compaction microbenchmark ---

func BenchmarkCompaction_2_Segments_1000pts(b *testing.B) {
	be := newSegTestBackend()
	cfg := VectorConfig{
		Bucket:                "bench",
		SegmentSizeBytes:      1, // every batch overflows
		WALFlushCount:         1,
		HNSWm:                 8,
		HNSWefConstruct:       40,
		HNSWefSearch:          40,
		CacheMemoryBytes:      256 * 1024 * 1024,
		CompactionMaxSegments: 0, // we'll trigger compaction manually
	}
	e, _ := NewEngine(be, cfg)
	ctx := context.Background()
	e.CreateCollection(ctx, Collection{Name: "cmp", Dimensions: 64, Distance: L2})

	// Build two ~500-point sealed segments.
	rng := mrand.New(mrand.NewSource(2))
	for batch := 0; batch < 2; batch++ {
		pts := make([]Point, 500)
		for i := range pts {
			pts[i] = Point{ID: uint64(batch*1000 + i + 1), Vector: benchRandVec(64, rng)}
		}
		e.Insert(ctx, "cmp", pts)
		// Wait for the seal triggered by SegmentSizeBytes=1.
		e.SyncSealing("cmp")
	}

	e.cfg.CompactionMaxSegments = 1 // force compaction-eligible
	e.mu.RLock()
	cs := e.collections["cmp"]
	e.mu.RUnlock()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset compaction state by re-running maybeCompact directly. Each
		// call merges the smallest pair, so subsequent iterations are cheap;
		// useful for steady-state cost rather than peak.
		_ = e.maybeCompact(ctx, cs, cs.shards[0])
	}
	b.StopTimer()
	e.Close()
}

// BenchmarkSearch_768d_WithFilter — filtered search
func BenchmarkSearch_768d_WithFilter(b *testing.B) {
	engine := benchSetup(b, 5000, 768)
	rng := mrand.New(mrand.NewSource(99))
	query := benchRandVec(768, rng)
	ctx := context.Background()
	filter := &Filter{Field: "type", Op: OpEq, Value: "type_3"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Search(ctx, SearchRequest{Collection: "bench", Vector: query, TopK: 10, Filter: filter})
	}
}
