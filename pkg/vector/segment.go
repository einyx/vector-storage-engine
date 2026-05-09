//go:build vector

package vector

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/prysmsh/warp-storage-engine/internal/storage"
)

func segmentPath(collection string, shard int, segID ulid.ULID) string {
	return fmt.Sprintf("%s/%d/segments/%s", collection, shard, segID.String())
}

// GrowingSegment is a mutable, in-memory segment accepting writes.
type GrowingSegment struct {
	mu       sync.RWMutex
	meta     SegmentMeta
	points   []Point
	pointMap map[PointID]*Point
	distFunc DistanceFunc
}

// NewGrowingSegment creates a new growing segment.
func NewGrowingSegment(id SegmentID, collection string, shard, dim int, metric DistanceMetric) *GrowingSegment {
	df, _ := DistanceFuncFor(metric)
	return &GrowingSegment{
		meta: SegmentMeta{
			ID:         id,
			Collection: collection,
			Shard:      shard,
			State:      SegmentGrowing,
			Dimensions: dim,
			Distance:   metric,
			CreatedAt:  time.Now(),
		},
		pointMap: make(map[PointID]*Point),
		distFunc: df,
	}
}

func (g *GrowingSegment) Insert(p Point) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := p
	g.points = append(g.points, cp)
	g.pointMap[p.ID] = &g.points[len(g.points)-1]
	g.meta.PointCount = len(g.points)
	g.meta.SizeBytes += int64(len(p.Vector)*4 + 8)
}

func (g *GrowingSegment) Get(id PointID) (Point, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	p, ok := g.pointMap[id]
	if !ok {
		return Point{}, false
	}
	return *p, true
}

func (g *GrowingSegment) Count() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.points)
}

func (g *GrowingSegment) Meta() SegmentMeta {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.meta
}

// Search does brute-force search over all points.
func (g *GrowingSegment) Search(query []float32, topK int, allow map[uint64]struct{}) []ScoredPoint {
	g.mu.RLock()
	defer g.mu.RUnlock()

	type scored struct {
		point Point
		dist  float32
	}
	var results []scored
	for _, p := range g.points {
		if allow != nil {
			if _, ok := allow[p.ID]; !ok {
				continue
			}
		}
		d := g.distFunc(query, p.Vector)
		results = append(results, scored{p, d})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].dist < results[j].dist
	})
	if len(results) > topK {
		results = results[:topK]
	}
	out := make([]ScoredPoint, len(results))
	for i, r := range results {
		out[i] = ScoredPoint{Point: r.point, Score: r.dist}
	}
	return out
}

func (g *GrowingSegment) Points() []Point {
	g.mu.RLock()
	defer g.mu.RUnlock()
	cp := make([]Point, len(g.points))
	copy(cp, g.points)
	return cp
}

// SealedSegment is an immutable, indexed segment.
type SealedSegment struct {
	Meta     SegmentMeta
	vectors  []Point
	pointMap map[PointID]int
	hnsw     *HNSWIndex
	bitmaps  *BitmapIndex
	distFunc DistanceFunc
}

// SealSegment freezes a growing segment, builds indices, writes to backend.
// The optional quantization argument controls vector storage encoding;
// QuantNone (or "") writes float32, QuantInt8 writes scalar-quantized int8.
func SealSegment(ctx context.Context, gs *GrowingSegment, be storage.Backend, bucket string, hnswM, hnswEfConstruct int, quant ...Quantization) (*SealedSegment, error) {
	q := QuantNone
	if len(quant) > 0 {
		q = quant[0]
	}
	points := gs.Points()
	gsMeta := gs.Meta()

	distFunc, err := DistanceFuncFor(gsMeta.Distance)
	if err != nil {
		return nil, err
	}

	hnsw := NewHNSWIndex(gsMeta.Dimensions, hnswM, hnswEfConstruct, distFunc)
	for _, p := range points {
		hnsw.Insert(p.ID, p.Vector)
	}

	bitmaps := NewBitmapIndex()
	for _, p := range points {
		for k, v := range p.Payload {
			bitmaps.Add(p.ID, k, fmt.Sprint(v))
		}
	}

	now := time.Now()
	meta := SegmentMeta{
		ID:           gsMeta.ID,
		Collection:   gsMeta.Collection,
		Shard:        gsMeta.Shard,
		State:        SegmentIndexed,
		PointCount:   len(points),
		Dimensions:   gsMeta.Dimensions,
		Distance:     gsMeta.Distance,
		Quantization: q,
		CreatedAt:    gsMeta.CreatedAt,
		SealedAt:     &now,
	}

	ss := &SealedSegment{
		Meta:     meta,
		vectors:  points,
		pointMap: make(map[PointID]int, len(points)),
		hnsw:     hnsw,
		bitmaps:  bitmaps,
		distFunc: distFunc,
	}
	for i, p := range points {
		ss.pointMap[p.ID] = i
	}

	prefix := segmentPath(meta.Collection, meta.Shard, meta.ID)

	// vectors[_q].bin — encoding depends on meta.Quantization.
	var vecData []byte
	var vecKey string
	if q == QuantInt8 {
		qp := computeQuantParams(points, meta.Dimensions)
		vecData = encodeQuantizedVectors(points, qp, meta.Dimensions)
		vecKey = prefix + "/vectors_q.bin"
	} else {
		var vecBuf bytes.Buffer
		binary.Write(&vecBuf, binary.LittleEndian, uint32(len(points)))
		binary.Write(&vecBuf, binary.LittleEndian, uint32(meta.Dimensions))
		for _, p := range points {
			binary.Write(&vecBuf, binary.LittleEndian, p.ID)
			for _, v := range p.Vector {
				binary.Write(&vecBuf, binary.LittleEndian, math.Float32bits(v))
			}
		}
		vecData = vecBuf.Bytes()
		vecKey = prefix + "/vectors.bin"
	}
	meta.SizeBytes += int64(len(vecData))
	if err := be.PutObject(ctx, bucket, vecKey, bytes.NewReader(vecData), int64(len(vecData)), nil); err != nil {
		return nil, fmt.Errorf("write vectors: %w", err)
	}

	// hnsw.bin
	hnswData, err := hnsw.Serialize()
	if err != nil {
		return nil, fmt.Errorf("serialize hnsw: %w", err)
	}
	meta.SizeBytes += int64(len(hnswData))
	if err := be.PutObject(ctx, bucket, prefix+"/hnsw.bin", bytes.NewReader(hnswData), int64(len(hnswData)), nil); err != nil {
		return nil, fmt.Errorf("write hnsw.bin: %w", err)
	}

	// payload.bin
	payloadJSON, err := json.Marshal(payloadsFromPoints(points))
	if err != nil {
		return nil, fmt.Errorf("marshal payloads: %w", err)
	}
	bitmapData, err := bitmaps.Serialize()
	if err != nil {
		return nil, fmt.Errorf("serialize bitmaps: %w", err)
	}
	var payBuf bytes.Buffer
	binary.Write(&payBuf, binary.LittleEndian, uint32(len(bitmapData)))
	payBuf.Write(bitmapData)
	binary.Write(&payBuf, binary.LittleEndian, uint32(len(payloadJSON)))
	payBuf.Write(payloadJSON)
	payData := payBuf.Bytes()
	meta.SizeBytes += int64(len(payData))
	if err := be.PutObject(ctx, bucket, prefix+"/payload.bin", bytes.NewReader(payData), int64(len(payData)), nil); err != nil {
		return nil, fmt.Errorf("write payload.bin: %w", err)
	}

	// meta.json
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal meta: %w", err)
	}
	if err := be.PutObject(ctx, bucket, prefix+"/meta.json", bytes.NewReader(metaJSON), int64(len(metaJSON)), nil); err != nil {
		return nil, fmt.Errorf("write meta.json: %w", err)
	}

	ss.Meta = meta
	return ss, nil
}

// LoadSealedSegment reads a sealed segment from the storage backend.
func LoadSealedSegment(ctx context.Context, be storage.Backend, bucket, collection string, shard int, segID ulid.ULID, metric DistanceMetric) (*SealedSegment, error) {
	prefix := segmentPath(collection, shard, segID)
	distFunc, err := DistanceFuncFor(metric)
	if err != nil {
		return nil, err
	}

	metaObj, err := be.GetObject(ctx, bucket, prefix+"/meta.json")
	if err != nil {
		return nil, fmt.Errorf("read meta.json: %w", err)
	}
	metaBytes, err := io.ReadAll(metaObj.Body)
	metaObj.Body.Close()
	if err != nil {
		return nil, err
	}
	var meta SegmentMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal meta: %w", err)
	}

	vecKey := prefix + "/vectors.bin"
	if meta.Quantization == QuantInt8 {
		vecKey = prefix + "/vectors_q.bin"
	}
	vecObj, err := be.GetObject(ctx, bucket, vecKey)
	if err != nil {
		return nil, fmt.Errorf("read vectors: %w", err)
	}
	vecData, err := io.ReadAll(vecObj.Body)
	vecObj.Body.Close()
	if err != nil {
		return nil, err
	}
	var points []Point
	if meta.Quantization == QuantInt8 {
		points, err = decodeQuantizedVectors(vecData, meta.Dimensions)
	} else {
		points, err = decodeVectors(vecData, meta.Dimensions)
	}
	if err != nil {
		return nil, fmt.Errorf("decode vectors: %w", err)
	}

	hnswObj, err := be.GetObject(ctx, bucket, prefix+"/hnsw.bin")
	if err != nil {
		return nil, fmt.Errorf("read hnsw.bin: %w", err)
	}
	hnswData, err := io.ReadAll(hnswObj.Body)
	hnswObj.Body.Close()
	if err != nil {
		return nil, err
	}
	hnsw, err := DeserializeHNSWIndex(hnswData, distFunc)
	if err != nil {
		return nil, fmt.Errorf("deserialize hnsw: %w", err)
	}

	payObj, err := be.GetObject(ctx, bucket, prefix+"/payload.bin")
	if err != nil {
		return nil, fmt.Errorf("read payload.bin: %w", err)
	}
	payData, err := io.ReadAll(payObj.Body)
	payObj.Body.Close()
	if err != nil {
		return nil, err
	}
	bitmaps, payloads, err := decodePayloads(payData)
	if err != nil {
		return nil, fmt.Errorf("decode payloads: %w", err)
	}

	for i := range points {
		if pl, ok := payloads[points[i].ID]; ok {
			points[i].Payload = pl
		}
	}

	ss := &SealedSegment{
		Meta:     meta,
		vectors:  points,
		pointMap: make(map[PointID]int, len(points)),
		hnsw:     hnsw,
		bitmaps:  bitmaps,
		distFunc: distFunc,
	}
	for i, p := range points {
		ss.pointMap[p.ID] = i
	}
	return ss, nil
}

func (s *SealedSegment) Search(query []float32, topK, efSearch int, allow map[uint64]struct{}) []ScoredPoint {
	results := s.hnsw.Search(query, topK, efSearch, allow)
	out := make([]ScoredPoint, 0, len(results))
	for _, r := range results {
		if idx, ok := s.pointMap[r.ID]; ok {
			out = append(out, ScoredPoint{Point: s.vectors[idx], Score: r.Distance})
		}
	}
	return out
}

func (s *SealedSegment) Get(id PointID) (Point, bool) {
	if idx, ok := s.pointMap[id]; ok {
		return s.vectors[idx], true
	}
	return Point{}, false
}

func (s *SealedSegment) BitmapIndex() *BitmapIndex {
	return s.bitmaps
}

// Points returns a copy of the segment's point slice. Used by compaction.
func (s *SealedSegment) Points() []Point {
	out := make([]Point, len(s.vectors))
	copy(out, s.vectors)
	return out
}

// DeleteFromBackend removes all object keys for this segment from the backend.
func (s *SealedSegment) DeleteFromBackend(ctx context.Context, be storage.Backend, bucket string) error {
	prefix := segmentPath(s.Meta.Collection, s.Meta.Shard, s.Meta.ID)
	suffixes := []string{"/meta.json", "/hnsw.bin", "/payload.bin"}
	if s.Meta.Quantization == QuantInt8 {
		suffixes = append(suffixes, "/vectors_q.bin")
	} else {
		suffixes = append(suffixes, "/vectors.bin")
	}
	for _, suffix := range suffixes {
		if err := be.DeleteObject(ctx, bucket, prefix+suffix); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ---

type payloadMap map[PointID]map[string]any

func payloadsFromPoints(points []Point) payloadMap {
	m := make(payloadMap, len(points))
	for _, p := range points {
		if len(p.Payload) > 0 {
			m[p.ID] = p.Payload
		}
	}
	return m
}

func decodeVectors(data []byte, dim int) ([]Point, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("vectors data too short")
	}
	count := int(binary.LittleEndian.Uint32(data[0:4]))
	dataDim := int(binary.LittleEndian.Uint32(data[4:8]))
	if dataDim != dim {
		return nil, fmt.Errorf("dimension mismatch: expected %d, got %d", dim, dataDim)
	}

	points := make([]Point, 0, count)
	off := 8
	for i := 0; i < count; i++ {
		if off+8 > len(data) {
			return nil, fmt.Errorf("truncated at point %d", i)
		}
		id := binary.LittleEndian.Uint64(data[off:])
		off += 8
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			vec[j] = math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))
			off += 4
		}
		points = append(points, Point{ID: id, Vector: vec})
	}
	return points, nil
}

func decodePayloads(data []byte) (*BitmapIndex, payloadMap, error) {
	if len(data) < 4 {
		return nil, nil, fmt.Errorf("payload data too short")
	}
	off := 0
	bmLen := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	if off+bmLen > len(data) {
		return nil, nil, fmt.Errorf("bitmap data truncated")
	}
	bitmaps, err := DeserializeBitmapIndex(data[off : off+bmLen])
	if err != nil {
		return nil, nil, err
	}
	off += bmLen

	if off+4 > len(data) {
		return nil, nil, fmt.Errorf("payload JSON length truncated")
	}
	jsonLen := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	if off+jsonLen > len(data) {
		return nil, nil, fmt.Errorf("payload JSON truncated")
	}
	var payloads payloadMap
	if err := json.Unmarshal(data[off:off+jsonLen], &payloads); err != nil {
		return nil, nil, fmt.Errorf("unmarshal payloads: %w", err)
	}

	return bitmaps, payloads, nil
}
