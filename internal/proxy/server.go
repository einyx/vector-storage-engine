package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/prysmsh/warp-storage-engine/internal/auth"
	"github.com/prysmsh/warp-storage-engine/internal/cache"
	"github.com/prysmsh/warp-storage-engine/internal/config"
	"github.com/prysmsh/warp-storage-engine/internal/database"
	"github.com/prysmsh/warp-storage-engine/internal/encryption"
	"github.com/prysmsh/warp-storage-engine/internal/metrics"
	"github.com/prysmsh/warp-storage-engine/internal/middleware"
	"github.com/prysmsh/warp-storage-engine/internal/storage"
	"github.com/prysmsh/warp-storage-engine/internal/virustotal"
	"github.com/prysmsh/warp-storage-engine/pkg/s3"
)

const (
	// Cache configuration constants
	defaultMaxMemory     = 1024 * 1024 * 1024 // 1GB
	defaultMaxObjectSize = 10 * 1024 * 1024   // 10MB
	defaultCacheTTL      = 5 * time.Minute
)

// responseRecorder captures HTTP response for debugging
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       *strings.Builder
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	// Don't write header yet - we'll do it manually later
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	// Don't write to the underlying ResponseWriter here - we'll do it manually later
	return len(b), nil
}

type Server struct {
	config           *config.Config
	storage          storage.Backend
	auth             auth.Provider
	router           *mux.Router
	s3Handler        *s3.Handler
	metrics          *metrics.Metrics
	auth0            *Auth0Handler
	authManager      *AuthenticationManager
	shareLinkHandler *ShareLinkHandler
	tenantHandlers   *TenantHandlers
	db               *database.DB // Database connection for auth
	scanner          *virustotal.Scanner
	shuttingDown     int32 // atomic flag for shutdown state
}

// NewServer initializes proxy server with configured storage backend
func NewServer(cfg *config.Config) (*Server, error) {
	storageBackend, err := storage.NewBackend(cfg.Storage)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage backend: %w", err)
	}

	// Wrap with caching if enabled
	if cacheEnabled := os.Getenv("ENABLE_OBJECT_CACHE"); cacheEnabled == "true" {
		maxMemory := int64(defaultMaxMemory)
		if envMem := os.Getenv("CACHE_MAX_MEMORY"); envMem != "" {
			if parsed, parseErr := strconv.ParseInt(envMem, 10, 64); parseErr == nil {
				maxMemory = parsed
			}
		}

		maxObjectSize := int64(defaultMaxObjectSize)
		if envSize := os.Getenv("CACHE_MAX_OBJECT_SIZE"); envSize != "" {
			if parsed, parseErr := strconv.ParseInt(envSize, 10, 64); parseErr == nil {
				maxObjectSize = parsed
			}
		}

		ttl := defaultCacheTTL
		if envTTL := os.Getenv("CACHE_TTL"); envTTL != "" {
			if parsed, parseErr := time.ParseDuration(envTTL); parseErr == nil {
				ttl = parsed
			}
		}

		objectCache, cacheErr := cache.NewObjectCache(maxMemory, maxObjectSize, ttl)
		if cacheErr != nil {
			logrus.WithError(cacheErr).Warn("Failed to create object cache, continuing without cache")
		} else {
			logrus.WithFields(logrus.Fields{
				"maxMemory":     maxMemory,
				"maxObjectSize": maxObjectSize,
				"ttl":           ttl,
			}).Info("Object caching enabled")
			storageBackend = cache.NewCachingBackend(storageBackend, objectCache)
		}
	}

	// Build the metrics handle before encryption so KMS / encryption layers can
	// record into the shared registry. NewMetrics is a sync.Once singleton, so
	// reusing this handle below is fine.
	metricsHandle := metrics.NewMetrics("foundation_storage_engine")

	// Wrap with encryption if enabled. Encryption is the outermost storage
	// layer so cache (if present) holds ciphertext, not plaintext.
	if cfg.Encryption.Enabled {
		ctx := context.Background()
		encManager, err := encryption.NewFromConfig(ctx, &cfg.Encryption, metricsHandle)
		if err != nil {
			return nil, fmt.Errorf("failed to create encryption manager: %w", err)
		}
		if err := validateEncryptionAtStartup(ctx, encManager); err != nil {
			return nil, fmt.Errorf("encryption startup validation failed: %w", err)
		}
		storageBackend = storage.NewEncryptedBackend(storageBackend, encManager)
		logrus.WithField("algorithm", cfg.Encryption.Algorithm).
			WithField("key_provider", cfg.Encryption.KeyProvider).
			Info("Storage encryption enabled")
	}

	// Create auth provider based on configuration
	var authProvider auth.Provider
	var db *database.DB

	if cfg.Auth.Type == "database" && cfg.Database.Enabled {
		// Initialize database connection for authentication
		dbConfig := database.Config{
			Driver:           cfg.Database.Driver,
			ConnectionString: cfg.Database.ConnectionString,
			MaxOpenConns:     cfg.Database.MaxOpenConns,
			MaxIdleConns:     cfg.Database.MaxIdleConns,
			ConnMaxLifetime:  cfg.Database.ConnMaxLifetime,
		}

		var err error
		db, err = database.NewConnection(dbConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create database connection: %w", err)
		}

		baseProvider := auth.NewDatabaseProvider(db)
		
		// Wrap with OPA if enabled
		if cfg.OPA.Enabled {
			authProvider = auth.NewOPAProvider(cfg.Auth, cfg.OPA, baseProvider)
			logrus.WithField("opa_url", cfg.OPA.URL).Info("Database authentication provider initialized with OPA authorization")
		} else {
			authProvider = baseProvider
			logrus.Info("Database authentication provider initialized")
		}
	} else if cfg.Auth.Type == "vault_multiuser" && cfg.Multitenancy.Enabled {
		// Multi-tenant Vault provider with dedicated config
		vaultProvider, err := auth.NewVaultMultiUserProviderWithConfig(cfg.Auth, cfg.Multitenancy)
		if err != nil {
			return nil, fmt.Errorf("failed to create vault multi-user provider: %w", err)
		}
		authProvider = vaultProvider
		logrus.Info("Vault multi-user authentication provider initialized")
	} else {
		authProvider, err = auth.NewProviderWithOPA(cfg.Auth, cfg.OPA)
		if err != nil {
			return nil, fmt.Errorf("failed to create auth provider: %w", err)
		}

		if cfg.OPA.Enabled {
			logrus.WithField("opa_url", cfg.OPA.URL).Info("Authentication provider initialized with OPA authorization")
		} else {
			logrus.Info("Authentication provider initialized")
		}
	}


	s := &Server{
		config:  cfg,
		storage: storageBackend,
		auth:    authProvider,
		router:  mux.NewRouter(),
		metrics: metricsHandle,
		db:      db,
	}

	// Initialize Auth0 if enabled
	if cfg.Auth0.Enabled {
		s.auth0 = NewAuth0Handler(&cfg.Auth0)
	}

	// Initialize Authentication Manager
	s.authManager = NewAuthenticationManager(cfg, s.auth0, s.auth)

	// Initialize VirusTotal scanner
	scanner, err := virustotal.NewScanner(&cfg.VirusTotal)
	if err != nil {
		return nil, fmt.Errorf("failed to create VirusTotal scanner: %w", err)
	}
	s.scanner = scanner

	if scanner.IsEnabled() {
		logrus.Info("VirusTotal scanning enabled")
	}

	// Initialize multi-tenancy if enabled
	if cfg.Multitenancy.Enabled {
		if s.db == nil {
			// Multi-tenancy requires a database connection
			dbConfig := database.Config{
				Driver:           cfg.Database.Driver,
				ConnectionString: cfg.Database.ConnectionString,
				MaxOpenConns:     cfg.Database.MaxOpenConns,
				MaxIdleConns:     cfg.Database.MaxIdleConns,
				ConnMaxLifetime:  cfg.Database.ConnMaxLifetime,
			}
			s.db, err = database.NewConnection(dbConfig)
			if err != nil {
				return nil, fmt.Errorf("failed to create database connection for multitenancy: %w", err)
			}
		}

		// Decide the bucket mode once, here, so both the
		// TenantAwareBackend wrapper and the tenant HTTP handlers
		// agree. WARP_BUCKET_MODE=dedicated flips the 1:1 path
		// (one physical Scaleway bucket per virtual). Anything
		// else stays in shared mode for backwards compatibility
		// with deployments that pre-declare physical buckets in
		// config.yaml's bucket_mapping block.
		dedicatedBucketMode := false
		switch strings.ToLower(strings.TrimSpace(os.Getenv("WARP_BUCKET_MODE"))) {
		case "dedicated", "1to1", "1-to-1":
			dedicatedBucketMode = true
		}

		// If auth type is vault_multiuser, set up tenant handlers with vault provider
		var vp *auth.VaultMultiUserProvider
		if vaultProvider, ok := s.auth.(*auth.VaultMultiUserProvider); ok {
			vp = vaultProvider
		}
		s.tenantHandlers = NewTenantHandlersWithStorage(
			s.db, vp, cfg.Multitenancy.DefaultPhysicalBucket,
			s.storage, dedicatedBucketMode,
		)

		// Wrap storage backend with tenant-aware layer
		s.storage = storage.NewTenantAwareBackend(s.storage, s.db)
		logrus.Info("Multi-tenancy initialized with tenant-aware storage")
	}

	s.s3Handler = s3.NewHandler(s.storage, s.auth, cfg.S3, cfg.Chunking)
	s.s3Handler.SetScanner(s.scanner)

	// Initialize share link handler
	s.shareLinkHandler = NewShareLinkHandler(s.storage, s.s3Handler)

	s.setupRoutes()

	// Apply middleware to all routes. Order matters: request-logger
	// runs first so every request shows up in logs even if the
	// service-token bypass elevates it; the bypass runs BEFORE the
	// metrics middleware so admin-elevated requests still get
	// counted in /metrics; auth providers (registered downstream
	// inside per-route handlers) run after this and respect the
	// `authenticated` flag the bypass sets.
	s.router.Use(middleware.RequestLogger(middleware.DefaultLoggerConfig()))
	s.router.Use(middleware.ServiceTokenMiddleware())
	s.router.Use(s.metrics.Middleware())

	// Apply Sentry middleware if enabled
	if s.config.Sentry.Enabled {
		s.router.Use(middleware.SentryRecoveryMiddleware())
		s.router.Use(middleware.SentryMiddleware(false))
		logrus.Info("Sentry middleware enabled")
	}

	return s, nil
}

// Storage returns the configured storage backend. Exposed so auxiliary
// listeners (e.g. the OCI Distribution frontend in cmd/…/main.go) can share
// the same backend and any tenant-aware wrapping.
func (s *Server) Storage() storage.Backend {
	return s.storage
}

// ServeHTTP handles incoming requests with security headers and preprocessing
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Add security headers to all responses
	s.setSecurityHeaders(w)

	// Preprocess request to fix mc client issues
	userAgent := r.Header.Get("User-Agent")
	if strings.Contains(strings.ToLower(userAgent), "minio") || strings.Contains(strings.ToLower(userAgent), "mc") {
		// Try to fix authorization header before routing
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			cleanedHeader := strings.ReplaceAll(authHeader, "\n", "")
			cleanedHeader = strings.ReplaceAll(cleanedHeader, "\r", "")
			if cleanedHeader != authHeader {
				r.Header.Set("Authorization", cleanedHeader)
			}
		}
	}

	s.router.ServeHTTP(w, r)
}

// setSecurityHeaders applies security headers
func (s *Server) setSecurityHeaders(w http.ResponseWriter) {
	// Prevent clickjacking attacks
	w.Header().Set("X-Frame-Options", "DENY")

	// Prevent MIME type sniffing
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Enable XSS filter in older browsers
	w.Header().Set("X-XSS-Protection", "1; mode=block")

	// Enforce HTTPS - Always set HSTS header as TLS termination might be handled by a reverse proxy
	// max-age=31536000 (1 year), includeSubDomains
	w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

	// Content Security Policy - restrictive by default
	// Allow self for scripts/styles, data: for images (base64), and 'unsafe-inline' for styles (needed by some UI frameworks)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.tailwindcss.com https://unpkg.com https://browser.sentry-cdn.com https://*.sentry.io https://cdnjs.cloudflare.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdn.tailwindcss.com https://cdnjs.cloudflare.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: blob:; connect-src 'self' https://*.sentry.io; frame-src 'self' https://*.sentry.io; frame-ancestors 'none';")

	// Referrer Policy - don't leak referrer information
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

	// Permissions Policy (formerly Feature Policy) - disable unnecessary features
	w.Header().Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()")
}

func (s *Server) requireLocalHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			logrus.WithFields(logrus.Fields{
				"remote_addr":       r.RemoteAddr,
				"x_forwarded_for":   r.Header.Get("X-Forwarded-For"),
				"x_real_ip":         r.Header.Get("X-Real-Ip"),
				"requested_endpoint": r.URL.Path,
			}).Warn("Rejected non-local access to profiling endpoint")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackRequest(r *http.Request) bool {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ip := strings.TrimSpace(strings.Split(forwarded, ",")[0])
		return isLoopbackIP(ip)
	}

	addr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}

	return isLoopbackIP(addr)
}

func isLoopbackIP(addr string) bool {
	if addr == "" {
		return false
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func (s *Server) setupAPIDocumentation() {
	// Read the OpenAPI spec
	openAPISpec, err := os.ReadFile("api/openapi.yaml")
	if err != nil {
		logrus.WithError(err).Warn("Failed to load OpenAPI specification, API documentation will not be available")
		return
	}

	// Serve Swagger UI at both /docs/ and /api/docs/
	s.router.PathPrefix("/docs/").HandlerFunc(ServeSwaggerUI(openAPISpec, "/docs")).Methods("GET")
	s.router.PathPrefix("/api/docs/").HandlerFunc(ServeSwaggerUI(openAPISpec, "/api/docs")).Methods("GET")

	// Redirect /docs to /docs/ and /api/docs to /api/docs/
	s.router.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
	}).Methods("GET")
	s.router.HandleFunc("/api/docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/docs/", http.StatusMovedPermanently)
	}).Methods("GET")

	logrus.Info("API documentation available at /docs/ and /api/docs/")
}

func (s *Server) setupRoutes() {
	// Register monitoring endpoints first (highest priority)
	s.router.HandleFunc("/health", s.healthCheck).Methods("GET", "HEAD")
	s.router.HandleFunc("/ready", s.readinessCheck).Methods("GET", "HEAD")
	s.router.Handle("/metrics", s.metrics.Handler()).Methods("GET")
	s.router.Handle("/stats", s.metrics.StatsHandler()).Methods("GET")

	// Register pprof endpoints if enabled
	if s.config.Monitoring.PprofEnabled {
		logrus.Info("pprof profiling endpoints enabled at /debug/pprof/ (loopback only)")
		localOnly := s.requireLocalHandler

		s.router.Handle("/debug/pprof/", localOnly(http.HandlerFunc(pprof.Index)))
		s.router.Handle("/debug/pprof/cmdline", localOnly(http.HandlerFunc(pprof.Cmdline)))
		s.router.Handle("/debug/pprof/profile", localOnly(http.HandlerFunc(pprof.Profile)))
		s.router.Handle("/debug/pprof/symbol", localOnly(http.HandlerFunc(pprof.Symbol)))
		s.router.Handle("/debug/pprof/trace", localOnly(http.HandlerFunc(pprof.Trace)))
		s.router.Handle("/debug/pprof/heap", localOnly(pprof.Handler("heap")))
		s.router.Handle("/debug/pprof/goroutine", localOnly(pprof.Handler("goroutine")))
		s.router.Handle("/debug/pprof/threadcreate", localOnly(pprof.Handler("threadcreate")))
		s.router.Handle("/debug/pprof/block", localOnly(pprof.Handler("block")))
		s.router.Handle("/debug/pprof/mutex", localOnly(pprof.Handler("mutex")))
		s.router.Handle("/debug/pprof/allocs", localOnly(pprof.Handler("allocs")))
	}

	// Register API documentation endpoint
	s.setupAPIDocumentation()

	// Register Auth0 routes if enabled
	if s.config.Auth0.Enabled && s.auth0 != nil {
		logrus.Info("Auth0 authentication enabled")
		s.router.HandleFunc("/api/auth/login", s.auth0.LoginHandler).Methods("GET")
		s.router.HandleFunc("/api/auth/callback", s.auth0.CallbackHandler).Methods("GET")
		s.router.HandleFunc("/api/auth/logout", s.auth0.LogoutHandler).Methods("GET")
		s.router.HandleFunc("/api/auth/userinfo", s.auth0.UserInfoHandler).Methods("GET")
		s.router.HandleFunc("/api/auth/status", s.auth0.AuthStatusHandler).Methods("GET")
		
		// API Key management endpoints
		s.router.HandleFunc("/api/auth/keys", s.auth0.CreateAPIKeyHandler).Methods("POST")
		s.router.HandleFunc("/api/auth/keys", s.auth0.ListAPIKeysHandler).Methods("GET")
		s.router.HandleFunc("/api/auth/keys/revoke", s.auth0.RevokeAPIKeyHandler).Methods("POST")
		
		// Admin endpoints for group/role management
		adminHandlers := NewAdminHandlers(s.auth0)
		s.router.HandleFunc("/api/admin/group-mappings", adminHandlers.ListGroupMappingsHandler).Methods("GET")
		s.router.HandleFunc("/api/admin/group-mappings", adminHandlers.CreateGroupMappingHandler).Methods("POST")
		s.router.HandleFunc("/api/admin/group-mappings", adminHandlers.DeleteGroupMappingHandler).Methods("DELETE")
		s.router.HandleFunc("/api/admin/effective-roles", adminHandlers.GetEffectiveRolesHandler).Methods("GET")
	}

	// Register tenant management API routes if multitenancy is enabled
	if s.config.Multitenancy.Enabled && s.tenantHandlers != nil {
		logrus.Info("Multi-tenancy enabled - registering tenant API routes")
		s.router.HandleFunc("/api/orgs", s.tenantHandlers.CreateOrgHandler).Methods("POST")
		s.router.HandleFunc("/api/orgs/{slug}", s.tenantHandlers.GetOrgHandler).Methods("GET")
		s.router.HandleFunc("/api/orgs/{slug}/users", s.tenantHandlers.AddUserToOrgHandler).Methods("POST")
		s.router.HandleFunc("/api/orgs/{slug}/users", s.tenantHandlers.ListOrgUsersHandler).Methods("GET")
		s.router.HandleFunc("/api/orgs/{slug}/users/{id}", s.tenantHandlers.RemoveUserFromOrgHandler).Methods("DELETE")
		s.router.HandleFunc("/api/orgs/{slug}/buckets", s.tenantHandlers.CreateBucketMappingHandler).Methods("POST")
		s.router.HandleFunc("/api/orgs/{slug}/buckets", s.tenantHandlers.ListBucketMappingsHandler).Methods("GET")
		s.router.HandleFunc("/api/orgs/{slug}/buckets/{name}", s.tenantHandlers.DeleteBucketMappingHandler).Methods("DELETE")
		s.router.HandleFunc("/api/orgs/{slug}/usage", s.tenantHandlers.UsageHandler).Methods("GET")
	}

	// Register auth validation endpoint
	s.router.HandleFunc("/api/auth/validate", s.validateCredentials).Methods("POST")

	// Register feature flags endpoint
	s.router.HandleFunc("/api/features", s.getFeatures).Methods("GET")

	// Register vector engine API if enabled
	if os.Getenv("VECTOR_ENABLED") == "true" {
		InitVectorEngine(s.router, s.storage, nil)
	}

	// Register share link routes
	s.router.HandleFunc("/api/share/create", s.shareLinkHandler.CreateShareLinkHandler).Methods("POST")
	s.router.HandleFunc("/api/share/{shareID}", s.shareLinkHandler.ServeSharedFile).Methods("GET", "HEAD")

	// Register UI routes if enabled
	if s.config.UI.Enabled {
		logrus.WithFields(logrus.Fields{
			"basePath":   s.config.UI.BasePath,
			"staticPath": s.config.UI.StaticPath,
			"auth0":      s.config.Auth0.Enabled,
		}).Info("Web UI enabled")

		if s.config.Auth0.Enabled && s.auth0 != nil {
			// Serve secure UI with Auth0 authentication
			s.router.HandleFunc(s.config.UI.BasePath, s.auth0.SecureUIHandler).Methods("GET")
			s.router.HandleFunc(s.config.UI.BasePath+"/", s.auth0.SecureUIHandler).Methods("GET")
			
			// Block unsafe static files that bypass auth
			s.router.HandleFunc(s.config.UI.BasePath + "/login.html", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/api/auth/login", http.StatusTemporaryRedirect)
			}).Methods("GET")
			s.router.HandleFunc(s.config.UI.BasePath + "/index.html", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, s.config.UI.BasePath, http.StatusTemporaryRedirect)
			}).Methods("GET")
			
			// Add profile page route
			s.router.HandleFunc(s.config.UI.BasePath + "/profile", s.auth0.ProfileHandler).Methods("GET")
			
			// Add admin page route (protected)
			s.router.HandleFunc(s.config.UI.BasePath + "/admin", s.serveAdminUI).Methods("GET")
			
			// Protected static assets - these should be served without auth middleware
			// but the session check will happen on the main UI pages that load these assets
			s.router.PathPrefix(s.config.UI.BasePath + "/js/").Handler(
				http.StripPrefix(s.config.UI.BasePath, s.uiHandler()),
			).Methods("GET")
			s.router.PathPrefix(s.config.UI.BasePath + "/css/").Handler(
				http.StripPrefix(s.config.UI.BasePath, s.uiHandler()),
			).Methods("GET")
			s.router.HandleFunc(s.config.UI.BasePath + "/browser.html", s.auth0.RequireUIAuth(s.uiHandler().ServeHTTP)).Methods("GET")

			logrus.Info("UI protected with Auth0 authentication")
		} else {
			// Serve UI without authentication
			s.router.PathPrefix(s.config.UI.BasePath + "/").Handler(
				http.StripPrefix(s.config.UI.BasePath, s.uiHandler()),
			).Methods("GET")

			// Redirect /ui to /ui/
			s.router.HandleFunc(s.config.UI.BasePath, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, s.config.UI.BasePath+"/", http.StatusMovedPermanently)
			}).Methods("GET")
		}
	}

	// Handle common web files that should not be treated as S3 buckets
	commonWebFiles := []string{"/favicon.ico", "/robots.txt", "/.well-known", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png"}
	for _, path := range commonWebFiles {
		s.router.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}).Methods("GET", "HEAD")
	}

	// Add explicit exclusions for known non-bucket paths BEFORE S3 routes
	// Only exclude paths that might be interpreted as bucket names but shouldn't be
	excludedPaths := []string{"api", "recent", "admin", "features"}
	for _, path := range excludedPaths {
		// Handle both /path and /path/* to catch all sub-paths
		s.router.HandleFunc("/"+path, func(w http.ResponseWriter, r *http.Request) {
			logrus.WithField("excluded_path", r.URL.Path).Debug("Explicitly excluded path")
			http.NotFound(w, r)
		}).Methods("GET", "PUT", "DELETE", "HEAD", "POST")
		s.router.PathPrefix("/"+path+"/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logrus.WithField("excluded_path", r.URL.Path).Debug("Explicitly excluded sub-path")
			http.NotFound(w, r)
		}).Methods("GET", "PUT", "DELETE", "HEAD", "POST")
	}

	// Register S3 bucket operations (must be after exclusions)
	// Use simple patterns that explicitly exclude known paths
	s.router.HandleFunc("/", s.handleS3Request).Methods("GET", "PUT", "DELETE", "HEAD", "POST")
	
	// Simple bucket patterns without complex regex
	s.router.HandleFunc("/{bucket:[a-zA-Z0-9._-]+}", s.handleS3Request).Methods("GET", "PUT", "DELETE", "HEAD", "POST")
	s.router.HandleFunc("/{bucket:[a-zA-Z0-9._-]+}/", s.handleS3Request).Methods("GET", "PUT", "DELETE", "HEAD", "POST")
	s.router.HandleFunc("/{bucket:[a-zA-Z0-9._-]+}/{key:.+}", s.handleS3Request).Methods("GET", "PUT", "DELETE", "HEAD", "POST")
	
	// Add debug handler to catch unmatched routes
	s.router.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logrus.WithFields(logrus.Fields{
			"path":   r.URL.Path,
			"method": r.Method,
			"query":  r.URL.RawQuery,
		}).Warn("Route not found - 404")
		http.NotFound(w, r)
	})
}

func (s *Server) handleS3Request(w http.ResponseWriter, r *http.Request) {
	// Handle virtual-hosted-style requests
	// Extract bucket from Host header if it matches pattern: bucket.s3.domain
	host := r.Host
	if strings.Contains(host, ".s3.") {
		parts := strings.Split(host, ".")
		if len(parts) >= 3 && parts[1] == "s3" {
			bucket := parts[0]
			// Rewrite the request to path-style
			if r.URL.Path == "/" {
				r.URL.Path = "/" + bucket + "/"
			} else {
				r.URL.Path = "/" + bucket + r.URL.Path
			}
			// Update mux vars
			vars := mux.Vars(r)
			if vars == nil {
				vars = make(map[string]string)
			}
			vars["bucket"] = bucket
			r = mux.SetURLVars(r, vars)

			logrus.WithFields(logrus.Fields{
				"host":          host,
				"bucket":        bucket,
				"rewrittenPath": r.URL.Path,
			}).Debug("Converted virtual-hosted-style to path-style")
		}
	}

	logrus.WithFields(logrus.Fields{
		"path":                r.URL.Path,
		"method":              r.Method,
		"authType":            s.config.Auth.Type,
		"hasAuth":             r.Header.Get("Authorization") != "",
		"contentLength":       r.ContentLength,
		"contentLengthHeader": r.Header.Get("Content-Length"),
		"userAgent":           r.Header.Get("User-Agent"),
		"accept":              r.Header.Get("Accept"),
	}).Info("S3 request received")

	// Perform authentication using the new AuthenticationManager
	authCtx, err := s.authManager.AuthenticateRequest(r)
	if err != nil {
		if authErr, ok := err.(*AuthenticationError); ok && authErr.Type == "auth0_required" {
			// Auth0 authentication required for UI access
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		
		// Other authentication errors (AWS signature failed, etc.)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
		return
	}

	// Apply authentication context to the request
	r = s.authManager.ApplyAuthContext(r, authCtx)

	// Clean headers as needed
	s.authManager.CleanHeaders(r)

	// }

	// Debug log before passing to S3 handler
	logrus.WithFields(logrus.Fields{
		"path":                r.URL.Path,
		"contentLength":       r.ContentLength,
		"contentLengthHeader": r.Header.Get("Content-Length"),
		"method":              r.Method,
	}).Debug("Passing to S3 handler")

	// Add CORS headers for all S3 requests
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Amz-*")
	
	// Pass to S3 handler normally (no more duplicate response)
	s.s3Handler.ServeHTTP(w, r)
}

func (s *Server) healthCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	// Check if server is shutting down
	isShuttingDown := atomic.LoadInt32(&s.shuttingDown) == 1
	
	// Add status headers
	if s.config.Sentry.Enabled {
		w.Header().Set("X-Sentry-Enabled", "true")
	} else {
		w.Header().Set("X-Sentry-Enabled", "false")
	}
	
	if isShuttingDown {
		w.Header().Set("X-Shutdown-Status", "in-progress")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"shutting-down","ready":false}`))
	} else {
		w.Header().Set("X-Shutdown-Status", "active")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","ready":true}`))
	}
}

// SetShuttingDown marks the server as shutting down
func (s *Server) SetShuttingDown() {
	atomic.StoreInt32(&s.shuttingDown, 1)
	logrus.Info("Server marked as shutting down - health checks will return 503")
}

// IsShuttingDown returns true if the server is shutting down
func (s *Server) IsShuttingDown() bool {
	return atomic.LoadInt32(&s.shuttingDown) == 1
}

// readinessCheck indicates if the server is ready to accept requests
func (s *Server) readinessCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	if s.IsShuttingDown() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":false,"status":"shutting-down"}`))
	} else {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ready":true,"status":"active"}`))
	}
}

// uiHandler returns a handler that serves static files and processes HTML files
// to inject environment variables
func (s *Server) uiHandler() http.Handler {
	fileServer := http.FileServer(http.Dir(s.config.UI.StaticPath))
	
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if this is an HTML file
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" || r.URL.Path == "" {
			// Read the file
			filePath := r.URL.Path
			if filePath == "/" || filePath == "" {
				filePath = "/index.html"
			}
			
			fullPath := s.config.UI.StaticPath + filePath
			content, err := os.ReadFile(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			
			// Replace placeholders with actual values
			html := string(content)
			html = strings.ReplaceAll(html, "{{SENTRY_ENABLED}}", strconv.FormatBool(s.config.Sentry.Enabled))
			html = strings.ReplaceAll(html, "{{SENTRY_DSN}}", s.config.Sentry.DSN)
			html = strings.ReplaceAll(html, "{{SENTRY_ENVIRONMENT}}", s.config.Sentry.Environment)
			
			// Add version timestamp for cache busting
			version := fmt.Sprintf("%d", time.Now().Unix())
			html = strings.ReplaceAll(html, "alpinejs@3.x.x/dist/cdn.min.js", "alpinejs@3.x.x/dist/cdn.min.js?v="+version)
			html = strings.ReplaceAll(html, "{{VERSION}}", version)
			
			// Set content type and cache headers
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, private, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			w.Header().Set("X-Accel-Expires", "0") // For nginx
			w.Header().Set("Surrogate-Control", "no-store") // For CDN/frontdoor
			w.Header().Set("Vary", "*") // Prevent caching based on any header
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(html))
			return
		}
		
		// For non-HTML files, add cache headers for JS/CSS files
		if strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// validateCredentials checks S3 credentials
func (s *Server) validateCredentials(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse JSON request body
	var creds struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}

	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Invalid request body"}`))
		return
	}

	// Validate credentials based on auth type
	switch s.config.Auth.Type {
	case "awsv4", "awsv2":
		// Check if credentials match configured values
		if creds.AccessKey == s.config.Auth.Identity && creds.SecretKey == s.config.Auth.Credential {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"valid":true,"message":"Credentials valid"}`))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"valid":false,"message":"Invalid credentials"}`))
		}
	case "database":
		// For database auth, check against database
		if dbProvider, ok := s.auth.(*auth.DatabaseProvider); ok {
			secretKey, err := dbProvider.GetSecretKey(creds.AccessKey)
			if err != nil || secretKey != creds.SecretKey {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"valid":false,"message":"Invalid credentials"}`))
			} else {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"valid":true,"message":"Credentials valid"}`))
			}
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"Database auth not properly configured"}`))
		}
	case "none":
		// No auth required
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"valid":true,"message":"No authentication required"}`))
	default:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Unknown auth type"}`))
	}
}

// getFeatures lists enabled modules
func (s *Server) getFeatures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	features := map[string]interface{}{
		"virustotal": map[string]interface{}{
			"enabled":      s.config.VirusTotal.Enabled,
			"scanUploads":  s.config.VirusTotal.ScanUploads,
			"blockThreats": s.config.VirusTotal.BlockThreats,
			"maxFileSize":  s.config.VirusTotal.MaxFileSize,
		},
		"auth0": map[string]interface{}{
			"enabled": s.config.Auth0.Enabled,
		},
		"ui": map[string]interface{}{
			"enabled": s.config.UI.Enabled,
		},
		"shareLinks": map[string]interface{}{
			"enabled": s.config.ShareLinks.Enabled,
		},
	}

	jsonData, err := json.Marshal(features)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Failed to marshal features"}`))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(jsonData)
}

// loggingMiddleware is currently unused but kept for future use
//
//nolint:unused
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fast path: skip logging for health and readiness checks  
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authMiddleware is no longer used - auth is handled inline in setupRoutes
//
//nolint:unused
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health and readiness checks
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		// Fast path: inline authentication check
		if s.auth == nil || s.config.Auth.Type == "none" {
			next.ServeHTTP(w, r)
			return
		}

		// Check for authorization
		if err := s.auth.Authenticate(r); err != nil {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// corsMiddleware is currently unused but kept for future use
//
//nolint:unused
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-amz-*")
		w.Header().Set("Access-Control-Expose-Headers", "ETag, x-amz-*")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}







func (s *Server) serveAdminUI(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if s.auth0 == nil || !s.auth0.IsAuthenticated(r) {
		http.Redirect(w, r, "/api/auth/login", http.StatusTemporaryRedirect)
		return
	}

	// Check if user is admin
	session, _ := s.auth0.store.Get(r, sessionName)
	var isAdmin bool
	if rolesStr, ok := session.Values["user_roles"].(string); ok && rolesStr != "" {
		roles := strings.Split(rolesStr, ",")
		for _, role := range roles {
			if role == "admin" || role == "storage-admin" {
				isAdmin = true
				break
			}
		}
	}

	if !isAdmin {
		http.Error(w, "Access denied. Admin role required.", http.StatusForbidden)
		return
	}

	// Serve the admin.html file
	http.ServeFile(w, r, filepath.Join(s.config.UI.StaticPath, "admin.html"))
}

// Close releases server resources gracefully
func (s *Server) Close() error {
	// Mark server as shutting down immediately
	s.SetShuttingDown()
	
	logrus.Info("Starting graceful shutdown of proxy server resources...")
	
	var errors []error
	
	// Close database connections
	if s.db != nil {
		logrus.Info("Closing database connections...")
		if err := s.db.Close(); err != nil {
			logrus.WithError(err).Error("Failed to close database connections")
			errors = append(errors, fmt.Errorf("database close error: %w", err))
		} else {
			logrus.Info("Database connections closed successfully")
		}
	}
	
	// Close VirusTotal scanner if it has cleanup needs
	if s.scanner != nil {
		logrus.Info("Cleaning up VirusTotal scanner...")
		// VirusTotal scanner doesn't currently have a Close method, but we log for completeness
		logrus.Info("VirusTotal scanner cleanup completed")
	}
	
	// Close Auth0 resources if enabled
	if s.auth0 != nil {
		logrus.Info("Cleaning up Auth0 resources...")
		// Auth0 handler doesn't currently have cleanup, but we prepare for it
		logrus.Info("Auth0 cleanup completed")
	}
	
	// Close storage backend if it has cleanup needs
	if s.storage != nil {
		logrus.Info("Cleaning up storage backend...")
		// Storage backends don't currently implement Close(), but we prepare for it
		logrus.Info("Storage backend cleanup completed")
	}
	
	if len(errors) > 0 {
		logrus.WithField("error_count", len(errors)).Error("Some resources failed to close gracefully")
		return fmt.Errorf("multiple close errors: %v", errors)
	}
	
	logrus.Info("Proxy server resources closed successfully")
	return nil
}

// validateEncryptionAtStartup performs a small encrypt+decrypt round-trip to
// verify the configured key provider is reachable and produces a usable DEK
// before the listener accepts traffic. Refuses startup on any failure so the
// engine cannot reach a healthy state with a broken encryption configuration.
func validateEncryptionAtStartup(ctx context.Context, m *encryption.Manager) error {
	if !m.IsEnabled() {
		return nil
	}

	canary := []byte("warp-storage-engine-encryption-self-test")

	encReader, encMeta, err := m.Encrypt(ctx, bytes.NewReader(canary), int64(len(canary)))
	if err != nil {
		return fmt.Errorf("self-test encrypt: %w", err)
	}
	encBytes, err := io.ReadAll(encReader)
	if err != nil {
		return fmt.Errorf("self-test encrypt read: %w", err)
	}
	if len(encMeta) == 0 {
		return fmt.Errorf("self-test encrypt produced no metadata")
	}

	decReader, err := m.Decrypt(ctx, bytes.NewReader(encBytes), encMeta)
	if err != nil {
		return fmt.Errorf("self-test decrypt: %w", err)
	}
	decBytes, err := io.ReadAll(decReader)
	if err != nil {
		return fmt.Errorf("self-test decrypt read: %w", err)
	}
	if !bytes.Equal(decBytes, canary) {
		return fmt.Errorf("self-test round-trip mismatch")
	}
	return nil
}
