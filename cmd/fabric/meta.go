// meta.go — kronaxis_meta repository functions + per-tenant provisioning.
//
// Holds:
//   - Per-tenant schema DDL template (string equivalent of migration 002).
//   - provisionTenant() — wraps the entire DDL+register dance in a single
//     transaction with automatic rollback on any failure.
//   - Helpers: schema name validator + UUID v7 generator (locally implemented
//     to avoid an extra dependency).
//   - Audit log writer.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Validates per-tenant schema name. Matches Section 1.1: tenant_<8 hex>(_N)?.
var schemaNameRE = regexp.MustCompile(`^tenant_[a-f0-9]{8}(_[0-9]+)?$`)

const tenantZeroID = "00000000-0000-7000-8000-000000000000"
const tenantZeroSchema = "tenant_00000000"

// perTenantSchemaTpl — kept in sync with migrations/20260527_002_per_tenant_template.sql.
// {{.Schema}} is replaced by the validated schema name.
const perTenantSchemaTpl = `
CREATE SCHEMA IF NOT EXISTS {{.Schema}};

CREATE TABLE IF NOT EXISTS {{.Schema}}.memos (
  id BIGSERIAL PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'general',
  tags TEXT[] NOT NULL DEFAULT '{}',
  sha256 TEXT NOT NULL,
  tsv tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('english', coalesce(title,'')), 'A') ||
    setweight(to_tsvector('english', content), 'B')
  ) STORED,
  embedding vector(768),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  -- 004 (2026-06-06): supersession key. ON CONFLICT (upsert_key) DO UPDATE.
  upsert_key TEXT,
  -- 006 (2026-08-02): provenance (poisoning defence) + world valid-time.
  author_session TEXT,
  write_source TEXT,
  trust_tier SMALLINT NOT NULL DEFAULT 1,
  valid_from TIMESTAMPTZ,
  valid_to TIMESTAMPTZ,
  -- 007 (2026-08-02): write-time admission quarantine (withheld from retrieval until released).
  quarantined BOOLEAN NOT NULL DEFAULT false,
  quarantine_reason TEXT,
  -- 008 (2026-08-04): trusted memo the contradiction judge flagged against established memos.
  contested BOOLEAN NOT NULL DEFAULT false,
  contested_reason TEXT,
  CONSTRAINT memos_sha256_uniq UNIQUE (sha256)
);

CREATE INDEX IF NOT EXISTS memos_tsv_idx
  ON {{.Schema}}.memos USING gin (tsv);
CREATE INDEX IF NOT EXISTS memos_type_idx
  ON {{.Schema}}.memos (type) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_created_idx
  ON {{.Schema}}.memos (created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_embedding_ivf
  ON {{.Schema}}.memos USING ivfflat (embedding vector_cosine_ops)
  WITH (lists = 100);
CREATE UNIQUE INDEX IF NOT EXISTS memos_upsert_key_uniq
  ON {{.Schema}}.memos (upsert_key)
  WHERE upsert_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_valid_time_idx
  ON {{.Schema}}.memos (valid_from, valid_to) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_trust_idx
  ON {{.Schema}}.memos (trust_tier) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_quarantined_idx
  ON {{.Schema}}.memos (created_at DESC) WHERE quarantined = true AND deleted_at IS NULL;

-- ---------- memo_versions: bitemporal history (005 + 006) ----------
-- BEFORE UPDATE trigger snapshots the OLD row on content (sha256) change, so
-- supersession/edits retire-in-place. valid_from/superseded_at = transaction time;
-- valid_from_w/valid_to_w = world valid time carried from memos.
CREATE TABLE IF NOT EXISTS {{.Schema}}.memo_versions (
  version_id BIGSERIAL PRIMARY KEY,
  memo_id BIGINT NOT NULL,
  upsert_key TEXT,
  title TEXT, content TEXT, type TEXT, tags TEXT[], sha256 TEXT,
  valid_from TIMESTAMPTZ,
  superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  author_session TEXT, write_source TEXT, trust_tier SMALLINT,
  valid_from_w TIMESTAMPTZ, valid_to_w TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS memo_versions_key_idx
  ON {{.Schema}}.memo_versions (upsert_key) WHERE upsert_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS memo_versions_memo_idx
  ON {{.Schema}}.memo_versions (memo_id);

CREATE OR REPLACE FUNCTION {{.Schema}}.snapshot_memo_version() RETURNS trigger AS $t$
BEGIN
  IF OLD.sha256 IS DISTINCT FROM NEW.sha256 THEN
    INSERT INTO {{.Schema}}.memo_versions
      (memo_id, upsert_key, title, content, type, tags, sha256, valid_from, superseded_at,
       author_session, write_source, trust_tier, valid_from_w, valid_to_w)
    VALUES
      (OLD.id, OLD.upsert_key, OLD.title, OLD.content, OLD.type, OLD.tags, OLD.sha256, OLD.created_at, now(),
       OLD.author_session, OLD.write_source, OLD.trust_tier, OLD.valid_from, OLD.valid_to);
  END IF;
  RETURN NEW;
END;
$t$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS memo_version_snapshot ON {{.Schema}}.memos;
CREATE TRIGGER memo_version_snapshot BEFORE UPDATE ON {{.Schema}}.memos
  FOR EACH ROW EXECUTE FUNCTION {{.Schema}}.snapshot_memo_version();

CREATE TABLE IF NOT EXISTS {{.Schema}}.prospect_interactions (
  id BIGSERIAL PRIMARY KEY,
  prospect_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  turn_idx INT NOT NULL,
  vertical TEXT NOT NULL DEFAULT '',
  stage TEXT NOT NULL DEFAULT '',
  user_text TEXT,
  agent_text TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT '',
  latency_ms INT NOT NULL DEFAULT 0,
  embedding vector(768),
  memo_id BIGINT REFERENCES {{.Schema}}.memos(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS prospect_interactions_prospect_idx
  ON {{.Schema}}.prospect_interactions (prospect_id, created_at DESC);
CREATE INDEX IF NOT EXISTS prospect_interactions_call_idx
  ON {{.Schema}}.prospect_interactions (call_id, turn_idx);
CREATE INDEX IF NOT EXISTS prospect_interactions_embedding_ivf
  ON {{.Schema}}.prospect_interactions USING ivfflat (embedding vector_cosine_ops)
  WITH (lists = 50);
`

// renderTenantSchemaSQL fills the template safely (schema name pre-validated).
func renderTenantSchemaSQL(schema string) (string, error) {
	if !schemaNameRE.MatchString(schema) {
		return "", fmt.Errorf("invalid schema name %q", schema)
	}
	tpl, err := template.New("tenant").Parse(perTenantSchemaTpl)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, map[string]string{"Schema": schema}); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// newUUIDv7 generates a UUID v7 (RFC 9562) without external deps.
//   - 48 bits unix_ms timestamp
//   - 4 bits version (0b0111 = 7)
//   - 12 bits random
//   - 2 bits variant (0b10)
//   - 62 bits random
func newUUIDv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UTC().UnixMilli())
	// Encode 48-bit ms timestamp in the first 6 bytes (big-endian).
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	// Version = 7 in upper nibble of byte 6
	b[6] = (b[6] & 0x0F) | 0x70
	// Variant = 10xx in upper bits of byte 8
	b[8] = (b[8] & 0x3F) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// schemaFromUUID returns "tenant_<first 8 hex>" — strips dashes from UUID and
// takes the leading 8 chars. With UUIDv7 these are the high-order timestamp
// bits, ensuring monotonic spread.
func schemaFromUUID(u string) string {
	u = strings.ReplaceAll(u, "-", "")
	if len(u) < 8 {
		return ""
	}
	return "tenant_" + strings.ToLower(u[:8])
}

// randomBearer returns a URL-safe 32-byte secret prefixed with "pmem_".
func randomBearer() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "pmem_" + hex.EncodeToString(b[:]), nil
}

// keyPrefix returns the first 8 chars of the plaintext for ops lookup.
func keyPrefix(plaintext string) string {
	if len(plaintext) < 8 {
		return plaintext
	}
	return plaintext[:8]
}

// hashPlaintext sha256-hexes the bearer; kept in this file so meta.go is
// self-contained.
func hashPlaintext(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// auditWrite appends one row to kronaxis_meta.audit_log. Best-effort; never
// propagates an error to the caller.
func auditWrite(ctx context.Context, pool *pgxpool.Pool, actorTenant *string, actorKey *int64, action string, targetTenant *string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO kronaxis_meta.audit_log (actor_tenant_id, actor_key_id, action, target_tenant_id, detail)
		VALUES ($1, $2, $3, $4, $5::jsonb)`,
		actorTenant, actorKey, action, targetTenant, mustJSON(detail),
	)
	if err != nil {
		// Use the package-level logger; importing log here would create a cycle.
		// Print to stderr instead.
		fmt.Printf("audit warn: %v\n", err)
	}
}

func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// tenantSummary is the canonical row shape returned by /v1/tenants.
type tenantSummary struct {
	ID            string         `json:"id"`
	ParentID      *string        `json:"parent_tenant_id,omitempty"`
	DisplayAlias  string         `json:"display_alias"`
	SchemaName    string         `json:"schema_name"`
	TenantType    string         `json:"tenant_type"`
	Status        string         `json:"status"`
	EmbedderURL   *string        `json:"embedder_url,omitempty"`
	RerankURL     *string        `json:"rerank_url,omitempty"`
	RetentionDays *int           `json:"retention_days,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// provisionTenantParams is the input to a new tenant.
type provisionTenantParams struct {
	DisplayAlias   string
	TenantType     string // 'platform' | 'customer' | 'persona'
	ParentTenantID *string
	EmbedderURL    *string
	RerankURL      *string
	RetentionDays  *int
	Metadata       map[string]any
	BearerScope    string // 'tenant' | 'admin'
}

type provisionTenantResult struct {
	TenantID    string
	SchemaName  string
	BearerKey   string
	BearerKeyID int64
}

// provisionTenant runs the full provisioning playbook (Section 2.5). On any
// failure inside the transaction, the schema CREATE is rolled back and the
// tenants row is not inserted. Schema name collision is handled by appending
// `_<n>` suffixes.
func provisionTenant(ctx context.Context, pool *pgxpool.Pool, p provisionTenantParams) (*provisionTenantResult, error) {
	if p.DisplayAlias == "" {
		return nil, errors.New("display_alias required")
	}
	if p.TenantType == "" {
		p.TenantType = "persona"
	}
	switch p.TenantType {
	case "platform", "customer", "persona":
	default:
		return nil, fmt.Errorf("invalid tenant_type %q", p.TenantType)
	}
	if p.BearerScope == "" {
		p.BearerScope = "tenant"
	}
	if p.BearerScope != "tenant" && p.BearerScope != "admin" {
		return nil, fmt.Errorf("invalid bearer scope %q", p.BearerScope)
	}

	tenantID, err := newUUIDv7()
	if err != nil {
		return nil, fmt.Errorf("uuid: %w", err)
	}
	schema := schemaFromUUID(tenantID)
	if schema == "" {
		return nil, errors.New("schema derivation failed")
	}
	// Resolve schema-name collision (extremely unlikely with UUIDv7 timestamp
	// prefix, but cheap to guard).
	for n := 2; n < 100; n++ {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM kronaxis_meta.tenants WHERE schema_name=$1)`, schema).Scan(&exists); err != nil {
			return nil, fmt.Errorf("collision check: %w", err)
		}
		if !exists {
			break
		}
		schema = fmt.Sprintf("%s_%d", schemaFromUUID(tenantID), n)
	}
	if !schemaNameRE.MatchString(schema) {
		return nil, fmt.Errorf("derived schema %q failed validator", schema)
	}

	plaintext, err := randomBearer()
	if err != nil {
		return nil, fmt.Errorf("bearer: %w", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Apply tenant schema DDL (cannot use template-substituted args; SQL is
	// already validated by schemaNameRE).
	ddl, err := renderTenantSchemaSQL(schema)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return nil, fmt.Errorf("schema ddl: %w", err)
	}

	// Insert tenants row.
	metaJSON := mustJSON(p.Metadata)
	if _, err := tx.Exec(ctx, `
		INSERT INTO kronaxis_meta.tenants
		  (id, parent_tenant_id, display_alias, schema_name, tenant_type,
		   embedder_url, rerank_url, retention_days, metadata)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
		tenantID, p.ParentTenantID, p.DisplayAlias, schema, p.TenantType,
		p.EmbedderURL, p.RerankURL, p.RetentionDays, metaJSON,
	); err != nil {
		return nil, fmt.Errorf("insert tenants: %w", err)
	}

	// Insert initial bearer key.
	var keyID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO kronaxis_meta.tenant_keys
		  (tenant_id, key_hash, key_prefix, scope)
		VALUES ($1::uuid, $2, $3, $4)
		RETURNING id`,
		tenantID, hashPlaintext(plaintext), keyPrefix(plaintext), p.BearerScope,
	).Scan(&keyID); err != nil {
		return nil, fmt.Errorf("insert key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Best-effort audit AFTER commit so it doesn't leak on rollback.
	auditWrite(context.Background(), pool, nil, &keyID, "tenant.create", &tenantID, map[string]any{
		"display_alias": p.DisplayAlias,
		"schema":        schema,
		"tenant_type":   p.TenantType,
		"bearer_scope":  p.BearerScope,
	})

	return &provisionTenantResult{
		TenantID:    tenantID,
		SchemaName:  schema,
		BearerKey:   plaintext,
		BearerKeyID: keyID,
	}, nil
}

// listTenants returns all rows (optionally filtered by type / parent).
func listTenants(ctx context.Context, pool *pgxpool.Pool, filterType, filterParent string) ([]tenantSummary, error) {
	q := `SELECT id::text, parent_tenant_id::text, display_alias, schema_name, tenant_type, status,
	             embedder_url, rerank_url, retention_days, metadata, created_at
	      FROM kronaxis_meta.tenants WHERE 1=1`
	args := []any{}
	if filterType != "" {
		args = append(args, filterType)
		q += fmt.Sprintf(" AND tenant_type = $%d", len(args))
	}
	if filterParent != "" {
		args = append(args, filterParent)
		q += fmt.Sprintf(" AND parent_tenant_id = $%d::uuid", len(args))
	}
	q += " ORDER BY created_at"
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []tenantSummary{}
	for rows.Next() {
		var t tenantSummary
		var parentID *string
		var metaJSON []byte
		if err := rows.Scan(&t.ID, &parentID, &t.DisplayAlias, &t.SchemaName, &t.TenantType, &t.Status,
			&t.EmbedderURL, &t.RerankURL, &t.RetentionDays, &metaJSON, &t.CreatedAt); err != nil {
			continue
		}
		t.ParentID = parentID
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &t.Metadata)
		}
		out = append(out, t)
	}
	return out, nil
}

func getTenant(ctx context.Context, pool *pgxpool.Pool, id string) (*tenantSummary, error) {
	var t tenantSummary
	var parentID *string
	var metaJSON []byte
	err := pool.QueryRow(ctx, `
		SELECT id::text, parent_tenant_id::text, display_alias, schema_name, tenant_type, status,
		       embedder_url, rerank_url, retention_days, metadata, created_at
		FROM kronaxis_meta.tenants WHERE id = $1::uuid`,
		id,
	).Scan(&t.ID, &parentID, &t.DisplayAlias, &t.SchemaName, &t.TenantType, &t.Status,
		&t.EmbedderURL, &t.RerankURL, &t.RetentionDays, &metaJSON, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.ParentID = parentID
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &t.Metadata)
	}
	return &t, nil
}

// rotateKey revokes the existing tenant key(s) and issues a fresh one.
func rotateKey(ctx context.Context, pool *pgxpool.Pool, tenantID string, scope string) (string, int64, error) {
	if scope == "" {
		scope = "tenant"
	}
	plaintext, err := randomBearer()
	if err != nil {
		return "", 0, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE kronaxis_meta.tenant_keys
		SET revoked_at = now()
		WHERE tenant_id = $1::uuid AND revoked_at IS NULL`,
		tenantID,
	); err != nil {
		return "", 0, fmt.Errorf("revoke: %w", err)
	}
	var keyID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO kronaxis_meta.tenant_keys
		  (tenant_id, key_hash, key_prefix, scope)
		VALUES ($1::uuid, $2, $3, $4) RETURNING id`,
		tenantID, hashPlaintext(plaintext), keyPrefix(plaintext), scope,
	).Scan(&keyID); err != nil {
		return "", 0, fmt.Errorf("insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, err
	}
	auditWrite(context.Background(), pool, nil, &keyID, "tenant.rotate_key", &tenantID, map[string]any{
		"scope": scope,
	})
	return plaintext, keyID, nil
}

// softDeleteTenant marks status=soft_deleted, revokes all keys, and renames
// the schema to `deleted_<unix_ts>_<orig>`.
func softDeleteTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string) error {
	t, err := getTenant(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	if t.Status == "soft_deleted" {
		return nil
	}
	newSchema := fmt.Sprintf("deleted_%d_%s", time.Now().Unix(), t.SchemaName)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER SCHEMA %s RENAME TO %s`, t.SchemaName, newSchema)); err != nil {
		return fmt.Errorf("rename schema: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE kronaxis_meta.tenants
		SET status='soft_deleted', soft_deleted_at=now(), schema_name=$2
		WHERE id=$1::uuid`,
		tenantID, newSchema,
	); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE kronaxis_meta.tenant_keys
		SET revoked_at=now()
		WHERE tenant_id=$1::uuid AND revoked_at IS NULL`,
		tenantID,
	); err != nil {
		return fmt.Errorf("revoke keys: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	auditWrite(context.Background(), pool, nil, nil, "tenant.soft_delete", &tenantID, map[string]any{
		"old_schema": t.SchemaName,
		"new_schema": newSchema,
	})
	return nil
}

// purgeTenant drops the schema CASCADE and deletes the tenants row. Hard,
// irreversible. confirm MUST match tenantID.
func purgeTenant(ctx context.Context, pool *pgxpool.Pool, tenantID, confirm string) error {
	if tenantID != confirm {
		return errors.New("confirm token does not match tenant_id")
	}
	t, err := getTenant(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	// Cascade: tenant_keys, audit refs (set to NULL on delete via FK), schema.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, t.SchemaName)); err != nil {
		return fmt.Errorf("drop schema: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM kronaxis_meta.tenants WHERE id=$1::uuid`, tenantID); err != nil {
		return fmt.Errorf("delete tenants: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	auditWrite(context.Background(), pool, nil, nil, "tenant.purge", &tenantID, map[string]any{
		"schema": t.SchemaName,
	})
	return nil
}
