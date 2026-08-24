package store

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// blobStore owns the directory holding oversized bodies. Files are flat and
// named `<id>-<side>`: a debug session never accumulates enough of them to
// justify a directory hierarchy.
type blobStore struct {
	dir       string
	maxInline int64
}

func newBlobStore(dir string, maxInline int64) (*blobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create blobs dir: %w", err)
	}
	return &blobStore{dir: dir, maxInline: maxInline}, nil
}

func (b *blobStore) path(id string, side Side) string {
	return filepath.Join(b.dir, id+"-"+string(side))
}

func (b *blobStore) buffer(id string, side Side) *bodyBuffer {
	return &bodyBuffer{blobs: b, id: id, side: side}
}

// removeFor deletes both body files of an entry, ignoring missing ones.
func (b *blobStore) removeFor(id string) {
	for _, side := range []Side{SideRequest, SideResponse} {
		_ = os.Remove(b.path(id, side))
	}
}

func (b *blobStore) removeAll() error {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(b.dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// removeOrphans drops body files whose id no longer appears in the database.
// An abrupt container stop can leave such files behind.
func (b *blobStore) removeOrphans(known func() (map[string]struct{}, error)) error {
	ids, err := known()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		id := name
		if i := strings.LastIndex(name, "-"); i > 0 {
			id = name[:i]
		}
		if _, ok := ids[id]; !ok {
			_ = os.Remove(filepath.Join(b.dir, name))
		}
	}
	return nil
}

// bodyBuffer accumulates a body in memory and switches to a file once it grows
// past the inline threshold. After the switch only a leading preview is kept in
// memory, which is what the list and the detail view display before the user
// asks for the raw payload.
type bodyBuffer struct {
	mu      sync.Mutex
	blobs   *blobStore
	id      string
	side    Side
	preview []byte
	size    int64
	path    string
	file    *os.File
	err     error
}

func (b *bodyBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}

	if b.file == nil && b.size+int64(len(p)) <= b.blobs.maxInline {
		b.preview = append(b.preview, p...)
		b.size += int64(len(p))
		return len(p), nil
	}

	if b.file == nil {
		path := b.blobs.path(b.id, b.side)
		f, err := os.Create(path)
		if err != nil {
			b.err = err
			return 0, err
		}
		if _, err := f.Write(b.preview); err != nil {
			b.err = err
			_ = f.Close()
			return 0, err
		}
		b.file, b.path = f, path
	}

	if _, err := b.file.Write(p); err != nil {
		b.err = err
		return 0, err
	}
	b.size += int64(len(p))
	if room := b.blobs.maxInline - int64(len(b.preview)); room > 0 {
		n := int64(len(p))
		if n > room {
			n = room
		}
		b.preview = append(b.preview, p[:n]...)
	}
	return len(p), nil
}

func (b *bodyBuffer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		_ = b.file.Sync()
		_ = b.file.Close()
		b.file = nil
	}
}

// state returns the preview, the on-disk path if any, the full size, and
// whether the body spilled to disk.
func (b *bodyBuffer) state() ([]byte, string, int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.preview...), b.path, b.size, b.path != ""
}

func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
