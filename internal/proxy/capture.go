package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/DrSmithFr/http-debug/internal/store"
)

// detectFormat identifies a payload format from the Content-Type header first,
// falling back to sniffing the leading bytes of the body when the header is
// absent or unhelpful.
func detectFormat(contentType string, body []byte) store.Format {
	if f, ok := formatFromContentType(contentType); ok {
		return f
	}
	return sniffFormat(body)
}

func formatFromContentType(contentType string) (store.Format, bool) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case ct == "":
		return "", false
	case strings.Contains(ct, "event-stream"):
		return store.FormatSSE, true
	case strings.Contains(ct, "ndjson"), strings.Contains(ct, "jsonlines"), strings.HasSuffix(ct, "jsonl"):
		return store.FormatNDJSON, true
	case ct == "application/json", strings.HasSuffix(ct, "+json"), ct == "text/json":
		return store.FormatJSON, true
	case ct == "application/xml", ct == "text/xml", strings.HasSuffix(ct, "+xml"):
		return store.FormatXML, true
	}
	return "", false
}

// sniffFormat inspects the first non-blank bytes of a body. NDJSON is only
// claimed when every non-empty line parses on its own: a pretty-printed JSON
// document also spans several lines and must not be mistaken for it.
func sniffFormat(body []byte) store.Format {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return store.FormatRaw
	}
	switch trimmed[0] {
	case '{', '[':
		if isNDJSON(trimmed) {
			return store.FormatNDJSON
		}
		return store.FormatJSON
	case '<':
		return store.FormatXML
	}
	if looksLikeSSE(trimmed) {
		return store.FormatSSE
	}
	return store.FormatRaw
}

func isNDJSON(body []byte) bool {
	lines := 0
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return false
		}
		lines++
	}
	return lines > 1
}

func looksLikeSSE(body []byte) bool {
	for _, prefix := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(body, []byte(prefix)) {
			return true
		}
	}
	return false
}

// isStreaming reports whether a format should be captured fragment by fragment
// and reported live over the event stream.
func isStreaming(f store.Format) bool {
	return f == store.FormatNDJSON || f == store.FormatSSE
}

// ollamaPaths are the endpoints that raise the is_ollama display flag. The flag
// never drives capture: an Ollama call with "stream": false answers with a
// single JSON object, so the capture strategy follows the detected format.
var ollamaPaths = []string{"/api/generate", "/api/chat", "/api/embeddings"}

func isOllamaPath(path string) bool {
	for _, p := range ollamaPaths {
		if path == p || strings.HasSuffix(path, p) {
			return true
		}
	}
	return false
}

// ollamaFragment covers both shapes emitted by Ollama: /api/generate carries the
// text in `response`, /api/chat in `message.content`.
type ollamaFragment struct {
	Response string `json:"response"`
	Message  struct {
		Content string `json:"content"`
	} `json:"message"`
}

// OllamaPreview reconstructs the assistant message from a captured body, the
// way the end user would have seen it.
func OllamaPreview(format store.Format, body []byte) string {
	var b strings.Builder
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if format == store.FormatSSE {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			line = bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(line, []byte("[DONE]")) {
				continue
			}
		}
		var f ollamaFragment
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		b.WriteString(f.Response)
		b.WriteString(f.Message.Content)
	}
	return b.String()
}

// captureReader wraps the upstream response body and copies bytes into the
// store as the client reads them. It must never delay nor alter the stream it
// forwards, so every store interaction happens after the bytes are in hand and
// nothing is buffered on the way out.
type captureReader struct {
	rc     io.ReadCloser
	store  *store.Store
	id     string
	format store.Format
	stream bool
	// sniffed guards the one-shot format detection performed on the first
	// chunk when the response carried no usable Content-Type.
	sniffed bool

	pending   []byte
	firstAt   time.Time
	lastAt    time.Time
	closeOnce sync.Once
	onClose   func(streamMs *int64)
}

func newCaptureReader(rc io.ReadCloser, st *store.Store, id string, format store.Format, onClose func(*int64)) *captureReader {
	return &captureReader{
		rc:      rc,
		store:   st,
		id:      id,
		format:  format,
		stream:  isStreaming(format),
		sniffed: format != store.FormatRaw,
		onClose: onClose,
	}
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		now := time.Now()
		if c.firstAt.IsZero() {
			c.firstAt = now
		}
		c.lastAt = now
		c.consume(p[:n])
	}
	return n, err
}

func (c *captureReader) consume(chunk []byte) {
	if !c.sniffed {
		c.sniffed = true
		if f := sniffFormat(chunk); f != store.FormatRaw {
			c.format, c.stream = f, isStreaming(f)
			_ = c.store.Update(c.id, func(e *store.Entry) { e.ResponseFormat = f })
		}
	}

	if !c.stream {
		_ = c.store.AppendResponse(c.id, chunk, false)
		return
	}

	// Streamed formats are accumulated line by line so each complete fragment
	// reaches the UI as its own delta event.
	c.pending = append(c.pending, chunk...)
	for {
		i := bytes.IndexByte(c.pending, '\n')
		if i < 0 {
			return
		}
		_ = c.store.AppendResponse(c.id, c.pending[:i+1], true)
		c.pending = c.pending[i+1:]
	}
}

func (c *captureReader) Close() error {
	err := c.rc.Close()
	c.closeOnce.Do(func() {
		if len(c.pending) > 0 {
			_ = c.store.AppendResponse(c.id, c.pending, true)
			c.pending = nil
		}
		var streamMs *int64
		if c.stream && !c.firstAt.IsZero() {
			ms := c.lastAt.Sub(c.firstAt).Milliseconds()
			streamMs = &ms
		}
		c.onClose(streamMs)
	})
	return err
}
