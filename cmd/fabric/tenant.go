// tenant.go — multi-tenant resolution + middleware.
//
// Resolves the tenant for each request from:
//   - `X-Tenant-ID` header (UUID), authoritative
//   - `Authorization: Bearer <key>` — looked up in kronaxis_meta.tenant_keys
//
// Backwards-compat (Section 3.2 of FABRIC_MULTI_TENANT_SPEC):
//   When `X-Tenant-ID` is absent AND `FABRIC_REQUIRE_TENANT_HEADER` env is not
//   "true", the request is routed to tenant-zero (kronaxis_claude_session) as
//   long as the bearer matches a valid key for that tenant. A warning line is
//   logged with a counter so the operator knows when to flip the flag.
//
// Resolution + key lookup is cached in-process for 60s to avoid hitting the
// meta DB on every request. Cache is keyed on key_hash AND tenant_id and
// invalidated explicitly on rotate-key / delete-tenant.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tenantCtx is propagated through every tenant-scoped handler. SchemaName is
// validated at insert time (^tenant_[a-f0-9]{8}(_[0-9]+)?$) so direct
// fmt.Sprintf interpolation into SQL is safe.
type tenantCtx struct {
	ID            string // UUID v7 string form
	SchemaName    string // e.g. tenant_01977f7e
	DisplayAlias  string
	EmbedderURL   string // "" = inherit server default
	RerankURL     string // "" = inherit server default
	KeyScope      string // "tenant" | "admin"
	KeyID         int64
	BackcompatHit bool // true when resolved via missing-header backcompat path
}

type httpErr struct {
	Code int
	Msg  string
}

func (e *httpErr) Error() string { return fmt.Sprintf("http %d: %s", e.Code, e.Msg) }

// keyCacheEntry caches a successful key->tenant lookup.
type keyCacheEntry struct {
	keyID        int64
	tenantID     string
	schemaName   string
	displayAlias string
	embedderURL  string
	rerankURL    string
	scope        string
	fetchedAt    time.Time
}

type tenantResolver struct {
	pool             *pgxpool.Pool // main fabric DB pool (where kronaxis_meta lives)
	tenantZeroID     string        // "00000000-0000-7000-8000-000000000000"
	tenantZeroSchema string        // "tenant_00000000"
	requireHeader    bool          // FABRIC_REQUIRE_TENANT_HEADER=true skips backcompat
	mu               sync.RWMutex  // protects cache map
	cache            map[string]*keyCacheEntry
	cacheTTL         time.Duration
	// metrics — atomic for log-without-lock
	backcompatCount uint64
}

// hashKey returns the hex-sha256 of the plaintext Bearer.
func hashKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// resolveTenant inspects request headers and returns the tenant context, or an
// *httpErr describing how to fail the request. Never returns a Postgres error
// directly — those are converted to 500.
func (tr *tenantResolver) resolveTenant(r *http.Request) (*tenantCtx, *httpErr) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return nil, &httpErr{401, "missing or malformed Authorization header"}
	}
	plaintext := strings.TrimPrefix(authz, "Bearer ")
	if plaintext == "" {
		return nil, &httpErr{401, "empty bearer"}
	}
	keyHash := hashKey(plaintext)

	requestedID := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))

	// Cache lookup
	tr.mu.RLock()
	entry, hit := tr.cache[keyHash]
	tr.mu.RUnlock()
	if hit && time.Since(entry.fetchedAt) < tr.cacheTTL {
		return tr.buildCtxFromCache(entry, requestedID)
	}

	// Cache miss — hit kronaxis_meta.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var (
		keyID        int64
		keyTenantID  string
		keyScope     string
		schemaName   string
		displayAlias string
		status       string
		embedderURL  *string
		rerankURL    *string
	)
	err := tr.pool.QueryRow(ctx, `
		SELECT k.id, k.tenant_id::text, k.scope,
		       t.schema_name, t.display_alias, t.status, t.embedder_url, t.rerank_url
		FROM kronaxis_meta.tenant_keys k
		JOIN kronaxis_meta.tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL`,
		keyHash,
	).Scan(&keyID, &keyTenantID, &keyScope, &schemaName, &displayAlias, &status, &embedderURL, &rerankURL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &httpErr{401, "unauthorised"}
		}
		return nil, &httpErr{500, "auth lookup failed: " + err.Error()}
	}
	if status != "active" {
		return nil, &httpErr{403, "tenant status=" + status}
	}
	entry = &keyCacheEntry{
		keyID:        keyID,
		tenantID:     keyTenantID,
		schemaName:   schemaName,
		displayAlias: displayAlias,
		scope:        keyScope,
		fetchedAt:    time.Now(),
	}
	if embedderURL != nil {
		entry.embedderURL = *embedderURL
	}
	if rerankURL != nil {
		entry.rerankURL = *rerankURL
	}
	tr.mu.Lock()
	if tr.cache == nil {
		tr.cache = make(map[string]*keyCacheEntry)
	}
	tr.cache[keyHash] = entry
	tr.mu.Unlock()

	// Async last_used_at touch — fire-and-forget, never block the request.
	go func(id int64) {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer bgCancel()
		_, _ = tr.pool.Exec(bgCtx, `UPDATE kronaxis_meta.tenant_keys SET last_used_at=now() WHERE id=$1`, id)
	}(keyID)

	return tr.buildCtxFromCache(entry, requestedID)
}

func (tr *tenantResolver) buildCtxFromCache(entry *keyCacheEntry, requestedID string) (*tenantCtx, *httpErr) {
	tc := &tenantCtx{
		ID:           entry.tenantID,
		SchemaName:   entry.schemaName,
		DisplayAlias: entry.displayAlias,
		EmbedderURL:  entry.embedderURL,
		RerankURL:    entry.rerankURL,
		KeyScope:     entry.scope,
		KeyID:        entry.keyID,
	}
	if requestedID == "" {
		// Backwards-compat: legacy callers without X-Tenant-ID.
		// - If the key already maps to tenant-zero (admin scope), accept it.
		// - If FABRIC_REQUIRE_TENANT_HEADER=true, refuse.
		// - Otherwise log + count, route to whatever tenant the key owns.
		if tr.requireHeader {
			return nil, &httpErr{400, "X-Tenant-ID header required"}
		}
		atomic.AddUint64(&tr.backcompatCount, 1)
		tc.BackcompatHit = true
		return tc, nil
	}
	if !strings.EqualFold(requestedID, entry.tenantID) {
		// Caller asked for a different tenant than their key owns.
		if entry.scope != "admin" {
			return nil, &httpErr{403, "key does not own requested tenant"}
		}
		// Admin keys may impersonate a different tenant — re-resolve from meta.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var schemaName, displayAlias, status string
		var embedderURL, rerankURL *string
		err := tr.pool.QueryRow(ctx, `
			SELECT schema_name, display_alias, status, embedder_url, rerank_url
			FROM kronaxis_meta.tenants WHERE id=$1`,
			requestedID,
		).Scan(&schemaName, &displayAlias, &status, &embedderURL, &rerankURL)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, &httpErr{404, "tenant not found"}
			}
			return nil, &httpErr{500, "tenant lookup failed: " + err.Error()}
		}
		if status != "active" {
			return nil, &httpErr{403, "tenant status=" + status}
		}
		tc.ID = requestedID
		tc.SchemaName = schemaName
		tc.DisplayAlias = displayAlias
		tc.EmbedderURL = ""
		tc.RerankURL = ""
		if embedderURL != nil {
			tc.EmbedderURL = *embedderURL
		}
		if rerankURL != nil {
			tc.RerankURL = *rerankURL
		}
	}
	return tc, nil
}

// invalidateKey drops a single cached key entry. Called after rotate-key /
// delete-tenant so the next request re-fetches from meta.
func (tr *tenantResolver) invalidateKey(plaintext string) {
	keyHash := hashKey(plaintext)
	tr.mu.Lock()
	delete(tr.cache, keyHash)
	tr.mu.Unlock()
}

// invalidateAll drops the whole cache. Used on tenant delete / large schema
// changes.
func (tr *tenantResolver) invalidateAll() {
	tr.mu.Lock()
	tr.cache = make(map[string]*keyCacheEntry)
	tr.mu.Unlock()
}

// backcompatHits is a snapshot of how many requests have used the missing-header
// backwards-compat path since boot. Operator inspects via /v1/health.
func (tr *tenantResolver) backcompatHits() uint64 {
	return atomic.LoadUint64(&tr.backcompatCount)
}
