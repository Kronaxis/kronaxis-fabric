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

const version = "0.2.0"

const embeddingDim = 768
const embeddingModel = "nomic-embed-text"

const schemaSQL = `
CREATE SCHEMA IF NOT EXISTS fabric;
CREATE EXTENSION IF NOT EXISTS vector;

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

-- coord_messages may already exist in public; we leave it alone here.
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
	ID        int64     `json:"id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Host      string    `json:"host"`
	TS        time.Time `json:"ts"`
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
		INSERT INTO public.coord_messages (sender, recipient, subject, body, host)
		VALUES ($1, $2, $3, $4, $5)
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

	query := `SELECT id, sender, recipient, subject, body, COALESCE(host,''), ts FROM public.coord_messages WHERE 1=1`
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
		if err := rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &m.Host, &m.TS); err != nil {
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

	log.Printf("fabric v%s listening on %s (ollama=%s, model=%s, dim=%d)", version, listen, s.ollamaURL, embeddingModel, embeddingDim)
	if err := http.ListenAndServe(listen, s.routes()); err != nil {
		log.Fatal(err)
	}
}
