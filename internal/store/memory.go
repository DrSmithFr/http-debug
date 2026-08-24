package store

import (
	"sync"
	"time"
)

// memStore holds the entries still in flight. Finished entries leave it for the
// database, so it stays small even under sustained traffic.
type memStore struct {
	mu      sync.RWMutex
	entries map[string]*liveEntry
}

func newMemStore() *memStore {
	return &memStore{entries: make(map[string]*liveEntry)}
}

// liveEntry couples a pending entry with the buffers accumulating its bodies.
type liveEntry struct {
	mu       sync.Mutex
	entry    Entry
	req      *bodyBuffer
	resp     *bodyBuffer
	lastSeen time.Time
}

func (m *memStore) put(e *Entry, blobs *blobStore) *liveEntry {
	live := &liveEntry{
		entry:    *e,
		req:      blobs.buffer(e.ID, SideRequest),
		resp:     blobs.buffer(e.ID, SideResponse),
		lastSeen: time.Now(),
	}
	m.mu.Lock()
	m.entries[e.ID] = live
	m.mu.Unlock()
	return live
}

func (m *memStore) get(id string) (*liveEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	live, ok := m.entries[id]
	return live, ok
}

func (m *memStore) delete(id string) {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
}

// list returns snapshots of the pending entries older than before, newest first.
func (m *memStore) list(before time.Time) []*Entry {
	m.mu.RLock()
	live := make([]*liveEntry, 0, len(m.entries))
	for _, e := range m.entries {
		live = append(live, e)
	}
	m.mu.RUnlock()

	out := make([]*Entry, 0, len(live))
	for _, l := range live {
		e := l.snapshot()
		if !before.IsZero() && !e.StartedAt.Before(before) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// staleIDs lists pending entries with no activity for longer than timeout.
func (m *memStore) staleIDs(timeout time.Duration) []string {
	cutoff := time.Now().Add(-timeout)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ids []string
	for id, live := range m.entries {
		live.mu.Lock()
		idle := live.lastSeen.Before(cutoff)
		live.mu.Unlock()
		if idle {
			ids = append(ids, id)
		}
	}
	return ids
}

func (l *liveEntry) update(fn func(*Entry)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(&l.entry)
	l.lastSeen = time.Now()
}

func (l *liveEntry) touch() {
	l.mu.Lock()
	l.lastSeen = time.Now()
	l.mu.Unlock()
}

// snapshot copies the entry and folds in the current state of the body buffers.
func (l *liveEntry) snapshot() *Entry {
	l.mu.Lock()
	e := l.entry.Clone()
	l.mu.Unlock()

	e.RequestBody, e.RequestBodyPath, e.RequestBodySize, e.RequestSpilled = l.req.state()
	e.ResponseBody, e.ResponseBodyPath, e.ResponseBodySize, e.ResponseSpilled = l.resp.state()
	return e
}
