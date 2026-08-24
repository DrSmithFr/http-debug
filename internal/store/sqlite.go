package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
    id                  TEXT PRIMARY KEY,
    method              TEXT NOT NULL,
    url                 TEXT NOT NULL,
    status              TEXT NOT NULL,
    status_code         INTEGER,
    error               TEXT,
    request_format      TEXT,
    response_format     TEXT,
    is_ollama           INTEGER NOT NULL DEFAULT 0,
    is_replay           INTEGER NOT NULL DEFAULT 0,
    request_headers     TEXT,
    request_body        BLOB,
    request_body_path   TEXT,
    request_body_size   INTEGER NOT NULL DEFAULT 0,
    response_headers    TEXT,
    response_body       BLOB,
    response_body_path  TEXT,
    response_body_size  INTEGER NOT NULL DEFAULT 0,
    started_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    ttfb_ms             INTEGER,
    stream_ms           INTEGER,
    total_ms            INTEGER
);

CREATE INDEX IF NOT EXISTS idx_requests_started_at ON requests (started_at DESC);
`

// sqlDB holds the finished entries. The driver is a pure Go implementation so
// the binary still builds statically with CGO_ENABLED=0.
type sqlDB struct {
	db *sql.DB
}

func openDB(path string) (*sqlDB, error) {
	// WAL keeps the UI reading the list while an entry is being written.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// A single writer avoids `database is locked` under concurrent finalizes.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	return &sqlDB{db: db}, nil
}

func (s *sqlDB) Close() error { return s.db.Close() }

func (s *sqlDB) knownIDs() (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT id FROM requests`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func (s *sqlDB) insert(e *Entry) error {
	reqHeaders, err := marshalHeader(e.RequestHeaders)
	if err != nil {
		return err
	}
	respHeaders, err := marshalHeader(e.ResponseHeaders)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT OR REPLACE INTO requests (
			id, method, url, status, status_code, error,
			request_format, response_format, is_ollama, is_replay,
			request_headers, request_body, request_body_path, request_body_size,
			response_headers, response_body, response_body_path, response_body_size,
			started_at, finished_at, ttfb_ms, stream_ms, total_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Method, e.URL, string(e.Status), nullInt(int64(e.StatusCode), e.StatusCode != 0), nullString(e.Error),
		nullString(string(e.RequestFormat)), nullString(string(e.ResponseFormat)), e.IsOllama, e.IsReplay,
		reqHeaders, blobColumn(e.RequestBody, e.RequestSpilled), nullString(e.RequestBodyPath), e.RequestBodySize,
		respHeaders, blobColumn(e.ResponseBody, e.ResponseSpilled), nullString(e.ResponseBodyPath), e.ResponseBodySize,
		e.StartedAt.UnixMilli(), nullTime(e.FinishedAt), nullInt64(e.TTFBMs), nullInt64(e.StreamMs), nullInt64(e.TotalMs),
	)
	if err != nil {
		return fmt.Errorf("store: insert entry: %w", err)
	}
	return nil
}

const selectColumns = `
	id, method, url, status, status_code, error,
	request_format, response_format, is_ollama, is_replay,
	request_headers, request_body, request_body_path, request_body_size,
	response_headers, response_body, response_body_path, response_body_size,
	started_at, finished_at, ttfb_ms, stream_ms, total_ms`

func (s *sqlDB) get(id string) (*Entry, error) {
	row := s.db.QueryRow(`SELECT `+selectColumns+` FROM requests WHERE id = ?`, id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (s *sqlDB) list(limit int, before time.Time) ([]*Entry, error) {
	query := `SELECT ` + selectColumns + ` FROM requests ORDER BY started_at DESC LIMIT ?`
	args := []any{limit}
	if !before.IsZero() {
		query = `SELECT ` + selectColumns + ` FROM requests WHERE started_at < ? ORDER BY started_at DESC LIMIT ?`
		args = []any{before.UnixMilli(), limit}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// applyRetention trims the history down to maxEntries and reports the ids that
// were dropped, so their body files can be removed in the same operation.
func (s *sqlDB) applyRetention(maxEntries int) ([]string, error) {
	// The rows are collected and closed before deleting: the pool holds a
	// single connection, so an open cursor would block the writes below.
	victims, err := s.overflowIDs(maxEntries)
	if err != nil {
		return nil, err
	}
	for _, id := range victims {
		if _, err := s.db.Exec(`DELETE FROM requests WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return victims, nil
}

// overflowIDs lists the entries sitting beyond the retention cap.
func (s *sqlDB) overflowIDs(maxEntries int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM requests ORDER BY started_at DESC LIMIT -1 OFFSET ?`, maxEntries)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var victims []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		victims = append(victims, id)
	}
	return victims, rows.Err()
}

func (s *sqlDB) clear() ([]string, error) {
	ids, err := s.knownIDs()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM requests`); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out, nil
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(sc scanner) (*Entry, error) {
	var (
		e                             Entry
		statusCode                    sql.NullInt64
		errMsg, reqFormat, respFormat sql.NullString
		reqHeaders, respHeaders       sql.NullString
		reqBodyPath, respBodyPath     sql.NullString
		reqBody, respBody             []byte
		startedAt                     int64
		finishedAt                    sql.NullInt64
		ttfb, streamMs, totalMs       sql.NullInt64
		status                        string
	)
	err := sc.Scan(
		&e.ID, &e.Method, &e.URL, &status, &statusCode, &errMsg,
		&reqFormat, &respFormat, &e.IsOllama, &e.IsReplay,
		&reqHeaders, &reqBody, &reqBodyPath, &e.RequestBodySize,
		&respHeaders, &respBody, &respBodyPath, &e.ResponseBodySize,
		&startedAt, &finishedAt, &ttfb, &streamMs, &totalMs,
	)
	if err != nil {
		return nil, err
	}

	e.Status = Status(status)
	e.StatusCode = int(statusCode.Int64)
	e.Error = errMsg.String
	e.RequestFormat = Format(reqFormat.String)
	e.ResponseFormat = Format(respFormat.String)
	e.RequestBody, e.RequestBodyPath, e.RequestSpilled = reqBody, reqBodyPath.String, reqBodyPath.Valid
	e.ResponseBody, e.ResponseBodyPath, e.ResponseSpilled = respBody, respBodyPath.String, respBodyPath.Valid
	e.StartedAt = time.UnixMilli(startedAt)

	if e.RequestHeaders, err = unmarshalHeader(reqHeaders); err != nil {
		return nil, err
	}
	if e.ResponseHeaders, err = unmarshalHeader(respHeaders); err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		t := time.UnixMilli(finishedAt.Int64)
		e.FinishedAt = &t
	}
	e.TTFBMs = optInt64(ttfb)
	e.StreamMs = optInt64(streamMs)
	e.TotalMs = optInt64(totalMs)
	return &e, nil
}

func marshalHeader(h http.Header) (any, error) {
	if len(h) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func unmarshalHeader(v sql.NullString) (http.Header, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	var h http.Header
	if err := json.Unmarshal([]byte(v.String), &h); err != nil {
		return nil, err
	}
	return h, nil
}

// blobColumn stores the payload inline, or NULL once it lives on disk.
func blobColumn(body []byte, spilled bool) any {
	if spilled || len(body) == 0 {
		return nil
	}
	return body
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64, ok bool) any {
	if !ok {
		return nil
	}
	return v
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func optInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
