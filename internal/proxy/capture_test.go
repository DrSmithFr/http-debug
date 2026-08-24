package proxy

import (
	"testing"

	"github.com/DrSmithFr/http-debug/internal/store"
)

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        store.Format
	}{
		{"json content type", "application/json; charset=utf-8", "", store.FormatJSON},
		{"json suffix content type", "application/problem+json", "", store.FormatJSON},
		{"ndjson content type", "application/x-ndjson", "", store.FormatNDJSON},
		{"sse content type", "text/event-stream", "", store.FormatSSE},
		{"xml content type", "text/xml", "", store.FormatXML},

		// The header wins even when the body would suggest otherwise: it is
		// what the client itself used to decide how to read the payload.
		{"header beats body", "text/event-stream", `{"a":1}`, store.FormatSSE},

		// No Content-Type: fall back to sniffing the leading bytes.
		{"no content type, json object", "", `  {"a": 1}`, store.FormatJSON},
		{"no content type, json array", "", `[1, 2, 3]`, store.FormatJSON},
		{"no content type, xml", "", `<?xml version="1.0"?><root/>`, store.FormatXML},
		{"no content type, sse", "", "data: hello\n\n", store.FormatSSE},
		{"no content type, plain text", "", "hello world", store.FormatRaw},

		{"empty body and no content type", "", "", store.FormatRaw},
		{"blank body and no content type", "", "   \n\t ", store.FormatRaw},

		// A truncated document is still best described as JSON: the raw view
		// is what the user needs, and claiming `raw` would hide that.
		{"truncated json", "", `{"a": 1, "b":`, store.FormatJSON},

		{"ndjson body", "", "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n", store.FormatNDJSON},

		// Only the first line parses, so this is not NDJSON. It falls back to
		// JSON rather than claiming a line-delimited stream that is not one.
		{"ndjson with only the first line valid", "", "{\"a\":1}\n{\"a\":\nbroken\n", store.FormatJSON},

		// A pretty-printed document spans several lines but is a single value.
		{"pretty printed json is not ndjson", "", "{\n  \"a\": 1,\n  \"b\": 2\n}\n", store.FormatJSON},

		// A single JSON line is one document, not a stream of them.
		{"single json line", "", "{\"a\":1}\n", store.FormatJSON},

		{"unknown content type falls back to sniffing", "application/octet-stream", `{"a":1}`, store.FormatJSON},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectFormat(tc.contentType, []byte(tc.body)); got != tc.want {
				t.Errorf("detectFormat(%q, %q) = %q, want %q", tc.contentType, tc.body, got, tc.want)
			}
		})
	}
}

func TestIsStreaming(t *testing.T) {
	streaming := map[store.Format]bool{
		store.FormatNDJSON: true,
		store.FormatSSE:    true,
		store.FormatJSON:   false,
		store.FormatXML:    false,
		store.FormatRaw:    false,
	}
	for format, want := range streaming {
		if got := isStreaming(format); got != want {
			t.Errorf("isStreaming(%q) = %v, want %v", format, got, want)
		}
	}
}

func TestIsOllamaPath(t *testing.T) {
	tests := map[string]bool{
		"/api/generate":         true,
		"/api/chat":             true,
		"/api/embeddings":       true,
		"/ollama/api/chat":      true, // reachable behind a path prefix
		"/api/tags":             false,
		"/v1/chat/completions":  false,
		"/api/chat/completions": false,
		"/":                     false,
	}
	for path, want := range tests {
		if got := isOllamaPath(path); got != want {
			t.Errorf("isOllamaPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestOllamaPreview(t *testing.T) {
	tests := []struct {
		name   string
		format store.Format
		body   string
		want   string
	}{
		{
			// /api/generate carries the text in `response`.
			name:   "generate fragments",
			format: store.FormatNDJSON,
			body: `{"model":"llama3","response":"Hello","done":false}
{"model":"llama3","response":", ","done":false}
{"model":"llama3","response":"world","done":false}
{"model":"llama3","response":"","done":true}
`,
			want: "Hello, world",
		},
		{
			// /api/chat carries it in `message.content`.
			name:   "chat fragments",
			format: store.FormatNDJSON,
			body: `{"model":"llama3","message":{"role":"assistant","content":"Hi"},"done":false}
{"model":"llama3","message":{"role":"assistant","content":" there"},"done":false}
{"model":"llama3","message":{"role":"assistant","content":""},"done":true}
`,
			want: "Hi there",
		},
		{
			// A non-streamed call answers with a single JSON object.
			name:   "single json object",
			format: store.FormatJSON,
			body:   `{"model":"llama3","response":"All at once","done":true}`,
			want:   "All at once",
		},
		{
			name:   "sse fragments",
			format: store.FormatSSE,
			body: `data: {"message":{"content":"Streamed"}}

data: {"message":{"content":" over SSE"}}

data: [DONE]

`,
			want: "Streamed over SSE",
		},
		{
			// A truncated last fragment is skipped rather than failing the
			// whole reconstruction: a live stream is read while incomplete.
			name:   "trailing partial fragment is skipped",
			format: store.FormatNDJSON,
			body:   "{\"response\":\"ok\"}\n{\"response\":\"tru",
			want:   "ok",
		},
		{
			name:   "empty body",
			format: store.FormatNDJSON,
			body:   "",
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := OllamaPreview(tc.format, []byte(tc.body)); got != tc.want {
				t.Errorf("OllamaPreview() = %q, want %q", got, tc.want)
			}
		})
	}
}
