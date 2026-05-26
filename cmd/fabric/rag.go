package main

// v0.10 RAG endpoint (Router <-> Fabric platform integration).
//
// The Router product can optionally delegate its prompt-augmentation
// pre-stage to Fabric. When configured with fabric_url, Router POSTs the
// user prompt to /v1/rag and receives a ranked list of chunks suitable
// for splicing into the system prompt.
//
// Chunks are smaller than memos, content-addressed, and stored in their
// own table (fabric.chunks). Ranking weights are configurable per call
// because callers have different signal preferences -- e.g. Router asks
// for pure cosine on code chunks; Fabric's memo-search default of
// cosine 0.5 + tsvector 0.3 + recency 0.2 is wrong for that workload.
//
// Population of fabric.chunks is a separate workstream. This endpoint
// returns a clean empty list when the table is empty so the integration
// can be wired and smoke-tested today.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
)

// ---------- types ----------

type ragFilters struct {
	SourcePaths []string `json:"source_paths,omitempty"`
	Type        string   `json:"type,omitempty"`
	MinScore    float64  `json:"min_score,omitempty"`
}

type ragWeights struct {
	Cosine   float64 `json:"cosine"`
	TSVector float64 `json:"tsvector"`
	Recency  float64 `json:"recency"`
}

// normalised returns the weights with negatives clamped to 0 and the
// sum forced to a positive total (defaulting to pure cosine when the
// caller sends all zeros, so we always produce a deterministic ranking).
func (w ragWeights) normalised() ragWeights {
	if w.Cosine < 0 {
		w.Cosine = 0
	}
	if w.TSVector < 0 {
		w.TSVector = 0
	}
	if w.Recency < 0 {
		w.Recency = 0
	}
	if w.Cosine == 0 && w.TSVector == 0 && w.Recency == 0 {
		w.Cosine = 1.0
	}
	return w
}

func (w ragWeights) explain() string {
	return fmt.Sprintf("cosine=%g, tsvector=%g, recency=%g", w.Cosine, w.TSVector, w.Recency)
}

type ragReq struct {
	Q              string     `json:"q"`
	TopK           int        `json:"top_k"`
	Weights        ragWeights `json:"weights"`
	Filters        ragFilters `json:"filters"`
	MaxChunkTokens int        `json:"max_chunk_tokens"`
}

type ragChunk struct {
	ID       int64           `json:"id"`
	Content  string          `json:"content"`
	Source   string          `json:"source"`
	Score    float64         `json:"score"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type ragResp struct {
	Chunks              []ragChunk `json:"chunks"`
	TotalTokensEstimate int        `json:"total_tokens_estimate"`
	RankingExplanation  string     `json:"ranking_explanation"`
}

// ---------- handler ----------

func (s *server) handleRAG(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req ragReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Q) == "" {
		writeErr(w, 400, "q required")
		return
	}
	if req.TopK <= 0 {
		req.TopK = 8
	}
	if req.TopK > 64 {
		req.TopK = 64
	}
	if req.MaxChunkTokens <= 0 {
		req.MaxChunkTokens = 400
	}
	weights := req.Weights.normalised()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Embed the query (best-effort). If embedding fails AND cosine has
	// weight we drop cosine to zero and continue, so the endpoint stays
	// useful even when ollama is down.
	var qEmb []float32
	if weights.Cosine > 0 {
		if e, err := s.embed(ctx, req.Q); err == nil {
			qEmb = e
		} else {
			log.Printf("rag embed warn: %v", err)
			weights.Cosine = 0
			if weights.TSVector == 0 && weights.Recency == 0 {
				weights.TSVector = 1.0
			}
		}
	}

	// Build the ranking SQL. We pre-filter to candidates that match at
	// least one signal (have an embedding, or hit tsvector). Score is a
	// weighted sum; for cosine we use 1 - distance, for tsvector
	// ts_rank_cd, for recency a 30-day exponential-ish decay.
	args := []any{}
	scoreParts := []string{}
	whereParts := []string{}
	// Embedding-related arg slot is index 1 (if used), then query text.
	if weights.Cosine > 0 && qEmb != nil {
		args = append(args, pgvector.NewVector(qEmb))
		scoreParts = append(scoreParts,
			fmt.Sprintf("COALESCE((1.0 - (embedding <=> $%d)), 0) * %g", len(args), weights.Cosine))
	}
	if weights.TSVector > 0 {
		args = append(args, req.Q)
		scoreParts = append(scoreParts,
			fmt.Sprintf("COALESCE(ts_rank_cd(tsv, plainto_tsquery('english', $%d)), 0) * %g", len(args), weights.TSVector))
	}
	if weights.Recency > 0 {
		scoreParts = append(scoreParts,
			fmt.Sprintf("(1.0 / (1 + EXTRACT(EPOCH FROM (now()-created_at))/86400.0/30)) * %g", weights.Recency))
	}
	if len(scoreParts) == 0 {
		// Shouldn't happen after normalisation, but defend.
		scoreParts = append(scoreParts, "0")
	}
	scoreExpr := strings.Join(scoreParts, " + ")

	// Candidate filter: embedding present (if we have qEmb) OR tsv hit (if we have q text).
	if weights.Cosine > 0 && qEmb != nil {
		whereParts = append(whereParts, "embedding IS NOT NULL")
	}
	if weights.TSVector > 0 {
		// Use placeholder for the same query text we already pushed; rebuild safely.
		// To avoid duplicate placeholder issues, just plainto on the literal again.
		args = append(args, req.Q)
		whereParts = append(whereParts, fmt.Sprintf("tsv @@ plainto_tsquery('english', $%d)", len(args)))
	}
	candidateWhere := ""
	if len(whereParts) > 0 {
		candidateWhere = "AND (" + strings.Join(whereParts, " OR ") + ")"
	}

	// Source-path filter: case-insensitive ILIKE on any provided pattern.
	// We translate shell-style globs ("src/**/*.go") into SQL LIKE patterns
	// in a small, conservative way: '**' -> '%', '*' -> '%'. Fancier glob
	// semantics are left to the caller's chunk-ingestion strategy.
	pathFilter := ""
	if len(req.Filters.SourcePaths) > 0 {
		patterns := []string{}
		for _, p := range req.Filters.SourcePaths {
			args = append(args, globToLike(p))
			patterns = append(patterns, fmt.Sprintf("source_path ILIKE $%d", len(args)))
		}
		pathFilter = "AND (" + strings.Join(patterns, " OR ") + ")"
	}

	args = append(args, req.TopK)
	topKIdx := len(args)

	query := fmt.Sprintf(`
SELECT id, source_path, source_range, content, metadata,
       (%s) AS score
FROM fabric.chunks
WHERE 1=1
  %s
  %s
ORDER BY score DESC
LIMIT $%d`, scoreExpr, candidateWhere, pathFilter, topKIdx)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		// If chunks table is missing for any reason, return empty cleanly.
		if strings.Contains(err.Error(), "fabric.chunks") {
			writeJSON(w, 200, ragResp{Chunks: []ragChunk{}, RankingExplanation: weights.explain()})
			return
		}
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	defer rows.Close()

	chunks := []ragChunk{}
	totalChars := 0
	for rows.Next() {
		var c ragChunk
		var sourcePath, sourceRange, content string
		var rawMD []byte
		if err := rows.Scan(&c.ID, &sourcePath, &sourceRange, &content, &rawMD, &c.Score); err != nil {
			continue
		}
		if req.Filters.MinScore > 0 && c.Score < req.Filters.MinScore {
			continue
		}
		// Trim per-chunk content to max_chunk_tokens (rough 4 chars/token).
		maxChars := req.MaxChunkTokens * 4
		if maxChars > 0 && len(content) > maxChars {
			content = content[:maxChars] + "..."
		}
		c.Content = content
		if sourceRange != "" {
			c.Source = sourcePath + ":" + sourceRange
		} else {
			c.Source = sourcePath
		}
		if len(rawMD) > 0 {
			c.Metadata = rawMD
		}
		chunks = append(chunks, c)
		totalChars += len(content)
	}

	writeJSON(w, 200, ragResp{
		Chunks:              chunks,
		TotalTokensEstimate: totalChars / 4,
		RankingExplanation:  weights.explain(),
	})
}

// ---------- chunk upsert (companion endpoint for the ingestion workstream) ----------

type chunkUpsertReq struct {
	SourcePath  string                 `json:"source_path"`
	SourceRange string                 `json:"source_range"`
	Content     string                 `json:"content"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

type chunkUpsertResp struct {
	ID       int64  `json:"id"`
	SHA256   string `json:"sha256"`
	Deduped  bool   `json:"deduped"`
	Embedded bool   `json:"embedded"`
}

func (s *server) handleChunkUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(r) {
		writeErr(w, 401, "unauthorised")
		return
	}
	var req chunkUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Content == "" || req.SourcePath == "" {
		writeErr(w, 400, "source_path and content required")
		return
	}
	hashBytes := sha256.Sum256([]byte(req.SourcePath + "\n" + req.SourceRange + "\n" + req.Content))
	hash := hex.EncodeToString(hashBytes[:])

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var emb []float32
	if e, err := s.embed(ctx, req.Content); err == nil {
		emb = e
	} else {
		log.Printf("rag chunk embed warn: %v", err)
	}

	mdJSON := []byte("{}")
	if req.Metadata != nil {
		if b, err := json.Marshal(req.Metadata); err == nil {
			mdJSON = b
		}
	}

	var id int64
	var inserted bool
	var err error
	if emb != nil {
		err = s.pool.QueryRow(ctx, `
			INSERT INTO fabric.chunks (source_path, source_range, content, content_sha256, metadata, embedding)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)
			ON CONFLICT (content_sha256) DO UPDATE SET content_sha256 = EXCLUDED.content_sha256
			RETURNING id, (xmax = 0)`,
			req.SourcePath, req.SourceRange, req.Content, hash, string(mdJSON), pgvector.NewVector(emb),
		).Scan(&id, &inserted)
	} else {
		err = s.pool.QueryRow(ctx, `
			INSERT INTO fabric.chunks (source_path, source_range, content, content_sha256, metadata)
			VALUES ($1, $2, $3, $4, $5::jsonb)
			ON CONFLICT (content_sha256) DO UPDATE SET content_sha256 = EXCLUDED.content_sha256
			RETURNING id, (xmax = 0)`,
			req.SourcePath, req.SourceRange, req.Content, hash, string(mdJSON),
		).Scan(&id, &inserted)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	writeJSON(w, 200, chunkUpsertResp{ID: id, SHA256: hash, Deduped: !inserted, Embedded: emb != nil})
}

// ---------- helpers ----------

// globToLike converts a simple shell-style glob to a SQL LIKE pattern.
// Conservative: '**' and '*' both map to '%'. '?' maps to '_'. Other
// characters pass through. Good enough for the source_paths filter on
// /v1/rag; the caller can also send plain LIKE patterns directly.
func globToLike(glob string) string {
	if glob == "" {
		return "%"
	}
	// Replace ** first to avoid double-substituting.
	g := strings.ReplaceAll(glob, "**", "%")
	g = strings.ReplaceAll(g, "*", "%")
	g = strings.ReplaceAll(g, "?", "_")
	return g
}

// silence unused: pgx import is used elsewhere; this var keeps go vet happy
// if a future refactor strips it from this file's direct use.
var _ = pgx.ErrNoRows
