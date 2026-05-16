//go:build vector

package vector

import (
	"container/heap"
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

type hnswNode struct {
	id uint64
	// Exactly one of vector / vectorQ is populated, depending on whether the
	// owning index was built in quantized mode.
	vector  []float32
	vectorQ []int8
	level   int
	friends [][]uint64 // friends[layer] = neighbor IDs at that layer
}

// HNSWIndex is a pure-Go Hierarchical Navigable Small World vector index.
type HNSWIndex struct {
	mu          sync.RWMutex
	nodes       map[uint64]*hnswNode
	entryPoint  uint64
	hasEntry    bool
	maxLevel    int
	m           int
	mMax0       int
	efConstruct int
	dim         int
	distFunc    DistanceFunc
	levelMult   float64
	rng         *rand.Rand
	// qp is non-nil iff this index is built in int8-quantized mode.
	// In that mode, hnswNode.vector is empty and vectorQ holds the int8
	// representation; distance() dequantizes on the fly into a pooled buffer.
	qp      *quantParams
	bufPool sync.Pool
}

// NewHNSWIndex creates a new HNSW index over float32 vectors.
func NewHNSWIndex(dim, m, efConstruction int, distFunc DistanceFunc) *HNSWIndex {
	return &HNSWIndex{
		nodes:       make(map[uint64]*hnswNode),
		m:           m,
		mMax0:       2 * m,
		efConstruct: efConstruction,
		dim:         dim,
		distFunc:    distFunc,
		levelMult:   1.0 / math.Log(float64(m)),
		rng:         rand.New(rand.NewSource(rand.Int63())),
	}
}

// NewHNSWIndexQuantized creates an HNSW index that stores nodes as int8 using
// the provided per-dimension quantization parameters. Distance calls
// transparently dequantize stored vectors into a pooled buffer.
func NewHNSWIndexQuantized(dim, m, efConstruction int, distFunc DistanceFunc, qp quantParams) *HNSWIndex {
	h := NewHNSWIndex(dim, m, efConstruction, distFunc)
	qpCopy := qp
	h.qp = &qpCopy
	h.bufPool = sync.Pool{New: func() any {
		b := make([]float32, dim)
		return &b
	}}
	return h
}

// distance computes distFunc(query, node) honoring the index's quantization
// mode. In quantized mode it dequantizes the node's int8 storage into a
// pooled float32 buffer to avoid per-call allocation.
func (h *HNSWIndex) distance(query []float32, n *hnswNode) float32 {
	if h.qp == nil {
		return h.distFunc(query, n.vector)
	}
	bufp := h.bufPool.Get().(*[]float32)
	buf := *bufp
	for d, b := range n.vectorQ {
		buf[d] = float32(int(b)+128)*h.qp.scale[d] + h.qp.min[d]
	}
	dist := h.distFunc(query, buf)
	h.bufPool.Put(bufp)
	return dist
}

// Len returns the number of points in the index.
func (h *HNSWIndex) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.nodes)
}

// Insert adds a point to the index. The caller passes a float32 vector; in
// quantized mode it is converted to int8 before storage.
func (h *HNSWIndex) Insert(id uint64, vector []float32) {
	h.mu.Lock()
	defer h.mu.Unlock()

	level := h.randomLevel()
	node := &hnswNode{
		id:      id,
		level:   level,
		friends: make([][]uint64, level+1),
	}
	if h.qp != nil {
		node.vectorQ = quantizeOne(vector, *h.qp)
	} else {
		node.vector = vector
	}
	h.nodes[id] = node

	if !h.hasEntry {
		h.entryPoint = id
		h.hasEntry = true
		h.maxLevel = level
		return
	}

	ep := h.entryPoint
	epDist := h.distance(vector, h.nodes[ep])

	// Traverse from top to the level just above the new node's level
	for lc := h.maxLevel; lc > level; lc-- {
		changed := true
		for changed {
			changed = false
			if epNode, ok := h.nodes[ep]; ok && lc < len(epNode.friends) {
				for _, fid := range epNode.friends[lc] {
					if fNode, ok := h.nodes[fid]; ok {
						d := h.distance(vector, fNode)
						if d < epDist {
							ep = fid
							epDist = d
							changed = true
						}
					}
				}
			}
		}
	}

	for lc := min(level, h.maxLevel); lc >= 0; lc-- {
		candidates := h.searchLayer(vector, ep, h.efConstruct, lc)
		mMax := h.m
		if lc == 0 {
			mMax = h.mMax0
		}
		neighbors := h.selectNeighborsSimple(candidates, mMax)

		node.friends[lc] = make([]uint64, len(neighbors))
		for i, n := range neighbors {
			node.friends[lc][i] = n.ID
		}

		for _, n := range neighbors {
			friendNode := h.nodes[n.ID]
			if lc >= len(friendNode.friends) {
				continue
			}
			friendNode.friends[lc] = append(friendNode.friends[lc], id)
			if len(friendNode.friends[lc]) > mMax {
				var friendCandidates []HNSWSearchResult
				for _, fid := range friendNode.friends[lc] {
					if fn, ok := h.nodes[fid]; ok {
						// In quant mode we need a float32 view of friendNode
						// to feed into distance(). Reconstruct on the fly.
						var ref []float32
						if h.qp != nil {
							bufp := h.bufPool.Get().(*[]float32)
							buf := *bufp
							for d, b := range friendNode.vectorQ {
								buf[d] = float32(int(b)+128)*h.qp.scale[d] + h.qp.min[d]
							}
							ref = buf
							friendCandidates = append(friendCandidates, HNSWSearchResult{
								ID:       fid,
								Distance: h.distance(ref, fn),
							})
							h.bufPool.Put(bufp)
						} else {
							ref = friendNode.vector
							friendCandidates = append(friendCandidates, HNSWSearchResult{
								ID:       fid,
								Distance: h.distFunc(ref, fn.vector),
							})
						}
					}
				}
				trimmed := h.selectNeighborsSimple(friendCandidates, mMax)
				friendNode.friends[lc] = make([]uint64, len(trimmed))
				for i, tr := range trimmed {
					friendNode.friends[lc][i] = tr.ID
				}
			}
		}

		if len(candidates) > 0 {
			ep = candidates[0].ID
		}
	}

	if level > h.maxLevel {
		h.maxLevel = level
		h.entryPoint = id
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
		epDist := h.distance(query, h.nodes[ep])
		for changed {
			changed = false
			if epNode, ok := h.nodes[ep]; ok && lc < len(epNode.friends) {
				for _, fid := range epNode.friends[lc] {
					if fNode, ok := h.nodes[fid]; ok {
						d := h.distance(query, fNode)
						if d < epDist {
							ep = fid
							epDist = d
							changed = true
						}
					}
				}
			}
		}
	}

	candidates := h.searchLayer(query, ep, max(ef, topK), 0)

	if allow != nil {
		filtered := candidates[:0]
		for _, c := range candidates {
			if _, ok := allow[c.ID]; ok {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	return candidates
}

func (h *HNSWIndex) searchLayer(query []float32, entryID uint64, ef, layer int) []HNSWSearchResult {
	visited := make(map[uint64]struct{})
	visited[entryID] = struct{}{}

	entryDist := h.distance(query, h.nodes[entryID])

	candidates := &minHeap{{ID: entryID, Distance: entryDist}}
	heap.Init(candidates)

	results := &maxHeap{{ID: entryID, Distance: entryDist}}
	heap.Init(results)

	for candidates.Len() > 0 {
		closest := heap.Pop(candidates).(HNSWSearchResult)
		farthestResult := (*results)[0]

		if closest.Distance > farthestResult.Distance {
			break
		}

		node := h.nodes[closest.ID]
		if layer >= len(node.friends) {
			continue
		}

		for _, fid := range node.friends[layer] {
			if _, ok := visited[fid]; ok {
				continue
			}
			visited[fid] = struct{}{}

			fNode, ok := h.nodes[fid]
			if !ok {
				continue
			}
			d := h.distance(query, fNode)
			farthestResult = (*results)[0]

			if d < farthestResult.Distance || results.Len() < ef {
				heap.Push(candidates, HNSWSearchResult{ID: fid, Distance: d})
				heap.Push(results, HNSWSearchResult{ID: fid, Distance: d})
				if results.Len() > ef {
					heap.Pop(results)
				}
			}
		}
	}

	out := make([]HNSWSearchResult, results.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(results).(HNSWSearchResult)
	}
	return out
}

func (h *HNSWIndex) selectNeighborsSimple(candidates []HNSWSearchResult, m int) []HNSWSearchResult {
	for i := 0; i < len(candidates) && i < m; i++ {
		minIdx := i
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].Distance < candidates[minIdx].Distance {
				minIdx = j
			}
		}
		candidates[i], candidates[minIdx] = candidates[minIdx], candidates[i]
	}
	if len(candidates) > m {
		return candidates[:m]
	}
	return candidates
}

func (h *HNSWIndex) randomLevel() int {
	level := 0
	for h.rng.Float64() < 1.0/math.E && level < 32 {
		level++
	}
	return level
}

// hnswMagicV2 marks the quantization-aware serialization format. Files
// without this magic are legacy float32 dumps and read with the v1 path.
var hnswMagicV2 = [4]byte{'H', 'N', 'S', '2'}

// Serialize encodes the HNSW index to binary.
//
// Format (v2):
//
//	magic "HNS2" (4) | u8 hasQuant
//	if hasQuant: u32 dim, dim*f32 mins, dim*f32 scales
//	u32 dim, u32 m, u32 ef, u32 maxLevel, u64 entryPoint, u8 hasEntry,
//	u32 nodeCount,
//	N * { u64 id, u32 level, u32 vecLen, vecLen * (u32 f32 OR i8),
//	      u32 layerCount, layerCount * (u32 friendCount, u64 friend...) }
func (h *HNSWIndex) Serialize() ([]byte, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	buf := make([]byte, 0, 1024*1024)
	buf = append(buf, hnswMagicV2[:]...)
	if h.qp != nil {
		buf = append(buf, 1)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(h.dim))
		for _, m := range h.qp.min {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(m))
		}
		for _, s := range h.qp.scale {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(s))
		}
	} else {
		buf = append(buf, 0)
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.dim))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.m))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.efConstruct))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.maxLevel))
	buf = binary.LittleEndian.AppendUint64(buf, h.entryPoint)
	if h.hasEntry {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(h.nodes)))

	for _, node := range h.nodes {
		buf = binary.LittleEndian.AppendUint64(buf, node.id)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(node.level))
		if h.qp != nil {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(node.vectorQ)))
			for _, b := range node.vectorQ {
				buf = append(buf, byte(b))
			}
		} else {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(node.vector)))
			for _, v := range node.vector {
				buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
			}
		}
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(node.friends)))
		for _, layer := range node.friends {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(layer)))
			for _, fid := range layer {
				buf = binary.LittleEndian.AppendUint64(buf, fid)
			}
		}
	}

	return buf, nil
}

// DeserializeHNSWIndex reconstructs an HNSW index from binary data. Detects
// the v2 quantization-aware format by magic; falls back to v1 (float32-only)
// when absent so legacy hnsw.bin files still load.
func DeserializeHNSWIndex(data []byte, distFunc DistanceFunc) (*HNSWIndex, error) {
	if len(data) < 25 {
		return nil, fmt.Errorf("data too short for HNSW header")
	}
	off := 0
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

	var qp *quantParams
	if len(data) >= 4 && data[0] == hnswMagicV2[0] && data[1] == hnswMagicV2[1] && data[2] == hnswMagicV2[2] && data[3] == hnswMagicV2[3] {
		off = 4
		hasQuant := data[off] == 1
		off++
		if hasQuant {
			dim := int(readU32())
			mins := make([]float32, dim)
			for d := 0; d < dim; d++ {
				mins[d] = math.Float32frombits(readU32())
			}
			scales := make([]float32, dim)
			for d := 0; d < dim; d++ {
				scales[d] = math.Float32frombits(readU32())
			}
			qp = &quantParams{min: mins, scale: scales}
		}
	}

	dim := int(readU32())
	m := int(readU32())
	efConstruct := int(readU32())
	maxLevel := int(readU32())
	entryPoint := readU64()
	hasEntry := data[off] == 1
	off++
	nodeCount := int(readU32())

	h := &HNSWIndex{
		nodes:       make(map[uint64]*hnswNode, nodeCount),
		entryPoint:  entryPoint,
		hasEntry:    hasEntry,
		maxLevel:    maxLevel,
		m:           m,
		mMax0:       2 * m,
		efConstruct: efConstruct,
		dim:         dim,
		distFunc:    distFunc,
		levelMult:   1.0 / math.Log(float64(m)),
		rng:         rand.New(rand.NewSource(rand.Int63())),
		qp:          qp,
	}
	if qp != nil {
		h.bufPool = sync.Pool{New: func() any {
			b := make([]float32, dim)
			return &b
		}}
	}

	for i := 0; i < nodeCount; i++ {
		id := readU64()
		level := int(readU32())
		vecLen := int(readU32())
		node := &hnswNode{id: id, level: level}
		if qp != nil {
			node.vectorQ = make([]int8, vecLen)
			for j := 0; j < vecLen; j++ {
				node.vectorQ[j] = int8(data[off])
				off++
			}
		} else {
			vec := make([]float32, vecLen)
			for j := range vec {
				vec[j] = math.Float32frombits(readU32())
			}
			node.vector = vec
		}
		friendLayers := int(readU32())
		friends := make([][]uint64, friendLayers)
		for l := 0; l < friendLayers; l++ {
			count := int(readU32())
			friends[l] = make([]uint64, count)
			for j := range friends[l] {
				friends[l][j] = readU64()
			}
		}
		node.friends = friends
		h.nodes[id] = node
	}

	return h, nil
}

// --- heap implementations ---

type minHeap []HNSWSearchResult

func (h minHeap) Len() int            { return len(h) }
func (h minHeap) Less(i, j int) bool  { return h[i].Distance < h[j].Distance }
func (h minHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x interface{}) { *h = append(*h, x.(HNSWSearchResult)) }
func (h *minHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

type maxHeap []HNSWSearchResult

func (h maxHeap) Len() int            { return len(h) }
func (h maxHeap) Less(i, j int) bool  { return h[i].Distance > h[j].Distance }
func (h maxHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x interface{}) { *h = append(*h, x.(HNSWSearchResult)) }
func (h *maxHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
