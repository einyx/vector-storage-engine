//go:build vector

package vector

import (
	"time"

	"github.com/oklog/ulid/v2"
)

type DistanceMetric string

const (
	Cosine       DistanceMetric = "cosine"
	L2           DistanceMetric = "l2"
	InnerProduct DistanceMetric = "ip"
)

type ConsistencyLevel string

const (
	Eventual ConsistencyLevel = "eventual"
	Strong   ConsistencyLevel = "strong"
)

type FieldType string

const (
	FieldInt64       FieldType = "int64"
	FieldFloat64     FieldType = "float64"
	FieldString      FieldType = "string"
	FieldBool        FieldType = "bool"
	FieldStringArray FieldType = "string[]"
)

type FieldSchema struct {
	Name    string    `json:"name"`
	Type    FieldType `json:"type"`
	Indexed bool      `json:"indexed"`
}

type Quantization string

const (
	QuantNone Quantization = ""
	QuantInt8 Quantization = "int8"
)

type Collection struct {
	Name        string           `json:"name"`
	Dimensions  int              `json:"dimensions"`
	Distance    DistanceMetric   `json:"distance"`
	Schema      []FieldSchema    `json:"schema"`
	Replication int              `json:"replication"`
	Consistency ConsistencyLevel `json:"consistency"`
	ShardCount  int              `json:"shard_count"`
	// Quantization, if set, controls the on-disk encoding for sealed-segment
	// vectors. Values are dequantized on load — RAM footprint is unchanged
	// in this revision; only object storage and network bytes shrink.
	Quantization Quantization `json:"quantization,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

type PointID = uint64

type Point struct {
	ID      PointID        `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload,omitempty"`
}

type ScoredPoint struct {
	Point
	Score float32 `json:"score"`
}

type SegmentID = ulid.ULID

type SegmentState string

const (
	SegmentGrowing   SegmentState = "growing"
	SegmentSealed    SegmentState = "sealed"
	SegmentIndexed   SegmentState = "indexed"
	SegmentOffloaded SegmentState = "offloaded"
)

type SegmentMeta struct {
	ID           SegmentID      `json:"id"`
	Collection   string         `json:"collection"`
	Shard        int            `json:"shard"`
	State        SegmentState   `json:"state"`
	PointCount   int            `json:"point_count"`
	SizeBytes    int64          `json:"size_bytes"`
	Dimensions   int            `json:"dimensions"`
	Distance     DistanceMetric `json:"distance"`
	Quantization Quantization   `json:"quantization,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	SealedAt     *time.Time     `json:"sealed_at,omitempty"`
}

type FilterOp string

const (
	OpEq       FilterOp = "eq"
	OpNeq      FilterOp = "neq"
	OpGt       FilterOp = "gt"
	OpGte      FilterOp = "gte"
	OpLt       FilterOp = "lt"
	OpLte      FilterOp = "lte"
	OpIn       FilterOp = "in"
	OpContains FilterOp = "contains"
)

type Filter struct {
	And   []Filter `json:"and,omitempty"`
	Or    []Filter `json:"or,omitempty"`
	Not   *Filter  `json:"not,omitempty"`
	Field string   `json:"field,omitempty"`
	Op    FilterOp `json:"op,omitempty"`
	Value any      `json:"value,omitempty"`
}

type SearchRequest struct {
	Collection string    `json:"collection"`
	Vector     []float32 `json:"vector"`
	TopK       int       `json:"top_k"`
	Filter     *Filter   `json:"filter,omitempty"`
	EfSearch   int       `json:"ef_search,omitempty"`
}

type SearchResponse struct {
	Points   []ScoredPoint `json:"points"`
	SearchMs int64         `json:"search_ms"`
}

type DistanceFunc func(a, b []float32) float32

type VectorConfig struct {
	Bucket           string        `mapstructure:"bucket" envconfig:"VECTOR_BUCKET" default:"warp-vectors"`
	SegmentSizeBytes int64         `mapstructure:"segment_size" envconfig:"VECTOR_SEGMENT_SIZE" default:"67108864"`
	WALFlushInterval time.Duration `mapstructure:"wal_flush_interval" envconfig:"VECTOR_WAL_FLUSH_INTERVAL" default:"100ms"`
	WALFlushCount    int           `mapstructure:"wal_flush_count" envconfig:"VECTOR_WAL_FLUSH_COUNT" default:"1000"`
	HNSWm            int           `mapstructure:"hnsw_m" envconfig:"VECTOR_HNSW_M" default:"16"`
	HNSWefConstruct  int           `mapstructure:"hnsw_ef_construction" envconfig:"VECTOR_HNSW_EF_CONSTRUCTION" default:"200"`
	HNSWefSearch     int           `mapstructure:"hnsw_ef_search" envconfig:"VECTOR_HNSW_EF_SEARCH" default:"128"`
	CacheMemoryBytes int64         `mapstructure:"cache_memory_budget" envconfig:"VECTOR_CACHE_MEMORY_BUDGET" default:"1073741824"`
	CacheSSDPath     string        `mapstructure:"cache_ssd_path" envconfig:"VECTOR_CACHE_SSD_PATH" default:""`
	CacheSSDBytes    int64         `mapstructure:"cache_ssd_budget" envconfig:"VECTOR_CACHE_SSD_BUDGET" default:"10737418240"`
	// CompactionMaxSegments: when len(sealed) exceeds this, the sealer merges
	// the two smallest sealed segments into one. 0 disables compaction.
	CompactionMaxSegments int `mapstructure:"compaction_max_segments" envconfig:"VECTOR_COMPACTION_MAX_SEGMENTS" default:"8"`
}
