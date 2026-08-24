package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// Engine is one search backend under test.
type Engine interface {
	Name() string
	// Load wipes and bulk-loads the corpus, returning nothing; callers time it.
	Load(ctx context.Context, n int, p *plan) error
	// Search returns the top-k doc ids for a query, or ErrUnsupported.
	Search(ctx context.Context, q Query, k int) ([]int, error)
	// IndexBytes is the on-disk size of the search structures.
	IndexBytes(ctx context.Context) (int64, error)
}

var errUnsupported = fmt.Errorf("unsupported query shape")

func postJSON(ctx context.Context, method, url string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------------------------------------------------------------- Postgres

type pgEngine struct {
	dsn string
	db  *sql.DB
}

func newPG(dsn string) (*pgEngine, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	return &pgEngine{dsn: dsn, db: db}, nil
}

func (p *pgEngine) Name() string { return "postgres" }

func (p *pgEngine) Load(ctx context.Context, n int, pl *plan) error {
	for _, s := range []string{
		`DROP TABLE IF EXISTS docs`,
		// tuning: generated stored tsvector + GIN, built after the load
		`SET maintenance_work_mem = '1GB'`,
		`CREATE TABLE docs (
			id int PRIMARY KEY,
			title text NOT NULL,
			body text NOT NULL,
			tsv tsvector GENERATED ALWAYS AS
				(to_tsvector('english', title || ' ' || body)) STORED
		)`,
	} {
		if _, err := p.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SET maintenance_work_mem='1GB'`); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	st, err := tx.PrepareContext(ctx, `COPY docs (id, title, body) FROM STDIN`)
	if err != nil {
		return err
	}
	err = gen(n, pl, func(d Doc) error {
		_, e := st.ExecContext(ctx, d.ID, d.Title, d.Body)
		return e
	})
	if err != nil {
		return err
	}
	if _, err := st.ExecContext(ctx); err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, s := range []string{
		`CREATE INDEX docs_tsv_gin ON docs USING gin (tsv)`,
		`ANALYZE docs`,
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (p *pgEngine) Search(ctx context.Context, q Query, k int) ([]int, error) {
	if q.Shape == ShapeTypo {
		// Postgres FTS has no typo tolerance out of the box. Capability gap,
		// not a slow number.
		return nil, errUnsupported
	}
	parse := "plainto_tsquery"
	if q.Shape == ShapePhrase {
		parse = "phraseto_tsquery"
	}
	return p.query(ctx, `WITH t AS (SELECT `+parse+`('english',$1) q) `+
		`SELECT id FROM docs, t WHERE tsv @@ q ORDER BY ts_rank_cd(tsv, q) DESC LIMIT $2`,
		strings.Join(q.Terms, " "), k)
}

func (p *pgEngine) query(ctx context.Context, s, arg string, k int) ([]int, error) {
	rows, err := p.db.QueryContext(ctx, s, arg, k)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *pgEngine) IndexBytes(ctx context.Context) (int64, error) {
	var total, idx int64
	err := p.db.QueryRowContext(ctx,
		`SELECT pg_total_relation_size('docs'), pg_relation_size('docs_tsv_gin')`).Scan(&total, &idx)
	if err != nil {
		return 0, err
	}
	// report the whole stored footprint: table (incl. tsvector column) + GIN
	return total, nil
}

// ----------------------------------------------------------- Elasticsearch

type esEngine struct{ url string }

func (e *esEngine) Name() string { return "elasticsearch" }

func (e *esEngine) Load(ctx context.Context, n int, pl *plan) error {
	_ = postJSON(ctx, http.MethodDelete, e.url+"/docs", nil, nil)
	body := map[string]any{
		"settings": map[string]any{
			"number_of_shards": 1, "number_of_replicas": 0,
			"refresh_interval": "-1", // disabled during bulk load, restored after
		},
		"mappings": map[string]any{"properties": map[string]any{
			"title": map[string]any{"type": "text"},
			"body":  map[string]any{"type": "text"},
		}},
	}
	if err := postJSON(ctx, http.MethodPut, e.url+"/docs", body, nil); err != nil {
		return err
	}
	var buf bytes.Buffer
	count := 0
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/docs/_bulk", bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		var r struct {
			Errors bool `json:"errors"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return err
		}
		if r.Errors {
			return fmt.Errorf("bulk errors")
		}
		buf.Reset()
		return nil
	}
	enc := json.NewEncoder(&buf)
	err := gen(n, pl, func(d Doc) error {
		fmt.Fprintf(&buf, "{\"index\":{\"_id\":\"%d\"}}\n", d.ID)
		if err := enc.Encode(map[string]string{"title": d.Title, "body": d.Body}); err != nil {
			return err
		}
		count++
		if count%5000 == 0 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if err := postJSON(ctx, http.MethodPut, e.url+"/docs/_settings",
		map[string]any{"refresh_interval": "1s"}, nil); err != nil {
		return err
	}
	if err := postJSON(ctx, http.MethodPost, e.url+"/docs/_refresh", nil, nil); err != nil {
		return err
	}
	return postJSON(ctx, http.MethodPost, e.url+"/docs/_forcemerge?max_num_segments=1", nil, nil)
}

func (e *esEngine) Search(ctx context.Context, q Query, k int) ([]int, error) {
	text := strings.Join(q.Terms, " ")
	var query map[string]any
	switch q.Shape {
	case ShapePhrase:
		query = map[string]any{"multi_match": map[string]any{
			"query": text, "fields": []string{"title", "body"}, "type": "phrase"}}
	case ShapeTypo:
		query = map[string]any{"multi_match": map[string]any{
			"query": text, "fields": []string{"title", "body"}, "fuzziness": "AUTO"}}
	default:
		query = map[string]any{"multi_match": map[string]any{
			"query": text, "fields": []string{"title", "body"}, "operator": "and"}}
	}
	var res struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	err := postJSON(ctx, http.MethodPost, e.url+"/docs/_search",
		map[string]any{"size": k, "_source": false, "query": query}, &res)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, k)
	for _, h := range res.Hits.Hits {
		var id int
		if _, err := fmt.Sscanf(h.ID, "%d", &id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func (e *esEngine) IndexBytes(ctx context.Context) (int64, error) {
	var res struct {
		Indices map[string]struct {
			Total struct {
				Store struct {
					SizeInBytes int64 `json:"size_in_bytes"`
				} `json:"store"`
			} `json:"total"`
		} `json:"indices"`
	}
	if err := postJSON(ctx, http.MethodGet, e.url+"/docs/_stats/store", nil, &res); err != nil {
		return 0, err
	}
	return res.Indices["docs"].Total.Store.SizeInBytes, nil
}

// ------------------------------------------------------------- Meilisearch

type meiliEngine struct{ url, key string }

func (m *meiliEngine) Name() string { return "meilisearch" }

func (m *meiliEngine) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.url+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, b)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type meiliTask struct {
	TaskUID int    `json:"taskUid"`
	UID     int    `json:"uid"`
	Status  string `json:"status"`
	Error   any    `json:"error"`
}

func (m *meiliEngine) wait(ctx context.Context, uid int) error {
	for {
		var t meiliTask
		if err := m.do(ctx, http.MethodGet, fmt.Sprintf("/tasks/%d", uid), nil, &t); err != nil {
			return err
		}
		switch t.Status {
		case "succeeded":
			return nil
		case "failed", "canceled":
			return fmt.Errorf("meili task %d %s: %v", uid, t.Status, t.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *meiliEngine) Load(ctx context.Context, n int, pl *plan) error {
	_ = m.do(ctx, http.MethodDelete, "/indexes/docs", nil, nil)
	var t meiliTask
	if err := m.do(ctx, http.MethodPost, "/indexes",
		map[string]any{"uid": "docs", "primaryKey": "id"}, &t); err != nil {
		return err
	}
	if err := m.wait(ctx, t.TaskUID); err != nil {
		return err
	}
	if err := m.do(ctx, http.MethodPatch, "/indexes/docs/settings", map[string]any{
		"searchableAttributes": []string{"title", "body"},
		"displayedAttributes":  []string{"id"},
	}, &t); err != nil {
		return err
	}
	if err := m.wait(ctx, t.TaskUID); err != nil {
		return err
	}

	batch := make([]Doc, 0, 20000)
	var last int
	send := func() error {
		if len(batch) == 0 {
			return nil
		}
		var tt meiliTask
		if err := m.do(ctx, http.MethodPost, "/indexes/docs/documents", batch, &tt); err != nil {
			return err
		}
		last = tt.TaskUID
		batch = batch[:0]
		return nil
	}
	err := gen(n, pl, func(d Doc) error {
		batch = append(batch, d)
		if len(batch) == cap(batch) {
			return send()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := send(); err != nil {
		return err
	}
	return m.wait(ctx, last)
}

func (m *meiliEngine) Search(ctx context.Context, q Query, k int) ([]int, error) {
	text := strings.Join(q.Terms, " ")
	req := map[string]any{"q": text, "limit": k, "matchingStrategy": "all"}
	if q.Shape == ShapePhrase {
		req["q"] = `"` + text + `"`
	}
	var res struct {
		Hits []struct {
			ID int `json:"id"`
		} `json:"hits"`
	}
	if err := m.do(ctx, http.MethodPost, "/indexes/docs/search", req, &res); err != nil {
		return nil, err
	}
	out := make([]int, 0, k)
	for _, h := range res.Hits {
		out = append(out, h.ID)
	}
	return out, nil
}

func (m *meiliEngine) IndexBytes(ctx context.Context) (int64, error) {
	var res struct {
		DatabaseSize int64 `json:"databaseSize"`
		UsedSize     int64 `json:"usedDatabaseSize"`
	}
	if err := m.do(ctx, http.MethodGet, "/stats", nil, &res); err != nil {
		return 0, err
	}
	if res.UsedSize > 0 {
		return res.UsedSize, nil
	}
	return res.DatabaseSize, nil
}
