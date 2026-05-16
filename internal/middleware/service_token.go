package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
)

// ServiceTokenMiddleware short-circuits authentication for trusted
// service-to-service callers (currently: prysm-backend's bucket
// reverse-proxy at /api/v1/buckets/*).
//
// When the request carries `Authorization: Bearer <WARP_SERVICE_TOKEN>`
// and that token matches the env var, the middleware sets:
//
//   - authenticated = true   (skips AWS-V4 sig in AuthenticationMiddleware)
//   - is_admin      = true   (passes AdminAuthMiddleware + isOrgAdmin)
//   - org_id        = X-Prysm-Organization-Slug header value (if set)
//   - service       = "prysm-backend" (for log breadcrumbs)
//
// and continues. Tokens that don't match are NOT rejected — the
// request falls through to the rest of the auth chain (AWS-V4 sig,
// vault, etc.) so end-user S3 clients still work.
//
// Empty WARP_SERVICE_TOKEN disables the bypass entirely (production-
// safe default: a typo in the env wires deny-by-default rather than
// "everyone is admin"). The constant-time comparison prevents
// timing-based token guessing — the cost is negligible since this
// fires once per request and only on the bypass path.
//
// Register BEFORE AuthenticationMiddleware in the chain so the
// short-circuit happens before sig validation runs.
func ServiceTokenMiddleware() func(http.Handler) http.Handler {
	expected := strings.TrimSpace(os.Getenv("WARP_SERVICE_TOKEN"))
	if expected == "" {
		// No token configured → no-op middleware. Logging here so
		// operators see "service-token bypass: disabled" once at
		// startup rather than wondering why their proxied calls
		// 403 forever.
		logrus.Info("service-token bypass disabled (WARP_SERVICE_TOKEN unset)")
		return func(next http.Handler) http.Handler { return next }
	}
	logrus.Info("service-token bypass enabled")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(authHeader, prefix) {
				next.ServeHTTP(w, r)
				return
			}
			supplied := strings.TrimSpace(authHeader[len(prefix):])

			// Constant-time comparison — paranoid but cheap. Length
			// check first because subtle.ConstantTimeCompare returns
			// 0 for unequal-length inputs without telling us the
			// reason. Logging the length-mismatch case helps debug
			// "I copied the token from the env, what gives?".
			if len(supplied) != len(expected) ||
				subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
				next.ServeHTTP(w, r)
				return
			}

			// Token matched. Elevate the request.
			ctx := r.Context()
			ctx = context.WithValue(ctx, "authenticated", true)
			ctx = context.WithValue(ctx, "is_admin", true)
			ctx = context.WithValue(ctx, "service", "prysm-backend")
			if slug := r.Header.Get("X-Prysm-Organization-Slug"); slug != "" {
				// Some downstream handlers (tenant_handlers.isOrgAdmin)
				// also check role+org match. Setting both lets either
				// path succeed regardless of which is checked first.
				ctx = context.WithValue(ctx, "org_id", slug)
				ctx = context.WithValue(ctx, "user_role", "admin")
			}
			r = r.WithContext(ctx)

			logrus.WithFields(logrus.Fields{
				"path":   r.URL.Path,
				"method": r.Method,
				"org":    r.Header.Get("X-Prysm-Organization-Slug"),
			}).Debug("service-token bypass: request elevated to admin")

			next.ServeHTTP(w, r)
		})
	}
}
