// Package store keeps the captured requests. Pending entries live in memory,
// finished ones are flushed to SQLite. Callers use the Store API and never need
// to know where a given entry currently lives.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrNotFound is returned when no entry matches the requested id.
var ErrNotFound = errors.New("entry not found")

// Status is the lifecycle state of an entry.
type Status string

const (
	StatusPending Status = "pending"
	StatusDone    Status = "done"
	StatusError   Status = "error"
)

// Format is the payload format detected on a request or response body.
type Format string

const (
	FormatJSON   Format = "json"
	FormatXML    Format = "xml"
	FormatNDJSON Format = "ndjson"
	FormatSSE    Format = "sse"
	FormatRaw    Format = "raw"
)

// Side selects the request or the response half of an entry.
type Side string

const (
	SideRequest  Side = "request"
	SideResponse Side = "response"
)

// Entry is one captured exchange, shared by the memory and the SQLite backends.
type Entry struct {
	ID              string      `json:"id"`
	Method          string      `json:"method"`
	URL             string      `json:"url"`
	Status          Status      `json:"status"`
	StatusCode      int         `json:"status_code,omitempty"`
	Error           string      `json:"error,omitempty"`
	RequestFormat   Format      `json:"request_format,omitempty"`
	ResponseFormat  Format      `json:"response_format,omitempty"`
	IsOllama        bool        `json:"is_ollama"`
	IsReplay        bool        `json:"is_replay"`
	RequestHeaders  http.Header `json:"request_headers,omitempty"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`

	// Body fields hold the whole payload when it stayed under the inline
	// threshold, and only a leading preview once it spilled to disk.
	RequestBody      []byte `json:"-"`
	RequestBodyPath  string `json:"-"`
	RequestBodySize  int64  `json:"request_body_size"`
	RequestSpilled   bool   `json:"-"`
	ResponseBody     []byte `json:"-"`
	ResponseBodyPath string `json:"-"`
	ResponseBodySize int64  `json:"response_body_size"`
	ResponseSpilled  bool   `json:"-"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	TTFBMs     *int64     `json:"ttfb_ms,omitempty"`
	StreamMs   *int64     `json:"stream_ms,omitempty"`
	TotalMs    *int64     `json:"total_ms,omitempty"`
}

// Clone returns a deep-enough copy for safe use outside the store lock.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	c := *e
	c.RequestHeaders = cloneHeader(e.RequestHeaders)
	c.ResponseHeaders = cloneHeader(e.ResponseHeaders)
	c.RequestBody = append([]byte(nil), e.RequestBody...)
	c.ResponseBody = append([]byte(nil), e.ResponseBody...)
	if e.FinishedAt != nil {
		t := *e.FinishedAt
		c.FinishedAt = &t
	}
	c.TTFBMs = cloneInt64(e.TTFBMs)
	c.StreamMs = cloneInt64(e.StreamMs)
	c.TotalMs = cloneInt64(e.TotalMs)
	return &c
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	return h.Clone()
}

func cloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

// Options configures a Store.
type Options struct {
	// DataDir holds the SQLite database and the blobs directory.
	DataDir string
	// MaxInlineBodySize is the size past which a body is written to disk.
	MaxInlineBodySize int64
	// MaxEntries caps the number of finished entries kept in the database.
	MaxEntries int
}

// Store arbitrates between the in-memory pending entries and the SQLite history.
type Store struct {
	mem    *memStore
	db     *sqlDB
	blobs  *blobStore
	events *Broker
	opts   Options
}

// New opens the database and prepares the blobs directory.
func New(opts Options) (*Store, error) {
	if opts.DataDir == "" {
		return nil, errors.New("store: DataDir is required")
	}
	if opts.MaxInlineBodySize <= 0 {
		opts.MaxInlineBodySize = 1 << 20
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 1000
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	blobs, err := newBlobStore(filepath.Join(opts.DataDir, "blobs"), opts.MaxInlineBodySize)
	if err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(opts.DataDir, "requests.db"))
	if err != nil {
		return nil, err
	}
	s := &Store{
		mem:    newMemStore(),
		db:     db,
		blobs:  blobs,
		events: NewBroker(),
		opts:   opts,
	}
	if err := s.blobs.removeOrphans(db.knownIDs); err != nil {
		return nil, err
	}
	return s, nil
}

// Events exposes the event broker feeding the SSE stream.
func (s *Store) Events() *Broker { return s.events }

// Close releases the database handle and the broker.
func (s *Store) Close() error {
	s.events.Close()
	return s.db.Close()
}

// Put registers a new pending entry, assigns it an id and publishes `created`.
func (s *Store) Put(e *Entry) (string, error) {
	if e.ID == "" {
		id, err := newID()
		if err != nil {
			return "", err
		}
		e.ID = id
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now()
	}
	if e.Status == "" {
		e.Status = StatusPending
	}
	live := s.mem.put(e, s.blobs)
	s.events.Publish(Event{Type: EventCreated, ID: e.ID, Entry: live.snapshot()})
	return e.ID, nil
}

// SetRequestBody stores the request payload, spilling to disk when oversized.
func (s *Store) SetRequestBody(id string, body []byte) error {
	live, ok := s.mem.get(id)
	if !ok {
		return ErrNotFound
	}
	_, err := live.req.Write(body)
	return err
}

// AppendResponse appends a response fragment. When emitDelta is set the chunk is
// also published on the event stream, which is how the UI follows a live stream.
func (s *Store) AppendResponse(id string, chunk []byte, emitDelta bool) error {
	live, ok := s.mem.get(id)
	if !ok {
		return ErrNotFound
	}
	if _, err := live.resp.Write(chunk); err != nil {
		return err
	}
	live.touch()
	if emitDelta && len(chunk) > 0 {
		s.events.Publish(Event{Type: EventDelta, ID: id, Chunk: string(chunk)})
	}
	return nil
}

// Update mutates a pending entry under lock and publishes `updated`.
func (s *Store) Update(id string, fn func(*Entry)) error {
	live, ok := s.mem.get(id)
	if !ok {
		return ErrNotFound
	}
	live.update(fn)
	e := live.snapshot()
	s.events.Publish(Event{Type: EventUpdated, ID: id, Entry: e})
	return nil
}

// Finalize closes an entry: bodies are flushed, the row is written to the
// database, the memory copy is dropped and retention is applied.
func (s *Store) Finalize(id string, fn func(*Entry)) error {
	live, ok := s.mem.get(id)
	if !ok {
		return ErrNotFound
	}
	live.update(func(e *Entry) {
		if fn != nil {
			fn(e)
		}
		if e.Status == StatusPending {
			e.Status = StatusDone
		}
		if e.FinishedAt == nil {
			now := time.Now()
			e.FinishedAt = &now
		}
		if e.TotalMs == nil {
			ms := e.FinishedAt.Sub(e.StartedAt).Milliseconds()
			e.TotalMs = &ms
		}
	})
	live.req.Close()
	live.resp.Close()

	e := live.snapshot()
	if err := s.db.insert(e); err != nil {
		return err
	}
	s.mem.delete(id)
	s.events.Publish(Event{Type: EventUpdated, ID: id, Entry: e})

	purged, err := s.db.applyRetention(s.opts.MaxEntries)
	if err != nil {
		return err
	}
	for _, id := range purged {
		s.blobs.removeFor(id)
		s.events.Publish(Event{Type: EventDeleted, ID: id})
	}
	return nil
}

// Get returns an entry, looking in memory first and falling back to the database.
func (s *Store) Get(id string) (*Entry, error) {
	if live, ok := s.mem.get(id); ok {
		return live.snapshot(), nil
	}
	return s.db.get(id)
}

// List returns entries newest first. A non-zero before acts as a cursor on
// StartedAt, returning only entries strictly older than it.
func (s *Store) List(limit int, before time.Time) ([]*Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.list(limit, before)
	if err != nil {
		return nil, err
	}
	merged := append(s.mem.list(before), rows...)
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].StartedAt.After(merged[j].StartedAt)
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// Body opens the raw payload of one side of an entry.
func (s *Store) Body(id string, side Side) (io.ReadCloser, int64, error) {
	e, err := s.Get(id)
	if err != nil {
		return nil, 0, err
	}
	path, inline, size := e.ResponseBodyPath, e.ResponseBody, e.ResponseBodySize
	if side == SideRequest {
		path, inline, size = e.RequestBodyPath, e.RequestBody, e.RequestBodySize
	}
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		return f, size, nil
	}
	return io.NopCloser(newBytesReader(inline)), size, nil
}

// Clear drops the whole history, database rows and body files alike.
func (s *Store) Clear() error {
	ids, err := s.db.clear()
	if err != nil {
		return err
	}
	if err := s.blobs.removeAll(); err != nil {
		return err
	}
	for _, id := range ids {
		s.events.Publish(Event{Type: EventDeleted, ID: id})
	}
	return nil
}

// SweepPending closes in error every pending entry idle for longer than timeout,
// so a backend that never answers cannot pile up in memory forever.
func (s *Store) SweepPending(timeout time.Duration) int {
	stale := s.mem.staleIDs(timeout)
	for _, id := range stale {
		_ = s.Finalize(id, func(e *Entry) {
			e.Status = StatusError
			if e.Error == "" {
				e.Error = fmt.Sprintf("no response after %s", timeout)
			}
		})
	}
	return len(stale)
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
