package store

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustPut(t *testing.T, s *Store, e *Entry) string {
	t.Helper()
	id, err := s.Put(e)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return id
}

// TestEntryLifecycle walks an entry from creation to finalization and checks it
// is served from memory first, then from the database.
func TestEntryLifecycle(t *testing.T) {
	s := newTestStore(t, Options{})

	events, unsubscribe := s.Events().Subscribe()
	defer unsubscribe()

	id := mustPut(t, s, &Entry{
		Method:         http.MethodPost,
		URL:            "http://backend/api/chat",
		RequestHeaders: http.Header{"Content-Type": {"application/json"}},
		RequestFormat:  FormatJSON,
		IsOllama:       true,
		StartedAt:      time.Now(),
	})

	if ev := <-events; ev.Type != EventCreated || ev.ID != id {
		t.Fatalf("expected created event for %s, got %v/%s", id, ev.Type, ev.ID)
	}

	if err := s.SetRequestBody(id, []byte(`{"model":"llama3"}`)); err != nil {
		t.Fatalf("SetRequestBody: %v", err)
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get while pending: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if string(got.RequestBody) != `{"model":"llama3"}` {
		t.Errorf("request body = %q", got.RequestBody)
	}

	if err := s.AppendResponse(id, []byte(`{"response":"hi"}`+"\n"), true); err != nil {
		t.Fatalf("AppendResponse: %v", err)
	}
	if ev := <-events; ev.Type != EventDelta || ev.Chunk != `{"response":"hi"}`+"\n" {
		t.Fatalf("expected delta event, got %v %q", ev.Type, ev.Chunk)
	}

	if err := s.Update(id, func(e *Entry) { e.StatusCode = http.StatusOK }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if ev := <-events; ev.Type != EventUpdated {
		t.Fatalf("expected updated event, got %v", ev.Type)
	}

	if err := s.Finalize(id, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// The memory copy is gone; the entry is now served from the database.
	if _, ok := s.mem.get(id); ok {
		t.Error("entry still in memory after Finalize")
	}
	got, err = s.Get(id)
	if err != nil {
		t.Fatalf("Get after Finalize: %v", err)
	}
	if got.Status != StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("status_code = %d, want 200", got.StatusCode)
	}
	if !got.IsOllama {
		t.Error("is_ollama lost on the way to the database")
	}
	if got.RequestFormat != FormatJSON {
		t.Errorf("request_format = %q, want json", got.RequestFormat)
	}
	if got.TotalMs == nil {
		t.Error("total_ms not computed at finalization")
	}
	if got.FinishedAt == nil {
		t.Error("finished_at not set at finalization")
	}
	if string(got.ResponseBody) != `{"response":"hi"}`+"\n" {
		t.Errorf("response body = %q", got.ResponseBody)
	}
	if got.RequestHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("request headers not round-tripped: %v", got.RequestHeaders)
	}
}

// TestBodySpillsToDisk checks the switch from memory to a file once a body
// grows past the inline threshold.
func TestBodySpillsToDisk(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, Options{DataDir: dir, MaxInlineBodySize: 16})

	id := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/big", StartedAt: time.Now()})
	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = 'x'
	}
	// Written in fragments, the way a streamed response arrives.
	for i := 0; i < 4; i++ {
		if err := s.AppendResponse(id, payload[i*32:(i+1)*32], false); err != nil {
			t.Fatalf("AppendResponse: %v", err)
		}
	}

	// While pending, only a preview capped at the threshold stays in memory.
	pending, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get while pending: %v", err)
	}
	if !pending.ResponseSpilled {
		t.Fatal("body should have spilled to disk")
	}
	if len(pending.ResponseBody) != 16 {
		t.Errorf("preview length = %d, want the inline threshold (16)", len(pending.ResponseBody))
	}

	if err := s.Finalize(id, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.ResponseSpilled {
		t.Fatal("stored entry should reference the spilled body")
	}
	if got.ResponseBodySize != 128 {
		t.Errorf("size = %d, want 128", got.ResponseBodySize)
	}
	// A spilled body is stored as NULL in the database: the file is the truth.
	if len(got.ResponseBody) != 0 {
		t.Errorf("inline body = %q, want none once the body lives on disk", got.ResponseBody)
	}

	want := filepath.Join(dir, "blobs", id+"-response")
	if got.ResponseBodyPath != want {
		t.Errorf("path = %q, want %q", got.ResponseBodyPath, want)
	}

	rc, size, err := s.Body(id, SideResponse)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	defer rc.Close()
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if size != 128 || len(content) != 128 {
		t.Errorf("read %d bytes (size %d), want 128", len(content), size)
	}
}

// TestRetentionDropsOldestAndTheirFiles checks that trimming the history also
// removes the body files of the purged entries.
func TestRetentionDropsOldestAndTheirFiles(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, Options{DataDir: dir, MaxInlineBodySize: 4, MaxEntries: 2})

	base := time.Now().Add(-time.Hour)
	var ids []string
	for i := 0; i < 3; i++ {
		id := mustPut(t, s, &Entry{
			Method:    "GET",
			URL:       "http://backend/x",
			StartedAt: base.Add(time.Duration(i) * time.Minute),
		})
		if err := s.AppendResponse(id, []byte("payload-that-spills"), false); err != nil {
			t.Fatalf("AppendResponse: %v", err)
		}
		if err := s.Finalize(id, nil); err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		ids = append(ids, id)
	}

	if _, err := s.Get(ids[0]); err == nil {
		t.Error("oldest entry should have been purged by retention")
	}
	for _, id := range ids[1:] {
		if _, err := s.Get(id); err != nil {
			t.Errorf("entry %s should have been kept: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", ids[0]+"-response")); !os.IsNotExist(err) {
		t.Error("body file of the purged entry is still on disk")
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", ids[2]+"-response")); err != nil {
		t.Errorf("body file of a kept entry was removed: %v", err)
	}
}

// TestClearRemovesEverything checks that clearing drops rows and files alike.
func TestClearRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, Options{DataDir: dir, MaxInlineBodySize: 4})

	id := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/x", StartedAt: time.Now()})
	if err := s.AppendResponse(id, []byte("payload-that-spills"), false); err != nil {
		t.Fatalf("AppendResponse: %v", err)
	}
	if err := s.Finalize(id, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if _, err := s.Get(id); err == nil {
		t.Error("entry survived Clear")
	}
	files, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("blobs directory still holds %d file(s)", len(files))
	}
}

// TestOrphanBlobsRemovedAtStartup covers the files an abrupt stop can leave
// behind, with no matching row in the database.
func TestOrphanBlobsRemovedAtStartup(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, Options{DataDir: dir})

	id := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/x", StartedAt: time.Now()})
	if err := s.Finalize(id, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	kept := filepath.Join(dir, "blobs", id+"-response")
	if err := os.WriteFile(kept, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "blobs", "deadbeef-response")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newTestStore(t, Options{DataDir: dir})
	_ = reopened

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan blob survived the startup sweep")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("referenced blob was removed: %v", err)
	}
}

// TestSweepPendingClosesStaleEntries covers the periodic sweep that keeps a
// backend which never answers from filling memory.
func TestSweepPendingClosesStaleEntries(t *testing.T) {
	s := newTestStore(t, Options{})

	stale := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/hang", StartedAt: time.Now().Add(-time.Hour)})
	s.mem.entries[stale].lastSeen = time.Now().Add(-time.Hour)
	fresh := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/live", StartedAt: time.Now()})

	if n := s.SweepPending(time.Minute); n != 1 {
		t.Fatalf("swept %d entries, want 1", n)
	}
	got, err := s.Get(stale)
	if err != nil {
		t.Fatalf("Get stale entry: %v", err)
	}
	if got.Status != StatusError {
		t.Errorf("stale entry status = %q, want error", got.Status)
	}
	if got.Error == "" {
		t.Error("stale entry has no error message")
	}
	if _, ok := s.mem.get(fresh); !ok {
		t.Error("a fresh pending entry was swept")
	}
}

// TestListMergesMemoryAndDatabase checks the single view over both backends.
func TestListMergesMemoryAndDatabase(t *testing.T) {
	s := newTestStore(t, Options{})
	base := time.Now().Add(-time.Hour)

	done := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/done", StartedAt: base})
	if err := s.Finalize(done, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	pending := mustPut(t, s, &Entry{Method: "GET", URL: "http://backend/pending", StartedAt: base.Add(time.Minute)})

	entries, err := s.List(10, time.Time{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].ID != pending || entries[1].ID != done {
		t.Errorf("entries are not sorted newest first: %s, %s", entries[0].ID, entries[1].ID)
	}

	// The cursor keeps only entries strictly older than the given instant.
	older, err := s.List(10, base.Add(30*time.Second))
	if err != nil {
		t.Fatalf("List with cursor: %v", err)
	}
	if len(older) != 1 || older[0].ID != done {
		t.Errorf("cursor did not filter correctly: %+v", older)
	}
}
