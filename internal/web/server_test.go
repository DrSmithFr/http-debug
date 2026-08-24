package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DrSmithFr/http-debug/internal/proxy"
	"github.com/DrSmithFr/http-debug/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.New(store.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	target, _ := url.Parse("http://backend.local")
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	px := proxy.New(proxy.Config{Target: target, PublicURL: "http://debug.local"}, st, log)

	srv := httptest.NewServer(New(st, px, target.String(), log).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func seedEntry(t *testing.T, st *store.Store, url string, finalize bool) string {
	t.Helper()
	id, err := st.Put(&store.Entry{
		Method:         http.MethodPost,
		URL:            url,
		RequestHeaders: http.Header{"Content-Type": {"application/json"}},
		RequestFormat:  store.FormatJSON,
		IsOllama:       strings.Contains(url, "/api/chat"),
		StartedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.SetRequestBody(id, []byte(`{"prompt":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendResponse(id, []byte(`{"message":{"content":"hello"}}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(id, func(e *store.Entry) {
		e.StatusCode = http.StatusOK
		e.ResponseFormat = store.FormatNDJSON
		e.ResponseHeaders = http.Header{"Content-Type": {"application/x-ndjson"}}
	}); err != nil {
		t.Fatal(err)
	}
	if finalize {
		if err := st.Finalize(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// getJSON performs a GET, decodes the body into `into` when given, and returns
// the status code. The response body is closed here so callers cannot leak it.
func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func TestListRequests(t *testing.T) {
	srv, st := newTestServer(t)
	seedEntry(t, st, "http://backend.local/api/chat", true)
	seedEntry(t, st, "http://backend.local/api/tags", false)

	var body struct {
		Requests []map[string]any `json:"requests"`
	}
	getJSON(t, srv.URL+"/api/requests", &body)

	if len(body.Requests) != 2 {
		t.Fatalf("got %d rows, want 2", len(body.Requests))
	}
	// List rows carry metadata only; bodies are fetched from the detail route.
	for _, row := range body.Requests {
		if _, ok := row["request"]; ok {
			t.Error("list rows must not carry bodies")
		}
		for _, field := range []string{"id", "method", "url", "status", "started_at", "is_ollama", "is_replay"} {
			if _, ok := row[field]; !ok {
				t.Errorf("list row is missing %q", field)
			}
		}
	}

	if status := getJSON(t, srv.URL+"/api/requests?limit=abc", nil); status != http.StatusBadRequest {
		t.Errorf("bad limit returned %d, want 400", status)
	}
	if status := getJSON(t, srv.URL+"/api/requests?before=nope", nil); status != http.StatusBadRequest {
		t.Errorf("bad cursor returned %d, want 400", status)
	}
}

func TestGetRequestDetail(t *testing.T) {
	srv, st := newTestServer(t)
	id := seedEntry(t, st, "http://backend.local/api/chat", true)

	var detail struct {
		ID              string            `json:"id"`
		IsOllama        bool              `json:"is_ollama"`
		RequestHeaders  map[string]string `json:"request_headers"`
		ResponseHeaders map[string]string `json:"response_headers"`
		Request         bodyPayload       `json:"request"`
		Response        bodyPayload       `json:"response"`
		OllamaPreview   string            `json:"ollama_preview"`
	}
	getJSON(t, srv.URL+"/api/requests/"+id, &detail)

	if detail.ID != id {
		t.Errorf("id = %q, want %q", detail.ID, id)
	}
	if detail.Request.Body != `{"prompt":"hi"}` {
		t.Errorf("request body = %q", detail.Request.Body)
	}
	if detail.Request.Truncated {
		t.Error("a small body should be inline, not truncated")
	}
	if want := "/api/requests/" + id + "/body/request"; detail.Request.URL != want {
		t.Errorf("request body url = %q, want %q", detail.Request.URL, want)
	}
	if detail.RequestHeaders["Content-Type"] != "application/json" {
		t.Errorf("request headers = %v", detail.RequestHeaders)
	}
	// The Ollama preview is the message reconstructed from the fragments.
	if detail.OllamaPreview != "hello" {
		t.Errorf("ollama_preview = %q, want %q", detail.OllamaPreview, "hello")
	}

	if status := getJSON(t, srv.URL+"/api/requests/unknown", nil); status != http.StatusNotFound {
		t.Errorf("unknown id returned %d, want 404", status)
	}
}

func TestGetBodyRestoresContentType(t *testing.T) {
	srv, st := newTestServer(t)
	id := seedEntry(t, st, "http://backend.local/api/chat", true)

	resp, err := http.Get(srv.URL + "/api/requests/" + id + "/body/response")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", resp.Header.Get("Content-Type"))
	}
	if want := `{"message":{"content":"hello"}}` + "\n"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	if status := getJSON(t, srv.URL+"/api/requests/"+id+"/body/sideways", nil); status != http.StatusBadRequest {
		t.Errorf("invalid side returned %d, want 400", status)
	}
}

func TestClearHistory(t *testing.T) {
	srv, st := newTestServer(t)
	seedEntry(t, st, "http://backend.local/api/chat", true)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/requests", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE returned %d, want 204", resp.StatusCode)
	}

	var body struct {
		Requests []map[string]any `json:"requests"`
	}
	getJSON(t, srv.URL+"/api/requests", &body)
	if len(body.Requests) != 0 {
		t.Errorf("history still holds %d rows", len(body.Requests))
	}
}

func TestReplayRejectsInvalidInput(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/api/requests", "application/json", strings.NewReader(`{"url":"/relative"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("relative url returned %d, want 400", resp.StatusCode)
	}

	resp2, err := http.Post(srv.URL+"/api/requests", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed JSON returned %d, want 400", resp2.StatusCode)
	}
}

// TestEventStream checks the SSE contract: named events, JSON payloads, and no
// full body on the wire.
func TestEventStream(t *testing.T) {
	srv, st := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// The greeting comment proves the handler has subscribed. Events produced
	// from here on are buffered per subscriber, so seeding synchronously is
	// enough for them all to be waiting when we start reading.
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	seedEntry(t, st, "http://backend.local/api/chat", true)

	seen := map[string]json.RawMessage{}
	for len(seen) < 3 {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		data, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		seen[name] = json.RawMessage(strings.TrimSpace(strings.TrimPrefix(data, "data: ")))
	}

	for _, name := range []string{"created", "delta", "updated"} {
		payload, ok := seen[name]
		if !ok {
			t.Errorf("no %q event received", name)
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Errorf("%q payload is not JSON: %v", name, err)
			continue
		}
		if _, ok := decoded["id"]; !ok {
			t.Errorf("%q payload has no id: %s", name, payload)
		}
	}
	if delta, ok := seen["delta"]; ok {
		var decoded struct {
			Chunk string `json:"chunk"`
		}
		_ = json.Unmarshal(delta, &decoded)
		if decoded.Chunk == "" {
			t.Errorf("delta event carries no chunk: %s", delta)
		}
	}
}

func TestStaticUIAndDetailRoute(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{"/", "/r/abc123", "/app.js", "/style.css"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned an empty document", path)
		}
	}
}

func TestConfigRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	var cfg struct {
		TargetURL string `json:"target_url"`
	}
	getJSON(t, srv.URL+"/api/config", &cfg)
	if cfg.TargetURL != "http://backend.local" {
		t.Errorf("target_url = %q", cfg.TargetURL)
	}
}
