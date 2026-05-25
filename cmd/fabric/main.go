package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

const version = "0.9.0"

const embeddingDim = 768
const embeddingModel = "nomic-embed-text"

const schemaSQL = `
CREATE SCHEMA IF NOT EXISTS fabric;
CREATE EXTENSION IF NOT EXISTS vector;

-- ---------- memos (v0.2) ----------
CREATE TABLE IF NOT EXISTS fabric.memos (
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
  CONSTRAINT memos_sha256_uniq UNIQUE (sha256)
);

ALTER TABLE fabric.memos ADD COLUMN IF NOT EXISTS embedding vector(768);
ALTER TABLE fabric.memos ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE fabric.memos ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS memos_tsv_idx ON fabric.memos USING gin (tsv);
CREATE INDEX IF NOT EXISTS memos_type_idx ON fabric.memos (type) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_created_idx ON fabric.memos (created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_embedding_ivf ON fabric.memos USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);

-- coord_messages may already exist in public; ensure origin_host column for v0.7.
ALTER TABLE IF EXISTS public.coord_messages ADD COLUMN IF NOT EXISTS origin_host TEXT NOT NULL DEFAULT 'local';

-- ---------- v0.5 code graph ----------
CREATE TABLE IF NOT EXISTS fabric.symbols (
  id BIGSERIAL PRIMARY KEY,
  repo TEXT NOT NULL,
  file_path TEXT NOT NULL,
  symbol_name TEXT NOT NULL,
  symbol_kind TEXT NOT NULL,
  language TEXT NOT NULL,
  line_start INT NOT NULL,
  line_end INT NOT NULL,
  signature TEXT,
  docstring TEXT,
  embedding vector(768),
  sha256 TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  CONSTRAINT symbols_uniq UNIQUE (repo, file_path, symbol_name, symbol_kind)
);

CREATE TABLE IF NOT EXISTS fabric.symbol_edges (
  src_id BIGINT NOT NULL REFERENCES fabric.symbols(id) ON DELETE CASCADE,
  dst_id BIGINT NOT NULL REFERENCES fabric.symbols(id) ON DELETE CASCADE,
  edge_kind TEXT NOT NULL,
  PRIMARY KEY (src_id, dst_id, edge_kind)
);

CREATE INDEX IF NOT EXISTS symbols_repo_idx ON fabric.symbols (repo) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS symbols_name_idx ON fabric.symbols (symbol_name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS symbols_file_idx ON fabric.symbols (repo, file_path) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS symbols_emb_ivf ON fabric.symbols USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);

-- ---------- v0.6 orchestrator ----------
CREATE TABLE IF NOT EXISTS fabric.sessions (
  id TEXT PRIMARY KEY,
  host TEXT NOT NULL,
  capabilities TEXT[] NOT NULL DEFAULT '{}',
  status TEXT NOT NULL DEFAULT 'idle',
  current_task_id BIGINT,
  last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now(),
  registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS fabric.tasks (
  id BIGSERIAL PRIMARY KEY,
  title TEXT NOT NULL,
  brief TEXT NOT NULL,
  required_capabilities TEXT[] NOT NULL DEFAULT '{}',
  assigned_session TEXT REFERENCES fabric.sessions(id) ON DELETE SET NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  result TEXT,
  created_by TEXT NOT NULL,
  origin_host TEXT NOT NULL DEFAULT 'local',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  claimed_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ
);

ALTER TABLE fabric.tasks ADD COLUMN IF NOT EXISTS origin_host TEXT NOT NULL DEFAULT 'local';

CREATE INDEX IF NOT EXISTS sessions_heartbeat_idx ON fabric.sessions (last_heartbeat DESC);
CREATE INDEX IF NOT EXISTS tasks_status_idx ON fabric.tasks (status, created_at);
CREATE INDEX IF NOT EXISTS tasks_assigned_idx ON fabric.tasks (assigned_session) WHERE status IN ('claimed','in_progress');

-- ---------- v0.7 federation ----------
CREATE TABLE IF NOT EXISTS fabric.federation_peers (
  id TEXT PRIMARY KEY,
  url TEXT NOT NULL,
  bearer_token TEXT NOT NULL,
  last_pull_at TIMESTAMPTZ,
  last_pull_high_water BIGINT NOT NULL DEFAULT 0
);

-- ---------- v0.8 router observations ----------
CREATE TABLE IF NOT EXISTS fabric.router_observations (
  id BIGSERIAL PRIMARY KEY,
  request_hash TEXT NOT NULL,
  task_category TEXT NOT NULL,
  model_id TEXT NOT NULL,
  cost_usd NUMERIC NOT NULL,
  latency_ms INT NOT NULL,
  outcome TEXT NOT NULL DEFAULT 'unknown',
  outcome_score REAL,
  observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS router_obs_category_model_idx
  ON fabric.router_observations (task_category, model_id, observed_at DESC);
`

type server struct {
	pool       *pgxpool.Pool
	apiKey     string
	ollamaURL  string
	httpClient *http.Client
}

// ---------- types ----------

type memoCreateReq struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Type    string   `json:"type"`
	Tags    []string `json:"tags"`
}

type memoUpdateReq struct {
	Title   *string   `json:"title,omitempty"`
	Content *string   `json:"content,omitempty"`
	Type    *string   `json:"type,omitempty"`
	Tags    *[]string `json:"tags,omitempty"`
}

type memoCreateResp struct {
	ID       int64  `json:"id"`
	SHA256   string `json:"sha256"`
	Deduped  bool   `json:"deduped"`
	Embedded bool   `json:"embedded"`
}

type searchReq struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
	Type  string `json:"type"`
	Mode  string `json:"mode"` // "hybrid" (default) | "tsvector" | "semantic"
}

type searchHit struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Excerpt   string    `json:"excerpt"`
	Score     float64   `json:"score"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

type coordSendReq struct {
	Sender    string `json:"sender"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

type coordMsg struct {
	ID         int64     `json:"id"`
	Sender     string    `json:"sender"`
	Recipient  string    `json:"recipient"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	Host       string    `json:"host"`
	OriginHost string    `json:"origin_host"`
	TS         time.Time `json:"ts"`
}

// ---------- v0.5 code graph types ----------

type symbolUpsertReq struct {
	Repo        string  `json:"repo"`
	FilePath    string  `json:"file_path"`
	SymbolName  string  `json:"symbol_name"`
	SymbolKind  string  `json:"symbol_kind"`
	Language    string  `json:"language"`
	LineStart   int     `json:"line_start"`
	LineEnd     int     `json:"line_end"`
	Signature   string  `json:"signature"`
	Docstring   string  `json:"docstring"`
	EmbedSource *string `json:"embed_source,omitempty"` // optional text to embed; defaults to signature+docstring
}

type symbolEdgeReq struct {
	SrcID    int64  `json:"src_id"`
	DstID    int64  `json:"dst_id"`
	EdgeKind string `json:"edge_kind"`
}

type symbolSearchReq struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
	Repo  string `json:"repo"`
	Kind  string `json:"symbol_kind"`
	Mode  string `json:"mode"` // hybrid | semantic | name
}

type symbolHit struct {
	ID         int64   `json:"id"`
	Repo       string  `json:"repo"`
	FilePath   string  `json:"file_path"`
	SymbolName string  `json:"symbol_name"`
	SymbolKind string  `json:"symbol_kind"`
	Language   string  `json:"language"`
	LineStart  int     `json:"line_start"`
	LineEnd    int     `json:"line_end"`
	Signature  string  `json:"signature"`
	Score      float64 `json:"score"`
}

// ---------- v0.6 orchestrator types ----------

type sessionRegisterReq struct {
	ID           string   `json:"id"`
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities"`
}

type sessionRow struct {
	ID            string    `json:"id"`
	Host          string    `json:"host"`
	Capabilities  []string  `json:"capabilities"`
	Status        string    `json:"status"`
	CurrentTaskID *int64    `json:"current_task_id,omitempty"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	RegisteredAt  time.Time `json:"registered_at"`
}

type taskCreateReq struct {
	Title                string   `json:"title"`
	Brief                string   `json:"brief"`
	RequiredCapabilities []string `json:"required_capabilities"`
	CreatedBy            string   `json:"created_by"`
}

type taskClaimReq struct {
	SessionID string `json:"session_id"`
}

type taskCompleteReq struct {
	Result string `json:"result"`
	Status string `json:"status"`
}

type taskRow struct {
	ID                   int64      `json:"id"`
	Title                string     `json:"title"`
	Brief                string     `json:"brief"`
	RequiredCapabilities []string   `json:"required_capabilities"`
	AssignedSession      *string    `json:"assigned_session,omitempty"`
	Status               string     `json:"status"`
	Result               *string    `json:"result,omitempty"`
	CreatedBy            string     `json:"created_by"`
	OriginHost           string     `json:"origin_host"`
	CreatedAt            time.Time  `json:"created_at"`
	ClaimedAt            *time.Time `json:"claimed_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
}

// ---------- v0.7 federation types ----------

type federationPeerReq struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	BearerToken string `json:"bearer_token"`
}

type federationPeerRow struct {
	ID                string     `json:"id"`
	URL               string     `json:"url"`
	LastPullAt        *time.Time `json:"last_pull_at,omitempty"`
	LastPullHighWater int64      `json:"last_pull_high_water"`
}

type federationImportMsg struct {
	Sender     string    `json:"sender"`
	Recipient  string    `json:"recipient"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	OriginHost string    `json:"origin_host"`
	TS         time.Time `json:"ts"`
}

// ---------- v0.8 router learning types ----------

type routerObservationReq struct {
	RequestHash  string   `json:"request_hash"`
	TaskCategory string   `json:"task_category"`
	ModelID      string   `json:"model_id"`
	CostUSD      float64  `json:"cost_usd"`
	LatencyMs    int      `json:"latency_ms"`
	Outcome      string   `json:"outcome"`
	OutcomeScore *float64 `json:"outcome_score,omitempty"`
}

type routerRecommendation struct {
	ModelID      string  `json:"model_id"`
	CostUSDAvg   float64 `json:"cost_usd_avg"`
	LatencyP50   float64 `json:"latency_p50"`
	SuccessRate  float64 `json:"success_rate"`
	SampleSize   int     `json:"sample_size"`
}

// ---------- Ollama embedding ----------

type ollamaEmbedReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResp struct {
	Embedding []float32 `json:"embedding"`
}

func (s *server) embed(ctx context.Context, text string) ([]float32, error) {
	// nomic-embed-text 8192-token ctx ~= 30KB; cap at 6KB to keep headroom
	if len(text) > 2000 {
		text = text[:2000]
	}
	body, _ := json.Marshal(ollamaEmbedReq{Model: embeddingModel, Prompt: text})
	req, err := http.NewRequestWithContext(ctx, "POST", s.ollamaURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, string(b))
	}
	var er ollamaEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	if len(er.Embedding) != embeddingDim {
		return nil, fmt.Errorf("ollama returned %d dim, expected %d", len(er.Embedding), embeddingDim)
	}
	return er.Embedding, nil
}

// ---------- helpers ----------

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) requireAuth(r *http.Request) bool {
	expected := "Bearer " + s.apiKey
	return r.Header.Get("Authorization") == expected
}

func excerpt(content string, n int) string {
	if len(content) <= n {
		return content
	}
	return content[:n] + "..."
}

// ---------- handlers ----------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	dbStatus := "ok"
	if err := s.pool.Ping(r.Context()); err != nil {
		dbStatus = "err: " + err.Error()[:80]
	}
	writeJSON(w, 200, map[string]any{
		"ok":             dbStatus == "ok",
		"version":        version,
		"db":             dbStatus,
		"embedding_model": embeddingModel,
		"embedding_dim":  embeddingDim,
	})
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req memoCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Content == "" {
		writeErr(w, 400, "content required")
		return
	}
	if req.Type == "" {
		req.Type = "general"
	}
	hash := sha256hex(req.Title + "\n" + req.Content)

	// Try insert; on conflict return existing id
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// Generate embedding (best-effort; on failure store NULL)
	var emb []float32
	if e, err := s.embed(ctx, req.Title+"\n"+req.Content); err == nil {
		emb = e
	} else {
		log.Printf("embed warn (memo): %v", err)
	}

	var id int64
	var deduped bool
	var inserted bool
	if emb != nil {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO fabric.memos (title, content, type, tags, sha256, embedding)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (sha256) DO UPDATE SET sha256 = EXCLUDED.sha256
			RETURNING id, (xmax = 0)`,
			req.Title, req.Content, req.Type, append([]string{}, req.Tags...), hash, pgvector.NewVector(emb),
		).Scan(&id, &inserted)
		if err != nil {
			writeErr(w, 500, "db: "+err.Error())
			return
		}
	} else {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO fabric.memos (title, content, type, tags, sha256)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (sha256) DO UPDATE SET sha256 = EXCLUDED.sha256
			RETURNING id, (xmax = 0)`,
			req.Title, req.Content, req.Type, append([]string{}, req.Tags...), hash,
		).Scan(&id, &inserted)
		if err != nil {
			writeErr(w, 500, "db: "+err.Error())
			return
		}
	}
	deduped = !inserted
	writeJSON(w, 200, memoCreateResp{ID: id, SHA256: hash, Deduped: deduped, Embedded: emb != nil})
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req memoUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// Read existing
	var title, content, mtype string
	var tags []string
	err := s.pool.QueryRow(ctx, `SELECT title, content, type, tags FROM fabric.memos WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&title, &content, &mtype, &tags)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, 404, "not found")
			return
		}
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	if req.Title != nil {
		title = *req.Title
	}
	if req.Content != nil {
		content = *req.Content
	}
	if req.Type != nil {
		mtype = *req.Type
	}
	if req.Tags != nil {
		tags = *req.Tags
	}
	hash := sha256hex(title + "\n" + content)
	// Re-embed
	var emb []float32
	if e, err := s.embed(ctx, title+"\n"+content); err == nil {
		emb = e
	}
	if emb != nil {
		_, err = s.pool.Exec(ctx, `UPDATE fabric.memos SET title=$1, content=$2, type=$3, tags=$4, sha256=$5, embedding=$6, updated_at=now() WHERE id=$7`,
			title, content, mtype, tags, hash, pgvector.NewVector(emb), id)
	} else {
		_, err = s.pool.Exec(ctx, `UPDATE fabric.memos SET title=$1, content=$2, type=$3, tags=$4, sha256=$5, updated_at=now() WHERE id=$6`,
			title, content, mtype, tags, hash, id)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "sha256": hash, "embedded": emb != nil})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `UPDATE fabric.memos SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "not found or already deleted")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var title, content, mtype string
	var tags []string
	var createdAt, updatedAt time.Time
	err := s.pool.QueryRow(ctx, `SELECT title, content, type, tags, created_at, updated_at FROM fabric.memos WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&title, &content, &mtype, &tags, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, 404, "not found")
			return
		}
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "title": title, "content": content, "type": mtype, "tags": tags,
		"created_at": createdAt, "updated_at": updatedAt,
	})
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req searchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Query == "" {
		writeErr(w, 400, "query required")
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	if req.Mode == "" {
		req.Mode = "hybrid"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Compute query embedding for semantic + hybrid modes
	var qEmb []float32
	if req.Mode == "semantic" || req.Mode == "hybrid" {
		if e, err := s.embed(ctx, req.Query); err == nil {
			qEmb = e
		} else {
			log.Printf("embed warn (search): %v", err)
			// fall back to tsvector if embedding fails
			req.Mode = "tsvector"
		}
	}

	var rows pgx.Rows
	var err error
	switch req.Mode {
	case "tsvector":
		rows, err = s.pool.Query(ctx, `
			SELECT id, title, content,
			       ts_rank(tsv, plainto_tsquery('english', $1)) * 0.7
			       + (1.0 / (1 + EXTRACT(EPOCH FROM (now()-created_at))/86400.0/30)) * 0.3 AS score,
			       type, created_at
			FROM fabric.memos
			WHERE deleted_at IS NULL
			  AND tsv @@ plainto_tsquery('english', $1)
			  AND ($2 = '' OR type = $2)
			ORDER BY score DESC LIMIT $3`,
			req.Query, req.Type, req.TopK)
	case "semantic":
		rows, err = s.pool.Query(ctx, `
			SELECT id, title, content,
			       (1.0 - (embedding <=> $1)) * 0.8
			       + (1.0 / (1 + EXTRACT(EPOCH FROM (now()-created_at))/86400.0/30)) * 0.2 AS score,
			       type, created_at
			FROM fabric.memos
			WHERE deleted_at IS NULL
			  AND embedding IS NOT NULL
			  AND ($2 = '' OR type = $2)
			ORDER BY embedding <=> $1 ASC LIMIT $3`,
			pgvector.NewVector(qEmb), req.Type, req.TopK)
	default: // hybrid
		rows, err = s.pool.Query(ctx, `
			SELECT id, title, content,
			       CASE WHEN embedding IS NULL THEN
			           ts_rank(tsv, plainto_tsquery('english', $2)) * 0.7
			           + (1.0 / (1 + EXTRACT(EPOCH FROM (now()-created_at))/86400.0/30)) * 0.3
			       ELSE
			           (1.0 - (embedding <=> $1)) * 0.5
			           + COALESCE(ts_rank(tsv, plainto_tsquery('english', $2)), 0) * 0.3
			           + (1.0 / (1 + EXTRACT(EPOCH FROM (now()-created_at))/86400.0/30)) * 0.2
			       END AS score,
			       type, created_at
			FROM fabric.memos
			WHERE deleted_at IS NULL
			  AND ($3 = '' OR type = $3)
			  AND (
			      embedding IS NOT NULL
			      OR tsv @@ plainto_tsquery('english', $2)
			  )
			ORDER BY score DESC LIMIT $4`,
			pgvector.NewVector(qEmb), req.Query, req.Type, req.TopK)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()

	results := []searchHit{}
	for rows.Next() {
		var h searchHit
		var content string
		if err := rows.Scan(&h.ID, &h.Title, &content, &h.Score, &h.Type, &h.CreatedAt); err != nil {
			continue
		}
		h.Excerpt = excerpt(content, 200)
		results = append(results, h)
	}
	writeJSON(w, 200, map[string]any{"results": results, "mode": req.Mode})
}

// ---------- coord endpoints ----------

func (s *server) handleCoordSend(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req coordSendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Sender == "" || req.Recipient == "" || req.Subject == "" {
		writeErr(w, 400, "sender, recipient, subject required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO public.coord_messages (sender, recipient, subject, body, host, origin_host)
		VALUES ($1, $2, $3, $4, $5, $5)
		RETURNING id`,
		req.Sender, req.Recipient, req.Subject, req.Body, host,
	).Scan(&id)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	// pg_notify trigger should already fire; do it explicitly too for safety
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('coord', $1)`, fmt.Sprintf(`{"id":%d,"sender":%q,"recipient":%q,"subject":%q}`, id, req.Sender, req.Recipient, req.Subject))
	writeJSON(w, 200, map[string]any{"id": id, "sent": true})
}

func (s *server) handleCoordRecent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	since := r.URL.Query().Get("since") // RFC3339 timestamp
	recipient := r.URL.Query().Get("recipient")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	query := `SELECT id, sender, recipient, subject, body, COALESCE(host,''), COALESCE(origin_host,'local'), ts FROM public.coord_messages WHERE 1=1`
	args := []any{}
	if since != "" {
		args = append(args, since)
		query += fmt.Sprintf(" AND ts > $%d", len(args))
	}
	if recipient != "" {
		args = append(args, recipient)
		query += fmt.Sprintf(" AND (recipient = $%d OR recipient = 'all' OR recipient = 'ALL')", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY ts DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	msgs := []coordMsg{}
	for rows.Next() {
		var m coordMsg
		if err := rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &m.Host, &m.OriginHost, &m.TS); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	writeJSON(w, 200, map[string]any{"messages": msgs, "count": len(msgs)})
}

// ---------- backfill ----------

func (s *server) handleBackfillEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	rows, err := s.pool.Query(ctx, `SELECT id, title, content FROM fabric.memos WHERE embedding IS NULL AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	type todo struct {
		ID      int64
		Title   string
		Content string
	}
	var pending []todo
	for rows.Next() {
		var t todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Content); err != nil {
			continue
		}
		pending = append(pending, t)
	}
	rows.Close()

	stats := map[string]int{"total": len(pending), "ok": 0, "fail": 0}
	for _, t := range pending {
		ec, ecancel := context.WithTimeout(ctx, 15*time.Second)
		emb, err := s.embed(ec, t.Title+"\n"+t.Content)
		ecancel()
		if err != nil {
			stats["fail"]++
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE fabric.memos SET embedding=$1 WHERE id=$2`, pgvector.NewVector(emb), t.ID); err != nil {
			stats["fail"]++
			continue
		}
		stats["ok"]++
	}
	writeJSON(w, 200, stats)
}

// ---------- v0.5 code graph handlers ----------

func (s *server) handleSymbolUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req symbolUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Repo == "" || req.FilePath == "" || req.SymbolName == "" || req.SymbolKind == "" || req.Language == "" {
		writeErr(w, 400, "repo, file_path, symbol_name, symbol_kind, language required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	embedText := req.Signature + "\n" + req.Docstring
	if req.EmbedSource != nil {
		embedText = *req.EmbedSource
	}
	hash := sha256hex(fmt.Sprintf("%s|%s|%s|%s|%d|%d|%s", req.Repo, req.FilePath, req.SymbolName, req.SymbolKind, req.LineStart, req.LineEnd, req.Signature))

	var emb []float32
	if strings.TrimSpace(embedText) != "" {
		if e, err := s.embed(ctx, embedText); err == nil {
			emb = e
		} else {
			log.Printf("embed warn (symbol): %v", err)
		}
	}

	var id int64
	var inserted bool
	if emb != nil {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO fabric.symbols (repo, file_path, symbol_name, symbol_kind, language, line_start, line_end, signature, docstring, embedding, sha256)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (repo, file_path, symbol_name, symbol_kind) DO UPDATE
			  SET language=EXCLUDED.language,
			      line_start=EXCLUDED.line_start,
			      line_end=EXCLUDED.line_end,
			      signature=EXCLUDED.signature,
			      docstring=EXCLUDED.docstring,
			      embedding=EXCLUDED.embedding,
			      sha256=EXCLUDED.sha256,
			      updated_at=now(),
			      deleted_at=NULL
			RETURNING id, (xmax = 0)`,
			req.Repo, req.FilePath, req.SymbolName, req.SymbolKind, req.Language,
			req.LineStart, req.LineEnd, req.Signature, req.Docstring, pgvector.NewVector(emb), hash,
		).Scan(&id, &inserted)
		if err != nil {
			writeErr(w, 500, "db: "+err.Error())
			return
		}
	} else {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO fabric.symbols (repo, file_path, symbol_name, symbol_kind, language, line_start, line_end, signature, docstring, sha256)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (repo, file_path, symbol_name, symbol_kind) DO UPDATE
			  SET language=EXCLUDED.language,
			      line_start=EXCLUDED.line_start,
			      line_end=EXCLUDED.line_end,
			      signature=EXCLUDED.signature,
			      docstring=EXCLUDED.docstring,
			      sha256=EXCLUDED.sha256,
			      updated_at=now(),
			      deleted_at=NULL
			RETURNING id, (xmax = 0)`,
			req.Repo, req.FilePath, req.SymbolName, req.SymbolKind, req.Language,
			req.LineStart, req.LineEnd, req.Signature, req.Docstring, hash,
		).Scan(&id, &inserted)
		if err != nil {
			writeErr(w, 500, "db: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": id, "inserted": inserted, "embedded": emb != nil})
}

func (s *server) handleSymbolEdge(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req symbolEdgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.SrcID == 0 || req.DstID == 0 || req.EdgeKind == "" {
		writeErr(w, 400, "src_id, dst_id, edge_kind required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO fabric.symbol_edges (src_id, dst_id, edge_kind)
		VALUES ($1,$2,$3)
		ON CONFLICT DO NOTHING`, req.SrcID, req.DstID, req.EdgeKind)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) handleSymbolSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req symbolSearchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Query == "" {
		writeErr(w, 400, "query required")
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	if req.Mode == "" {
		req.Mode = "hybrid"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var qEmb []float32
	if req.Mode == "semantic" || req.Mode == "hybrid" {
		if e, err := s.embed(ctx, req.Query); err == nil {
			qEmb = e
		} else {
			log.Printf("embed warn (symbol search): %v", err)
			req.Mode = "name"
		}
	}

	var rows pgx.Rows
	var err error
	switch req.Mode {
	case "name":
		rows, err = s.pool.Query(ctx, `
			SELECT id, repo, file_path, symbol_name, symbol_kind, language, line_start, line_end, COALESCE(signature,''),
			       CASE WHEN symbol_name = $1 THEN 1.0
			            WHEN symbol_name ILIKE $1 || '%' THEN 0.85
			            WHEN symbol_name ILIKE '%' || $1 || '%' THEN 0.6
			            ELSE 0.3 END AS score
			FROM fabric.symbols
			WHERE deleted_at IS NULL
			  AND ($2 = '' OR repo = $2)
			  AND ($3 = '' OR symbol_kind = $3)
			  AND symbol_name ILIKE '%' || $1 || '%'
			ORDER BY score DESC, symbol_name LIMIT $4`,
			req.Query, req.Repo, req.Kind, req.TopK)
	case "semantic":
		rows, err = s.pool.Query(ctx, `
			SELECT id, repo, file_path, symbol_name, symbol_kind, language, line_start, line_end, COALESCE(signature,''),
			       (1.0 - (embedding <=> $1)) AS score
			FROM fabric.symbols
			WHERE deleted_at IS NULL
			  AND embedding IS NOT NULL
			  AND ($2 = '' OR repo = $2)
			  AND ($3 = '' OR symbol_kind = $3)
			ORDER BY embedding <=> $1 ASC LIMIT $4`,
			pgvector.NewVector(qEmb), req.Repo, req.Kind, req.TopK)
	default: // hybrid: exact-name dominates; otherwise blend semantic + name signal
		rows, err = s.pool.Query(ctx, `
			SELECT id, repo, file_path, symbol_name, symbol_kind, language, line_start, line_end, COALESCE(signature,''),
			       CASE
			         WHEN symbol_name = $2 THEN
			           0.9 + COALESCE((1.0 - (embedding <=> $1)) * 0.1, 0.1)
			         WHEN symbol_name ILIKE $2 || '%' THEN
			           0.7 + COALESCE((1.0 - (embedding <=> $1)) * 0.2, 0.1)
			         WHEN symbol_name ILIKE '%' || $2 || '%' THEN
			           0.5 + COALESCE((1.0 - (embedding <=> $1)) * 0.3, 0.1)
			         WHEN embedding IS NOT NULL THEN
			           (1.0 - (embedding <=> $1)) * 0.6
			         ELSE 0.3
			       END AS score
			FROM fabric.symbols
			WHERE deleted_at IS NULL
			  AND ($3 = '' OR repo = $3)
			  AND ($4 = '' OR symbol_kind = $4)
			  AND (embedding IS NOT NULL OR symbol_name ILIKE '%' || $2 || '%')
			ORDER BY score DESC LIMIT $5`,
			pgvector.NewVector(qEmb), req.Query, req.Repo, req.Kind, req.TopK)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	hits := []symbolHit{}
	for rows.Next() {
		var h symbolHit
		if err := rows.Scan(&h.ID, &h.Repo, &h.FilePath, &h.SymbolName, &h.SymbolKind, &h.Language, &h.LineStart, &h.LineEnd, &h.Signature, &h.Score); err != nil {
			continue
		}
		hits = append(hits, h)
	}
	writeJSON(w, 200, map[string]any{"results": hits, "mode": req.Mode})
}

func (s *server) handleSymbolNeighbours(w http.ResponseWriter, r *http.Request, id int64, direction string) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var query string
	if direction == "callers" {
		// who calls this symbol → src for edges pointing at id
		query = `
			SELECT s.id, s.repo, s.file_path, s.symbol_name, s.symbol_kind, s.language, s.line_start, s.line_end, COALESCE(s.signature,''),
			       e.edge_kind
			FROM fabric.symbol_edges e
			JOIN fabric.symbols s ON s.id = e.src_id
			WHERE e.dst_id = $1 AND s.deleted_at IS NULL
			ORDER BY s.repo, s.file_path, s.symbol_name`
	} else {
		// callees: what this symbol calls
		query = `
			SELECT s.id, s.repo, s.file_path, s.symbol_name, s.symbol_kind, s.language, s.line_start, s.line_end, COALESCE(s.signature,''),
			       e.edge_kind
			FROM fabric.symbol_edges e
			JOIN fabric.symbols s ON s.id = e.dst_id
			WHERE e.src_id = $1 AND s.deleted_at IS NULL
			ORDER BY s.repo, s.file_path, s.symbol_name`
	}
	rows, err := s.pool.Query(ctx, query, id)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	type neighbour struct {
		symbolHit
		EdgeKind string `json:"edge_kind"`
	}
	out := []neighbour{}
	for rows.Next() {
		var n neighbour
		if err := rows.Scan(&n.ID, &n.Repo, &n.FilePath, &n.SymbolName, &n.SymbolKind, &n.Language, &n.LineStart, &n.LineEnd, &n.Signature, &n.EdgeKind); err != nil {
			continue
		}
		out = append(out, n)
	}
	writeJSON(w, 200, map[string]any{"results": out, "direction": direction, "count": len(out)})
}

func (s *server) handleSymbolReindex(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	// Body: {repo, file_path} — soft-delete any symbols in that file so the indexer can rewrite cleanly.
	var req struct {
		Repo     string `json:"repo"`
		FilePath string `json:"file_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Repo == "" || req.FilePath == "" {
		writeErr(w, 400, "repo and file_path required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `UPDATE fabric.symbols SET deleted_at = now() WHERE repo=$1 AND file_path=$2 AND deleted_at IS NULL`, req.Repo, req.FilePath)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"cleared": tag.RowsAffected(), "repo": req.Repo, "file_path": req.FilePath})
}

// ---------- v0.6 orchestrator handlers ----------

func (s *server) handleSessionRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req sessionRegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.ID == "" || req.Host == "" {
		writeErr(w, 400, "id and host required")
		return
	}
	if req.Capabilities == nil {
		req.Capabilities = []string{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO fabric.sessions (id, host, capabilities, status, last_heartbeat, registered_at)
		VALUES ($1,$2,$3,'idle', now(), now())
		ON CONFLICT (id) DO UPDATE SET
		  host=EXCLUDED.host,
		  capabilities=EXCLUDED.capabilities,
		  status=CASE WHEN fabric.sessions.status='offline' THEN 'idle' ELSE fabric.sessions.status END,
		  last_heartbeat=now()`,
		req.ID, req.Host, req.Capabilities)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "registered": true})
}

func (s *server) handleSessionHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
		UPDATE fabric.sessions
		SET last_heartbeat=now(),
		    status=CASE WHEN status='offline' THEN 'idle' ELSE status END
		WHERE id=$1`, id)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "session not registered")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "heartbeat": time.Now().UTC()})
}

func (s *server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	statusFilter := r.URL.Query().Get("status")
	capability := r.URL.Query().Get("capability")

	query := `SELECT id, host, capabilities,
	                 CASE WHEN last_heartbeat < now() - INTERVAL '90 seconds' THEN 'offline' ELSE status END AS effective_status,
	                 current_task_id, last_heartbeat, registered_at
	          FROM fabric.sessions WHERE 1=1`
	args := []any{}
	if statusFilter != "" {
		args = append(args, statusFilter)
		query += fmt.Sprintf(" AND (CASE WHEN last_heartbeat < now() - INTERVAL '90 seconds' THEN 'offline' ELSE status END) = $%d", len(args))
	}
	if capability != "" {
		args = append(args, capability)
		query += fmt.Sprintf(" AND $%d = ANY(capabilities)", len(args))
	}
	query += " ORDER BY last_heartbeat DESC"
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	out := []sessionRow{}
	for rows.Next() {
		var sr sessionRow
		var current *int64
		if err := rows.Scan(&sr.ID, &sr.Host, &sr.Capabilities, &sr.Status, &current, &sr.LastHeartbeat, &sr.RegisteredAt); err != nil {
			continue
		}
		sr.CurrentTaskID = current
		out = append(out, sr)
	}
	writeJSON(w, 200, map[string]any{"sessions": out, "count": len(out)})
}

func (s *server) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req taskCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Title == "" || req.Brief == "" || req.CreatedBy == "" {
		writeErr(w, 400, "title, brief, created_by required")
		return
	}
	if req.RequiredCapabilities == nil {
		req.RequiredCapabilities = []string{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO fabric.tasks (title, brief, required_capabilities, created_by, origin_host)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id`,
		req.Title, req.Brief, req.RequiredCapabilities, req.CreatedBy, host,
	).Scan(&id)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "created": true})
}

func (s *server) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req taskClaimReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.SessionID == "" {
		writeErr(w, 400, "session_id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Verify the session exists and pick out its capabilities.
	var caps []string
	err := s.pool.QueryRow(ctx, `SELECT capabilities FROM fabric.sessions WHERE id=$1`, req.SessionID).Scan(&caps)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, 404, "session not registered")
			return
		}
		writeErr(w, 500, "db: "+err.Error())
		return
	}

	// Atomically claim the next pending task whose required capabilities are a subset of this session's.
	var t taskRow
	var assigned, result *string
	var claimed, completed *time.Time
	err = s.pool.QueryRow(ctx, `
		UPDATE fabric.tasks
		SET status='claimed', assigned_session=$1, claimed_at=now()
		WHERE id = (
		  SELECT id FROM fabric.tasks
		  WHERE status='pending'
		    AND (required_capabilities = '{}'::TEXT[] OR required_capabilities <@ $2)
		  ORDER BY created_at
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1
		)
		RETURNING id, title, brief, required_capabilities, assigned_session, status, result, created_by, origin_host, created_at, claimed_at, completed_at`,
		req.SessionID, caps,
	).Scan(&t.ID, &t.Title, &t.Brief, &t.RequiredCapabilities, &assigned, &t.Status, &result, &t.CreatedBy, &t.OriginHost, &t.CreatedAt, &claimed, &completed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.WriteHeader(204)
			return
		}
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	t.AssignedSession = assigned
	t.Result = result
	t.ClaimedAt = claimed
	t.CompletedAt = completed

	// Mark the session busy + remember its current task.
	_, _ = s.pool.Exec(ctx, `UPDATE fabric.sessions SET status='busy', current_task_id=$1, last_heartbeat=now() WHERE id=$2`, t.ID, req.SessionID)

	writeJSON(w, 200, t)
}

func (s *server) handleTaskComplete(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req taskCompleteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	status := req.Status
	if status == "" {
		status = "done"
	}
	if status != "done" && status != "failed" && status != "in_progress" {
		writeErr(w, 400, "status must be done|failed|in_progress")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var assignedSession *string
	completedAt := pgxNullTime()
	if status != "in_progress" {
		completedAt = pgxNowTime()
	}
	err := s.pool.QueryRow(ctx, `
		UPDATE fabric.tasks
		SET status=$1, result=$2, completed_at=$3
		WHERE id=$4
		RETURNING assigned_session`,
		status, req.Result, completedAt, id,
	).Scan(&assignedSession)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, 404, "task not found")
			return
		}
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	if assignedSession != nil && status != "in_progress" {
		_, _ = s.pool.Exec(ctx, `UPDATE fabric.sessions SET status='idle', current_task_id=NULL, last_heartbeat=now() WHERE id=$1`, *assignedSession)
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": status})
}

func (s *server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	statusFilter := r.URL.Query().Get("status")
	assigned := r.URL.Query().Get("assigned")
	since := r.URL.Query().Get("since")

	query := `SELECT id, title, brief, required_capabilities, assigned_session, status, result, created_by, origin_host, created_at, claimed_at, completed_at
	          FROM fabric.tasks WHERE 1=1`
	args := []any{}
	if statusFilter != "" {
		args = append(args, statusFilter)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if assigned != "" {
		args = append(args, assigned)
		query += fmt.Sprintf(" AND assigned_session = $%d", len(args))
	}
	if since != "" {
		args = append(args, since)
		query += fmt.Sprintf(" AND created_at > $%d", len(args))
	}
	query += " ORDER BY created_at DESC LIMIT 200"
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	out := []taskRow{}
	for rows.Next() {
		var t taskRow
		var assignedSession, result *string
		var claimed, completed *time.Time
		if err := rows.Scan(&t.ID, &t.Title, &t.Brief, &t.RequiredCapabilities, &assignedSession, &t.Status, &result, &t.CreatedBy, &t.OriginHost, &t.CreatedAt, &claimed, &completed); err != nil {
			continue
		}
		t.AssignedSession = assignedSession
		t.Result = result
		t.ClaimedAt = claimed
		t.CompletedAt = completed
		out = append(out, t)
	}
	writeJSON(w, 200, map[string]any{"tasks": out, "count": len(out)})
}

// pgxNullTime / pgxNowTime: tiny helpers so we can pass either a NULL or now() through pgx.
func pgxNullTime() any { return nil }
func pgxNowTime() any  { return time.Now().UTC() }

// ---------- v0.7 federation handlers ----------

func (s *server) handleFederationCoordSince(w http.ResponseWriter, r *http.Request, sinceID int64) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	rows, err := s.pool.Query(ctx, `
		SELECT id, sender, recipient, subject, body, COALESCE(host,''), COALESCE(origin_host, $1), ts
		FROM public.coord_messages
		WHERE id > $2 AND COALESCE(origin_host, $1) = $1
		ORDER BY id ASC LIMIT 500`, host, sinceID)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	msgs := []coordMsg{}
	for rows.Next() {
		var m coordMsg
		if err := rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &m.Host, &m.OriginHost, &m.TS); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	writeJSON(w, 200, map[string]any{"messages": msgs, "count": len(msgs), "origin_host": host})
}

func (s *server) handleFederationCoordImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var msgs []federationImportMsg
	if err := json.NewDecoder(r.Body).Decode(&msgs); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	imported := 0
	for _, m := range msgs {
		if m.OriginHost == "" || m.OriginHost == host {
			// Skip messages with no origin marker or that originated here — never re-import our own.
			continue
		}
		ts := m.TS
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO public.coord_messages (sender, recipient, subject, body, host, origin_host, ts)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			m.Sender, m.Recipient, m.Subject, m.Body, m.OriginHost, m.OriginHost, ts)
		if err != nil {
			log.Printf("federation import warn: %v", err)
			continue
		}
		imported++
	}
	writeJSON(w, 200, map[string]any{"imported": imported, "received": len(msgs)})
}

func (s *server) handleFederationPeerUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req federationPeerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.ID == "" || req.URL == "" || req.BearerToken == "" {
		writeErr(w, 400, "id, url, bearer_token required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO fabric.federation_peers (id, url, bearer_token)
		VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET url=EXCLUDED.url, bearer_token=EXCLUDED.bearer_token`,
		req.ID, req.URL, req.BearerToken)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "registered": true})
}

func (s *server) handleFederationPeersList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT id, url, last_pull_at, last_pull_high_water FROM fabric.federation_peers ORDER BY id`)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	out := []federationPeerRow{}
	for rows.Next() {
		var p federationPeerRow
		var lpa *time.Time
		if err := rows.Scan(&p.ID, &p.URL, &lpa, &p.LastPullHighWater); err != nil {
			continue
		}
		p.LastPullAt = lpa
		out = append(out, p)
	}
	writeJSON(w, 200, map[string]any{"peers": out, "count": len(out)})
}

// ---------- v0.8 router learning handlers ----------

func (s *server) handleRouterObservation(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req routerObservationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.RequestHash == "" || req.TaskCategory == "" || req.ModelID == "" {
		writeErr(w, 400, "request_hash, task_category, model_id required")
		return
	}
	if req.Outcome == "" {
		req.Outcome = "unknown"
	}

	// v0.9 D4: if caller did not supply outcome_score, grade it automatically
	// so /v1/router/recommend ranks more sharply on partial telemetry.
	var graded *float64
	if req.OutcomeScore == nil {
		score := gradeOutcome(req)
		graded = &score
	} else {
		graded = req.OutcomeScore
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO fabric.router_observations (request_hash, task_category, model_id, cost_usd, latency_ms, outcome, outcome_score)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		req.RequestHash, req.TaskCategory, req.ModelID, req.CostUSD, req.LatencyMs, req.Outcome, graded,
	).Scan(&id)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "recorded": true, "outcome_score": graded, "graded": req.OutcomeScore == nil})
}

// gradeOutcome assigns a 0..1 quality score from coarse signals on the
// router observation. Conservative — explicit caller scores always override.
func gradeOutcome(req routerObservationReq) float64 {
	outcome := strings.ToLower(strings.TrimSpace(req.Outcome))
	switch outcome {
	case "failed", "error", "timeout":
		return 0.0
	case "partial":
		return 0.4
	}
	score := 0.9
	if outcome == "unknown" {
		score = 0.6
	}
	if req.LatencyMs > 60_000 {
		score -= 0.2
	}
	if req.CostUSD < 0 {
		score -= 0.3
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score
}

func (s *server) handleRouterRecommend(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	category := r.URL.Query().Get("category")
	if category == "" {
		writeErr(w, 400, "category required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT model_id,
		       AVG(cost_usd)::float8 AS cost_usd_avg,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms)::float8 AS latency_p50,
		       (SUM(CASE WHEN outcome='success' THEN 1 ELSE 0 END)::float8 / NULLIF(COUNT(*),0))::float8 AS success_rate,
		       COUNT(*)::int AS sample_size
		FROM fabric.router_observations
		WHERE task_category = $1 AND observed_at > now() - INTERVAL '30 days'
		GROUP BY model_id
		HAVING COUNT(*) >= 1`, category)
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()
	out := []routerRecommendation{}
	for rows.Next() {
		var rec routerRecommendation
		if err := rows.Scan(&rec.ModelID, &rec.CostUSDAvg, &rec.LatencyP50, &rec.SuccessRate, &rec.SampleSize); err != nil {
			continue
		}
		out = append(out, rec)
	}
	// Sort by (success_rate / cost_usd_avg) DESC — cheapest model meeting quality wins.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			si := out[i].SuccessRate / max(out[i].CostUSDAvg, 1e-9)
			sj := out[j].SuccessRate / max(out[j].CostUSDAvg, 1e-9)
			if sj > si {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	writeJSON(w, 200, map[string]any{"category": category, "recommendations": out})
}

// ---------- background workers ----------

// sessionsReaper marks sessions whose heartbeat is older than 90s as offline,
// and reverts tasks claimed by an offline session after 5 minutes of no progress.
func (s *server) sessionsReaper(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rc, cancel := context.WithTimeout(ctx, 10*time.Second)
			if _, err := s.pool.Exec(rc, `
				UPDATE fabric.sessions
				SET status='offline', current_task_id=NULL
				WHERE last_heartbeat < now() - INTERVAL '90 seconds' AND status <> 'offline'`); err != nil {
				log.Printf("reaper: sessions update warn: %v", err)
			}
			if _, err := s.pool.Exec(rc, `
				UPDATE fabric.tasks
				SET status='pending', assigned_session=NULL, claimed_at=NULL
				WHERE status IN ('claimed','in_progress')
				  AND claimed_at < now() - INTERVAL '5 minutes'
				  AND (assigned_session IS NULL OR assigned_session IN (
				        SELECT id FROM fabric.sessions WHERE status='offline'
				  ))`); err != nil {
				log.Printf("reaper: tasks revert warn: %v", err)
			}
			cancel()
		}
	}
}

// federationPoller polls each registered peer's /v1/federation/coord/since/:high_water every 5s
// and bulk-imports anything new.
func (s *server) federationPoller(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollPeersOnce(ctx)
		}
	}
}

func (s *server) pollPeersOnce(ctx context.Context) {
	rc, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(rc, `SELECT id, url, bearer_token, last_pull_high_water FROM fabric.federation_peers`)
	if err != nil {
		log.Printf("federation poll: list peers warn: %v", err)
		return
	}
	type peer struct {
		id        string
		url       string
		token     string
		highWater int64
	}
	var peers []peer
	for rows.Next() {
		var p peer
		if err := rows.Scan(&p.id, &p.url, &p.token, &p.highWater); err != nil {
			continue
		}
		peers = append(peers, p)
	}
	rows.Close()

	for _, p := range peers {
		s.pullFromPeer(ctx, p.id, p.url, p.token, p.highWater)
	}
}

func (s *server) pullFromPeer(ctx context.Context, peerID, peerURL, token string, highWater int64) {
	endpoint := strings.TrimRight(peerURL, "/") + "/v1/federation/coord/since/" + strconv.FormatInt(highWater, 10)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("federation poll %s: %v", peerID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("federation poll %s: status %d: %s", peerID, resp.StatusCode, string(body))
		return
	}
	var payload struct {
		Messages   []coordMsg `json:"messages"`
		OriginHost string     `json:"origin_host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		log.Printf("federation poll %s: decode: %v", peerID, err)
		return
	}
	if len(payload.Messages) == 0 {
		return
	}
	host, _ := os.Hostname()
	var newHigh int64 = highWater
	imported := 0
	for _, m := range payload.Messages {
		origin := m.OriginHost
		if origin == "" {
			origin = payload.OriginHost
		}
		if origin == "" || origin == host {
			continue
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO public.coord_messages (sender, recipient, subject, body, host, origin_host, ts)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			m.Sender, m.Recipient, m.Subject, m.Body, origin, origin, m.TS)
		if err != nil {
			log.Printf("federation import %s id=%d warn: %v", peerID, m.ID, err)
			continue
		}
		imported++
		if m.ID > newHigh {
			newHigh = m.ID
		}
	}
	if newHigh > highWater {
		if _, err := s.pool.Exec(ctx, `
			UPDATE fabric.federation_peers SET last_pull_at=now(), last_pull_high_water=$1 WHERE id=$2`,
			newHigh, peerID); err != nil {
			log.Printf("federation poll %s: high-water update warn: %v", peerID, err)
		}
		log.Printf("federation pull %s: imported=%d new_high_water=%d", peerID, imported, newHigh)
	}
}

// ---------- routing ----------

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/memo/search", s.handleSearch)
	mux.HandleFunc("/v1/memo/backfill", s.handleBackfillEmbeddings)
	mux.HandleFunc("/v1/memo/", func(w http.ResponseWriter, r *http.Request) {
		// /v1/memo/:id with GET/PUT/DELETE
		idStr := strings.TrimPrefix(r.URL.Path, "/v1/memo/")
		if idStr == "" {
			writeErr(w, 404, "memo id required")
			return
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeErr(w, 400, "bad id")
			return
		}
		switch r.Method {
		case "GET":
			s.handleGet(w, r, id)
		case "PUT":
			s.handleUpdate(w, r, id)
		case "DELETE":
			s.handleDelete(w, r, id)
		default:
			writeErr(w, 405, "method not allowed")
		}
	})
	mux.HandleFunc("/v1/memo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleCreate(w, r)
	})
	mux.HandleFunc("/v1/coord", s.handleCoordSend)
	mux.HandleFunc("/v1/coord/recent", s.handleCoordRecent)

	// ---------- v0.5 code graph ----------
	mux.HandleFunc("/v1/symbol/search", s.handleSymbolSearch)
	mux.HandleFunc("/v1/symbol/edge", s.handleSymbolEdge)
	mux.HandleFunc("/v1/symbol/reindex", s.handleSymbolReindex)
	mux.HandleFunc("/v1/symbol/", func(w http.ResponseWriter, r *http.Request) {
		// /v1/symbol/:id/callers | /v1/symbol/:id/callees
		rest := strings.TrimPrefix(r.URL.Path, "/v1/symbol/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && (parts[1] == "callers" || parts[1] == "callees") {
			id, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				writeErr(w, 400, "bad id")
				return
			}
			if r.Method != "GET" {
				writeErr(w, 405, "GET only")
				return
			}
			s.handleSymbolNeighbours(w, r, id, parts[1])
			return
		}
		writeErr(w, 404, "unknown symbol route")
	})
	mux.HandleFunc("/v1/symbol", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleSymbolUpsert(w, r)
	})

	// ---------- v0.6 orchestrator ----------
	mux.HandleFunc("/v1/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleSessionRegister(w, r)
	})
	mux.HandleFunc("/v1/sessions", s.handleSessionsList)
	mux.HandleFunc("/v1/session/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/session/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && parts[1] == "heartbeat" {
			if r.Method != "POST" {
				writeErr(w, 405, "POST only")
				return
			}
			s.handleSessionHeartbeat(w, r, parts[0])
			return
		}
		writeErr(w, 404, "unknown session route")
	})
	mux.HandleFunc("/v1/task", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleTaskCreate(w, r)
	})
	mux.HandleFunc("/v1/tasks", s.handleTasksList)
	mux.HandleFunc("/v1/task/claim", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleTaskClaim(w, r)
	})
	mux.HandleFunc("/v1/task/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/task/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && parts[1] == "complete" {
			id, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				writeErr(w, 400, "bad id")
				return
			}
			if r.Method != "POST" {
				writeErr(w, 405, "POST only")
				return
			}
			s.handleTaskComplete(w, r, id)
			return
		}
		writeErr(w, 404, "unknown task route")
	})

	// ---------- v0.7 federation ----------
	mux.HandleFunc("/v1/federation/coord/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleFederationCoordImport(w, r)
	})
	mux.HandleFunc("/v1/federation/coord/since/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writeErr(w, 405, "GET only")
			return
		}
		idStr := strings.TrimPrefix(r.URL.Path, "/v1/federation/coord/since/")
		sinceID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeErr(w, 400, "bad since id")
			return
		}
		s.handleFederationCoordSince(w, r, sinceID)
	})
	mux.HandleFunc("/v1/federation/peer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleFederationPeerUpsert(w, r)
	})
	mux.HandleFunc("/v1/federation/peers", s.handleFederationPeersList)

	// ---------- v0.8 router learning ----------
	mux.HandleFunc("/v1/router/observation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeErr(w, 405, "POST only")
			return
		}
		s.handleRouterObservation(w, r)
	})
	mux.HandleFunc("/v1/router/recommend", s.handleRouterRecommend)

	return mux
}

// ---------- boot ----------

func main() {
	apiKey := os.Getenv("FABRIC_KEY")
	if apiKey == "" {
		log.Fatal("FABRIC_KEY required")
	}
	dsn := os.Getenv("FABRIC_PG_DSN")
	if dsn == "" {
		log.Fatal("FABRIC_PG_DSN required")
	}
	listen := os.Getenv("FABRIC_LISTEN")
	if listen == "" {
		listen = ":8201"
	}
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pgx pool: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		log.Fatalf("schema apply failed: %v", err)
	}

	s := &server{
		pool:      pool,
		apiKey:    apiKey,
		ollamaURL: strings.TrimRight(ollamaURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	// Background workers — cancelled on process exit.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go s.sessionsReaper(bgCtx)
	go s.federationPoller(bgCtx)

	log.Printf("fabric v%s listening on %s (ollama=%s, model=%s, dim=%d)", version, listen, s.ollamaURL, embeddingModel, embeddingDim)
	if err := http.ListenAndServe(listen, s.routes()); err != nil {
		log.Fatal(err)
	}
}
