package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/prysmsh/warp-storage-engine/internal/database"
	"github.com/prysmsh/warp-storage-engine/internal/middleware"
	"github.com/sirupsen/logrus"
)

// BucketMode controls how virtual buckets map to physical storage.
//
//	BucketModeShared (default): every virtual bucket points at the
//	  same physical bucket (e.g. "warehouse") with a per-org prefix.
//	  Cheap on Scaleway (no per-bucket overhead) but couples all
//	  tenants' data into one container.
//
//	BucketModeDedicated: every virtual bucket gets its OWN physical
//	  bucket on the upstream provider — name is deterministic
//	  ("prysm-<orgSlug>-<virtual>"), prefix is empty. Each tenant's
//	  data lives in its own Scaleway bucket; deletion + replication
//	  + lifecycle policies can be configured per virtual bucket.
//	  This is what the prysm-buckets feature uses.
type BucketMode int

const (
	BucketModeShared BucketMode = iota
	BucketModeDedicated
)

// TenantAwareBackend wraps a storage Backend and translates virtual buckets
// to physical bucket+prefix based on the org's bucket mappings.
type TenantAwareBackend struct {
	backend Backend
	db      database.UserStore
	mode    BucketMode
}

// NewTenantAwareBackend creates a tenant-aware storage wrapper.
// Mode is read from WARP_BUCKET_MODE env at construction time:
//
//	"" or "shared"     → BucketModeShared (legacy default; preserves
//	                     existing deployments where bucket_mapping.yaml
//	                     declares the canonical physical bucket list)
//	"dedicated"        → BucketModeDedicated (one Scaleway bucket per
//	                     virtual bucket; the model prysm-buckets uses)
func NewTenantAwareBackend(backend Backend, db database.UserStore) *TenantAwareBackend {
	mode := BucketModeShared
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WARP_BUCKET_MODE"))) {
	case "dedicated", "1to1", "1-to-1":
		mode = BucketModeDedicated
		logrus.Info("tenant storage: dedicated bucket mode (1:1 virtual → physical)")
	default:
		logrus.Info("tenant storage: shared bucket mode (default)")
	}
	return &TenantAwareBackend{
		backend: backend,
		db:      db,
		mode:    mode,
	}
}

// physicalBucketName returns the deterministic physical bucket name
// for a (org, virtual) pair in dedicated mode. The shape is
// "prysm-<orgSlug>-<virtualBucket>" with characters scrubbed to fit
// S3's bucket naming rules (3-63 chars, lowercase, alphanumeric +
// dashes, no leading/trailing dash, no consecutive dashes).
func physicalBucketName(orgSlug, virtual string) string {
	raw := "prysm-" + orgSlug + "-" + virtual
	raw = strings.ToLower(raw)
	raw = sanitizeBucketRegex.ReplaceAllString(raw, "-")
	raw = collapseDashRegex.ReplaceAllString(raw, "-")
	raw = strings.Trim(raw, "-")
	if len(raw) > 63 {
		raw = raw[:63]
		raw = strings.TrimRight(raw, "-")
	}
	if len(raw) < 3 {
		// Pad short names to satisfy the S3 minimum. Practically
		// impossible given our prefix, but defensive.
		raw = raw + "-bkt"
	}
	return raw
}

var (
	sanitizeBucketRegex = regexp.MustCompile(`[^a-z0-9-]`)
	collapseDashRegex   = regexp.MustCompile(`-{2,}`)
)

// resolveMapping looks up the physical bucket and prefix for a virtual bucket
func (t *TenantAwareBackend) resolveMapping(ctx context.Context, virtualBucket string) (physicalBucket, prefix string, err error) {
	orgID := middleware.GetOrgID(ctx)
	if orgID == "" {
		// No org context — pass through directly (backward compatibility)
		return virtualBucket, "", nil
	}

	mapping, err := t.db.GetBucketMapping(orgID, virtualBucket)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve bucket mapping: %w", err)
	}
	if mapping == nil {
		return "", "", fmt.Errorf("bucket %q not found for organization", virtualBucket)
	}

	return mapping.PhysicalBucket, mapping.Prefix, nil
}

// prefixKey prepends the org prefix to a key
func prefixKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	return prefix + key
}

// stripPrefix removes the org prefix from a key
func stripPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	return strings.TrimPrefix(key, prefix)
}

// ListBuckets returns only the virtual buckets belonging to the user's org
func (t *TenantAwareBackend) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	orgID := middleware.GetOrgID(ctx)
	if orgID == "" {
		// No org context — fall back to real backend
		return t.backend.ListBuckets(ctx)
	}

	mappings, err := t.db.GetBucketMappingsByOrgID(orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list bucket mappings: %w", err)
	}

	buckets := make([]BucketInfo, 0, len(mappings))
	for _, m := range mappings {
		buckets = append(buckets, BucketInfo{
			Name:         m.VirtualBucket,
			CreationDate: m.CreatedAt,
		})
	}

	return buckets, nil
}

// CreateBucket provisions a virtual bucket. In shared mode it just
// writes a DB row pointing at a pre-existing physical bucket with a
// per-org prefix. In dedicated mode it ALSO calls the underlying
// backend to create a new physical bucket on Scaleway / S3 / fs.
//
// Failure semantics:
//
//   - Physical create fails  → no DB row written. Caller can retry
//     and the call is naturally idempotent (the underlying backend's
//     CreateBucket no-ops on already-exists; the DB lookup short-
//     circuits before the second create).
//   - Physical create succeeds, DB write fails → orphan physical
//     bucket. We log loudly so an operator can `aws s3 rb` it; we
//     do NOT auto-delete because the user MAY have written to it
//     in the brief window between the two ops, and silent data
//     loss is worse than an orphan with a known name.
func (t *TenantAwareBackend) CreateBucket(ctx context.Context, bucket string) error {
	orgID := middleware.GetOrgID(ctx)
	if orgID == "" {
		return t.backend.CreateBucket(ctx, bucket)
	}

	// Idempotency check: if a mapping already exists, return nil
	// even if the caller is trying to "create" again. This matches
	// S3's PUT-bucket behaviour for already-owned buckets and lets
	// the prysm CLI's `buckets create foo` be replayed safely.
	existing, err := t.db.GetBucketMapping(orgID, bucket)
	if err != nil {
		return fmt.Errorf("failed to check bucket mapping: %w", err)
	}
	if existing != nil {
		return nil
	}

	orgSlug := middleware.GetOrgSlug(ctx)
	if orgSlug == "" {
		org, err := t.db.GetOrgByID(orgID)
		if err != nil || org == nil {
			return fmt.Errorf("failed to resolve org for bucket creation")
		}
		orgSlug = org.Slug
	}

	var (
		physicalBucket string
		prefix         string
	)
	switch t.mode {
	case BucketModeDedicated:
		// 1:1 model — virtual gets its own Scaleway bucket. Name is
		// deterministic so a destroyed-and-recreated mapping reuses
		// the same physical bucket (preserving any data the user
		// uploaded since first create).
		physicalBucket = physicalBucketName(orgSlug, bucket)
		prefix = "" // no per-org prefix needed; bucket itself isolates

		if err := t.backend.CreateBucket(ctx, physicalBucket); err != nil {
			return fmt.Errorf("create physical bucket %q: %w", physicalBucket, err)
		}
		logrus.WithFields(logrus.Fields{
			"org":      orgSlug,
			"virtual":  bucket,
			"physical": physicalBucket,
		}).Info("dedicated bucket created on backend")

	default: // BucketModeShared
		// Legacy: virtual maps to a same-named shared physical
		// bucket (which the operator pre-provisions via config),
		// with the org slug as the key prefix.
		physicalBucket = bucket
		prefix = orgSlug + "/"
	}

	mapping := &database.OrgBucketMapping{
		OrgID:          orgID,
		VirtualBucket:  bucket,
		PhysicalBucket: physicalBucket,
		Prefix:         prefix,
	}

	if err := t.db.CreateBucketMapping(mapping); err != nil {
		// In dedicated mode, the physical bucket exists but we
		// failed to record it. Log so an operator can clean up.
		// We deliberately don't auto-delete: a concurrent writer
		// could have already put data there.
		if t.mode == BucketModeDedicated {
			logrus.WithFields(logrus.Fields{
				"org":      orgSlug,
				"virtual":  bucket,
				"physical": physicalBucket,
				"error":    err,
			}).Error("orphan physical bucket — DB row failed after backend create")
		}
		return fmt.Errorf("record bucket mapping: %w", err)
	}
	return nil
}

// DeleteBucket removes the virtual bucket mapping. In dedicated mode
// it ALSO deletes the underlying physical bucket — matching what the
// operator would expect from `prysm buckets rm` ("destroy the bucket
// and its contents"). The backend's DeleteBucket is responsible for
// the empty-vs-non-empty semantics; for filesystem and S3 backends
// alike, deleting a non-empty bucket fails by default.
func (t *TenantAwareBackend) DeleteBucket(ctx context.Context, bucket string) error {
	orgID := middleware.GetOrgID(ctx)
	if orgID == "" {
		return t.backend.DeleteBucket(ctx, bucket)
	}

	mapping, err := t.db.GetBucketMapping(orgID, bucket)
	if err != nil {
		return fmt.Errorf("failed to look up mapping: %w", err)
	}
	if mapping == nil {
		// Nothing to delete; treat as success (idempotent) so a
		// retried delete after a partial failure doesn't error.
		return nil
	}

	// Drop the DB row first. If the backend delete then fails, we
	// have an orphan physical bucket but no virtual reference to
	// it — easier for an operator to GC than the reverse (DB row
	// pointing at a deleted physical bucket = phantom error on
	// every read).
	if err := t.db.DeleteBucketMapping(orgID, bucket); err != nil {
		return fmt.Errorf("delete mapping: %w", err)
	}

	// Only delete the physical bucket if we own it (dedicated mode).
	// In shared mode the physical bucket is operator-managed and
	// holds other tenants' prefixes — must not touch.
	if t.mode == BucketModeDedicated {
		if err := t.backend.DeleteBucket(ctx, mapping.PhysicalBucket); err != nil {
			logrus.WithFields(logrus.Fields{
				"physical": mapping.PhysicalBucket,
				"error":    err,
			}).Warn("dedicated bucket: backend delete failed (mapping already removed)")
			return fmt.Errorf("delete physical bucket %q: %w", mapping.PhysicalBucket, err)
		}
	}

	return nil
}

// BucketExists checks if the virtual bucket mapping exists
func (t *TenantAwareBackend) BucketExists(ctx context.Context, bucket string) (bool, error) {
	orgID := middleware.GetOrgID(ctx)
	if orgID == "" {
		return t.backend.BucketExists(ctx, bucket)
	}

	mapping, err := t.db.GetBucketMapping(orgID, bucket)
	if err != nil {
		return false, err
	}
	return mapping != nil, nil
}

// ListObjects lists objects under the virtual bucket with prefix translation
func (t *TenantAwareBackend) ListObjects(ctx context.Context, bucket, reqPrefix, marker string, maxKeys int) (*ListObjectsResult, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}

	fullPrefix := prefixKey(orgPrefix, reqPrefix)
	fullMarker := marker
	if marker != "" && orgPrefix != "" {
		fullMarker = prefixKey(orgPrefix, marker)
	}

	result, err := t.backend.ListObjects(ctx, physBucket, fullPrefix, fullMarker, maxKeys)
	if err != nil {
		return nil, err
	}

	// Strip prefix from returned keys
	if orgPrefix != "" {
		for i := range result.Contents {
			result.Contents[i].Key = stripPrefix(orgPrefix, result.Contents[i].Key)
		}
		if result.NextMarker != "" {
			result.NextMarker = stripPrefix(orgPrefix, result.NextMarker)
		}
	}

	return result, nil
}

// ListObjectsWithDelimiter lists objects with delimiter and prefix translation
func (t *TenantAwareBackend) ListObjectsWithDelimiter(ctx context.Context, bucket, reqPrefix, marker, delimiter string, maxKeys int) (*ListObjectsResult, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}

	fullPrefix := prefixKey(orgPrefix, reqPrefix)
	fullMarker := marker
	if marker != "" && orgPrefix != "" {
		fullMarker = prefixKey(orgPrefix, marker)
	}

	result, err := t.backend.ListObjectsWithDelimiter(ctx, physBucket, fullPrefix, fullMarker, delimiter, maxKeys)
	if err != nil {
		return nil, err
	}

	if orgPrefix != "" {
		for i := range result.Contents {
			result.Contents[i].Key = stripPrefix(orgPrefix, result.Contents[i].Key)
		}
		for i := range result.CommonPrefixes {
			result.CommonPrefixes[i] = stripPrefix(orgPrefix, result.CommonPrefixes[i])
		}
		if result.NextMarker != "" {
			result.NextMarker = stripPrefix(orgPrefix, result.NextMarker)
		}
	}

	return result, nil
}

// ListDeletedObjects lists deleted objects with prefix translation
func (t *TenantAwareBackend) ListDeletedObjects(ctx context.Context, bucket, reqPrefix, marker string, maxKeys int) (*ListObjectsResult, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}

	fullPrefix := prefixKey(orgPrefix, reqPrefix)
	fullMarker := marker
	if marker != "" && orgPrefix != "" {
		fullMarker = prefixKey(orgPrefix, marker)
	}

	result, err := t.backend.ListDeletedObjects(ctx, physBucket, fullPrefix, fullMarker, maxKeys)
	if err != nil {
		return nil, err
	}

	if orgPrefix != "" {
		for i := range result.Contents {
			result.Contents[i].Key = stripPrefix(orgPrefix, result.Contents[i].Key)
		}
		if result.NextMarker != "" {
			result.NextMarker = stripPrefix(orgPrefix, result.NextMarker)
		}
	}

	return result, nil
}

// GetObject retrieves an object with key prefix translation
func (t *TenantAwareBackend) GetObject(ctx context.Context, bucket, key string) (*Object, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}
	return t.backend.GetObject(ctx, physBucket, prefixKey(orgPrefix, key))
}

// PutObject stores an object with key prefix translation
func (t *TenantAwareBackend) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, metadata map[string]string) error {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{
		"virtual_bucket":  bucket,
		"physical_bucket": physBucket,
		"virtual_key":     key,
		"physical_key":    prefixKey(orgPrefix, key),
	}).Debug("Tenant storage: PutObject")

	return t.backend.PutObject(ctx, physBucket, prefixKey(orgPrefix, key), reader, size, metadata)
}

// DeleteObject deletes an object with key prefix translation
func (t *TenantAwareBackend) DeleteObject(ctx context.Context, bucket, key string) error {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return err
	}
	return t.backend.DeleteObject(ctx, physBucket, prefixKey(orgPrefix, key))
}

// RestoreObject restores an object with key prefix translation
func (t *TenantAwareBackend) RestoreObject(ctx context.Context, bucket, key, versionID string) error {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return err
	}
	return t.backend.RestoreObject(ctx, physBucket, prefixKey(orgPrefix, key), versionID)
}

// HeadObject retrieves object metadata with key prefix translation
func (t *TenantAwareBackend) HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}
	info, err := t.backend.HeadObject(ctx, physBucket, prefixKey(orgPrefix, key))
	if err != nil {
		return nil, err
	}
	if info != nil && orgPrefix != "" {
		info.Key = stripPrefix(orgPrefix, info.Key)
	}
	return info, nil
}

// GetObjectACL gets ACL with prefix translation
func (t *TenantAwareBackend) GetObjectACL(ctx context.Context, bucket, key string) (*ACL, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}
	return t.backend.GetObjectACL(ctx, physBucket, prefixKey(orgPrefix, key))
}

// PutObjectACL sets ACL with prefix translation
func (t *TenantAwareBackend) PutObjectACL(ctx context.Context, bucket, key string, acl *ACL) error {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return err
	}
	return t.backend.PutObjectACL(ctx, physBucket, prefixKey(orgPrefix, key), acl)
}

// InitiateMultipartUpload initiates multipart upload with key prefix translation
func (t *TenantAwareBackend) InitiateMultipartUpload(ctx context.Context, bucket, key string, metadata map[string]string) (string, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return "", err
	}
	return t.backend.InitiateMultipartUpload(ctx, physBucket, prefixKey(orgPrefix, key), metadata)
}

// UploadPart uploads a part with key prefix translation
func (t *TenantAwareBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return "", err
	}
	return t.backend.UploadPart(ctx, physBucket, prefixKey(orgPrefix, key), uploadID, partNumber, reader, size)
}

// CompleteMultipartUpload completes multipart upload with key prefix translation
func (t *TenantAwareBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) error {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return err
	}
	return t.backend.CompleteMultipartUpload(ctx, physBucket, prefixKey(orgPrefix, key), uploadID, parts)
}

// AbortMultipartUpload aborts multipart upload with key prefix translation
func (t *TenantAwareBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return err
	}
	return t.backend.AbortMultipartUpload(ctx, physBucket, prefixKey(orgPrefix, key), uploadID)
}

// ListParts lists parts with key prefix translation
func (t *TenantAwareBackend) ListParts(ctx context.Context, bucket, key, uploadID string, maxParts int, partNumberMarker int) (*ListPartsResult, error) {
	physBucket, orgPrefix, err := t.resolveMapping(ctx, bucket)
	if err != nil {
		return nil, err
	}
	return t.backend.ListParts(ctx, physBucket, prefixKey(orgPrefix, key), uploadID, maxParts, partNumberMarker)
}

// Ensure TenantAwareBackend implements Backend
var _ Backend = (*TenantAwareBackend)(nil)
