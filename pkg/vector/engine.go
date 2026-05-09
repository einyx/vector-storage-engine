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

// shardState holds all per-shard mutable state. A collection has one or more
// shards; each shard has its own growing segment, sealed-segment list, WAL,
// tombstone set, and async sealer goroutine.
type shardState struct {
	mu      sync.RWMutex
	index   int
	growing *GrowingSegment
	sealing []*GrowingSegment
	sealed  []*SealedSegment
	wal     *WAL
	deleted map[PointID]struct{}

	sealQueue chan sealJob
	sealerWG  sync.WaitGroup
}

type collectionState struct {
	mu         sync.RWMutex
	collection Collection
	shards     []*shardState
}

func (cs *collectionState) shardOf(id PointID) *shardState {
	if len(cs.shards) == 1 {
		return cs.shards[0]
	}
	return cs.shards[id%uint64(len(cs.shards))]
}

// sealedAcrossShards returns the union of sealed segments from every shard.
// Intended for tests and metrics — not for the hot search path.
func (cs *collectionState) sealedAcrossShards() []*SealedSegment {
	var out []*SealedSegment
	for _, sh := range cs.shards {
		sh.mu.RLock()
		out = append(out, sh.sealed...)
		sh.mu.RUnlock()
	}
	return out
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
		for _, sh := range cs.shards {
			sh.wal.Flush(ctx)
			close(sh.sealQueue)
		}
	}
	for _, cs := range collections {
		for _, sh := range cs.shards {
			sh.sealerWG.Wait()
		}
	}
	return nil
}

// SyncSealing blocks until any in-flight or queued seals for every shard of
// the named collection have completed.
func (e *Engine) SyncSealing(collection string) error {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("collection %q not found", collection)
	}
	for _, sh := range cs.shards {
		done := make(chan struct{})
		sh.sealQueue <- sealJob{barrier: done}
		<-done
	}
	return nil
}

// startSealer launches the background sealing goroutine for one shard.
func (e *Engine) startSealer(cs *collectionState, sh *shardState) {
	sh.sealerWG.Add(1)
	go func() {
		defer sh.sealerWG.Done()
		for job := range sh.sealQueue {
			e.runSealJob(cs, sh, job)
		}
	}()
}

func (e *Engine) runSealJob(cs *collectionState, sh *shardState, job sealJob) {
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
	colName := cs.collection.Name
	cs.mu.RUnlock()
	sealed, err := SealSegment(ctx, job.snapshot, e.backend, e.cfg.Bucket, e.cfg.HNSWm, e.cfg.HNSWefConstruct, quant)
	if err != nil {
		return
	}
	sh.mu.Lock()
	sh.sealed = append(sh.sealed, sealed)
	for i, sn := range sh.sealing {
		if sn == job.snapshot {
			sh.sealing = append(sh.sealing[:i], sh.sealing[i+1:]...)
			break
		}
	}
	tombSnapshot := make([]PointID, 0, len(sh.deleted))
	for id := range sh.deleted {
		tombSnapshot = append(tombSnapshot, id)
	}
	sh.mu.Unlock()
	e.cache.Put(sealed.Meta.ID, sealed)

	if err := writeTombstones(ctx, e.backend, e.cfg.Bucket, colName, sh.index, tombSnapshot); err != nil {
		return
	}
	_ = sh.wal.TruncateUpTo(ctx, job.walSeq)

	if e.cfg.CompactionMaxSegments > 0 {
		_ = e.maybeCompact(ctx, cs, sh)
	}
}

// maybeCompact merges the two smallest sealed segments in a single shard if
// the count exceeds CompactionMaxSegments.
func (e *Engine) maybeCompact(ctx context.Context, cs *collectionState, sh *shardState) error {
	sh.mu.RLock()
	if len(sh.sealed) <= e.cfg.CompactionMaxSegments {
		sh.mu.RUnlock()
		return nil
	}
	a, b := pickSmallestPair(sh.sealed)
	deletedSnapshot := make(map[PointID]struct{}, len(sh.deleted))
	for id := range sh.deleted {
		deletedSnapshot[id] = struct{}{}
	}
	sh.mu.RUnlock()

	if a == nil || b == nil {
		return nil
	}
	cs.mu.RLock()
	collection := cs.collection
	cs.mu.RUnlock()

	mergedID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	merged := NewGrowingSegment(mergedID, collection.Name, sh.index, collection.Dimensions, collection.Distance)
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

	sh.mu.Lock()
	newSealed := make([]*SealedSegment, 0, len(sh.sealed)-1)
	for _, ss := range sh.sealed {
		if ss == a || ss == b {
			continue
		}
		newSealed = append(newSealed, ss)
	}
	newSealed = append(newSealed, mergedSealed)
	sort.Slice(newSealed, func(i, j int) bool {
		return newSealed[i].Meta.ID.Compare(newSealed[j].Meta.ID) < 0
	})
	sh.sealed = newSealed
	sh.mu.Unlock()

	e.cache.Put(mergedSealed.Meta.ID, mergedSealed)
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

func (e *Engine) newShard(col Collection, idx int) *shardState {
	wal := NewWAL(e.backend, e.cfg.Bucket, col.Name, idx)
	wal.SetFlushThresholds(e.cfg.WALFlushCount, e.cfg.WALFlushInterval)
	segID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	return &shardState{
		index:     idx,
		growing:   NewGrowingSegment(segID, col.Name, idx, col.Dimensions, col.Distance),
		wal:       wal,
		deleted:   make(map[PointID]struct{}),
		sealQueue: make(chan sealJob, 1),
	}
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

	cs := &collectionState{
		collection: col,
		shards:     make([]*shardState, col.ShardCount),
	}
	for i := 0; i < col.ShardCount; i++ {
		cs.shards[i] = e.newShard(col, i)
	}
	e.collections[col.Name] = cs
	for _, sh := range cs.shards {
		e.startSealer(cs, sh)
	}
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

	for _, sh := range cs.shards {
		sh.wal.Flush(ctx)
		close(sh.sealQueue)
	}
	for _, sh := range cs.shards {
		sh.sealerWG.Wait()
	}
	return nil
}

func (e *Engine) Insert(ctx context.Context, collection string, points []Point) error {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("collection %q not found", collection)
	}

	// Group by shard before locking — avoids holding any shard lock while we
	// fan out, and keeps each shard's critical section short.
	grouped := make(map[int][]Point, len(cs.shards))
	for _, p := range points {
		if len(p.Vector) != cs.collection.Dimensions {
			return fmt.Errorf("point %d: expected %d dimensions, got %d", p.ID, cs.collection.Dimensions, len(p.Vector))
		}
		sh := cs.shardOf(p.ID)
		grouped[sh.index] = append(grouped[sh.index], p)
	}

	for shardIdx, batch := range grouped {
		sh := cs.shards[shardIdx]
		if err := e.insertIntoShard(ctx, cs, sh, batch); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) insertIntoShard(ctx context.Context, cs *collectionState, sh *shardState, batch []Point) error {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	for _, p := range batch {
		sh.growing.Insert(p)
		sh.wal.Append(WALEntry{Type: WALInsert, Point: p})
	}

	if sh.growing.Meta().SizeBytes >= e.cfg.SegmentSizeBytes {
		if err := e.sealGrowingLocked(ctx, cs, sh); err != nil {
			return fmt.Errorf("seal: %w", err)
		}
	}
	return nil
}

// sealGrowingLocked rotates the shard's growing segment under sh.mu and hands
// the old snapshot to the per-shard sealer.
func (e *Engine) sealGrowingLocked(ctx context.Context, cs *collectionState, sh *shardState) error {
	walSeq, err := sh.wal.Flush(ctx)
	if err != nil {
		return fmt.Errorf("flush WAL before seal: %w", err)
	}
	snapshot := sh.growing

	newID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	sh.growing = NewGrowingSegment(newID, cs.collection.Name, sh.index, cs.collection.Dimensions, cs.collection.Distance)
	sh.sealing = append(sh.sealing, snapshot)

	sh.mu.Unlock()
	sh.sealQueue <- sealJob{snapshot: snapshot, walSeq: walSeq}
	sh.mu.Lock()
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

	topK := req.TopK
	if topK == 0 {
		topK = 10
	}
	efSearch := req.EfSearch
	if efSearch == 0 {
		efSearch = e.cfg.HNSWefSearch
	}

	// Fan out across shards in parallel; each shard search is independent and
	// returns at most topK hits. The merge pass dedups across shards too.
	type shardResult struct {
		points []ScoredPoint
	}
	results := make([]shardResult, len(cs.shards))
	var wg sync.WaitGroup
	for i, sh := range cs.shards {
		wg.Add(1)
		go func(i int, sh *shardState) {
			defer wg.Done()
			results[i] = shardResult{points: e.searchShard(sh, req, topK, efSearch)}
		}(i, sh)
	}
	wg.Wait()

	// Final newest-wins dedup across shards (same ID can in theory exist in
	// different shards if ShardCount changes, but for now ID maps to a single
	// shard so this is purely defensive).
	merged := make(map[PointID]ScoredPoint)
	for _, r := range results {
		for _, p := range r.points {
			if existing, ok := merged[p.ID]; !ok || p.Score < existing.Score {
				merged[p.ID] = p
			}
		}
	}
	all := make([]ScoredPoint, 0, len(merged))
	for _, r := range merged {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score < all[j].Score })
	if len(all) > topK {
		all = all[:topK]
	}

	return &SearchResponse{
		Points:   all,
		SearchMs: time.Since(start).Milliseconds(),
	}, nil
}

func (e *Engine) searchShard(sh *shardState, req SearchRequest, topK, efSearch int) []ScoredPoint {
	sh.mu.RLock()
	defer sh.mu.RUnlock()

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
		addPoints(sh.growing.Points())
		for _, sn := range sh.sealing {
			addPoints(sn.Points())
		}
		for _, ss := range sh.sealed {
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
	if len(sh.deleted) > 0 {
		if allow != nil {
			for id := range sh.deleted {
				delete(allow, id)
			}
		} else {
			exclude = sh.deleted
		}
	}

	merged := make(map[PointID]ScoredPoint)
	keep := func(r ScoredPoint) {
		if exclude != nil {
			if _, ok := exclude[r.ID]; ok {
				return
			}
		}
		merged[r.ID] = r
	}
	for _, ss := range sh.sealed {
		for _, r := range ss.Search(req.Vector, topK, efSearch, allow) {
			keep(r)
		}
	}
	for _, sn := range sh.sealing {
		for _, r := range sn.Search(req.Vector, topK, allow) {
			keep(r)
		}
	}
	for _, r := range sh.growing.Search(req.Vector, topK, allow) {
		keep(r)
	}

	out := make([]ScoredPoint, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func (e *Engine) Get(_ context.Context, collection string, ids []PointID) ([]Point, error) {
	e.mu.RLock()
	cs, ok := e.collections[collection]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("collection %q not found", collection)
	}

	var results []Point
	for _, id := range ids {
		sh := cs.shardOf(id)
		sh.mu.RLock()
		if _, deleted := sh.deleted[id]; deleted {
			sh.mu.RUnlock()
			continue
		}
		if p, ok := sh.growing.Get(id); ok {
			results = append(results, p)
			sh.mu.RUnlock()
			continue
		}
		found := false
		for i := len(sh.sealing) - 1; i >= 0; i-- {
			if p, ok := sh.sealing[i].Get(id); ok {
				results = append(results, p)
				found = true
				break
			}
		}
		if found {
			sh.mu.RUnlock()
			continue
		}
		for i := len(sh.sealed) - 1; i >= 0; i-- {
			if p, ok := sh.sealed[i].Get(id); ok {
				results = append(results, p)
				break
			}
		}
		sh.mu.RUnlock()
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
	for _, id := range ids {
		sh := cs.shardOf(id)
		sh.mu.Lock()
		sh.deleted[id] = struct{}{}
		sh.wal.Append(WALEntry{Type: WALDelete, PointID: id})
		sh.mu.Unlock()
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
			if col.ShardCount == 0 {
				col.ShardCount = 1
			}

			cs := &collectionState{
				collection: col,
				shards:     make([]*shardState, col.ShardCount),
			}
			for i := 0; i < col.ShardCount; i++ {
				sh := e.newShard(col, i)

				deleted, err := readTombstones(ctx, e.backend, e.cfg.Bucket, col.Name, i)
				if err != nil {
					return fmt.Errorf("read tombstones for %q shard %d: %w", col.Name, i, err)
				}
				sh.deleted = deleted

				sealed, err := loadSealedSegmentsForShard(ctx, e.backend, e.cfg.Bucket, col.Name, i, col.Distance)
				if err != nil {
					return fmt.Errorf("load sealed segments for %q shard %d: %w", col.Name, i, err)
				}
				sh.sealed = sealed
				for _, ss := range sealed {
					e.cache.Put(ss.Meta.ID, ss)
				}

				entries, _ := ReplayWAL(ctx, e.backend, e.cfg.Bucket, col.Name, i)
				for _, entry := range entries {
					switch entry.Type {
					case WALInsert:
						sh.growing.Insert(entry.Point)
					case WALDelete:
						sh.deleted[entry.PointID] = struct{}{}
					}
				}
				cs.shards[i] = sh
			}

			e.mu.Lock()
			e.collections[col.Name] = cs
			e.mu.Unlock()
			for _, sh := range cs.shards {
				e.startSealer(cs, sh)
			}
		}
	}
	return nil
}

func tombstonesKey(col string, shard int) string {
	return fmt.Sprintf("%s/%d/tombstones.json", col, shard)
}

func writeTombstones(ctx context.Context, be storage.Backend, bucket, col string, shard int, ids []PointID) error {
	data, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("marshal tombstones: %w", err)
	}
	return be.PutObject(ctx, bucket, tombstonesKey(col, shard), bytes.NewReader(data), int64(len(data)), nil)
}

func readTombstones(ctx context.Context, be storage.Backend, bucket, col string, shard int) (map[PointID]struct{}, error) {
	obj, err := be.GetObject(ctx, bucket, tombstonesKey(col, shard))
	if err != nil {
		return map[PointID]struct{}{}, nil
	}
	data, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil {
		return nil, err
	}
	var ids []PointID
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("unmarshal tombstones: %w", err)
	}
	out := make(map[PointID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

func loadSealedSegmentsForShard(ctx context.Context, be storage.Backend, bucket, col string, shard int, metric DistanceMetric) ([]*SealedSegment, error) {
	prefix := fmt.Sprintf("%s/%d/segments/", col, shard)
	result, err := be.ListObjects(ctx, bucket, prefix, "", 10000)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}

	seen := make(map[string]struct{})
	var ids []ulid.ULID
	for _, obj := range result.Contents {
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
