//go:build vector

package vector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/prysmsh/warp-storage-engine/internal/storage"
)

type sealJob struct {
	snapshot *GrowingSegment
	walSeq   uint64
	// barrier is signalled after the job is processed. nil snapshot + non-nil
	// barrier denotes a flush-only sentinel used by SyncSealing.
	barrier chan struct{}
}

type collectionState struct {
	mu         sync.RWMutex
	collection Collection
	growing    *GrowingSegment
	// sealing tracks snapshots that have been rotated off `growing` but not
	// yet appended to `sealed`. Search must include these to avoid losing
	// visibility on points during the in-flight window.
	sealing []*GrowingSegment
	sealed  []*SealedSegment
	wal     *WAL
	deleted map[PointID]struct{}

	sealQueue chan sealJob
	sealerWG  sync.WaitGroup
}

// Engine is a single-node vector database engine.
type Engine struct {
	mu          sync.RWMutex
	backend     storage.Backend
	cfg         VectorConfig
	collections map[string]*collectionState
	cache       *SegmentCache
	closed      chan struct{}
}

// NewEngine creates a new vector engine.
func NewEngine(backend storage.Backend, cfg VectorConfig) (*Engine, error) {
	if cfg.Bucket == "" {
		cfg.Bucket = "warp-vectors"
	}
	if cfg.HNSWm == 0 {
		cfg.HNSWm = 16
	}
	if cfg.HNSWefConstruct == 0 {
		cfg.HNSWefConstruct = 200
	}
	if cfg.HNSWefSearch == 0 {
		cfg.HNSWefSearch = 128
	}
	if cfg.CacheMemoryBytes == 0 {
		cfg.CacheMemoryBytes = 1 << 30
	}
	return &Engine{
		backend:     backend,
		cfg:         cfg,
		collections: make(map[string]*collectionState),
		cache:       NewSegmentCache(cfg.CacheMemoryBytes),
		closed:      make(chan struct{}),
	}, nil
}

func (e *Engine) Close() error {
	select {
	case <-e.closed:
		return nil
	default:
		close(e.closed)
	}
	e.mu.RLock()
	collections := make([]*collectionState, 0, len(e.collections))
	for _, cs := range e.collections {
		collections = append(collections, cs)
	}
	e.mu.RUnlock()

	ctx := context.Background()
	for _, cs := range collections {
		cs.wal.Flush(ctx)
		close(cs.sealQueue)
	}
	for _, cs := range collections {
		cs.sealerWG.Wait()
	}
	return nil
}

// SyncSealing blocks until any in-flight or queued seals for the named
// collection have completed. Intended for tests and graceful shutdown paths
// that need a stable view of cs.sealed.
func (e *Engine) SyncSealing(collection string) error {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("collection %q not found", collection)
	}
	// Round-trip a barrier sentinel: when its done channel closes, the sealer
	// has finished all prior jobs (it is single-goroutine).
	done := make(chan struct{})
	cs.sealQueue <- sealJob{barrier: done}
	<-done
	return nil
}

// startSealer launches the per-collection background sealing goroutine.
func (e *Engine) startSealer(cs *collectionState) {
	cs.sealerWG.Add(1)
	go func() {
		defer cs.sealerWG.Done()
		for job := range cs.sealQueue {
			e.runSealJob(cs, job)
		}
	}()
}

func (e *Engine) runSealJob(cs *collectionState, job sealJob) {
	defer func() {
		if job.barrier != nil {
			close(job.barrier)
		}
	}()
	if job.snapshot == nil {
		return
	}
	ctx := context.Background()
	cs.mu.RLock()
	quant := cs.collection.Quantization
	cs.mu.RUnlock()
	sealed, err := SealSegment(ctx, job.snapshot, e.backend, e.cfg.Bucket, e.cfg.HNSWm, e.cfg.HNSWefConstruct, quant)
	if err != nil {
		// Data is still durable in the WAL; on restart the snapshot will be
		// rebuilt and re-sealed. Surface failure on the sealing channel via
		// a sticky error in a future iteration; for now, drop the job.
		return
	}
	cs.mu.Lock()
	cs.sealed = append(cs.sealed, sealed)
	// Drop the matching in-flight snapshot. Equality by pointer is safe — the
	// snapshot is owned by the sealer once handed off and isn't reused.
	for i, sn := range cs.sealing {
		if sn == job.snapshot {
			cs.sealing = append(cs.sealing[:i], cs.sealing[i+1:]...)
			break
		}
	}
	cs.mu.Unlock()
	e.cache.Put(sealed.Meta.ID, sealed)

	_ = cs.wal.TruncateUpTo(ctx, job.walSeq)

	if e.cfg.CompactionMaxSegments > 0 {
		_ = e.maybeCompact(ctx, cs)
	}
}

// maybeCompact merges the two smallest sealed segments if the count exceeds
// the configured threshold. Tombstoned points are dropped during the merge.
func (e *Engine) maybeCompact(ctx context.Context, cs *collectionState) error {
	cs.mu.RLock()
	if len(cs.sealed) <= e.cfg.CompactionMaxSegments {
		cs.mu.RUnlock()
		return nil
	}
	a, b := pickSmallestPair(cs.sealed)
	deletedSnapshot := make(map[PointID]struct{}, len(cs.deleted))
	for id := range cs.deleted {
		deletedSnapshot[id] = struct{}{}
	}
	collection := cs.collection
	cs.mu.RUnlock()

	if a == nil || b == nil {
		return nil
	}

	mergedID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	merged := NewGrowingSegment(mergedID, collection.Name, 0, collection.Dimensions, collection.Distance)
	// Insert b's points last so newest-wins for any duplicate ID across the
	// pair (b has the larger ULID and is therefore newer).
	older, newer := a, b
	if a.Meta.ID.Compare(b.Meta.ID) > 0 {
		older, newer = b, a
	}
	seenInNewer := make(map[PointID]struct{})
	for _, p := range newer.Points() {
		if _, dropped := deletedSnapshot[p.ID]; dropped {
			continue
		}
		merged.Insert(p)
		seenInNewer[p.ID] = struct{}{}
	}
	for _, p := range older.Points() {
		if _, dropped := deletedSnapshot[p.ID]; dropped {
			continue
		}
		if _, shadowed := seenInNewer[p.ID]; shadowed {
			continue
		}
		merged.Insert(p)
	}

	mergedSealed, err := SealSegment(ctx, merged, e.backend, e.cfg.Bucket, e.cfg.HNSWm, e.cfg.HNSWefConstruct, collection.Quantization)
	if err != nil {
		return fmt.Errorf("compact seal: %w", err)
	}

	// Atomically replace [a, b] with [mergedSealed] in cs.sealed.
	cs.mu.Lock()
	newSealed := make([]*SealedSegment, 0, len(cs.sealed)-1)
	for _, ss := range cs.sealed {
		if ss == a || ss == b {
			continue
		}
		newSealed = append(newSealed, ss)
	}
	newSealed = append(newSealed, mergedSealed)
	sort.Slice(newSealed, func(i, j int) bool {
		return newSealed[i].Meta.ID.Compare(newSealed[j].Meta.ID) < 0
	})
	cs.sealed = newSealed
	cs.mu.Unlock()

	e.cache.Put(mergedSealed.Meta.ID, mergedSealed)
	// Old segment files are garbage now; readers no longer reference them
	// because cs.sealed has been swapped under cs.mu.
	_ = a.DeleteFromBackend(ctx, e.backend, e.cfg.Bucket)
	_ = b.DeleteFromBackend(ctx, e.backend, e.cfg.Bucket)
	return nil
}

func pickSmallestPair(segs []*SealedSegment) (*SealedSegment, *SealedSegment) {
	if len(segs) < 2 {
		return nil, nil
	}
	idx := make([]int, len(segs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool {
		return segs[idx[i]].Meta.PointCount < segs[idx[j]].Meta.PointCount
	})
	return segs[idx[0]], segs[idx[1]]
}

func (e *Engine) CreateCollection(ctx context.Context, col Collection) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.collections[col.Name]; ok {
		return fmt.Errorf("collection %q already exists", col.Name)
	}

	now := time.Now()
	col.CreatedAt = now
	col.UpdatedAt = now
	if col.ShardCount == 0 {
		col.ShardCount = 1
	}
	if col.Consistency == "" {
		col.Consistency = Eventual
	}
	if col.Replication == 0 {
		col.Replication = 1
	}

	metaJSON, err := json.Marshal(col)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s/meta.json", col.Name)
	if err := e.backend.PutObject(ctx, e.cfg.Bucket, key, bytes.NewReader(metaJSON), int64(len(metaJSON)), nil); err != nil {
		return fmt.Errorf("persist collection meta: %w", err)
	}

	segID := ulid.MustNew(ulid.Timestamp(now), rand.Reader)
	wal := NewWAL(e.backend, e.cfg.Bucket, col.Name, 0)
	wal.SetFlushThresholds(e.cfg.WALFlushCount, e.cfg.WALFlushInterval)

	cs := &collectionState{
		collection: col,
		growing:    NewGrowingSegment(segID, col.Name, 0, col.Dimensions, col.Distance),
		wal:        wal,
		deleted:    make(map[PointID]struct{}),
		sealQueue:  make(chan sealJob, 1),
	}
	e.collections[col.Name] = cs
	e.startSealer(cs)
	return nil
}

func (e *Engine) GetCollection(_ context.Context, name string) (*Collection, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cs, ok := e.collections[name]
	if !ok {
		return nil, fmt.Errorf("collection %q not found", name)
	}
	col := cs.collection
	return &col, nil
}

func (e *Engine) ListCollections(_ context.Context) ([]Collection, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cols := make([]Collection, 0, len(e.collections))
	for _, cs := range e.collections {
		cols = append(cols, cs.collection)
	}
	return cols, nil
}

func (e *Engine) DeleteCollection(ctx context.Context, name string) error {
	e.mu.Lock()
	cs, ok := e.collections[name]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("collection %q not found", name)
	}
	delete(e.collections, name)
	e.mu.Unlock()

	cs.wal.Flush(ctx)
	close(cs.sealQueue)
	cs.sealerWG.Wait()
	return nil
}

func (e *Engine) Insert(ctx context.Context, collection string, points []Point) error {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("collection %q not found", collection)
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, p := range points {
		if len(p.Vector) != cs.collection.Dimensions {
			return fmt.Errorf("point %d: expected %d dimensions, got %d", p.ID, cs.collection.Dimensions, len(p.Vector))
		}
		cs.growing.Insert(p)
		cs.wal.Append(WALEntry{Type: WALInsert, Point: p})
	}

	if cs.growing.Meta().SizeBytes >= e.cfg.SegmentSizeBytes {
		if err := e.sealGrowingLocked(ctx, cs); err != nil {
			return fmt.Errorf("seal: %w", err)
		}
	}

	return nil
}

// sealGrowingLocked rotates the growing segment under cs.mu and hands the old
// snapshot to the per-collection sealer. The actual seal (HNSW build + S3 PUTs)
// runs asynchronously; the channel has capacity 1 so a second pending seal
// blocks the caller, applying backpressure when sealing falls behind.
func (e *Engine) sealGrowingLocked(ctx context.Context, cs *collectionState) error {
	walSeq, err := cs.wal.Flush(ctx)
	if err != nil {
		return fmt.Errorf("flush WAL before seal: %w", err)
	}
	snapshot := cs.growing

	newID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	cs.growing = NewGrowingSegment(newID, cs.collection.Name, 0, cs.collection.Dimensions, cs.collection.Distance)
	cs.sealing = append(cs.sealing, snapshot)

	// Release cs.mu while waiting for queue space — otherwise readers block
	// behind a slow sealer. Re-acquire before returning so the caller's defer
	// Unlock matches the lock state it expects.
	cs.mu.Unlock()
	cs.sealQueue <- sealJob{snapshot: snapshot, walSeq: walSeq}
	cs.mu.Lock()
	return nil
}

func (e *Engine) Search(_ context.Context, req SearchRequest) (*SearchResponse, error) {
	start := time.Now()

	e.mu.RLock()
	cs, ok := e.collections[req.Collection]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("collection %q not found", req.Collection)
	}

	cs.mu.RLock()
	defer cs.mu.RUnlock()

	topK := req.TopK
	if topK == 0 {
		topK = 10
	}
	efSearch := req.EfSearch
	if efSearch == 0 {
		efSearch = e.cfg.HNSWefSearch
	}

	var allow map[uint64]struct{}
	if req.Filter != nil {
		combined := NewBitmapIndex()
		addPoints := func(pts []Point) {
			for _, p := range pts {
				for k, v := range p.Payload {
					combined.Add(p.ID, k, fmt.Sprint(v))
				}
			}
		}
		addPoints(cs.growing.Points())
		for _, sn := range cs.sealing {
			addPoints(sn.Points())
		}
		for _, ss := range cs.sealed {
			bm := ss.BitmapIndex()
			for field, vals := range bm.bitmaps {
				for val, bits := range vals {
					iter := bits.Iterator()
					for iter.HasNext() {
						combined.Add(uint64(iter.Next()), field, val)
					}
				}
			}
		}
		filterResult := EvalFilter(*req.Filter, combined)
		allow = make(map[uint64]struct{})
		iter := filterResult.Iterator()
		for iter.HasNext() {
			allow[uint64(iter.Next())] = struct{}{}
		}
	}

	var exclude map[uint64]struct{}
	if len(cs.deleted) > 0 {
		if allow != nil {
			for id := range cs.deleted {
				delete(allow, id)
			}
		} else {
			exclude = cs.deleted
		}
	}

	// Merge with newest-wins dedup: iterate sources oldest → newest so later
	// writes overwrite earlier ones. Sealed segments are in creation order
	// (sorted by ULID); growing is the most recent source.
	merged := make(map[PointID]ScoredPoint)
	keep := func(r ScoredPoint) {
		if exclude != nil {
			if _, ok := exclude[r.ID]; ok {
				return
			}
		}
		merged[r.ID] = r
	}
	for _, ss := range cs.sealed {
		for _, r := range ss.Search(req.Vector, topK, efSearch, allow) {
			keep(r)
		}
	}
	// In-flight snapshots are older than the live growing segment but newer
	// than every sealed segment that wasn't built from them.
	for _, sn := range cs.sealing {
		for _, r := range sn.Search(req.Vector, topK, allow) {
			keep(r)
		}
	}
	for _, r := range cs.growing.Search(req.Vector, topK, allow) {
		keep(r)
	}

	all := make([]ScoredPoint, 0, len(merged))
	for _, r := range merged {
		all = append(all, r)
	}
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

func (e *Engine) Get(_ context.Context, collection string, ids []PointID) ([]Point, error) {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("collection %q not found", collection)
	}

	cs.mu.RLock()
	defer cs.mu.RUnlock()

	var results []Point
	for _, id := range ids {
		if _, deleted := cs.deleted[id]; deleted {
			continue
		}
		if p, ok := cs.growing.Get(id); ok {
			results = append(results, p)
			continue
		}
		found := false
		for i := len(cs.sealing) - 1; i >= 0; i-- {
			if p, ok := cs.sealing[i].Get(id); ok {
				results = append(results, p)
				found = true
				break
			}
		}
		if found {
			continue
		}
		for i := len(cs.sealed) - 1; i >= 0; i-- {
			if p, ok := cs.sealed[i].Get(id); ok {
				results = append(results, p)
				break
			}
		}
	}
	return results, nil
}

func (e *Engine) Delete(_ context.Context, collection string, ids []PointID) error {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("collection %q not found", collection)
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, id := range ids {
		cs.deleted[id] = struct{}{}
		cs.wal.Append(WALEntry{Type: WALDelete, PointID: id})
	}
	return nil
}

// LoadCollectionsFromBackend discovers and loads collections from storage.
func (e *Engine) LoadCollectionsFromBackend(ctx context.Context) error {
	result, err := e.backend.ListObjects(ctx, e.cfg.Bucket, "", "", 10000)
	if err != nil {
		return fmt.Errorf("list collections: %w", err)
	}

	seen := make(map[string]struct{})
	for _, obj := range result.Contents {
		parts := splitKey(obj.Key)
		if len(parts) == 2 && parts[1] == "meta.json" {
			colName := parts[0]
			if _, ok := seen[colName]; ok {
				continue
			}
			seen[colName] = struct{}{}

			metaObj, err := e.backend.GetObject(ctx, e.cfg.Bucket, obj.Key)
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(metaObj.Body)
			metaObj.Body.Close()

			var col Collection
			if json.Unmarshal(data, &col) != nil {
				continue
			}

			segID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
			wal := NewWAL(e.backend, e.cfg.Bucket, col.Name, 0)
			wal.SetFlushThresholds(e.cfg.WALFlushCount, e.cfg.WALFlushInterval)

			cs := &collectionState{
				collection: col,
				growing:    NewGrowingSegment(segID, col.Name, 0, col.Dimensions, col.Distance),
				wal:        wal,
				deleted:    make(map[PointID]struct{}),
				sealQueue:  make(chan sealJob, 1),
			}

			sealed, err := loadSealedSegmentsForShard(ctx, e.backend, e.cfg.Bucket, col.Name, 0, col.Distance)
			if err != nil {
				return fmt.Errorf("load sealed segments for %q: %w", col.Name, err)
			}
			cs.sealed = sealed
			for _, ss := range sealed {
				e.cache.Put(ss.Meta.ID, ss)
			}

			entries, _ := ReplayWAL(ctx, e.backend, e.cfg.Bucket, col.Name, 0)
			for _, entry := range entries {
				switch entry.Type {
				case WALInsert:
					cs.growing.Insert(entry.Point)
				case WALDelete:
					cs.deleted[entry.PointID] = struct{}{}
				}
			}

			e.mu.Lock()
			e.collections[col.Name] = cs
			e.mu.Unlock()
			e.startSealer(cs)
		}
	}
	return nil
}

// loadSealedSegmentsForShard discovers segment IDs under <col>/<shard>/segments/
// and loads each one. Segments are returned sorted by ULID (creation order).
func loadSealedSegmentsForShard(ctx context.Context, be storage.Backend, bucket, col string, shard int, metric DistanceMetric) ([]*SealedSegment, error) {
	prefix := fmt.Sprintf("%s/%d/segments/", col, shard)
	result, err := be.ListObjects(ctx, bucket, prefix, "", 10000)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}

	seen := make(map[string]struct{})
	var ids []ulid.ULID
	for _, obj := range result.Contents {
		// Key shape: <col>/<shard>/segments/<segID>/meta.json
		parts := splitKey(obj.Key)
		if len(parts) != 5 || parts[4] != "meta.json" {
			continue
		}
		idStr := parts[3]
		if _, ok := seen[idStr]; ok {
			continue
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			continue
		}
		seen[idStr] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })

	segments := make([]*SealedSegment, 0, len(ids))
	for _, id := range ids {
		ss, err := LoadSealedSegment(ctx, be, bucket, col, shard, id, metric)
		if err != nil {
			return nil, fmt.Errorf("load segment %s: %w", id.String(), err)
		}
		segments = append(segments, ss)
	}
	return segments, nil
}

func splitKey(key string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			if i > start {
				parts = append(parts, key[start:i])
			}
			start = i + 1
		}
	}
	if start < len(key) {
		parts = append(parts, key[start:])
	}
	return parts
}
