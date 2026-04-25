# Vector Engine Performance Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Achieve 20-50x performance improvement across search, insert, and HNSW build to reach Qdrant parity.

**Architecture:** Replace the naive HNSW implementation (map-based nodes, scalar distance, per-search allocations) with a flat memory layout, SIMD distance functions (ARM64 NEON), typed zero-alloc heaps, heuristic neighbor selection, parallel HNSW construction, scalar quantization, and engine-level fixes (incremental bitmaps, lock-free search).

**Tech Stack:** Go 1.24, ARM64 NEON assembly, roaring bitmaps, `unsafe.Pointer` for SIMD bridge, `sync.Pool` for buffer reuse.

---

## File Structure

### New Files

| File | Responsibility |
|---|---|
| `pkg/vector/heap.go` | Typed zero-alloc min/max/bounded heaps for search |
| `pkg/vector/heap_test.go` | Heap correctness tests |
| `pkg/vector/distance_generic.go` | `//go:build !arm64` — scalar distance fallback (current code, relocated) |
| `pkg/vector/distance_simd.go` | `//go:build arm64` — Go declarations for NEON distance functions |
| `pkg/vector/distance_arm64.s` | ARM64 NEON assembly for L2, cosine, inner product |
| `pkg/vector/quantize.go` | SQ8 encoding, decoding, quantized distance functions |
| `pkg/vector/quantize_test.go` | SQ8 correctness and recall tests |

### Modified Files

| File | Changes |
|---|---|
| `pkg/vector/distance.go` | Becomes dispatcher: `DistanceFuncFor()`, type def. Removes implementations. |
| `pkg/vector/distance_test.go` | No changes needed — tests call `CosineDistance` etc. which route via build tags |
| `pkg/vector/hnsw.go` | Complete rewrite: flat arrays, heuristic selection, generation-counter visited, parallel build, prefetch |
| `pkg/vector/hnsw_test.go` | Add recall test, parallel build test. Existing tests pass unchanged. |
| `pkg/vector/segment.go` | `GrowingSegment.Search` uses bounded heap. `SealSegment` uses parallel build + SQ8. New serialization. |
| `pkg/vector/segment_test.go` | Existing tests pass. Add SQ8 round-trip test. |
| `pkg/vector/engine.go` | Incremental bitmap, snapshot-based lock-free search, parallel segment search |
| `pkg/vector/engine_test.go` | Existing tests pass. Add concurrent search test. |
| `pkg/vector/types.go` | Add `SQParams`, `QuantizedVectors` types |
| `pkg/vector/benchmark_test.go` | Add 50k benchmarks, HNSW build benchmark, distance benchmarks |

### Unchanged Files

| File | Reason |
|---|---|
| `pkg/vector/bitmap.go` | No changes needed |
| `pkg/vector/filter.go` | No changes needed |
| `pkg/vector/cache.go` | No changes needed |
| `pkg/vector/wal.go` | No changes needed |

---

## Task 1: Typed Zero-Alloc Heaps

**Files:**
- Create: `pkg/vector/heap.go`
- Create: `pkg/vector/heap_test.go`

- [ ] **Step 1: Write failing tests for searchMinHeap**

```go
// pkg/vector/heap_test.go
//go:build vector

package vector

import "testing"

func TestSearchMinHeap_PushPop(t *testing.T) {
	var h searchMinHeap
	h.init(10)
	h.push(searchResult{id: 1, distance: 3.0})
	h.push(searchResult{id: 2, distance: 1.0})
	h.push(searchResult{id: 3, distance: 2.0})

	if h.len != 3 {
		t.Fatalf("expected len 3, got %d", h.len)
	}

	r := h.pop()
	if r.id != 2 || r.distance != 1.0 {
		t.Fatalf("expected id=2 dist=1.0, got id=%d dist=%f", r.id, r.distance)
	}
	r = h.pop()
	if r.id != 3 || r.distance != 2.0 {
		t.Fatalf("expected id=3 dist=2.0, got id=%d dist=%f", r.id, r.distance)
	}
	r = h.pop()
	if r.id != 1 || r.distance != 3.0 {
		t.Fatalf("expected id=1 dist=3.0, got id=%d dist=%f", r.id, r.distance)
	}
}

func TestSearchMaxHeap_PushPop(t *testing.T) {
	var h searchMaxHeap
	h.init(10)
	h.push(searchResult{id: 1, distance: 1.0})
	h.push(searchResult{id: 2, distance: 3.0})
	h.push(searchResult{id: 3, distance: 2.0})

	r := h.pop()
	if r.id != 2 || r.distance != 3.0 {
		t.Fatalf("expected id=2 dist=3.0, got id=%d dist=%f", r.id, r.distance)
	}
}

func TestSearchMaxHeap_Peek(t *testing.T) {
	var h searchMaxHeap
	h.init(10)
	h.push(searchResult{id: 1, distance: 1.0})
	h.push(searchResult{id: 2, distance: 5.0})
	h.push(searchResult{id: 3, distance: 3.0})

	r := h.peek()
	if r.distance != 5.0 {
		t.Fatalf("expected peek dist=5.0, got %f", r.distance)
	}
	if h.len != 3 {
		t.Fatalf("peek should not remove, len should be 3, got %d", h.len)
	}
}

func TestBoundedMaxHeap_TryPush(t *testing.T) {
	var h boundedMaxHeap
	h.init(3)

	h.tryPush(searchResult{id: 1, distance: 5.0})
	h.tryPush(searchResult{id: 2, distance: 3.0})
	h.tryPush(searchResult{id: 3, distance: 7.0})
	// Heap full with [7.0, 5.0, 3.0]. Now push something smaller than max.
	h.tryPush(searchResult{id: 4, distance: 1.0})

	if h.len != 3 {
		t.Fatalf("expected len 3, got %d", h.len)
	}
	// Max should now be 5.0 (7.0 was evicted)
	if h.peek().distance != 5.0 {
		t.Fatalf("expected max 5.0, got %f", h.peek().distance)
	}

	// Push something larger than max — should be rejected
	h.tryPush(searchResult{id: 5, distance: 10.0})
	if h.peek().distance != 5.0 {
		t.Fatalf("should not have inserted 10.0, max is %f", h.peek().distance)
	}
}

func TestBoundedMaxHeap_DrainSorted(t *testing.T) {
	var h boundedMaxHeap
	h.init(5)
	h.tryPush(searchResult{id: 1, distance: 5.0})
	h.tryPush(searchResult{id: 2, distance: 1.0})
	h.tryPush(searchResult{id: 3, distance: 3.0})

	sorted := h.drainSorted()
	if len(sorted) != 3 {
		t.Fatalf("expected 3, got %d", len(sorted))
	}
	for i := 1; i < len(sorted); i++ {
		if sorted[i].distance < sorted[i-1].distance {
			t.Fatalf("not sorted at %d: %f < %f", i, sorted[i].distance, sorted[i-1].distance)
		}
	}
}

func TestSearchMinHeap_Empty(t *testing.T) {
	var h searchMinHeap
	h.init(10)
	if h.len != 0 {
		t.Fatalf("expected len 0, got %d", h.len)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags vector -run TestSearchMinHeap -v ./pkg/vector/ 2>&1 | head -5`
Expected: compilation error — `searchMinHeap` undefined

- [ ] **Step 3: Implement typed heaps**

```go
// pkg/vector/heap.go
//go:build vector

package vector

// searchResult is the internal search result type used by heaps and HNSW.
// Uses uint32 internal IDs for cache efficiency.
type searchResult struct {
	id       uint32
	distance float32
}

// searchMinHeap is a min-heap ordered by distance. Zero-alloc after init.
type searchMinHeap struct {
	items []searchResult
	len   int
}

func (h *searchMinHeap) init(cap int) {
	if cap <= 0 {
		cap = 64
	}
	if h.items == nil || cap > len(h.items) {
		h.items = make([]searchResult, cap)
	}
	h.len = 0
}

func (h *searchMinHeap) push(item searchResult) {
	if h.len == len(h.items) {
		h.items = append(h.items, item)
	} else {
		h.items[h.len] = item
	}
	h.len++
	h.siftUp(h.len - 1)
}

func (h *searchMinHeap) pop() searchResult {
	top := h.items[0]
	h.len--
	h.items[0] = h.items[h.len]
	if h.len > 0 {
		h.siftDown(0)
	}
	return top
}

func (h *searchMinHeap) peek() searchResult {
	return h.items[0]
}

func (h *searchMinHeap) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h.items[i].distance >= h.items[parent].distance {
			break
		}
		h.items[i], h.items[parent] = h.items[parent], h.items[i]
		i = parent
	}
}

func (h *searchMinHeap) siftDown(i int) {
	for {
		smallest := i
		left := 2*i + 1
		right := 2*i + 2
		if left < h.len && h.items[left].distance < h.items[smallest].distance {
			smallest = left
		}
		if right < h.len && h.items[right].distance < h.items[smallest].distance {
			smallest = right
		}
		if smallest == i {
			break
		}
		h.items[i], h.items[smallest] = h.items[smallest], h.items[i]
		i = smallest
	}
}

// searchMaxHeap is a max-heap ordered by distance. Zero-alloc after init.
type searchMaxHeap struct {
	items []searchResult
	len   int
}

func (h *searchMaxHeap) init(cap int) {
	if cap <= 0 {
		cap = 64
	}
	if h.items == nil || cap > len(h.items) {
		h.items = make([]searchResult, cap)
	}
	h.len = 0
}

func (h *searchMaxHeap) push(item searchResult) {
	if h.len == len(h.items) {
		h.items = append(h.items, item)
	} else {
		h.items[h.len] = item
	}
	h.len++
	h.siftUp(h.len - 1)
}

func (h *searchMaxHeap) pop() searchResult {
	top := h.items[0]
	h.len--
	h.items[0] = h.items[h.len]
	if h.len > 0 {
		h.siftDown(0)
	}
	return top
}

func (h *searchMaxHeap) peek() searchResult {
	return h.items[0]
}

func (h *searchMaxHeap) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h.items[i].distance <= h.items[parent].distance {
			break
		}
		h.items[i], h.items[parent] = h.items[parent], h.items[i]
		i = parent
	}
}

func (h *searchMaxHeap) siftDown(i int) {
	for {
		largest := i
		left := 2*i + 1
		right := 2*i + 2
		if left < h.len && h.items[left].distance > h.items[largest].distance {
			largest = left
		}
		if right < h.len && h.items[right].distance > h.items[largest].distance {
			largest = right
		}
		if largest == i {
			break
		}
		h.items[i], h.items[largest] = h.items[largest], h.items[i]
		i = largest
	}
}

// boundedMaxHeap is a max-heap with fixed capacity for top-K collection.
// When full, only items smaller than the current max are accepted.
type boundedMaxHeap struct {
	items []searchResult
	len   int
	cap   int
}

func (h *boundedMaxHeap) init(capacity int) {
	if h.items == nil || capacity > len(h.items) {
		h.items = make([]searchResult, capacity)
	}
	h.len = 0
	h.cap = capacity
}

func (h *boundedMaxHeap) tryPush(item searchResult) bool {
	if h.len < h.cap {
		h.items[h.len] = item
		h.len++
		h.siftUp(h.len - 1)
		return true
	}
	if item.distance < h.items[0].distance {
		h.items[0] = item
		h.siftDown(0)
		return true
	}
	return false
}

func (h *boundedMaxHeap) peek() searchResult {
	return h.items[0]
}

func (h *boundedMaxHeap) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h.items[i].distance <= h.items[parent].distance {
			break
		}
		h.items[i], h.items[parent] = h.items[parent], h.items[i]
		i = parent
	}
}

func (h *boundedMaxHeap) siftDown(i int) {
	for {
		largest := i
		left := 2*i + 1
		right := 2*i + 2
		if left < h.len && h.items[left].distance > h.items[largest].distance {
			largest = left
		}
		if right < h.len && h.items[right].distance > h.items[largest].distance {
			largest = right
		}
		if largest == i {
			break
		}
		h.items[i], h.items[largest] = h.items[largest], h.items[i]
		i = largest
	}
}

// drainSorted removes all items and returns them sorted by distance ascending.
func (h *boundedMaxHeap) drainSorted() []searchResult {
	out := make([]searchResult, h.len)
	for i := h.len - 1; i >= 0; i-- {
		out[i] = h.items[0]
		h.len--
		h.items[0] = h.items[h.len]
		if h.len > 0 {
			h.siftDown(0)
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -tags vector -run "TestSearch(Min|Max)Heap|TestBoundedMaxHeap" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 5: Run all existing tests to confirm no regressions**

Run: `go test -tags vector -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/vector/heap.go pkg/vector/heap_test.go
git commit -m "feat(vector): add typed zero-alloc heaps for search

Adds searchMinHeap, searchMaxHeap, and boundedMaxHeap with inline
sift operations. Eliminates container/heap interface{} boxing overhead."
```

---

## Task 2: Refactor Distance Functions into Build-Tag Files

Split `distance.go` into dispatcher + platform-specific implementations so SIMD can be added later without changing the API.

**Files:**
- Modify: `pkg/vector/distance.go` (keep dispatcher only)
- Create: `pkg/vector/distance_generic.go` (scalar implementations)

- [ ] **Step 1: Create distance_generic.go with scalar implementations**

```go
// pkg/vector/distance_generic.go
//go:build vector && !arm64

package vector

import "math"

func cosineDistance(a, b []float32) float32 {
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	denom := float32(math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
	if denom == 0 {
		return 1
	}
	return 1 - dot/denom
}

func l2Distance(a, b []float32) float32 {
	var sum float32
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

func innerProductDistance(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return -dot
}
```

- [ ] **Step 2: Update distance.go to be the dispatcher only**

```go
// pkg/vector/distance.go
//go:build vector

package vector

import "fmt"

// Public wrappers call the platform-specific lowercase implementations.
// On arm64 these route to NEON assembly; elsewhere to scalar Go.
func CosineDistance(a, b []float32) float32       { return cosineDistance(a, b) }
func L2Distance(a, b []float32) float32           { return l2Distance(a, b) }
func InnerProductDistance(a, b []float32) float32  { return innerProductDistance(a, b) }

func DistanceFuncFor(metric DistanceMetric) (DistanceFunc, error) {
	switch metric {
	case Cosine:
		return CosineDistance, nil
	case L2:
		return L2Distance, nil
	case InnerProduct:
		return InnerProductDistance, nil
	default:
		return nil, fmt.Errorf("unsupported distance metric: %s", metric)
	}
}
```

- [ ] **Step 3: Run all existing tests to confirm no regressions**

Run: `go test -tags vector -v ./pkg/vector/`
Expected: all PASS (distance_test.go calls `CosineDistance`, `L2Distance`, etc. which now dispatch to `distance_generic.go`)

- [ ] **Step 4: Commit**

```bash
git add pkg/vector/distance.go pkg/vector/distance_generic.go
git commit -m "refactor(vector): split distance functions into build-tag files

Dispatcher in distance.go, scalar implementations in distance_generic.go.
Prepares for SIMD assembly on arm64/amd64 without changing the public API."
```

---

## Task 3: ARM64 NEON Distance Functions

**Files:**
- Create: `pkg/vector/distance_simd.go`
- Create: `pkg/vector/distance_arm64.s`

- [ ] **Step 1: Write SIMD correctness tests**

Add to `pkg/vector/distance_test.go`:

```go
func TestCosineDistance_HighDim(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	a := randomVector(768, rng)
	b := randomVector(768, rng)

	d := CosineDistance(a, b)
	if d < 0 || d > 2 {
		t.Fatalf("cosine distance out of range: %f", d)
	}

	// Verify against naive computation
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	expected := float32(1 - dot/(math.Sqrt(na)*math.Sqrt(nb)))
	if math.Abs(float64(d-expected)) > 1e-4 {
		t.Fatalf("cosine distance mismatch: got %f, expected %f", d, expected)
	}
}

func TestL2Distance_HighDim(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	a := randomVector(768, rng)
	b := randomVector(768, rng)

	d := L2Distance(a, b)

	var expected float64
	for i := range a {
		diff := float64(a[i]) - float64(b[i])
		expected += diff * diff
	}
	if math.Abs(float64(d)-expected) > 0.1 {
		t.Fatalf("L2 distance mismatch: got %f, expected %f", d, expected)
	}
}

func TestInnerProductDistance_HighDim(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	a := randomVector(768, rng)
	b := randomVector(768, rng)

	d := InnerProductDistance(a, b)

	var expected float64
	for i := range a {
		expected += float64(a[i]) * float64(b[i])
	}
	if math.Abs(float64(d)-(-float64(float32(expected)))) > 0.1 {
		t.Fatalf("IP distance mismatch: got %f, expected %f", d, -float32(expected))
	}
}

func TestL2Distance_SmallVectors(t *testing.T) {
	// Test tail handling (non-multiple-of-16 sizes)
	for _, dim := range []int{1, 3, 7, 15, 16, 17, 31, 32, 33} {
		rng := rand.New(rand.NewSource(int64(dim)))
		a := randomVector(dim, rng)
		b := randomVector(dim, rng)
		d := L2Distance(a, b)

		var expected float32
		for i := range a {
			diff := a[i] - b[i]
			expected += diff * diff
		}
		if math.Abs(float64(d-expected)) > 1e-3 {
			t.Fatalf("dim=%d: got %f, expected %f", dim, d, expected)
		}
	}
}
```

- [ ] **Step 2: Run tests on current scalar implementation to establish baseline**

Run: `go test -tags vector -run "TestCosineDistance_HighDim|TestL2Distance_HighDim|TestInnerProductDistance_HighDim|TestL2Distance_SmallVectors" -v ./pkg/vector/`
Expected: all PASS (these tests work with scalar too)

- [ ] **Step 3: Create ARM64 NEON Go declarations**

```go
// pkg/vector/distance_simd.go
//go:build vector && arm64

package vector

import (
	"math"
	"unsafe"
)

//go:noescape
func l2DistanceASM(a, b unsafe.Pointer, n uint64) float32

//go:noescape
func dotASM(a, b unsafe.Pointer, n uint64) float32

//go:noescape
func dotNormsASM(a, b unsafe.Pointer, n uint64) (dot, normA, normB float32)

func l2Distance(a, b []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	return l2DistanceASM(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), uint64(len(a)))
}

func cosineDistance(a, b []float32) float32 {
	if len(a) == 0 {
		return 1
	}
	dot, normA, normB := dotNormsASM(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), uint64(len(a)))
	denom := float32(math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
	if denom == 0 {
		return 1
	}
	return 1 - dot/denom
}

func innerProductDistance(a, b []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	return -dotASM(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), uint64(len(a)))
}
```

- [ ] **Step 4: Create ARM64 NEON assembly**

```asm
// pkg/vector/distance_arm64.s
//go:build vector && arm64

#include "textflag.h"

// func l2DistanceASM(a, b unsafe.Pointer, n uint64) float32
TEXT ·l2DistanceASM(SB), NOSPLIT, $0-28
	MOVD a+0(FP), R0       // a pointer
	MOVD b+8(FP), R1       // b pointer
	MOVD n+16(FP), R2      // element count

	// Zero 4 accumulator registers
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16

	// Process 16 floats per iteration
	CMP $16, R2
	BLT l2_tail8

l2_loop16:
	VLD1.P 64(R0), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R1), [V16.S4, V17.S4, V18.S4, V19.S4]
	VFSUB V16.S4, V4.S4, V4.S4
	VFSUB V17.S4, V5.S4, V5.S4
	VFSUB V18.S4, V6.S4, V6.S4
	VFSUB V19.S4, V7.S4, V7.S4
	VFMLA V4.S4, V4.S4, V0.S4
	VFMLA V5.S4, V5.S4, V1.S4
	VFMLA V6.S4, V6.S4, V2.S4
	VFMLA V7.S4, V7.S4, V3.S4
	SUB $16, R2, R2
	CMP $16, R2
	BGE l2_loop16

l2_tail8:
	CMP $8, R2
	BLT l2_tail4
	VLD1.P 32(R0), [V4.S4, V5.S4]
	VLD1.P 32(R1), [V16.S4, V17.S4]
	VFSUB V16.S4, V4.S4, V4.S4
	VFSUB V17.S4, V5.S4, V5.S4
	VFMLA V4.S4, V4.S4, V0.S4
	VFMLA V5.S4, V5.S4, V1.S4
	SUB $8, R2, R2

l2_tail4:
	CMP $4, R2
	BLT l2_tail1
	VLD1.P 16(R0), [V4.S4]
	VLD1.P 16(R1), [V16.S4]
	VFSUB V16.S4, V4.S4, V4.S4
	VFMLA V4.S4, V4.S4, V0.S4
	SUB $4, R2, R2

l2_tail1:
	// Horizontal add accumulators: V0+V1+V2+V3
	VFADD V1.S4, V0.S4, V0.S4
	VFADD V3.S4, V2.S4, V2.S4
	VFADD V2.S4, V0.S4, V0.S4
	// Horizontal add within V0: s0+s1+s2+s3
	FADDP V0.S4, V0.S4, V0.S4
	FADDP V0.S2, V0.S2, V0.S2   // V0.S[0] = sum

	// Scalar tail for remaining elements
	CBZ R2, l2_done
l2_scalar:
	FMOVS (R0), F4
	FMOVS (R1), F5
	FSUBS F5, F4, F4
	FMULS F4, F4, F4
	FADDS F4, F0, F0
	ADD $4, R0, R0
	ADD $4, R1, R1
	SUB $1, R2, R2
	CBNZ R2, l2_scalar

l2_done:
	FMOVS F0, ret+24(FP)
	RET

// func dotASM(a, b unsafe.Pointer, n uint64) float32
TEXT ·dotASM(SB), NOSPLIT, $0-28
	MOVD a+0(FP), R0
	MOVD b+8(FP), R1
	MOVD n+16(FP), R2

	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16

	CMP $16, R2
	BLT dot_tail4

dot_loop16:
	VLD1.P 64(R0), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R1), [V16.S4, V17.S4, V18.S4, V19.S4]
	VFMLA V4.S4, V16.S4, V0.S4
	VFMLA V5.S4, V17.S4, V1.S4
	VFMLA V6.S4, V18.S4, V2.S4
	VFMLA V7.S4, V19.S4, V3.S4
	SUB $16, R2, R2
	CMP $16, R2
	BGE dot_loop16

dot_tail4:
	CMP $4, R2
	BLT dot_tail1
	VLD1.P 16(R0), [V4.S4]
	VLD1.P 16(R1), [V16.S4]
	VFMLA V4.S4, V16.S4, V0.S4
	SUB $4, R2, R2
	B dot_tail4

dot_tail1:
	VFADD V1.S4, V0.S4, V0.S4
	VFADD V3.S4, V2.S4, V2.S4
	VFADD V2.S4, V0.S4, V0.S4
	FADDP V0.S4, V0.S4, V0.S4
	FADDP V0.S2, V0.S2, V0.S2

	CBZ R2, dot_done
dot_scalar:
	FMOVS (R0), F4
	FMOVS (R1), F5
	FMULS F4, F5, F4
	FADDS F4, F0, F0
	ADD $4, R0, R0
	ADD $4, R1, R1
	SUB $1, R2, R2
	CBNZ R2, dot_scalar

dot_done:
	FMOVS F0, ret+24(FP)
	RET

// func dotNormsASM(a, b unsafe.Pointer, n uint64) (dot, normA, normB float32)
TEXT ·dotNormsASM(SB), NOSPLIT, $0-36
	MOVD a+0(FP), R0
	MOVD b+8(FP), R1
	MOVD n+16(FP), R2

	// V0 = dot accumulator, V1 = normA, V2 = normB
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16

	CMP $4, R2
	BLT dn_tail1

dn_loop4:
	VLD1.P 16(R0), [V4.S4]
	VLD1.P 16(R1), [V5.S4]
	VFMLA V4.S4, V5.S4, V0.S4    // dot += a*b
	VFMLA V4.S4, V4.S4, V1.S4    // normA += a*a
	VFMLA V5.S4, V5.S4, V2.S4    // normB += b*b
	SUB $4, R2, R2
	CMP $4, R2
	BGE dn_loop4

dn_tail1:
	// Horizontal add each accumulator
	FADDP V0.S4, V0.S4, V0.S4
	FADDP V0.S2, V0.S2, V0.S2
	FADDP V1.S4, V1.S4, V1.S4
	FADDP V1.S2, V1.S2, V1.S2
	FADDP V2.S4, V2.S4, V2.S4
	FADDP V2.S2, V2.S2, V2.S2

	CBZ R2, dn_done
dn_scalar:
	FMOVS (R0), F4
	FMOVS (R1), F5
	FMULS F4, F5, F6
	FADDS F6, F0, F0
	FMULS F4, F4, F6
	FADDS F6, F1, F1
	FMULS F5, F5, F6
	FADDS F6, F2, F2
	ADD $4, R0, R0
	ADD $4, R1, R1
	SUB $1, R2, R2
	CBNZ R2, dn_scalar

dn_done:
	FMOVS F0, ret+24(FP)
	FMOVS F1, ret+28(FP)
	FMOVS F2, ret+32(FP)
	RET
```

- [ ] **Step 5: Run distance tests to verify SIMD correctness**

Run: `go test -tags vector -run "TestCosineDistance|TestL2Distance|TestInnerProduct" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 6: Run all tests to confirm no regressions**

Run: `go test -tags vector -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
git add pkg/vector/distance_simd.go pkg/vector/distance_arm64.s pkg/vector/distance_test.go
git commit -m "feat(vector): add ARM64 NEON SIMD distance functions

L2, cosine, and inner product implemented in NEON assembly.
Processes 16 floats/iteration with scalar tail handling.
Falls back to generic Go on non-arm64 platforms."
```

---

## Task 4: Flat Memory Layout HNSW Rewrite

Complete rewrite of `hnsw.go`. Maintains the same public API (`NewHNSWIndex`, `Insert`, `Search`, `Serialize`, `DeserializeHNSWIndex`, `Len`) so all existing tests pass.

**Files:**
- Modify: `pkg/vector/hnsw.go` (complete rewrite)

- [ ] **Step 1: Run existing HNSW tests to confirm they pass before rewrite**

Run: `go test -tags vector -run "TestHNSW" -v ./pkg/vector/`
Expected: all 7 tests PASS

- [ ] **Step 2: Rewrite hnsw.go with flat memory layout, heuristic selection, generation-counter visited set**

```go
// pkg/vector/hnsw.go
//go:build vector

package vector

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sync"
)

// HNSWSearchResult holds a point ID and its distance from the query.
type HNSWSearchResult struct {
	ID       uint64
	Distance float32
}

// HNSWIndex is a flat-layout HNSW vector index optimized for cache locality.
//
// Vectors are stored in a single contiguous []float32 block.
// Neighbor lists are packed per-layer with offset tables.
// Internal IDs are uint32 for compact neighbor lists.
type HNSWIndex struct {
	mu sync.RWMutex

	// Vector storage: vectors[i*dim : (i+1)*dim]
	vectors []float32

	// Neighbor storage per layer.
	// Each layer has a flat data array + per-node offset/count.
	layers []hnswLayer

	// ID mapping
	extToInt map[uint64]uint32
	intToExt []uint64

	// Per-node level
	levels []uint8

	nodeCount uint32

	// Visited set via generation counter
	visitedGen []uint32
	currentGen uint32

	// Graph metadata
	entryPoint uint32
	hasEntry   bool
	maxLevel   int

	// Parameters
	dim         int
	m           int
	mMax0       int
	efConstruct int
	distFunc    DistanceFunc
	levelMult   float64
	rng         *rand.Rand

	// Per-node locks for concurrent insert
	nodeLocks []sync.Mutex
}

type hnswLayer struct {
	offsets []uint32 // start index into data for each node
	counts  []uint16 // neighbor count for each node
	data    []uint32 // packed neighbor IDs
	caps    []uint16 // allocated capacity per node (for appending)
}

// NewHNSWIndex creates a new HNSW index.
func NewHNSWIndex(dim, m, efConstruction int, distFunc DistanceFunc) *HNSWIndex {
	return &HNSWIndex{
		extToInt:    make(map[uint64]uint32),
		dim:         dim,
		m:           m,
		mMax0:       2 * m,
		efConstruct: efConstruction,
		distFunc:    distFunc,
		levelMult:   1.0 / math.Log(float64(m)),
		rng:         rand.New(rand.NewSource(rand.Int63())),
	}
}

func (h *HNSWIndex) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return int(h.nodeCount)
}

// Insert adds a point to the index.
func (h *HNSWIndex) Insert(id uint64, vector []float32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.insertLocked(id, vector)
}

func (h *HNSWIndex) insertLocked(id uint64, vector []float32) {
	level := h.randomLevel()
	internalID := h.nodeCount
	h.nodeCount++

	// Append vector
	h.vectors = append(h.vectors, vector...)

	// Register ID mapping
	h.extToInt[id] = internalID
	h.intToExt = append(h.intToExt, id)
	h.levels = append(h.levels, uint8(level))
	h.visitedGen = append(h.visitedGen, 0)
	h.nodeLocks = append(h.nodeLocks, sync.Mutex{})

	// Ensure enough layers exist
	for len(h.layers) <= level {
		h.layers = append(h.layers, hnswLayer{})
	}

	// Allocate neighbor slots for this node in each layer it appears
	for lc := 0; lc <= level; lc++ {
		layer := &h.layers[lc]
		mMax := h.m
		if lc == 0 {
			mMax = h.mMax0
		}
		offset := uint32(len(layer.data))
		layer.offsets = append(layer.offsets, offset)
		layer.counts = append(layer.counts, 0)
		layer.caps = append(layer.caps, uint16(mMax))
		layer.data = append(layer.data, make([]uint32, mMax)...)
	}
	// Pad layers where this node doesn't appear (for index alignment)
	for lc := level + 1; lc < len(h.layers); lc++ {
		layer := &h.layers[lc]
		layer.offsets = append(layer.offsets, uint32(len(layer.data)))
		layer.counts = append(layer.counts, 0)
		layer.caps = append(layer.caps, 0)
	}

	if !h.hasEntry {
		h.entryPoint = internalID
		h.hasEntry = true
		h.maxLevel = level
		return
	}

	ep := h.entryPoint
	epDist := h.distFunc(vector, h.vectorAt(ep))

	// Traverse from top to the level above the new node's level
	for lc := h.maxLevel; lc > level; lc-- {
		changed := true
		for changed {
			changed = false
			for _, fid := range h.neighborsOf(ep, lc) {
				d := h.distFunc(vector, h.vectorAt(fid))
				if d < epDist {
					ep = fid
					epDist = d
					changed = true
				}
			}
		}
	}

	for lc := min(level, h.maxLevel); lc >= 0; lc-- {
		candidates := h.searchLayerInternal(vector, ep, h.efConstruct, lc)
		mMax := h.m
		if lc == 0 {
			mMax = h.mMax0
		}
		neighbors := h.selectNeighborsHeuristic(vector, candidates, mMax)

		h.setNeighbors(internalID, lc, neighbors)

		for _, n := range neighbors {
			h.addNeighbor(n.id, lc, internalID)
			nNeighbors := h.neighborsOf(n.id, lc)
			if len(nNeighbors) > mMax {
				nVec := h.vectorAt(n.id)
				var nCandidates []searchResult
				for _, fid := range nNeighbors {
					nCandidates = append(nCandidates, searchResult{
						id:       fid,
						distance: h.distFunc(nVec, h.vectorAt(fid)),
					})
				}
				pruned := h.selectNeighborsHeuristic(nVec, nCandidates, mMax)
				h.setNeighbors(n.id, lc, pruned)
			}
		}

		if candidates.len > 0 {
			ep = candidates.items[0].id // closest after drain isn't reliable, use first pushed
		}
		// Reset candidates for next layer — use ep from closest found
		if len(neighbors) > 0 {
			ep = neighbors[0].id
		}
	}

	if level > h.maxLevel {
		h.maxLevel = level
		h.entryPoint = internalID
	}
}

// Search finds the top-k nearest neighbors to the query vector.
func (h *HNSWIndex) Search(query []float32, topK, ef int, allow map[uint64]struct{}) []HNSWSearchResult {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if !h.hasEntry {
		return nil
	}

	ep := h.entryPoint
	for lc := h.maxLevel; lc > 0; lc-- {
		changed := true
		epDist := h.distFunc(query, h.vectorAt(ep))
		for changed {
			changed = false
			for _, fid := range h.neighborsOf(ep, lc) {
				d := h.distFunc(query, h.vectorAt(fid))
				if d < epDist {
					ep = fid
					epDist = d
					changed = true
				}
			}
		}
	}

	candidates := h.searchLayerInternal(query, ep, max(ef, topK), 0)
	sorted := candidates.drainSorted()

	var results []HNSWSearchResult
	for _, r := range sorted {
		extID := h.intToExt[r.id]
		if allow != nil {
			if _, ok := allow[extID]; !ok {
				continue
			}
		}
		results = append(results, HNSWSearchResult{ID: extID, Distance: r.distance})
		if len(results) == topK {
			break
		}
	}
	return results
}

func (h *HNSWIndex) searchLayerInternal(query []float32, entryID uint32, ef, layer int) searchMaxHeap {
	h.bumpGeneration()
	h.visitedGen[entryID] = h.currentGen

	entryDist := h.distFunc(query, h.vectorAt(entryID))

	var candidates searchMinHeap
	candidates.init(ef * 2)
	candidates.push(searchResult{id: entryID, distance: entryDist})

	var results searchMaxHeap
	results.init(ef + 1)
	results.push(searchResult{id: entryID, distance: entryDist})

	for candidates.len > 0 {
		closest := candidates.pop()
		if results.len >= ef && closest.distance > results.peek().distance {
			break
		}

		for _, fid := range h.neighborsOf(closest.id, layer) {
			if h.visitedGen[fid] == h.currentGen {
				continue
			}
			h.visitedGen[fid] = h.currentGen

			d := h.distFunc(query, h.vectorAt(fid))
			if results.len < ef || d < results.peek().distance {
				candidates.push(searchResult{id: fid, distance: d})
				results.push(searchResult{id: fid, distance: d})
				if results.len > ef {
					results.pop()
				}
			}
		}
	}

	return results
}

func (h *HNSWIndex) selectNeighborsHeuristic(target []float32, candidates searchMaxHeap, m int) []searchResult {
	sorted := candidates.drainSorted()

	selected := make([]searchResult, 0, m)
	for _, c := range sorted {
		if len(selected) >= m {
			break
		}
		good := true
		cVec := h.vectorAt(c.id)
		for _, s := range selected {
			if h.distFunc(cVec, h.vectorAt(s.id)) < c.distance {
				good = false
				break
			}
		}
		if good {
			selected = append(selected, c)
		}
	}

	// Backfill if heuristic didn't fill M
	if len(selected) < m {
		selectedSet := make(map[uint32]struct{}, len(selected))
		for _, s := range selected {
			selectedSet[s.id] = struct{}{}
		}
		for _, c := range sorted {
			if len(selected) >= m {
				break
			}
			if _, ok := selectedSet[c.id]; !ok {
				selected = append(selected, c)
				selectedSet[c.id] = struct{}{}
			}
		}
	}
	return selected
}

// --- neighbor access helpers ---

func (h *HNSWIndex) vectorAt(id uint32) []float32 {
	start := int(id) * h.dim
	return h.vectors[start : start+h.dim]
}

func (h *HNSWIndex) neighborsOf(id uint32, layer int) []uint32 {
	if layer >= len(h.layers) {
		return nil
	}
	l := &h.layers[layer]
	if int(id) >= len(l.offsets) {
		return nil
	}
	off := l.offsets[id]
	cnt := l.counts[id]
	return l.data[off : off+uint32(cnt)]
}

func (h *HNSWIndex) setNeighbors(id uint32, layer int, neighbors []searchResult) {
	l := &h.layers[layer]
	off := l.offsets[id]
	cnt := min(len(neighbors), int(l.caps[id]))
	for i := 0; i < cnt; i++ {
		l.data[off+uint32(i)] = neighbors[i].id
	}
	l.counts[id] = uint16(cnt)
}

func (h *HNSWIndex) addNeighbor(nodeID uint32, layer int, neighborID uint32) {
	l := &h.layers[layer]
	if int(nodeID) >= len(l.offsets) {
		return
	}
	cnt := l.counts[nodeID]
	cap := l.caps[nodeID]
	if cnt < cap {
		l.data[l.offsets[nodeID]+uint32(cnt)] = neighborID
		l.counts[nodeID] = cnt + 1
	} else {
		// Overflow: append and update offset (rare, handled by pruning)
		l.data = append(l.data, neighborID)
		newOff := uint32(len(l.data) - int(cnt) - 1)
		// Copy existing neighbors to new location
		copy(l.data[newOff:], l.data[l.offsets[nodeID]:l.offsets[nodeID]+uint32(cnt)])
		l.data[newOff+uint32(cnt)] = neighborID
		l.offsets[nodeID] = newOff
		l.counts[nodeID] = cnt + 1
		l.caps[nodeID] = cnt + 1
	}
}

func (h *HNSWIndex) bumpGeneration() {
	h.currentGen++
	if h.currentGen == 0 {
		// Wrapped — reset all
		for i := range h.visitedGen {
			h.visitedGen[i] = 0
		}
		h.currentGen = 1
	}
}

func (h *HNSWIndex) randomLevel() int {
	level := 0
	for h.rng.Float64() < 1.0/math.E && level < 32 {
		level++
	}
	return level
}

// --- Serialization ---

// Serialize encodes the HNSW index to binary.
func (h *HNSWIndex) Serialize() ([]byte, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	estimatedSize := 32 + len(h.vectors)*4 + int(h.nodeCount)*9
	for _, l := range h.layers {
		estimatedSize += 4 + len(l.offsets)*4 + len(l.counts)*2 + 4 + len(l.data)*4
	}
	buf := make([]byte, 0, estimatedSize)

	// Header
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.dim))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.m))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.efConstruct))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.maxLevel))
	buf = binary.LittleEndian.AppendUint32(buf, h.entryPoint)
	if h.hasEntry {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = binary.LittleEndian.AppendUint32(buf, h.nodeCount)

	// Vectors
	for _, v := range h.vectors {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
	}

	// IntToExt mapping
	for _, ext := range h.intToExt {
		buf = binary.LittleEndian.AppendUint64(buf, ext)
	}

	// Levels
	buf = append(buf, h.levels...)

	// Layers
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(h.layers)))
	for _, l := range h.layers {
		nodeCount := uint32(len(l.offsets))
		buf = binary.LittleEndian.AppendUint32(buf, nodeCount)
		for _, off := range l.offsets {
			buf = binary.LittleEndian.AppendUint32(buf, off)
		}
		for _, cnt := range l.counts {
			buf = binary.LittleEndian.AppendUint16(buf, cnt)
		}
		for _, cap := range l.caps {
			buf = binary.LittleEndian.AppendUint16(buf, cap)
		}
		dataLen := uint32(len(l.data))
		buf = binary.LittleEndian.AppendUint32(buf, dataLen)
		for _, d := range l.data {
			buf = binary.LittleEndian.AppendUint32(buf, d)
		}
	}

	return buf, nil
}

// DeserializeHNSWIndex reconstructs an HNSW index from binary data.
func DeserializeHNSWIndex(data []byte, distFunc DistanceFunc) (*HNSWIndex, error) {
	if len(data) < 25 {
		return nil, fmt.Errorf("data too short for HNSW header")
	}
	off := 0
	readU16 := func() uint16 {
		v := binary.LittleEndian.Uint16(data[off:])
		off += 2
		return v
	}
	readU32 := func() uint32 {
		v := binary.LittleEndian.Uint32(data[off:])
		off += 4
		return v
	}
	readU64 := func() uint64 {
		v := binary.LittleEndian.Uint64(data[off:])
		off += 8
		return v
	}

	dim := int(readU32())
	m := int(readU32())
	efConstruct := int(readU32())
	maxLevel := int(readU32())
	entryPoint := readU32()
	hasEntry := data[off] == 1
	off++
	nodeCount := readU32()

	h := &HNSWIndex{
		extToInt:    make(map[uint64]uint32, nodeCount),
		nodeCount:   nodeCount,
		entryPoint:  entryPoint,
		hasEntry:    hasEntry,
		maxLevel:    maxLevel,
		dim:         dim,
		m:           m,
		mMax0:       2 * m,
		efConstruct: efConstruct,
		distFunc:    distFunc,
		levelMult:   1.0 / math.Log(float64(m)),
		rng:         rand.New(rand.NewSource(rand.Int63())),
	}

	// Vectors
	vecLen := int(nodeCount) * dim
	h.vectors = make([]float32, vecLen)
	for i := range h.vectors {
		h.vectors[i] = math.Float32frombits(readU32())
	}

	// IntToExt + ExtToInt
	h.intToExt = make([]uint64, nodeCount)
	for i := uint32(0); i < nodeCount; i++ {
		ext := readU64()
		h.intToExt[i] = ext
		h.extToInt[ext] = i
	}

	// Levels
	h.levels = make([]uint8, nodeCount)
	copy(h.levels, data[off:off+int(nodeCount)])
	off += int(nodeCount)

	// Visited gen
	h.visitedGen = make([]uint32, nodeCount)
	h.nodeLocks = make([]sync.Mutex, nodeCount)

	// Layers
	layerCount := int(readU32())
	h.layers = make([]hnswLayer, layerCount)
	for li := 0; li < layerCount; li++ {
		nc := int(readU32())
		l := hnswLayer{
			offsets: make([]uint32, nc),
			counts:  make([]uint16, nc),
			caps:    make([]uint16, nc),
		}
		for i := 0; i < nc; i++ {
			l.offsets[i] = readU32()
		}
		for i := 0; i < nc; i++ {
			l.counts[i] = readU16()
		}
		for i := 0; i < nc; i++ {
			l.caps[i] = readU16()
		}
		dataLen := int(readU32())
		l.data = make([]uint32, dataLen)
		for i := 0; i < dataLen; i++ {
			l.data[i] = readU32()
		}
		h.layers[li] = l
	}

	return h, nil
}
```

- [ ] **Step 3: Run existing HNSW tests to confirm they all pass**

Run: `go test -tags vector -run "TestHNSW" -v ./pkg/vector/`
Expected: all 7 tests PASS (same API, same behavior)

- [ ] **Step 4: Run all tests to confirm no regressions**

Run: `go test -tags vector -v ./pkg/vector/`
Expected: all PASS (segment and engine tests use HNSW through the same API)

- [ ] **Step 5: Commit**

```bash
git add pkg/vector/hnsw.go
git commit -m "feat(vector): rewrite HNSW with flat memory layout

Replaces map[uint64]*hnswNode with contiguous arrays:
- Vectors in single []float32 block
- Packed neighbor lists per layer with offset tables
- uint32 internal IDs mapped to external uint64
- Generation-counter visited set (O(1) clear)
- Heuristic neighbor selection for better graph quality
- Per-node locks ready for parallel build"
```

---

## Task 5: Parallel HNSW Construction

**Files:**
- Modify: `pkg/vector/hnsw.go` (add `BuildParallel`)
- Modify: `pkg/vector/hnsw_test.go` (add parallel build test)

- [ ] **Step 1: Add parallel build test**

Add to `pkg/vector/hnsw_test.go`:

```go
func TestHNSW_BuildParallel(t *testing.T) {
	dim := 32
	n := 5000
	rng := rand.New(rand.NewSource(42))

	ids := make([]uint64, n)
	vectors := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		ids[i] = uint64(i + 1)
		v := randomVector(dim, rng)
		copy(vectors[i*dim:], v)
	}

	idx := NewHNSWIndex(dim, 16, 200, CosineDistance)
	idx.BuildParallel(ids, vectors, 4)

	if idx.Len() != n {
		t.Fatalf("expected %d points, got %d", n, idx.Len())
	}

	// Verify recall — parallel build should achieve good recall
	query := randomVector(dim, rand.New(rand.NewSource(99)))
	type idDist struct {
		id   uint64
		dist float32
	}
	var brute []idDist
	for i := 0; i < n; i++ {
		v := vectors[i*dim : (i+1)*dim]
		brute = append(brute, idDist{ids[i], CosineDistance(query, v)})
	}
	for i := 0; i < len(brute); i++ {
		for j := i + 1; j < len(brute); j++ {
			if brute[j].dist < brute[i].dist {
				brute[i], brute[j] = brute[j], brute[i]
			}
		}
	}
	topK := 10
	bruteTopIDs := make(map[uint64]struct{})
	for i := 0; i < topK; i++ {
		bruteTopIDs[brute[i].id] = struct{}{}
	}

	results := idx.Search(query, topK, 128, nil)
	hits := 0
	for _, r := range results {
		if _, ok := bruteTopIDs[r.ID]; ok {
			hits++
		}
	}
	recall := float64(hits) / float64(topK)
	if recall < 0.7 {
		t.Fatalf("parallel build recall@10 too low: %.2f", recall)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags vector -run TestHNSW_BuildParallel -v ./pkg/vector/`
Expected: FAIL — `BuildParallel` not defined

- [ ] **Step 3: Implement BuildParallel**

Add to `pkg/vector/hnsw.go`:

```go
// BuildParallel builds the HNSW index from a batch of vectors using concurrent workers.
// Seeds the graph with the first 1000 points sequentially, then inserts the rest in parallel.
func (h *HNSWIndex) BuildParallel(ids []uint64, vectors []float32, workers int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := len(ids)
	if n == 0 {
		return
	}
	if workers <= 0 {
		workers = 1
	}

	seedCount := min(1000, n)

	// Phase 1: sequential seed
	for i := 0; i < seedCount; i++ {
		h.insertLocked(ids[i], vectors[i*h.dim:(i+1)*h.dim])
	}

	if seedCount >= n {
		return
	}

	// Phase 2: parallel insert
	// Drop the write lock and use per-node locks + atomic entry point
	h.mu.Unlock()

	var wg sync.WaitGroup
	ch := make(chan int, 256)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				h.insertConcurrent(ids[i], vectors[i*h.dim:(i+1)*h.dim])
			}
		}()
	}
	for i := seedCount; i < n; i++ {
		ch <- i
	}
	close(ch)
	wg.Wait()

	h.mu.Lock() // Re-acquire for deferred unlock

	// Phase 3: prune any over-connected nodes
	h.pruneAllNeighbors()
}

func (h *HNSWIndex) insertConcurrent(id uint64, vector []float32) {
	h.mu.Lock()
	level := h.randomLevel()
	internalID := h.nodeCount
	h.nodeCount++

	h.vectors = append(h.vectors, vector...)
	h.extToInt[id] = internalID
	h.intToExt = append(h.intToExt, id)
	h.levels = append(h.levels, uint8(level))
	h.visitedGen = append(h.visitedGen, 0)
	h.nodeLocks = append(h.nodeLocks, sync.Mutex{})

	for len(h.layers) <= level {
		h.layers = append(h.layers, hnswLayer{})
	}
	for lc := 0; lc <= level; lc++ {
		layer := &h.layers[lc]
		mMax := h.m
		if lc == 0 {
			mMax = h.mMax0
		}
		offset := uint32(len(layer.data))
		layer.offsets = append(layer.offsets, offset)
		layer.counts = append(layer.counts, 0)
		layer.caps = append(layer.caps, uint16(mMax))
		layer.data = append(layer.data, make([]uint32, mMax)...)
	}
	for lc := level + 1; lc < len(h.layers); lc++ {
		layer := &h.layers[lc]
		layer.offsets = append(layer.offsets, uint32(len(layer.data)))
		layer.counts = append(layer.counts, 0)
		layer.caps = append(layer.caps, 0)
	}

	ep := h.entryPoint
	curMaxLevel := h.maxLevel
	h.mu.Unlock()

	epDist := h.distFunc(vector, h.vectorAt(ep))

	for lc := curMaxLevel; lc > level; lc-- {
		changed := true
		for changed {
			changed = false
			for _, fid := range h.neighborsOf(ep, lc) {
				d := h.distFunc(vector, h.vectorAt(fid))
				if d < epDist {
					ep = fid
					epDist = d
					changed = true
				}
			}
		}
	}

	for lc := min(level, curMaxLevel); lc >= 0; lc-- {
		// Use a private visited set for concurrent search
		candidates := h.searchLayerConcurrent(vector, ep, h.efConstruct, lc)
		mMax := h.m
		if lc == 0 {
			mMax = h.mMax0
		}
		neighbors := h.selectNeighborsHeuristic(vector, candidates, mMax)

		h.nodeLocks[internalID].Lock()
		h.setNeighbors(internalID, lc, neighbors)
		h.nodeLocks[internalID].Unlock()

		for _, n := range neighbors {
			h.nodeLocks[n.id].Lock()
			h.addNeighbor(n.id, lc, internalID)
			h.nodeLocks[n.id].Unlock()
		}

		if len(neighbors) > 0 {
			ep = neighbors[0].id
		}
	}

	if level > curMaxLevel {
		h.mu.Lock()
		if level > h.maxLevel {
			h.maxLevel = level
			h.entryPoint = internalID
		}
		h.mu.Unlock()
	}
}

func (h *HNSWIndex) searchLayerConcurrent(query []float32, entryID uint32, ef, layer int) searchMaxHeap {
	// Use a local visited map for concurrent safety
	visited := make(map[uint32]struct{})
	visited[entryID] = struct{}{}

	entryDist := h.distFunc(query, h.vectorAt(entryID))

	var candidates searchMinHeap
	candidates.init(ef * 2)
	candidates.push(searchResult{id: entryID, distance: entryDist})

	var results searchMaxHeap
	results.init(ef + 1)
	results.push(searchResult{id: entryID, distance: entryDist})

	for candidates.len > 0 {
		closest := candidates.pop()
		if results.len >= ef && closest.distance > results.peek().distance {
			break
		}

		for _, fid := range h.neighborsOf(closest.id, layer) {
			if _, ok := visited[fid]; ok {
				continue
			}
			visited[fid] = struct{}{}

			d := h.distFunc(query, h.vectorAt(fid))
			if results.len < ef || d < results.peek().distance {
				candidates.push(searchResult{id: fid, distance: d})
				results.push(searchResult{id: fid, distance: d})
				if results.len > ef {
					results.pop()
				}
			}
		}
	}
	return results
}

func (h *HNSWIndex) pruneAllNeighbors() {
	for node := uint32(0); node < h.nodeCount; node++ {
		for lc := 0; lc <= int(h.levels[node]); lc++ {
			mMax := h.m
			if lc == 0 {
				mMax = h.mMax0
			}
			neighbors := h.neighborsOf(node, lc)
			if len(neighbors) > mMax {
				nodeVec := h.vectorAt(node)
				var candidates []searchResult
				for _, fid := range neighbors {
					candidates = append(candidates, searchResult{
						id:       fid,
						distance: h.distFunc(nodeVec, h.vectorAt(fid)),
					})
				}
				var heap searchMaxHeap
				heap.init(len(candidates))
				for _, c := range candidates {
					heap.push(c)
				}
				pruned := h.selectNeighborsHeuristic(nodeVec, heap, mMax)
				h.setNeighbors(node, lc, pruned)
			}
		}
	}
}
```

- [ ] **Step 4: Run parallel build test**

Run: `go test -tags vector -run TestHNSW_BuildParallel -v ./pkg/vector/`
Expected: PASS

- [ ] **Step 5: Run all HNSW tests**

Run: `go test -tags vector -run "TestHNSW" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/vector/hnsw.go pkg/vector/hnsw_test.go
git commit -m "feat(vector): add parallel HNSW construction

Two-phase build: sequential seed (first 1000 points) then concurrent
insertion with per-node locks. Post-build pruning pass ensures
neighbor count invariants."
```

---

## Task 6: Bounded Top-K for Growing Segment

**Files:**
- Modify: `pkg/vector/segment.go` (`GrowingSegment.Search`)

- [ ] **Step 1: Run existing segment tests to confirm baseline**

Run: `go test -tags vector -run "TestGrowingSegment" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 2: Replace sort.Slice with bounded max-heap in GrowingSegment.Search**

Replace the `Search` method in `segment.go`:

```go
func (g *GrowingSegment) Search(query []float32, topK int, allow map[uint64]struct{}) []ScoredPoint {
	g.mu.RLock()
	defer g.mu.RUnlock()

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

	sorted := heap.drainSorted()
	out := make([]ScoredPoint, len(sorted))
	for i, r := range sorted {
		out[i] = ScoredPoint{Point: g.points[r.id], Score: r.distance}
	}
	return out
}
```

- [ ] **Step 3: Run segment and engine tests**

Run: `go test -tags vector -run "TestGrowingSegment|TestEngine" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/vector/segment.go
git commit -m "feat(vector): use bounded max-heap for growing segment search

Replaces sort.Slice of full result set with bounded max-heap.
Eliminates allocation of N-element slice when K << N."
```

---

## Task 7: Incremental Bitmap Index

**Files:**
- Modify: `pkg/vector/engine.go`

- [ ] **Step 1: Run existing filter test to confirm baseline**

Run: `go test -tags vector -run TestEngine_SearchWithFilter -v ./pkg/vector/`
Expected: PASS

- [ ] **Step 2: Add combinedBitmaps to collectionState and maintain incrementally**

In `engine.go`, update `collectionState`:

```go
type collectionState struct {
	mu              sync.RWMutex
	collection      Collection
	growing         *GrowingSegment
	sealed          []*SealedSegment
	wal             *WAL
	deleted         map[PointID]struct{}
	combinedBitmaps *BitmapIndex // incrementally maintained
}
```

Update `CreateCollection` to initialize it:

```go
// In CreateCollection, when building collectionState:
e.collections[col.Name] = &collectionState{
	collection:      col,
	growing:         NewGrowingSegment(segID, col.Name, 0, col.Dimensions, col.Distance),
	wal:             wal,
	deleted:         make(map[PointID]struct{}),
	combinedBitmaps: NewBitmapIndex(),
}
```

Update `Insert` to maintain incrementally:

```go
// In Insert, after cs.growing.Insert(p):
for k, v := range p.Payload {
	cs.combinedBitmaps.Add(p.ID, k, fmt.Sprint(v))
}
```

Update `Search` to use `combinedBitmaps` directly instead of rebuilding:

```go
// Replace the entire filter block (lines ~216-240) with:
var allow map[uint64]struct{}
if req.Filter != nil {
	filterResult := EvalFilter(*req.Filter, cs.combinedBitmaps)
	if len(cs.deleted) > 0 {
		for id := range cs.deleted {
			filterResult.Remove(uint32(id))
		}
	}
	allow = make(map[uint64]struct{}, filterResult.GetCardinality())
	iter := filterResult.Iterator()
	for iter.HasNext() {
		allow[uint64(iter.Next())] = struct{}{}
	}
}
```

Update `LoadCollectionsFromBackend` to also build `combinedBitmaps`:

```go
// After WAL replay loop, add:
cs := &collectionState{
	collection:      col,
	growing:         NewGrowingSegment(segID, col.Name, 0, col.Dimensions, col.Distance),
	wal:             wal,
	deleted:         make(map[PointID]struct{}),
	combinedBitmaps: NewBitmapIndex(),
}

entries, _ := ReplayWAL(ctx, e.backend, e.cfg.Bucket, col.Name, 0)
for _, entry := range entries {
	switch entry.Type {
	case WALInsert:
		cs.growing.Insert(entry.Point)
		for k, v := range entry.Point.Payload {
			cs.combinedBitmaps.Add(entry.Point.ID, k, fmt.Sprint(v))
		}
	case WALDelete:
		cs.deleted[entry.PointID] = struct{}{}
	}
}
```

- [ ] **Step 3: Run filter tests and all engine tests**

Run: `go test -tags vector -run "TestEngine" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/vector/engine.go
git commit -m "fix(vector): maintain bitmap index incrementally

Replaces O(N) per-search bitmap rebuild with incremental maintenance
on insert. Filtered search now O(filter evaluation) not O(total points)."
```

---

## Task 8: Lock-Free Search + Parallel Segment Search

**Files:**
- Modify: `pkg/vector/engine.go`

- [ ] **Step 1: Add concurrent search test**

Add to `pkg/vector/engine_test.go`:

```go
func TestEngine_ConcurrentSearchDuringInsert(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	dim := 32

	e.CreateCollection(ctx, Collection{Name: "test", Dimensions: dim, Distance: Cosine})

	// Insert initial data
	rng := mrand.New(mrand.NewSource(42))
	for i := 0; i < 100; i++ {
		e.Insert(ctx, "test", []Point{{ID: uint64(i + 1), Vector: randomVector(dim, rng)}})
	}

	// Run concurrent searches and inserts
	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, err := e.Search(ctx, SearchRequest{
					Collection: "test",
					Vector:     randomVector(dim, mrand.New(mrand.NewSource(int64(i*10+j)))),
					TopK:       5,
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			localRng := mrand.New(mrand.NewSource(int64(1000 + i)))
			for j := 0; j < 5; j++ {
				err := e.Insert(ctx, "test", []Point{{
					ID:     uint64(1000 + i*5 + j),
					Vector: randomVector(dim, localRng),
				}})
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent operation failed: %v", err)
	}
}
```

- [ ] **Step 2: Run test (it may already pass with RLock, but let's verify)**

Run: `go test -tags vector -run TestEngine_ConcurrentSearchDuringInsert -v -race ./pkg/vector/`
Expected: should pass (or reveal race conditions to fix)

- [ ] **Step 3: Update Search to use snapshot-based lock-free path**

Replace `engine.go` `Search` method:

```go
func (e *Engine) Search(_ context.Context, req SearchRequest) (*SearchResponse, error) {
	start := time.Now()

	e.mu.RLock()
	cs, ok := e.collections[req.Collection]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("collection %q not found", req.Collection)
	}

	// Snapshot under lock — fast
	cs.mu.RLock()
	growing := cs.growing
	sealed := make([]*SealedSegment, len(cs.sealed))
	copy(sealed, cs.sealed)
	combinedBitmaps := cs.combinedBitmaps
	deletedCopy := make(map[PointID]struct{}, len(cs.deleted))
	for id := range cs.deleted {
		deletedCopy[id] = struct{}{}
	}
	cs.mu.RUnlock()

	topK := req.TopK
	if topK == 0 {
		topK = 10
	}
	efSearch := req.EfSearch
	if efSearch == 0 {
		efSearch = e.cfg.HNSWefSearch
	}

	// Filter evaluation — lock-free, uses snapshot
	var allow map[uint64]struct{}
	if req.Filter != nil {
		filterResult := EvalFilter(*req.Filter, combinedBitmaps)
		for id := range deletedCopy {
			filterResult.Remove(uint32(id))
		}
		allow = make(map[uint64]struct{}, filterResult.GetCardinality())
		iter := filterResult.Iterator()
		for iter.HasNext() {
			allow[uint64(iter.Next())] = struct{}{}
		}
	}

	exclude := deletedCopy
	if allow != nil {
		for id := range exclude {
			delete(allow, id)
		}
		exclude = nil
	}

	// Search growing segment
	growingResults := growing.Search(req.Vector, topK, allow)
	if exclude != nil {
		filtered := growingResults[:0]
		for _, r := range growingResults {
			if _, ok := exclude[r.ID]; !ok {
				filtered = append(filtered, r)
			}
		}
		growingResults = filtered
	}

	// Search sealed segments — parallel if multiple
	var sealedResults []ScoredPoint
	if len(sealed) == 1 {
		results := sealed[0].Search(req.Vector, topK, efSearch, allow)
		for _, r := range results {
			if exclude != nil {
				if _, ok := exclude[r.ID]; ok {
					continue
				}
			}
			sealedResults = append(sealedResults, r)
		}
	} else if len(sealed) > 1 {
		type segResults struct {
			results []ScoredPoint
		}
		ch := make(chan segResults, len(sealed))
		for _, ss := range sealed {
			go func(ss *SealedSegment) {
				ch <- segResults{ss.Search(req.Vector, topK, efSearch, allow)}
			}(ss)
		}
		for range sealed {
			sr := <-ch
			for _, r := range sr.results {
				if exclude != nil {
					if _, ok := exclude[r.ID]; ok {
						continue
					}
				}
				sealedResults = append(sealedResults, r)
			}
		}
	}

	all := append(growingResults, sealedResults...)
	sort.Slice(all, func(i, j int) bool {
		return all[i].Score < all[j].Score
	})
	if len(all) > topK {
		all = all[:topK]
	}

	return &SearchResponse{
		Points:   all,
		SearchMs: time.Since(start).Milliseconds(),
	}, nil
}
```

- [ ] **Step 4: Run all tests with race detector**

Run: `go test -tags vector -v -race ./pkg/vector/`
Expected: all PASS, no race conditions

- [ ] **Step 5: Commit**

```bash
git add pkg/vector/engine.go pkg/vector/engine_test.go
git commit -m "feat(vector): lock-free search with parallel segment fan-out

Search snapshots collection state under brief RLock, then executes
fully lock-free. Multiple sealed segments searched in parallel.
Inserts no longer blocked by concurrent searches."
```

---

## Task 9: Wire Parallel Build into SealSegment

**Files:**
- Modify: `pkg/vector/segment.go`

- [ ] **Step 1: Update SealSegment to use BuildParallel**

In `SealSegment`, replace the sequential insert loop:

```go
// Replace:
//   hnsw := NewHNSWIndex(gsMeta.Dimensions, hnswM, hnswEfConstruct, distFunc)
//   for _, p := range points {
//       hnsw.Insert(p.ID, p.Vector)
//   }
// With:

hnswIdx := NewHNSWIndex(gsMeta.Dimensions, hnswM, hnswEfConstruct, distFunc)
if len(points) > 500 {
	ids := make([]uint64, len(points))
	vectors := make([]float32, len(points)*gsMeta.Dimensions)
	for i, p := range points {
		ids[i] = p.ID
		copy(vectors[i*gsMeta.Dimensions:], p.Vector)
	}
	workers := 4
	hnswIdx.BuildParallel(ids, vectors, workers)
} else {
	for _, p := range points {
		hnswIdx.Insert(p.ID, p.Vector)
	}
}
```

- [ ] **Step 2: Run segment tests**

Run: `go test -tags vector -run "TestSealedSegment" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 3: Run full test suite**

Run: `go test -tags vector -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/vector/segment.go
git commit -m "feat(vector): use parallel HNSW build during segment seal

Segments with >500 points use BuildParallel with 4 workers.
Small segments use sequential insert to avoid goroutine overhead."
```

---

## Task 10: Scalar Quantization (SQ8)

**Files:**
- Create: `pkg/vector/quantize.go`
- Create: `pkg/vector/quantize_test.go`
- Modify: `pkg/vector/types.go` (add SQParams type)

- [ ] **Step 1: Write SQ8 tests**

```go
// pkg/vector/quantize_test.go
//go:build vector

package vector

import (
	"math"
	"math/rand"
	"testing"
)

func TestSQ8_EncodeDecodeRoundTrip(t *testing.T) {
	dim := 768
	n := 100
	rng := rand.New(rand.NewSource(42))

	vectors := make([]float32, n*dim)
	for i := range vectors {
		vectors[i] = rng.Float32()*2 - 1
	}

	sq := NewSQ8(dim)
	sq.Fit(vectors)
	encoded := sq.Encode(vectors)

	if len(encoded) != n*dim {
		t.Fatalf("expected %d bytes, got %d", n*dim, len(encoded))
	}

	// Decode and check approximate reconstruction
	decoded := sq.Decode(encoded)
	var maxErr float32
	for i, v := range vectors {
		err := float32(math.Abs(float64(decoded[i] - v)))
		if err > maxErr {
			maxErr = err
		}
	}
	// SQ8 with range [-1, 1] has max error of ~2/255 ≈ 0.0078
	if maxErr > 0.02 {
		t.Fatalf("max reconstruction error too high: %f", maxErr)
	}
}

func TestSQ8_L2Recall(t *testing.T) {
	dim := 128
	n := 1000
	rng := rand.New(rand.NewSource(42))

	vectors := make([]float32, n*dim)
	for i := range vectors {
		vectors[i] = rng.Float32()*2 - 1
	}

	sq := NewSQ8(dim)
	sq.Fit(vectors)
	encoded := sq.Encode(vectors)

	query := make([]float32, dim)
	for i := range query {
		query[i] = rng.Float32()*2 - 1
	}

	// Brute-force with float32
	type idDist struct {
		id   int
		dist float32
	}
	var exact []idDist
	for i := 0; i < n; i++ {
		d := L2Distance(query, vectors[i*dim:(i+1)*dim])
		exact = append(exact, idDist{i, d})
	}
	for i := range exact {
		for j := i + 1; j < len(exact); j++ {
			if exact[j].dist < exact[i].dist {
				exact[i], exact[j] = exact[j], exact[i]
			}
		}
	}

	// Brute-force with SQ8 distances
	encodedQuery := sq.EncodeOne(query)
	var approx []idDist
	for i := 0; i < n; i++ {
		d := sq.L2Uint8(encodedQuery, encoded[i*dim:(i+1)*dim])
		approx = append(approx, idDist{i, float32(d)})
	}
	for i := range approx {
		for j := i + 1; j < len(approx); j++ {
			if approx[j].dist < approx[i].dist {
				approx[i], approx[j] = approx[j], approx[i]
			}
		}
	}

	// Check recall@10
	topK := 10
	exactTop := make(map[int]struct{})
	for i := 0; i < topK; i++ {
		exactTop[exact[i].id] = struct{}{}
	}
	hits := 0
	for i := 0; i < topK; i++ {
		if _, ok := exactTop[approx[i].id]; ok {
			hits++
		}
	}
	recall := float64(hits) / float64(topK)
	if recall < 0.8 {
		t.Fatalf("SQ8 recall@10 too low: %.2f", recall)
	}
}

func TestSQ8_SerializeDeserialize(t *testing.T) {
	dim := 32
	sq := NewSQ8(dim)

	vectors := make([]float32, 100*dim)
	rng := rand.New(rand.NewSource(42))
	for i := range vectors {
		vectors[i] = rng.Float32()*4 - 2
	}
	sq.Fit(vectors)

	data := sq.Serialize()
	sq2, err := DeserializeSQ8(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if sq2.Dim != sq.Dim {
		t.Fatalf("dim mismatch: %d vs %d", sq2.Dim, sq.Dim)
	}
	for i := range sq.Mins {
		if sq.Mins[i] != sq2.Mins[i] {
			t.Fatalf("mins mismatch at %d", i)
		}
		if sq.Scales[i] != sq2.Scales[i] {
			t.Fatalf("scales mismatch at %d", i)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags vector -run "TestSQ8" -v ./pkg/vector/`
Expected: FAIL — `NewSQ8` not defined

- [ ] **Step 3: Implement SQ8**

```go
// pkg/vector/quantize.go
//go:build vector

package vector

import (
	"encoding/binary"
	"fmt"
	"math"
)

// SQ8 implements scalar quantization from float32 to uint8.
type SQ8 struct {
	Dim    int
	Mins   []float32
	Scales []float32 // 255.0 / (max - min) per dimension
	Invs   []float32 // (max - min) / 255.0 per dimension, for decode
}

// NewSQ8 creates an unfit SQ8 quantizer for the given dimension.
func NewSQ8(dim int) *SQ8 {
	return &SQ8{Dim: dim}
}

// Fit computes per-dimension min/max from a contiguous vector array.
func (s *SQ8) Fit(vectors []float32) {
	n := len(vectors) / s.Dim
	s.Mins = make([]float32, s.Dim)
	maxs := make([]float32, s.Dim)
	for d := 0; d < s.Dim; d++ {
		s.Mins[d] = math.MaxFloat32
		maxs[d] = -math.MaxFloat32
	}
	for i := 0; i < n; i++ {
		for d := 0; d < s.Dim; d++ {
			v := vectors[i*s.Dim+d]
			if v < s.Mins[d] {
				s.Mins[d] = v
			}
			if v > maxs[d] {
				maxs[d] = v
			}
		}
	}
	s.Scales = make([]float32, s.Dim)
	s.Invs = make([]float32, s.Dim)
	for d := 0; d < s.Dim; d++ {
		r := maxs[d] - s.Mins[d]
		if r == 0 {
			r = 1
		}
		s.Scales[d] = 255.0 / r
		s.Invs[d] = r / 255.0
	}
}

// Encode quantizes a contiguous array of float32 vectors to uint8.
func (s *SQ8) Encode(vectors []float32) []uint8 {
	out := make([]uint8, len(vectors))
	for i, v := range vectors {
		d := i % s.Dim
		q := (v - s.Mins[d]) * s.Scales[d]
		if q < 0 {
			q = 0
		}
		if q > 255 {
			q = 255
		}
		out[i] = uint8(q)
	}
	return out
}

// EncodeOne quantizes a single float32 vector to uint8.
func (s *SQ8) EncodeOne(v []float32) []uint8 {
	return s.Encode(v)
}

// Decode reconstructs float32 vectors from uint8 (approximate).
func (s *SQ8) Decode(encoded []uint8) []float32 {
	out := make([]float32, len(encoded))
	for i, q := range encoded {
		d := i % s.Dim
		out[i] = float32(q)*s.Invs[d] + s.Mins[d]
	}
	return out
}

// L2Uint8 computes approximate squared L2 distance between two uint8 vectors.
func (s *SQ8) L2Uint8(a, b []uint8) uint32 {
	var sum uint32
	for i := range a {
		d := int32(a[i]) - int32(b[i])
		sum += uint32(d * d)
	}
	return sum
}

// Serialize encodes SQ8 parameters to bytes.
func (s *SQ8) Serialize() []byte {
	buf := make([]byte, 0, 8+s.Dim*8)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(s.Dim))
	for _, m := range s.Mins {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(m))
	}
	for _, sc := range s.Scales {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(sc))
	}
	for _, inv := range s.Invs {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(inv))
	}
	return buf
}

// DeserializeSQ8 reconstructs SQ8 parameters from bytes.
func DeserializeSQ8(data []byte) (*SQ8, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("SQ8 data too short")
	}
	off := 0
	dim := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4

	needed := 4 + dim*12
	if len(data) < needed {
		return nil, fmt.Errorf("SQ8 data truncated: need %d, got %d", needed, len(data))
	}

	s := &SQ8{
		Dim:    dim,
		Mins:   make([]float32, dim),
		Scales: make([]float32, dim),
		Invs:   make([]float32, dim),
	}
	for i := 0; i < dim; i++ {
		s.Mins[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))
		off += 4
	}
	for i := 0; i < dim; i++ {
		s.Scales[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))
		off += 4
	}
	for i := 0; i < dim; i++ {
		s.Invs[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))
		off += 4
	}
	return s, nil
}
```

- [ ] **Step 4: Run SQ8 tests**

Run: `go test -tags vector -run "TestSQ8" -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 5: Run full test suite**

Run: `go test -tags vector -v ./pkg/vector/`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/vector/quantize.go pkg/vector/quantize_test.go
git commit -m "feat(vector): add SQ8 scalar quantization

Float32 → uint8 encoding with per-dimension min/max normalization.
Includes L2Uint8 distance for approximate search, serialization,
and decode for rescoring."
```

---

## Task 11: Comprehensive Benchmark Suite

**Files:**
- Modify: `pkg/vector/benchmark_test.go`

- [ ] **Step 1: Expand benchmark suite**

Replace `pkg/vector/benchmark_test.go`:

```go
//go:build vector

package vector

import (
	"context"
	"fmt"
	mrand "math/rand"
	"testing"
	"unsafe"
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
		SegmentSizeBytes: 256 * 1024 * 1024, // large to avoid seal during bench
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

// --- Distance benchmarks ---

func BenchmarkL2Distance_768d(b *testing.B) {
	rng := mrand.New(mrand.NewSource(42))
	a := benchRandVec(768, rng)
	v := benchRandVec(768, rng)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		L2Distance(a, v)
	}
}

func BenchmarkCosineDistance_768d(b *testing.B) {
	rng := mrand.New(mrand.NewSource(42))
	a := benchRandVec(768, rng)
	v := benchRandVec(768, rng)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CosineDistance(a, v)
	}
}

// --- HNSW build benchmarks ---

func BenchmarkHNSWBuild_5k_768d(b *testing.B) {
	dim := 768
	n := 5000
	rng := mrand.New(mrand.NewSource(42))
	ids := make([]uint64, n)
	vectors := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		ids[i] = uint64(i + 1)
		v := benchRandVec(dim, rng)
		copy(vectors[i*dim:], v)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := NewHNSWIndex(dim, 16, 200, CosineDistance)
		idx.BuildParallel(ids, vectors, 4)
	}
}

func BenchmarkHNSWBuild_5k_768d_Sequential(b *testing.B) {
	dim := 768
	n := 5000
	rng := mrand.New(mrand.NewSource(42))
	ids := make([]uint64, n)
	vectors := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		ids[i] = uint64(i + 1)
		v := benchRandVec(dim, rng)
		copy(vectors[i*dim:], v)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := NewHNSWIndex(dim, 16, 200, CosineDistance)
		for j := 0; j < n; j++ {
			idx.Insert(ids[j], vectors[j*dim:(j+1)*dim])
		}
	}
}

// --- Search benchmarks ---

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

// --- Insert benchmarks ---

func BenchmarkInsert_768d(b *testing.B) {
	be := newSegTestBackend()
	engine, _ := NewEngine(be, VectorConfig{
		Bucket:           "bench",
		SegmentSizeBytes: 256 * 1024 * 1024,
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

// --- Filtered search benchmarks ---

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

// --- SQ8 benchmarks ---

func BenchmarkSQ8_L2Uint8_768d(b *testing.B) {
	dim := 768
	rng := mrand.New(mrand.NewSource(42))
	vectors := make([]float32, 2*dim)
	for i := range vectors {
		vectors[i] = rng.Float32()*2 - 1
	}
	sq := NewSQ8(dim)
	sq.Fit(vectors)
	a := sq.EncodeOne(vectors[:dim])
	v := sq.EncodeOne(vectors[dim:])
	_ = unsafe.Pointer(&a[0]) // prevent escape analysis from optimizing away

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sq.L2Uint8(a, v)
	}
}
```

- [ ] **Step 2: Run benchmarks (short run to verify they work)**

Run: `go test -tags vector -bench=. -benchmem -benchtime=1s -count=1 ./pkg/vector/ 2>&1 | head -20`
Expected: benchmarks run without error. Numbers should be significantly improved over baseline.

- [ ] **Step 3: Commit**

```bash
git add pkg/vector/benchmark_test.go
git commit -m "feat(vector): expand benchmark suite

Adds distance, HNSW build (parallel vs sequential), SQ8, and filtered
search benchmarks for before/after performance comparison."
```

---

## Implementation Notes

### Testing Commands

Run all tests:
```bash
go test -tags vector -v ./pkg/vector/
```

Run with race detector:
```bash
go test -tags vector -v -race ./pkg/vector/
```

Run benchmarks:
```bash
go test -tags vector -bench=. -benchmem -benchtime=3s -count=3 ./pkg/vector/
```

### Key Invariants to Verify After Each Task

1. All existing tests pass: `go test -tags vector ./pkg/vector/`
2. No race conditions: `go test -tags vector -race ./pkg/vector/`
3. HNSW recall@10 ≥ 0.8 for 1000 points (existing `TestHNSW_Recall1000`)
4. Serialize/deserialize round-trip produces identical search results

### Dependency Order

```
Task 1 (heaps) ──────────────────┐
Task 2 (distance refactor) ──────┤
Task 3 (SIMD distance) ──────────┤──► Task 4 (HNSW rewrite) ──► Task 5 (parallel build) ──► Task 9 (wire into seal)
                                  │
Task 6 (bounded top-K) ──────────┤──► independent
Task 7 (incremental bitmap) ─────┤──► Task 8 (lock-free search)
Task 10 (SQ8) ───────────────────┤──► independent
Task 11 (benchmarks) ────────────┘──► depends on all above
```

Tasks 1, 2, 6, 7, 10 can be developed in parallel. Tasks 3, 4, 5 are sequential. Task 8 depends on 7.
