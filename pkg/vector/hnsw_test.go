//go:build vector

package vector

import (
	"math/rand"
	"testing"
)

func randomVector(dim int, rng *rand.Rand) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

func TestHNSW_QuantizedNodesAreInt8(t *testing.T) {
	const dim = 8
	rng := rand.New(rand.NewSource(1))
	pts := make([][]float32, 50)
	for i := range pts {
		pts[i] = randomVector(dim, rng)
	}
	qp := computeQuantParams(pointsFromSlice(pts), dim)
	idx := NewHNSWIndexQuantized(dim, 4, 20, L2Distance, qp)
	for i, v := range pts {
		idx.Insert(uint64(i+1), v)
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for id, n := range idx.nodes {
		if len(n.vector) != 0 {
			t.Fatalf("node %d should not hold float32 vector in quantized mode", id)
		}
		if len(n.vectorQ) != dim {
			t.Fatalf("node %d quantized vector wrong length: %d", id, len(n.vectorQ))
		}
	}
}

func TestHNSW_QuantizedSerializationRoundTrip(t *testing.T) {
	const dim = 8
	rng := rand.New(rand.NewSource(2))
	pts := make([][]float32, 30)
	for i := range pts {
		pts[i] = randomVector(dim, rng)
	}
	qp := computeQuantParams(pointsFromSlice(pts), dim)
	src := NewHNSWIndexQuantized(dim, 4, 20, L2Distance, qp)
	for i, v := range pts {
		src.Insert(uint64(i+1), v)
	}

	data, err := src.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	dst, err := DeserializeHNSWIndex(data, L2Distance)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if dst.qp == nil {
		t.Fatal("deserialized index lost quantization params")
	}

	q := randomVector(dim, rng)
	r1 := src.Search(q, 5, 20, nil)
	r2 := dst.Search(q, 5, 20, nil)
	if len(r1) != len(r2) {
		t.Fatalf("result count mismatch: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].ID != r2[i].ID {
			t.Fatalf("result %d: ID %d vs %d", i, r1[i].ID, r2[i].ID)
		}
	}
}

func pointsFromSlice(vs [][]float32) []Point {
	out := make([]Point, len(vs))
	for i, v := range vs {
		out[i] = Point{ID: uint64(i + 1), Vector: v}
	}
	return out
}

func TestHNSW_InsertAndSearchSingle(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	idx.Insert(1, []float32{1, 0, 0})
	results := idx.Search([]float32{1, 0, 0}, 1, 10, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != 1 {
		t.Fatalf("expected ID 1, got %d", results[0].ID)
	}
	if results[0].Distance > 1e-6 {
		t.Fatalf("expected distance ~0, got %f", results[0].Distance)
	}
}

func TestHNSW_SearchTopK(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	idx.Insert(1, []float32{1, 0, 0})
	idx.Insert(2, []float32{0.9, 0.1, 0})
	idx.Insert(3, []float32{0, 1, 0})
	idx.Insert(4, []float32{0, 0, 1})

	results := idx.Search([]float32{1, 0, 0}, 2, 10, nil)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ID != 1 {
		t.Fatalf("closest should be ID 1, got %d", results[0].ID)
	}
	if results[1].ID != 2 {
		t.Fatalf("second closest should be ID 2, got %d", results[1].ID)
	}
}

func TestHNSW_ResultsAreSortedByDistance(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	rng := rand.New(rand.NewSource(42))
	for i := uint64(1); i <= 100; i++ {
		idx.Insert(i, randomVector(3, rng))
	}
	results := idx.Search(randomVector(3, rng), 10, 50, nil)
	for i := 1; i < len(results); i++ {
		if results[i].Distance < results[i-1].Distance {
			t.Fatalf("results not sorted at index %d: %f < %f", i, results[i].Distance, results[i-1].Distance)
		}
	}
}

func TestHNSW_FilteredSearch(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	idx.Insert(1, []float32{1, 0, 0})
	idx.Insert(2, []float32{0.9, 0.1, 0})
	idx.Insert(3, []float32{0.8, 0.2, 0})

	allow := map[uint64]struct{}{2: {}, 3: {}}
	results := idx.Search([]float32{1, 0, 0}, 2, 10, allow)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ID != 2 {
		t.Fatalf("closest allowed should be ID 2, got %d", results[0].ID)
	}
}

func TestHNSW_EmptyIndex(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	results := idx.Search([]float32{1, 0, 0}, 5, 10, nil)
	if len(results) != 0 {
		t.Fatalf("expected 0 results from empty index, got %d", len(results))
	}
}

func TestHNSW_SerializeDeserialize(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	rng := rand.New(rand.NewSource(99))
	for i := uint64(1); i <= 50; i++ {
		idx.Insert(i, randomVector(3, rng))
	}

	data, err := idx.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	idx2, err := DeserializeHNSWIndex(data, CosineDistance)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	query := randomVector(3, rng)
	r1 := idx.Search(query, 5, 50, nil)
	r2 := idx2.Search(query, 5, 50, nil)
	if len(r1) != len(r2) {
		t.Fatalf("result count mismatch: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].ID != r2[i].ID {
			t.Fatalf("result %d ID mismatch: %d vs %d", i, r1[i].ID, r2[i].ID)
		}
	}
}

func TestHNSW_Recall1000(t *testing.T) {
	dim := 32
	n := 1000
	rng := rand.New(rand.NewSource(42))

	idx := NewHNSWIndex(dim, 16, 200, CosineDistance)
	vectors := make([][]float32, n)
	for i := 0; i < n; i++ {
		vectors[i] = randomVector(dim, rng)
		idx.Insert(uint64(i+1), vectors[i])
	}

	query := randomVector(dim, rng)
	type idDist struct {
		id   uint64
		dist float32
	}
	var brute []idDist
	for i, v := range vectors {
		brute = append(brute, idDist{uint64(i + 1), CosineDistance(query, v)})
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
	for i := 0; i < topK && i < len(brute); i++ {
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
	if recall < 0.8 {
		t.Fatalf("recall@10 too low: %.2f (expected >= 0.8)", recall)
	}
}

func TestHNSW_Len(t *testing.T) {
	idx := NewHNSWIndex(3, 4, 20, CosineDistance)
	if idx.Len() != 0 {
		t.Fatalf("expected len 0, got %d", idx.Len())
	}
	idx.Insert(1, []float32{1, 0, 0})
	idx.Insert(2, []float32{0, 1, 0})
	if idx.Len() != 2 {
		t.Fatalf("expected len 2, got %d", idx.Len())
	}
}
