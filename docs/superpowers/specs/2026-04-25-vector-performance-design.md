# Vector Engine Performance: Qdrant-Parity Optimization

**Date:** 2026-04-25
**Status:** Draft
**Depends on:** Existing vector engine (`pkg/vector/`)
**Goal:** 20-50x across-the-board performance improvement to reach Qdrant parity

## Problem

Current benchmarks on Apple M4, 768-dim vectors:

| Benchmark | Current | Qdrant Reference | Gap |
|---|---|---|---|
| Search 500pts | 550µs | ~50µs | 11x |
| Search 5000pts | 4.9ms | ~100µs | 49x |
| Insert single | 7.4ms | ~50µs | 148x |
| Seal+HNSW 40k pts | >11min (killed) | ~2-3s | >200x |
| Allocs per insert | 4568 | ~0 | ∞ |
| Allocs per search | 18-24 | ~0 | ∞ |

Root causes: scalar distance functions, pointer-chasing memory layout, per-search map allocations, simple neighbor selection, per-search bitmap rebuild for filters, sequential HNSW construction.

## Performance Targets

| Operation | Target |
|---|---|
| Search 500pts/768d (hot) | < 50µs |
| Search 5000pts/768d (hot) | < 100µs |
| Search 50k pts/768d (hot) | < 1ms |
| Insert single point | < 50µs |
| Seal+HNSW build 50k/768d | < 5s |
| Search allocs | 0 |
| Insert allocs | < 10 |
| Memory per vector (768d, SQ8) | ~800 bytes (vs 3KB today) |

## Architecture Overview

Six interconnected changes, ordered by dependency:

```
1. Flat Memory Layout ──────────┐
2. SIMD Distance Functions ─────┤
3. HNSW Algorithmic Improvements┤──► 4. Parallel HNSW Build
5. Scalar Quantization ─────────┘        │
6. Engine-Level Fixes ───────────────────┘
```

Changes 1-3 are independent foundations. Change 4 builds on all three. Change 5 builds on 1 and 2. Change 6 is independent of the HNSW changes.

## 1. Flat Memory Layout

Replace `map[uint64]*hnswNode` with contiguous arrays indexed by sequential uint32 internal IDs.

### Current Layout (per node)

```
hnswNode struct {          // heap-allocated, pointer-chased
    id      uint64
    vector  []float32      // separate heap allocation, 768*4 = 3072 bytes
    level   int
    friends [][]uint64     // slice of slices, each heap-allocated
}
nodes map[uint64]*hnswNode // hash map with pointer values
```

Every graph hop requires: map lookup → pointer dereference → vector slice header → vector data. Minimum 3 cache misses per hop.

### New Layout

```go
type HNSWIndex struct {
    // All vectors in a single contiguous block.
    // vectors[i*dim : (i+1)*dim] is the vector for internal node i.
    vectors []float32

    // Neighbor lists packed per layer.
    // neighborOffsets[layer][node] = start index into neighborData[layer]
    // neighborCounts[layer][node]  = number of neighbors at this layer
    // neighborData[layer][offset : offset+count] = neighbor internal IDs
    neighborOffsets [][]uint32
    neighborCounts  [][]uint16
    neighborData    [][]uint32

    // ID mapping
    extToInt map[uint64]uint32 // external PointID → internal sequential ID
    intToExt []uint64          // internal ID → external PointID

    // Per-node metadata
    levels    []uint8  // max layer for each node
    nodeCount uint32
    capacity  uint32   // pre-allocated size (known at seal time)

    // Visited tracking — generation counter avoids per-search allocation
    visitedGen []uint32 // per-node generation stamp
    currentGen uint32   // bumped each search

    // Index parameters
    dim         int
    m           int
    mMax0       int
    efConstruct int
    maxLevel    int
    entryPoint  uint32
    hasEntry    bool
    distFunc    DistanceFunc
    levelMult   float64
    rng         *rand.Rand
}
```

### Key Design Decisions

- **uint32 internal IDs:** halves neighbor list memory vs uint64. Supports up to 4B points per segment (segment max is 50k-100k, so this is plenty).
- **Pre-allocated capacity:** at seal time we know the exact point count. All arrays allocated once with exact size. Zero realloc during HNSW build.
- **Generation-counter visited set:** `visitedGen[node] == currentGen` means visited. Incrementing `currentGen` "clears" the set in O(1). When `currentGen` wraps (every ~4B searches), zero the array. Eliminates the `map[uint64]struct{}` allocation per search.
- **Vectors stored once:** the flat `vectors` slice is the single source of truth. No duplication between segment and HNSW.

### Serialization

Binary format changes to match new layout:

```
Header:     dim(u32) | m(u32) | efConstruct(u32) | maxLevel(u32) | entryPoint(u32) | hasEntry(u8) | nodeCount(u32)
Vectors:    [nodeCount * dim * 4 bytes, contiguous float32]
IntToExt:   [nodeCount * 8 bytes, uint64 per node]
Levels:     [nodeCount bytes, uint8 per node]
Per-layer:  layerCount(u32)
            for each layer:
              nodeCountAtLayer(u32)
              offsets[nodeCountAtLayer * 4 bytes]
              counts[nodeCountAtLayer * 2 bytes]
              dataLen(u32)
              data[dataLen * 4 bytes]
```

Deserialization is a single sequential read with known sizes — no per-node parsing loops.

## 2. SIMD Distance Functions

### File Structure

```
distance.go           — DistanceFunc type, DistanceFuncFor() dispatcher
distance_arm64.go     — //go:build arm64 — Go function declarations
distance_arm64.s      — ARM64 NEON assembly
distance_amd64.go     — //go:build amd64 — Go function declarations
distance_amd64.s      — AVX2 assembly
distance_generic.go   — //go:build !arm64 && !amd64 — scalar fallback
```

### ARM64 NEON Implementation

Process 16 floats per loop iteration (4 NEON registers × 4 float32 lanes):

**L2 Distance:**
```asm
// func l2DistanceNEON(a, b unsafe.Pointer, n uint64) float32
// V0-V3: four accumulator registers (zeroed)
// Loop (n/16 iterations):
//   VLD1.P 64(R0), [V4.S4, V5.S4, V6.S4, V7.S4]    // load 16 floats from a
//   VLD1.P 64(R1), [V16.S4, V17.S4, V18.S4, V19.S4] // load 16 floats from b
//   VSUB V16.S4, V4.S4, V4.S4                         // diff
//   FMLA V0.S4, V4.S4, V4.S4                          // accumulate diff²
//   ... (repeat for V5-V7)
// Horizontal add: FADDP across V0-V3 → single float32
// Tail: scalar loop for remaining n%16 elements
```

**Cosine Distance:**
Single-pass with 3 accumulators (dot, normA, normB). Same loop structure, uses `FMLA` for all three products.

**Inner Product:**
Single accumulator, simplest case.

### AMD64 AVX2 Implementation

Same algorithms, 256-bit registers (8 float32 per VFMADD). Detect AVX2 at init time; fall back to SSE2 or scalar if unavailable.

### Go-Callable Interface

```go
//go:build arm64

//go:noescape
func l2DistanceASM(a, b unsafe.Pointer, n uint64) float32

func L2Distance(a, b []float32) float32 {
    if len(a) == 0 {
        return 0
    }
    return l2DistanceASM(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), uint64(len(a)))
}
```

### Expected Speedup

ARM64 NEON on M4: ~6x for 768-dim L2 distance (measured by similar Go SIMD implementations). Cosine slightly less due to 3 accumulators competing for register ports (~5x).

## 3. HNSW Algorithmic Improvements

### 3a: Heuristic Neighbor Selection

Replace `selectNeighborsSimple` (closest-M) with Algorithm 4 from the HNSW paper.

```go
func (h *HNSWIndex) selectNeighborsHeuristic(
    target []float32,
    candidates []searchResult,
    m int,
) []searchResult {
    sort.Slice(candidates, func(i, j int) bool {
        return candidates[i].distance < candidates[j].distance
    })

    selected := make([]searchResult, 0, m)
    for _, c := range candidates {
        if len(selected) >= m {
            break
        }
        // Keep c if it's not "covered" by any already-selected neighbor
        good := true
        cVec := h.vectorAt(c.id)
        for _, s := range selected {
            sVec := h.vectorAt(s.id)
            if h.distFunc(cVec, sVec) < c.distance {
                good = false
                break
            }
        }
        if good {
            selected = append(selected, c)
        }
    }

    // Backfill with closest remaining if heuristic didn't fill M
    if len(selected) < m {
        for _, c := range candidates {
            if len(selected) >= m {
                break
            }
            found := false
            for _, s := range selected {
                if s.id == c.id {
                    found = true
                    break
                }
            }
            if !found {
                selected = append(selected, c)
            }
        }
    }
    return selected
}
```

**Impact:** 20-40% fewer distance computations per search (better graph connectivity means fewer hops). ~10% slower insert due to extra distance calcs during selection. Net positive.

### 3b: Typed Zero-Alloc Heaps

Replace `container/heap` with inline typed implementations.

```go
// searchMinHeap is a min-heap of search results ordered by distance.
// Pre-allocated, reusable, no interface{} boxing.
type searchMinHeap struct {
    items []searchResult
    len   int
}

func (h *searchMinHeap) reset(cap int) {
    if cap <= 0 { cap = 64 }
    if h.items == nil || cap > len(h.items) {
        h.items = make([]searchResult, cap)
    }
    h.len = 0
}

func (h *searchMinHeap) push(item searchResult) { /* inline siftUp */ }
func (h *searchMinHeap) pop() searchResult       { /* inline siftDown */ }
func (h *searchMinHeap) peek() searchResult       { return h.items[0] }

// searchMaxHeap — same structure, reversed comparison for results collection
type searchMaxHeap struct { /* identical but Less is > */ }

// boundedMaxHeap — for top-K collection with fixed capacity
type boundedMaxHeap struct {
    items []searchResult
    len   int
    cap   int
}

func (h *boundedMaxHeap) tryPush(item searchResult) bool {
    if h.len < h.cap {
        h.push(item)
        return true
    }
    if item.distance < h.items[0].distance {
        h.items[0] = item
        h.siftDown(0)
        return true
    }
    return false
}
```

Heap buffers come from a `sync.Pool` per goroutine, sized to `efSearch`.

**Impact:** eliminates 18-24 allocs per search, ~4500 allocs per insert.

### 3c: Bounded Top-K for Growing Segment Brute-Force

Replace `sort.Slice` of all results with a `boundedMaxHeap`:

```go
func (g *GrowingSegment) Search(query []float32, topK int, allow map[uint64]struct{}) []ScoredPoint {
    var heap boundedMaxHeap
    heap.init(topK)

    for i := range g.points {
        if allow != nil {
            if _, ok := allow[g.points[i].ID]; !ok {
                continue
            }
        }
        d := g.distFunc(query, g.points[i].Vector)
        heap.tryPush(searchResult{id: uint32(i), distance: d})
    }

    return heap.drainSorted(g.points) // maps internal indices back to ScoredPoint
}
```

**Impact:** avoids allocating and sorting a full N-element slice. For 5000 points with top-10, eliminates a 5000-element allocation.

## 4. Parallel HNSW Construction

### Two-Phase Build

**Phase 1 — Seed the graph sequentially:**

Insert the first ~1000 points single-threaded to establish a well-connected initial graph. This ensures the entry point and upper layers are stable before concurrent access.

**Phase 2 — Parallel insert remaining points:**

```go
func (h *HNSWIndex) BuildParallel(ids []uint64, vectors []float32, workers int) {
    seedCount := min(1000, len(ids))
    for i := 0; i < seedCount; i++ {
        h.insertSingle(i)
    }

    var wg sync.WaitGroup
    ch := make(chan int, 256)
    for w := 0; w < workers; w++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for idx := range ch {
                h.insertSingle(idx)
            }
        }()
    }
    for i := seedCount; i < len(ids); i++ {
        ch <- i
    }
    close(ch)
    wg.Wait()
}
```

### Fine-Grained Locking

Replace the single `sync.RWMutex` with per-node neighbor locks:

```go
type neighborLock struct {
    mu sync.Mutex
}

type HNSWIndex struct {
    // ...
    nodeLocks []neighborLock // one per node, allocated at capacity
    entryMu   sync.RWMutex  // protects entryPoint and maxLevel only
}
```

During insertion:
- **Graph traversal (read path):** lock-free. Reading neighbor lists is safe because we only append (slice data is stable for existing elements).
- **Neighbor update (write path):** lock only the specific node being updated. Two concurrent inserts only contend if they're updating the same node's neighbor list.
- **Entry point update:** `entryMu` write lock, only when a new node has a higher level than current max (rare after first ~100 inserts).

### Post-Build Pruning

After parallel build, a single pass over all nodes to re-apply heuristic selection where neighbor count exceeds M. This cleans up any over-connections from concurrent append races:

```go
func (h *HNSWIndex) pruneNeighbors() {
    for node := uint32(0); node < h.nodeCount; node++ {
        for layer := 0; layer <= int(h.levels[node]); layer++ {
            mMax := h.m
            if layer == 0 { mMax = h.mMax0 }
            neighbors := h.neighborsAt(node, layer)
            if len(neighbors) > mMax {
                h.pruneNode(node, layer, mMax)
            }
        }
    }
}
```

### Expected Build Performance

On M4 (10 cores), 50k points / 768-dim:
- Sequential + SIMD only: ~60s (6x speedup from SIMD distance)
- Parallel (8 workers) + SIMD: ~10-15s (additional 4-6x)
- Parallel + SIMD + flat layout: ~3-5s (cache efficiency gains)

## 5. Scalar Quantization (SQ8)

### Encoding

At seal time, compute per-dimension min/max across all vectors, then quantize:

```go
type SQParams struct {
    Mins   []float32 // per-dimension minimum value
    Scales []float32 // per-dimension: 255.0 / (max - min)
}

type QuantizedVectors struct {
    params SQParams
    data   []uint8 // [nodeCount * dim] contiguous
}

func (q *QuantizedVectors) Encode(vectors []float32, dim int) {
    n := len(vectors) / dim
    q.data = make([]uint8, len(vectors))

    // Compute min/max per dimension
    q.params.Mins = make([]float32, dim)
    maxs := make([]float32, dim)
    for d := 0; d < dim; d++ {
        q.params.Mins[d] = math.MaxFloat32
        maxs[d] = -math.MaxFloat32
    }
    for i := 0; i < n; i++ {
        for d := 0; d < dim; d++ {
            v := vectors[i*dim+d]
            if v < q.params.Mins[d] { q.params.Mins[d] = v }
            if v > maxs[d] { maxs[d] = v }
        }
    }

    q.params.Scales = make([]float32, dim)
    for d := 0; d < dim; d++ {
        r := maxs[d] - q.params.Mins[d]
        if r == 0 { r = 1 }
        q.params.Scales[d] = 255.0 / r
    }

    // Quantize
    for i := 0; i < n; i++ {
        for d := 0; d < dim; d++ {
            v := (vectors[i*dim+d] - q.params.Mins[d]) * q.params.Scales[d]
            q.data[i*dim+d] = uint8(clamp(v, 0, 255))
        }
    }
}
```

### Two-Phase Search with Rescoring

```go
func (s *SealedSegment) Search(query []float32, topK, efSearch int, allow map[uint64]struct{}) []ScoredPoint {
    // Phase 1: HNSW search using quantized distances (fast, approximate)
    quantizedQuery := s.quantized.QuantizeQuery(query)
    approxCandidates := s.hnsw.Search(quantizedQuery, efSearch, efSearch, allow, s.quantized.L2Uint8)

    // Phase 2: rescore top 4*topK with full float32 precision
    rescoreCount := min(4*topK, len(approxCandidates))
    for i := 0; i < rescoreCount; i++ {
        c := &approxCandidates[i]
        c.distance = s.distFunc(query, s.vectorAt(c.id))
    }

    // Sort rescored candidates and return topK
    sort.Slice(approxCandidates[:rescoreCount], func(i, j int) bool {
        return approxCandidates[i].distance < approxCandidates[j].distance
    })
    // ... build ScoredPoint results
}
```

### SIMD Integer Distance (ARM64)

```asm
// func l2DistanceUint8NEON(a, b unsafe.Pointer, n uint64) uint32
// Uses UABD (unsigned absolute difference) + UMLAL (multiply-accumulate long)
// Processes 16 bytes per iteration
// 16 uint8 elements per NEON register — 2x throughput vs float32
```

### When SQ8 is Used

- **Sealed segments with >1000 points:** SQ8 for HNSW traversal + float32 rescore
- **Sealed segments with ≤1000 points:** float32 only (quantization overhead not worth it)
- **Growing segments:** always float32 (mutable, no stable min/max)

### Serialization

SQ8 parameters and data added to segment format:

```
segments/{id}/quantized.bin:
  Header: dim(u32) | nodeCount(u32)
  Mins:   [dim * 4 bytes, float32]
  Scales: [dim * 4 bytes, float32]
  Data:   [nodeCount * dim bytes, uint8]
```

## 6. Engine-Level Fixes

### 6a: Incremental Bitmap Index

Maintain a persistent combined bitmap index per collection instead of rebuilding per search.

```go
type collectionState struct {
    // ...existing fields...
    combinedBitmaps *BitmapIndex // incrementally maintained
}

// On insert:
func (e *Engine) Insert(ctx context.Context, collection string, points []Point) error {
    // ...existing validation...
    for _, p := range points {
        cs.growing.Insert(p)
        cs.wal.Append(WALEntry{Type: WALInsert, Point: p})
        // Incremental bitmap update — O(1) per field
        for k, v := range p.Payload {
            cs.combinedBitmaps.Add(p.ID, k, fmt.Sprint(v))
        }
    }
}

// On search:
func (e *Engine) Search(...) {
    if req.Filter != nil {
        filterResult := EvalFilter(*req.Filter, cs.combinedBitmaps)
        if len(cs.deleted) > 0 {
            filterResult.AndNot(cs.deletedBitmap)
        }
        allow = bitmapToMap(filterResult)
    }
}
```

**Impact:** filtered search drops from O(total_points * fields) to O(filter_evaluation).

### 6b: Lock-Free Search Path

Snapshot segment state under a brief lock, then search without holding it:

```go
func (e *Engine) Search(_ context.Context, req SearchRequest) (*SearchResponse, error) {
    cs.mu.RLock()
    growing := cs.growing
    sealed := cs.sealed                  // slice header copy
    combinedBitmaps := cs.combinedBitmaps // pointer copy
    deletedSnap := cs.deletedBitmap.Clone()
    cs.mu.RUnlock()

    // Everything below is lock-free
    // ...search growing, search sealed, merge results...
}
```

### 6c: Parallel Multi-Segment Search

When multiple sealed segments exist, search them concurrently:

```go
if len(sealed) == 1 {
    sealedResults = sealed[0].Search(query, topK, efSearch, allow)
} else if len(sealed) > 1 {
    var wg sync.WaitGroup
    resultsCh := make(chan []ScoredPoint, len(sealed))
    for _, ss := range sealed {
        wg.Add(1)
        go func(ss *SealedSegment) {
            defer wg.Done()
            resultsCh <- ss.Search(query, topK, efSearch, allow)
        }(ss)
    }
    wg.Wait()
    close(resultsCh)
    for r := range resultsCh {
        sealedResults = append(sealedResults, r...)
    }
}
```

### 6d: Prefetch During HNSW Traversal

When visiting a node's neighbors, prefetch the next neighbor's vector data before computing distance on the current one:

```go
// In searchLayer, after loading neighbor list:
neighbors := h.neighborsAt(nodeID, layer)
for i, nid := range neighbors {
    // Prefetch next neighbor's vector while computing current distance
    if i+1 < len(neighbors) {
        prefetchL1(unsafe.Pointer(&h.vectors[neighbors[i+1]*uint32(h.dim)]))
    }
    // Compute distance for current neighbor
    d := h.distFunc(query, h.vectors[nid*uint32(h.dim):(nid+1)*uint32(h.dim)])
    // ...
}
```

ARM64: `PRFM PLDL1KEEP, [Xn]` instruction via assembly helper.

**Impact:** 10-30% improvement when index exceeds L2 cache (~1MB on M4 efficiency cores, ~4MB on performance cores). At 50k vectors × 768 dims × 4 bytes = 150MB, this is always cache-exceeded territory.

## File Changes

### New Files

```
pkg/vector/distance_arm64.go     — Go declarations for ARM64 NEON distance functions
pkg/vector/distance_arm64.s      — ARM64 NEON assembly
pkg/vector/distance_amd64.go     — Go declarations for AMD64 AVX2 distance functions
pkg/vector/distance_amd64.s      — AMD64 AVX2 assembly
pkg/vector/distance_generic.go   — Scalar fallback (renamed from distance.go)
pkg/vector/quantize.go           — SQ8 encoding/decoding, quantized distance functions
pkg/vector/heap.go               — Typed zero-alloc min/max/bounded heaps
```

### Modified Files

```
pkg/vector/hnsw.go       — Complete rewrite: flat layout, heuristic selection, parallel build,
                            generation-counter visited set, prefetch
pkg/vector/distance.go   — Becomes dispatcher only (DistanceFuncFor with build-tag routing)
pkg/vector/segment.go    — Add SQ8 to SealSegment, quantized search path in SealedSegment.Search
pkg/vector/engine.go     — Incremental bitmap, lock-free search, parallel segment search
pkg/vector/types.go      — Add SQParams, QuantizedVectors types
pkg/vector/benchmark_test.go — Add 50k benchmarks, HNSW build benchmark, filtered search benchmark,
                               comparison benchmarks (before/after)
```

### Unchanged Files

```
pkg/vector/bitmap.go     — No changes needed
pkg/vector/filter.go     — No changes needed
pkg/vector/cache.go      — No changes needed
pkg/vector/wal.go        — No changes needed
```

## Build Tags

All SIMD code gated behind architecture build tags. The `vector` build tag remains required for the entire package. No new build tags needed — architecture detection is automatic via Go's `//go:build arm64` / `//go:build amd64`.

## Testing Strategy

### Correctness Tests

- **HNSW recall test:** insert N random vectors, verify search returns true nearest neighbors (brute-force comparison) with recall@10 > 0.95 at ef=128
- **SQ8 recall test:** verify quantized search + rescore achieves recall@10 > 0.99 vs float32
- **Parallel build equivalence:** verify parallel-built index has same recall as sequential-built
- **Heuristic selection:** verify graph connectivity — every node reachable from entry point
- **SIMD correctness:** compare SIMD distance output vs scalar for random vectors, edge cases (zero vectors, identical vectors, all-ones)

### Benchmark Suite

```go
// Core benchmarks (before/after comparison)
BenchmarkL2Distance_768d              // SIMD vs scalar
BenchmarkCosineDistance_768d
BenchmarkHNSWBuild_50k_768d           // parallel vs sequential
BenchmarkHNSWSearch_50k_768d_top10
BenchmarkHNSWSearch_50k_768d_top10_SQ8
BenchmarkSearch_768d_500pts           // existing, expect 10x improvement
BenchmarkSearch_768d_5000pts          // existing, expect 20x improvement
BenchmarkSearch_768d_50000pts         // new scale benchmark
BenchmarkInsert_768d                  // existing, expect 100x improvement
BenchmarkFilteredSearch_5000pts       // existing, expect 100x improvement
BenchmarkSealSegment_50k_768d         // new, target <5s
```

## Implementation Order

1. **Typed heaps + bounded top-K** — pure Go, no dependencies, immediate alloc reduction
2. **Flat memory layout** — rewrite HNSWIndex internals, update serialization
3. **SIMD distance functions** — ARM64 first (dev machine), AMD64 second
4. **Heuristic neighbor selection** — swap in after flat layout is stable
5. **Generation-counter visited set** — trivial once flat layout is in place
6. **Engine fixes** — incremental bitmap, lock-free search, parallel segment search
7. **Parallel HNSW build** — add per-node locks, two-phase build
8. **Scalar quantization** — encoding, quantized distance, two-phase search
9. **Prefetch hints** — final polish, measure on large indices
10. **Benchmark suite** — comprehensive before/after comparison

Steps 1-5 can be developed and tested independently. Step 6 is independent of HNSW changes. Steps 7-9 build on earlier steps.
