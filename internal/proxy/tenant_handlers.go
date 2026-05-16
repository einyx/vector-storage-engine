package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/prysmsh/warp-storage-engine/internal/auth"
	"github.com/prysmsh/warp-storage-engine/internal/database"
	"github.com/prysmsh/warp-storage-engine/internal/middleware"
	"github.com/prysmsh/warp-storage-engine/internal/storage"
)

// TenantHandlers provides HTTP handlers for org/user/bucket management
type TenantHandlers struct {
	db            database.UserStore
	vaultProvider *auth.VaultMultiUserProvider
	defaultBucket string
	// storage is the underlying backend used for dedicated-mode
	// bucket creation/deletion. Optional: when nil, the handler
	// behaves in shared mode regardless of WARP_BUCKET_MODE.
	// Wired in NewTenantHandlersWithStorage; the legacy
	// NewTenantHandlers is preserved for backward compatibility.
	storage storage.Backend
	// dedicatedMode mirrors the WARP_BUCKET_MODE env so the
	// handler can derive a deterministic physical name when the
	// caller doesn't provide one.
	dedicatedMode bool
}

// NewTenantHandlers creates a new TenantHandlers instance.
// Backward-compatible signature: shared mode only (no physical
// bucket creation). For dedicated-mode support call
// NewTenantHandlersWithStorage instead.
func NewTenantHandlers(db database.UserStore, vaultProvider *auth.VaultMultiUserProvider, defaultBucket string) *TenantHandlers {
	return &TenantHandlers{
		db:            db,
		vaultProvider: vaultProvider,
		defaultBucket: defaultBucket,
	}
}

// NewTenantHandlersWithStorage wires the backend so dedicated-mode
// CreateBucketMappingHandler can provision a real Scaleway/S3
// bucket per virtual name.
func NewTenantHandlersWithStorage(db database.UserStore, vaultProvider *auth.VaultMultiUserProvider, defaultBucket string, backend storage.Backend, dedicated bool) *TenantHandlers {
	return &TenantHandlers{
		db:            db,
		vaultProvider: vaultProvider,
		defaultBucket: defaultBucket,
		storage:       backend,
		dedicatedMode: dedicated,
	}
}

var slugRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// resolveOrCreateOrg returns the org for slug, creating it on demand
// when the caller is admin-trusted (i.e. came in via the service-
// token bypass with is_admin=true). This is what makes the proxy
// from prysm-backend "just work" the first time a user creates a
// bucket — no separate "register your org with warp" step.
//
// For non-admin callers we preserve the historic 404 behaviour so
// random AWS-V4-signed users can't auto-mint org rows by guessing
// slugs.
func (h *TenantHandlers) resolveOrCreateOrg(r *http.Request, slug string) (*database.Organization, error) {
	org, err := h.db.GetOrgBySlug(slug)
	if err != nil {
		return nil, err
	}
	if org != nil {
		return org, nil
	}
	if !isRequestAdmin(r) {
		return nil, nil
	}
	if !isValidSlug(slug) {
		// Defence-in-depth: even admin can't mint malformed slugs
		// because they'd corrupt the URL surface.
		return nil, fmt.Errorf("invalid slug %q", slug)
	}
	newOrg := &database.Organization{
		Name: slug, // operator can rename via UI later
		Slug: slug,
		// settings is JSONB in postgres — empty string fails the
		// jsonb cast with "invalid input syntax for type json".
		// Default to {} so an unconfigured org is still valid JSON.
		Settings: "{}",
	}
	if err := h.db.CreateOrganization(newOrg); err != nil {
		// Race: a parallel request may have created it. Re-fetch
		// and accept whatever the canonical row is.
		existing, gerr := h.db.GetOrgBySlug(slug)
		if gerr == nil && existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("auto-create org %q: %w", slug, err)
	}
	logrus.WithField("slug", slug).Info("auto-created org for admin-trusted request")
	return newOrg, nil
}

func isValidSlug(s string) bool {
	if len(s) < 2 || len(s) > 100 {
		return false
	}
	return slugRegex.MatchString(s)
}

// CreateOrgHandler creates a new organization
// POST /api/orgs
func (h *TenantHandlers) CreateOrgHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.Name == "" || req.Slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and slug are required"})
		return
	}

	if !isValidSlug(req.Slug) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slug must be lowercase alphanumeric with hyphens, 2-100 chars"})
		return
	}

	// Check if slug is taken
	existing, err := h.db.GetOrgBySlug(req.Slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to check slug"})
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Organization slug already exists"})
		return
	}

	org := &database.Organization{
		Name:     req.Name,
		Slug:     req.Slug,
		Active:   true,
		Settings: "{}",
	}

	if err := h.db.CreateOrganization(org); err != nil {
		logrus.WithError(err).Error("Failed to create organization")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create organization"})
		return
	}

	logrus.WithFields(logrus.Fields{
		"org_id":   org.ID,
		"org_slug": org.Slug,
	}).Info("Organization created")

	writeJSON(w, http.StatusCreated, org)
}

// GetOrgHandler returns organization info
// GET /api/orgs/{slug}
func (h *TenantHandlers) GetOrgHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]

	org, err := h.db.GetOrgBySlug(slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to fetch organization"})
		return
	}
	if org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	// Check org membership (unless global admin)
	if !isRequestAdmin(r) {
		ctxOrgID := middleware.GetOrgID(r.Context())
		if ctxOrgID != org.ID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Access denied"})
			return
		}
	}

	writeJSON(w, http.StatusOK, org)
}

// AddUserToOrgHandler creates a user and adds them to an org
// POST /api/orgs/{slug}/users
func (h *TenantHandlers) AddUserToOrgHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]

	org, err := h.db.GetOrgBySlug(slug)
	if err != nil || org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	// Must be org admin or global admin
	if !h.isOrgAdmin(r, org.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Org admin access required"})
		return
	}

	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
		return
	}

	role := req.Role
	if role == "" {
		role = "member"
	}
	if role != "admin" && role != "member" && role != "readonly" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be admin, member, or readonly"})
		return
	}

	// Generate access key and secret key
	accessKey, err := generateAccessKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to generate access key"})
		return
	}

	secretKey, err := generateSecretKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to generate secret key"})
		return
	}

	// Store secret in Vault
	if h.vaultProvider != nil {
		if err := h.vaultProvider.StoreCredential(accessKey, secretKey, org.ID); err != nil {
			logrus.WithError(err).Error("Failed to store credential in Vault")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to store credential in Vault"})
			return
		}
	}

	// Store user metadata in DB (secret key stored as placeholder since real secret is in Vault)
	user := &database.User{
		Email:     req.Email,
		AccessKey: accessKey,
		SecretKey: "vault-managed", // Plaintext secret is only in Vault
		Active:    true,
		OrgID:     &org.ID,
		Role:      role,
	}

	if err := h.db.CreateUser(user); err != nil {
		logrus.WithError(err).Error("Failed to create user")
		// Try to clean up Vault credential
		if h.vaultProvider != nil {
			_ = h.vaultProvider.DeleteCredential(accessKey)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create user"})
		return
	}

	logrus.WithFields(logrus.Fields{
		"user_id":    user.ID,
		"org_id":     org.ID,
		"access_key": accessKey,
		"role":       role,
	}).Info("User created for organization")

	// Return credentials (only time secret is shown)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"access_key": accessKey,
		"secret_key": secretKey,
		"email":      req.Email,
		"role":       role,
		"org_slug":   org.Slug,
	})
}

// ListOrgUsersHandler lists users in an organization
// GET /api/orgs/{slug}/users
func (h *TenantHandlers) ListOrgUsersHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]

	org, err := h.db.GetOrgBySlug(slug)
	if err != nil || org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	if !h.isOrgAdmin(r, org.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Org admin access required"})
		return
	}

	users, err := h.db.GetUsersByOrgID(org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list users"})
		return
	}

	// Strip secret keys from response
	type safeUser struct {
		ID        int        `json:"id"`
		AccessKey string     `json:"access_key"`
		Email     string     `json:"email"`
		Role      string     `json:"role"`
		Active    bool       `json:"active"`
		CreatedAt time.Time  `json:"created_at"`
		LastLogin *time.Time `json:"last_login,omitempty"`
	}

	safeUsers := make([]safeUser, 0, len(users))
	for _, u := range users {
		safeUsers = append(safeUsers, safeUser{
			ID:        u.ID,
			AccessKey: u.AccessKey,
			Email:     u.Email,
			Role:      u.Role,
			Active:    u.Active,
			CreatedAt: u.CreatedAt,
			LastLogin: u.LastLogin,
		})
	}

	writeJSON(w, http.StatusOK, safeUsers)
}

// RemoveUserFromOrgHandler removes a user from an organization
// DELETE /api/orgs/{slug}/users/{id}
func (h *TenantHandlers) RemoveUserFromOrgHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]
	userIDStr := mux.Vars(r)["id"]

	org, err := h.db.GetOrgBySlug(slug)
	if err != nil || org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	if !h.isOrgAdmin(r, org.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Org admin access required"})
		return
	}

	var userID int
	if _, err := fmt.Sscanf(userIDStr, "%d", &userID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid user ID"})
		return
	}

	if err := h.db.DeleteUserByID(userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to remove user"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "User removed"})
}

// CreateBucketMappingHandler creates a virtual bucket mapping
// POST /api/orgs/{slug}/buckets
func (h *TenantHandlers) CreateBucketMappingHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]

	org, err := h.resolveOrCreateOrg(r, slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	if !h.isOrgAdmin(r, org.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Org admin access required"})
		return
	}

	var req struct {
		VirtualBucket  string `json:"virtual_bucket"`
		PhysicalBucket string `json:"physical_bucket"`
		Prefix         string `json:"prefix"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.VirtualBucket == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "virtual_bucket is required"})
		return
	}

	// Resolve physical name + prefix per current mode.
	//
	// Dedicated mode: ignore caller-supplied physical/prefix —
	// the server picks a deterministic name and provisions a fresh
	// physical bucket on the storage backend. This is what
	// prysm-buckets uses for the 1:1 model.
	//
	// Shared mode: honor the caller's hint (or default to the
	// shared physical bucket); namespace via per-org prefix.
	var (
		physicalBucket string
		prefix         string
	)
	if h.dedicatedMode && h.storage != nil {
		physicalBucket = derivedPhysicalBucketName(slug, req.VirtualBucket)
		prefix = ""
		// Provision the underlying bucket FIRST. Failure here
		// means no DB row written, which keeps the caller's
		// retry idempotent.
		if err := h.storage.CreateBucket(r.Context(), physicalBucket); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"org":      slug,
				"virtual":  req.VirtualBucket,
				"physical": physicalBucket,
			}).Error("dedicated bucket: backend create failed")
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "Failed to provision physical bucket: " + err.Error(),
			})
			return
		}
	} else {
		physicalBucket = req.PhysicalBucket
		if physicalBucket == "" {
			physicalBucket = h.defaultBucket
		}
		prefix = req.Prefix
		if prefix == "" {
			prefix = slug + "/"
		}
	}

	mapping := &database.OrgBucketMapping{
		OrgID:          org.ID,
		VirtualBucket:  req.VirtualBucket,
		PhysicalBucket: physicalBucket,
		Prefix:         prefix,
	}

	if err := h.db.CreateBucketMapping(mapping); err != nil {
		logrus.WithError(err).Error("Failed to create bucket mapping")
		// Orphan-bucket recovery: in dedicated mode the physical
		// bucket exists but no DB row points at it. Logged loudly
		// above (in the storage layer's Error path); operator GC
		// is the recovery vehicle. Don't auto-delete here — a
		// concurrent writer may have already touched it.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create bucket mapping"})
		return
	}

	logrus.WithFields(logrus.Fields{
		"org_id":          org.ID,
		"virtual_bucket":  req.VirtualBucket,
		"physical_bucket": physicalBucket,
		"prefix":          prefix,
		"mode":            modeName(h.dedicatedMode),
	}).Info("Bucket mapping created")

	writeJSON(w, http.StatusCreated, mapping)
}

func modeName(dedicated bool) string {
	if dedicated {
		return "dedicated"
	}
	return "shared"
}

// derivedPhysicalBucketName mirrors the storage-layer helper of the
// same intent. Duplicated here (rather than imported) to keep
// proxy → storage as a one-way dependency. If we ever extract a
// shared util package the duplication should collapse.
func derivedPhysicalBucketName(orgSlug, virtual string) string {
	raw := strings.ToLower("prysm-" + orgSlug + "-" + virtual)
	out := make([]byte, 0, len(raw))
	prevDash := false
	for _, r := range raw {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, byte(r))
			prevDash = false
		default:
			if !prevDash {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(string(out), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if len(s) < 3 {
		s = s + "-bkt"
	}
	return s
}

// ListBucketMappingsHandler lists bucket mappings for an org
// GET /api/orgs/{slug}/buckets
func (h *TenantHandlers) ListBucketMappingsHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]

	// Auto-create on admin reads too — first-time visitors to the
	// dashboard's Storage tab hit GET /buckets before they've ever
	// created one. Returning a 404 there would surface as the
	// confusing "Couldn't load buckets" error in the UI; lazy-
	// creating the org row makes the empty state render cleanly.
	org, err := h.resolveOrCreateOrg(r, slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	// Members can list buckets, not just admins
	if !isRequestAdmin(r) {
		ctxOrgID := middleware.GetOrgID(r.Context())
		if ctxOrgID != org.ID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Access denied"})
			return
		}
	}

	mappings, err := h.db.GetBucketMappingsByOrgID(org.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list bucket mappings"})
		return
	}

	writeJSON(w, http.StatusOK, mappings)
}

// DeleteBucketMappingHandler deletes a virtual bucket mapping
// DELETE /api/orgs/{slug}/buckets/{name}
func (h *TenantHandlers) DeleteBucketMappingHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]
	bucketName := mux.Vars(r)["name"]

	// Plain GetOrgBySlug here — we don't auto-create on delete
	// because "delete a bucket from a not-yet-provisioned org" is
	// almost certainly a typo or a stale URL, and silently
	// succeeding would be misleading.
	org, err := h.db.GetOrgBySlug(slug)
	if err != nil || org == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}

	if !h.isOrgAdmin(r, org.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Org admin access required"})
		return
	}

	// Look up the mapping BEFORE deleting so we know the physical
	// bucket to clean up (in dedicated mode). If the lookup fails or
	// returns nothing, treat as success — idempotent delete is the
	// cleanest UX for retried calls.
	mapping, err := h.db.GetBucketMapping(org.ID, bucketName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to look up bucket mapping"})
		return
	}
	if mapping == nil {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Bucket mapping deleted"})
		return
	}

	// DB row first, physical bucket second. If the backend delete
	// fails afterwards we end up with a phantom physical bucket
	// (no DB pointer) that an operator can GC; the reverse order
	// would leave a DB row pointing at a deleted bucket which
	// looks like silent corruption to the caller.
	if err := h.db.DeleteBucketMapping(org.ID, bucketName); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to delete bucket mapping"})
		return
	}

	if h.dedicatedMode && h.storage != nil {
		if err := h.storage.DeleteBucket(r.Context(), mapping.PhysicalBucket); err != nil {
			// Mapping is already gone — log and return a soft
			// error so the caller knows the cleanup is partial.
			// The dedicated bucket is named deterministically;
			// the operator can locate and delete by hand using
			// the prysm-<orgSlug>-<virtual> convention.
			logrus.WithError(err).WithFields(logrus.Fields{
				"physical": mapping.PhysicalBucket,
				"org":      slug,
				"virtual":  bucketName,
			}).Warn("dedicated bucket: backend delete failed (mapping already removed)")
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "Mapping deleted but physical bucket cleanup failed: " + err.Error(),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Bucket mapping deleted"})
}

// isOrgAdmin checks if the requesting user is an admin of the specified org
func (h *TenantHandlers) isOrgAdmin(r *http.Request, orgID string) bool {
	if isRequestAdmin(r) {
		return true
	}

	ctxOrgID := middleware.GetOrgID(r.Context())
	ctxRole := middleware.GetUserRole(r.Context())

	return ctxOrgID == orgID && ctxRole == "admin"
}

// isRequestAdmin checks if the request has global admin privileges
func isRequestAdmin(r *http.Request) bool {
	if v := r.Context().Value("is_admin"); v != nil {
		if admin, ok := v.(bool); ok {
			return admin
		}
	}
	return false
}

// writeJSON writes a JSON response
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logrus.WithError(err).Error("Failed to encode JSON response")
	}
}

// generateAccessKey creates a random AWS-style access key
func generateAccessKey() (string, error) {
	bytes := make([]byte, 20)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	key := base64.URLEncoding.EncodeToString(bytes)
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ToUpper(key)
	if len(key) > 20 {
		key = key[:20]
	}
	return key, nil
}

// generateSecretKey creates a random 40-character secret key
func generateSecretKey() (string, error) {
	bytes := make([]byte, 30)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	key := base64.URLEncoding.EncodeToString(bytes)
	if len(key) > 40 {
		key = key[:40]
	}
	return key, nil
}
