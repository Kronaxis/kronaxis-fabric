// admin.go — multi-tenant ops HTTP handlers (Section 3.3 of spec).
//
// All endpoints require Bearer with scope='admin'. The middleware in
// tenant.go has already resolved the caller. Per-endpoint logic re-checks
// scope just to be explicit.
//
// Endpoints:
//   POST   /v1/tenant                  — create tenant + initial bearer key
//   GET    /v1/tenants                 — list (admin)
//   GET    /v1/tenant/<id>             — get one
//   POST   /v1/tenant/<id>/rotate-key  — rotate
//   DELETE /v1/tenant/<id>             — soft delete (rename schema, revoke keys)
//   POST   /v1/tenant/<id>/purge       — hard delete (DROP SCHEMA + DELETE row)
//   POST   /v1/admin/cross-tenant-search — fan-out search across selected tenants
//
// Audit-logged via meta.auditWrite.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ---------- request/response shapes ----------

type tenantCreateReq struct {
	DisplayAlias   string         `json:"display_alias"`
	TenantType     string         `json:"tenant_type"`             // platform|customer|persona
	ParentTenantID *string        `json:"parent_tenant_id,omitempty"`
	EmbedderURL    *string        `json:"embedder_url,omitempty"`
	RerankURL      *string        `json:"rerank_url,omitempty"`
	RetentionDays  *int           `json:"retention_days,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	BearerScope    string         `json:"bearer_scope,omitempty"` // tenant|admin; default tenant
}

type tenantCreateResp struct {
	TenantID   string `json:"tenant_id"`
	SchemaName string `json:"schema_name"`
	BearerKey  string `json:"bearer_key"`           // returned ONCE
	Scope      string `json:"scope"`
}

type tenantRotateKeyReq struct {
	Scope string `json:"scope,omitempty"` // optional; defaults to tenant
}

type tenantRotateKeyResp struct {
	BearerKey string `json:"bearer_key"`
	Scope     string `json:"scope"`
}

type tenantPurgeReq struct {
	Confirm string `json:"confirm"`
}

type crossTenantSearchReq struct {
	Query     string   `json:"query"`
	TopK      int      `json:"top_k"`
	TenantIDs []string `json:"tenant_ids"`
	Rerank    bool     `json:"rerank,omitempty"`
}

// ---------- helpers ----------

func (s *server) requireAdmin(tc *tenantCtx, w http.ResponseWriter) bool {
	if tc.KeyScope != "admin" {
		writeErr(w, 403, "admin-scope bearer required")
		return false
	}
	return true
}

// ---------- handlers ----------

func (s *server) handleTenantCreate(tc *tenantCtx, w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(tc, w) {
		return
	}
	var req tenantCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.BearerScope == "" {
		req.BearerScope = "tenant"
	}
	res, err := provisionTenant(r.Context(), s.pool, provisionTenantParams{
		DisplayAlias:   req.DisplayAlias,
		TenantType:     req.TenantType,
		ParentTenantID: req.ParentTenantID,
		EmbedderURL:    req.EmbedderURL,
		RerankURL:      req.RerankURL,
		RetentionDays:  req.RetentionDays,
		Metadata:       req.Metadata,
		BearerScope:    req.BearerScope,
	})
	if err != nil {
		writeErr(w, 500, "provision: "+err.Error())
		return
	}
	writeJSON(w, 200, tenantCreateResp{
		TenantID:   res.TenantID,
		SchemaName: res.SchemaName,
		BearerKey:  res.BearerKey,
		Scope:      req.BearerScope,
	})
}

func (s *server) handleTenantList(tc *tenantCtx, w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(tc, w) {
		return
	}
	q := r.URL.Query()
	tenants, err := listTenants(r.Context(), s.pool, q.Get("type"), q.Get("parent"))
	if err != nil {
		writeErr(w, 500, "list: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"tenants": tenants, "count": len(tenants)})
}

func (s *server) handleTenantGet(tc *tenantCtx, w http.ResponseWriter, r *http.Request, id string) {
	// Allow non-admin to GET their own tenant only.
	if tc.KeyScope != "admin" && tc.ID != id {
		writeErr(w, 403, "not your tenant")
		return
	}
	t, err := getTenant(r.Context(), s.pool, id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, t)
}

func (s *server) handleTenantRotateKey(tc *tenantCtx, w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdmin(tc, w) {
		return
	}
	var req tenantRotateKeyReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	scope := req.Scope
	if scope == "" {
		scope = "tenant"
	}
	plaintext, _, err := rotateKey(r.Context(), s.pool, id, scope)
	if err != nil {
		writeErr(w, 500, "rotate: "+err.Error())
		return
	}
	// Invalidate any cached entry of the OLD key — we can't know the plaintext,
	// so simply drop the whole cache. Acceptable cost.
	s.tenantResolver.invalidateAll()
	writeJSON(w, 200, tenantRotateKeyResp{BearerKey: plaintext, Scope: scope})
}

func (s *server) handleTenantSoftDelete(tc *tenantCtx, w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdmin(tc, w) {
		return
	}
	if err := softDeleteTenant(r.Context(), s.pool, id); err != nil {
		writeErr(w, 500, "soft-delete: "+err.Error())
		return
	}
	s.tenantResolver.invalidateAll()
	writeJSON(w, 200, map[string]any{"id": id, "soft_deleted": true})
}

func (s *server) handleTenantPurge(tc *tenantCtx, w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdmin(tc, w) {
		return
	}
	var req tenantPurgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if err := purgeTenant(r.Context(), s.pool, id, req.Confirm); err != nil {
		writeErr(w, 400, "purge: "+err.Error())
		return
	}
	s.tenantResolver.invalidateAll()
	writeJSON(w, 200, map[string]any{"id": id, "purged": true})
}

// handleAdminCrossTenantSearch — fan-out across selected tenants. Admin-only.
// Re-uses handleSearch logic per-tenant and merges results by score.
func (s *server) handleAdminCrossTenantSearch(tc *tenantCtx, w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(tc, w) {
		return
	}
	var req crossTenantSearchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Query == "" || len(req.TenantIDs) == 0 {
		writeErr(w, 400, "query + tenant_ids required")
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	all := []searchHit{}
	queried := []string{}
	for _, tid := range req.TenantIDs {
		t, err := getTenant(r.Context(), s.pool, tid)
		if err != nil {
			continue
		}
		fakeCtx := &tenantCtx{
			ID:         t.ID,
			SchemaName: t.SchemaName,
		}
		hits, err := s.searchInSchema(r.Context(), fakeCtx, req.Query, "", "hybrid", req.TopK, req.Rerank)
		if err != nil {
			continue
		}
		// Tag each hit with its tenant for the caller.
		for i := range hits {
			hits[i].Title = "[" + t.DisplayAlias + "] " + hits[i].Title
		}
		all = append(all, hits...)
		queried = append(queried, t.ID)
	}
	// Sort merged by score desc.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].Score > all[i].Score {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if len(all) > req.TopK {
		all = all[:req.TopK]
	}
	auditWrite(r.Context(), s.pool, &tc.ID, &tc.KeyID, "cross_tenant_search", nil, map[string]any{
		"query":           req.Query,
		"tenants_queried": queried,
		"hits":            len(all),
	})
	writeJSON(w, 200, map[string]any{
		"results":         all,
		"tenants_queried": queried,
	})
}

// ---------- tiny ID extractors ----------

// parseTenantIDFromPath returns the UUID after the leading prefix, and any
// trailing sub-path (e.g. "rotate-key", "purge").
func parseTenantIDFromPath(path string) (id, sub string, ok bool) {
	rest := strings.TrimPrefix(path, "/v1/tenant/")
	if rest == "" || rest == path {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 1 {
		return parts[0], "", true
	}
	return parts[0], parts[1], true
}
