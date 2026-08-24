package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DrSmithFr/http-debug/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// setup puts a proxy in front of the given backend and returns the front door
// plus the store holding the captures.
func setup(t *testing.T, backendURL string) (*httptest.Server, *store.Store, *Proxy) {
	t.Helper()
	st, err := store.New(store.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	p := New(Config{Target: target, PublicURL: "http://debug.local"}, st, discardLogger())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front, st, p
}

// waitForEntry polls until the entry leaves the pending state.
func waitForEntry(t *testing.T, st *store.Store, id string) *store.Entry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entry, err := st.Get(id)
		if err == nil && entry.Status != store.StatusPending {
			return entry
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("entry %s never left the pending state", id)
	return nil
}

// TestRelayCapturesTheRealTraffic compares the captured entry with what the
// backend actually received and what the client actually got back.
func TestRelayCapturesTheRealTraffic(t *testing.T) {
	const requestBody = `{"model":"llama3","prompt":"hello"}`
	const responseBody = `{"id":"42","ok":true}`

	var seen struct {
		method string
		path   string
		query  string
		auth   string
		body   string
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.method, seen.path, seen.query = r.Method, r.URL.Path, r.URL.RawQuery
		seen.auth, seen.body = r.Header.Get("Authorization"), string(body)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Backend", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer backend.Close()

	front, st, _ := setup(t, backend.URL)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/api/generate?stream=false", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relayed request failed: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	// The relayed traffic is untouched apart from the correlation header.
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("client status = %d, want 201", resp.StatusCode)
	}
	if string(got) != responseBody {
		t.Errorf("client body = %q, want %q", got, responseBody)
	}
	if resp.Header.Get("X-Backend") != "yes" {
		t.Error("backend header did not reach the client")
	}
	if seen.method != http.MethodPost || seen.path != "/api/generate" || seen.query != "stream=false" {
		t.Errorf("backend saw %s %s?%s", seen.method, seen.path, seen.query)
	}
	if seen.auth != "Bearer secret" {
		t.Errorf("backend saw Authorization %q", seen.auth)
	}
	if seen.body != requestBody {
		t.Errorf("backend saw body %q, want %q", seen.body, requestBody)
	}

	debugURL := resp.Header.Get(DebugURLHeader)
	if debugURL == "" {
		t.Fatal("missing " + DebugURLHeader + " header")
	}
	id := debugURL[strings.LastIndex(debugURL, "/")+1:]
	if want := "http://debug.local/r/" + id; debugURL != want {
		t.Errorf("%s = %q, want %q", DebugURLHeader, debugURL, want)
	}

	entry := waitForEntry(t, st, id)
	if entry.Status != store.StatusDone {
		t.Errorf("status = %q, want done", entry.Status)
	}
	if entry.StatusCode != http.StatusCreated {
		t.Errorf("status_code = %d, want 201", entry.StatusCode)
	}
	if entry.Method != http.MethodPost {
		t.Errorf("method = %q", entry.Method)
	}
	if want := backend.URL + "/api/generate?stream=false"; entry.URL != want {
		t.Errorf("url = %q, want %q", entry.URL, want)
	}
	if string(entry.RequestBody) != requestBody {
		t.Errorf("captured request body = %q, want %q", entry.RequestBody, requestBody)
	}
	if string(entry.ResponseBody) != responseBody {
		t.Errorf("captured response body = %q, want %q", entry.ResponseBody, responseBody)
	}
	if entry.RequestHeaders.Get("Authorization") != "Bearer secret" {
		t.Error("captured request headers are missing Authorization")
	}
	if entry.ResponseHeaders.Get("X-Backend") != "yes" {
		t.Error("captured response headers are missing X-Backend")
	}
	if entry.RequestFormat != store.FormatJSON || entry.ResponseFormat != store.FormatJSON {
		t.Errorf("formats = %q/%q, want json/json", entry.RequestFormat, entry.ResponseFormat)
	}
	if !entry.IsOllama {
		t.Error("is_ollama should be set for /api/generate")
	}
	if entry.IsReplay {
		t.Error("is_replay should not be set for relayed traffic")
	}
	if entry.TTFBMs == nil || entry.TotalMs == nil {
		t.Error("ttfb_ms and total_ms should both be set")
	}
	// A non-streamed response has no stream duration.
	if entry.StreamMs != nil {
		t.Errorf("stream_ms = %d, want nil for a non-streamed response", *entry.StreamMs)
	}
}

// TestStreamedResponseReachesClientUnbuffered checks that fragments are
// forwarded as they are produced. ReverseProxy buffers by default and its
// auto-detection only covers text/event-stream, so NDJSON is the case that
// would regress first.
func TestStreamedResponseReachesClientUnbuffered(t *testing.T) {
	firstRead := make(chan struct{})
	backendDone := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(backendDone)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"response":"first","done":false}`+"\n")
		w.(http.Flusher).Flush()

		// The second fragment is only produced once the client has read the
		// first one. If anything buffered the response, this times out.
		select {
		case <-firstRead:
		case <-time.After(3 * time.Second):
			return
		}
		_, _ = io.WriteString(w, `{"response":"second","done":true}`+"\n")
		w.(http.Flusher).Flush()
	}))
	defer backend.Close()

	front, st, _ := setup(t, backend.URL)

	deltas := make(chan string, 16)
	events, unsubscribe := st.Events().Subscribe()
	defer unsubscribe()
	go func() {
		for ev := range events {
			if ev.Type == store.EventDelta {
				select {
				case deltas <- ev.Chunk:
				default:
				}
			}
		}
	}()

	resp, err := http.Get(front.URL + "/api/chat")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first fragment: %v", err)
	}
	if want := `{"response":"first","done":false}` + "\n"; line != want {
		t.Fatalf("first fragment = %q, want %q", line, want)
	}
	close(firstRead)

	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the second fragment: %v", err)
	}
	if want := `{"response":"second","done":true}` + "\n"; line != want {
		t.Fatalf("second fragment = %q, want %q", line, want)
	}

	select {
	case <-backendDone:
	case <-time.After(3 * time.Second):
		t.Fatal("backend never finished writing")
	}

	// Each complete line was published as its own delta event.
	for i, want := range []string{
		`{"response":"first","done":false}` + "\n",
		`{"response":"second","done":true}` + "\n",
	} {
		select {
		case got := <-deltas:
			if got != want {
				t.Errorf("delta %d = %q, want %q", i, got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("delta %d never arrived", i)
		}
	}

	id := resp.Header.Get(DebugURLHeader)
	id = id[strings.LastIndex(id, "/")+1:]
	entry := waitForEntry(t, st, id)
	if entry.ResponseFormat != store.FormatNDJSON {
		t.Errorf("response_format = %q, want ndjson", entry.ResponseFormat)
	}
	if entry.StreamMs == nil {
		t.Error("stream_ms should be set for a streamed response")
	}
	want := `{"response":"first","done":false}` + "\n" + `{"response":"second","done":true}` + "\n"
	if string(entry.ResponseBody) != want {
		t.Errorf("captured body = %q, want %q", entry.ResponseBody, want)
	}
	if got := OllamaPreview(entry.ResponseFormat, entry.ResponseBody); got != "firstsecond" {
		t.Errorf("reconstructed message = %q, want %q", got, "firstsecond")
	}
}

// TestUnreachableBackendIsCapturedAsError covers a backend that never answers.
func TestUnreachableBackendIsCapturedAsError(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing listens on that port any more

	front, st, _ := setup(t, deadURL)

	resp, err := http.Get(front.URL + "/whatever")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", resp.StatusCode)
	}

	entries, err := st.List(10, time.Time{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	entry := waitForEntry(t, st, entries[0].ID)
	if entry.Status != store.StatusError {
		t.Errorf("status = %q, want error", entry.Status)
	}
	if entry.Error == "" {
		t.Error("the network error should have been recorded")
	}
}

// TestDebugCookieIsOptional checks the cookie is only set when asked for.
func TestDebugCookieIsOptional(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	front, _, _ := setup(t, backend.URL)
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(resp.Cookies()) != 0 {
		t.Errorf("no cookie expected by default, got %v", resp.Cookies())
	}

	st, err := store.New(store.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	target, _ := url.Parse(backend.URL)
	withCookie := httptest.NewServer(New(Config{
		Target: target, PublicURL: "http://debug.local", SetDebugCookie: true,
	}, st, discardLogger()))
	defer withCookie.Close()

	resp2, err := http.Get(withCookie.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	cookies := resp2.Cookies()
	if len(cookies) != 1 || cookies[0].Name != DebugURLCookie {
		t.Fatalf("expected the %s cookie, got %v", DebugURLCookie, cookies)
	}
	if !strings.HasPrefix(cookies[0].Value, "http://debug.local/r/") {
		t.Errorf("cookie value = %q", cookies[0].Value)
	}
}

// TestReplayIsCapturedAsSuchAndCanTargetAnyURL covers the replay path: the
// request is sent by the server, so it is not bound to the configured target.
func TestReplayIsCapturedAsSuchAndCanTargetAnyURL(t *testing.T) {
	var seen struct {
		method string
		body   string
		header string
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.method, seen.body, seen.header = r.Method, string(body), r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"replayed":true}`)
	}))
	defer other.Close()

	unrelated := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the configured target should not have been called")
	}))
	defer unrelated.Close()

	_, st, p := setup(t, unrelated.URL)

	id, err := p.Replay(ReplayRequest{
		Method:  http.MethodPut,
		URL:     other.URL + "/anything",
		Headers: map[string][]string{"X-Custom": {"yes"}, "Content-Length": {"999"}},
		Body:    `{"hello":"world"}`,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	entry := waitForEntry(t, st, id)
	if !entry.IsReplay {
		t.Error("is_replay should be set")
	}
	if entry.StatusCode != http.StatusOK {
		t.Errorf("status_code = %d, want 200", entry.StatusCode)
	}
	if string(entry.ResponseBody) != `{"replayed":true}` {
		t.Errorf("response body = %q", entry.ResponseBody)
	}
	if seen.method != http.MethodPut || seen.body != `{"hello":"world"}` || seen.header != "yes" {
		t.Errorf("target saw %s %q header=%q", seen.method, seen.body, seen.header)
	}

	for _, bad := range []ReplayRequest{
		{Method: "GET", URL: "/relative"},
		{Method: "GET", URL: "ftp://example.com/file"},
		{Method: "GET", URL: "://nonsense"},
	} {
		if _, err := p.Replay(bad); err == nil {
			t.Errorf("Replay(%q) should have been rejected", bad.URL)
		}
	}
}

// TestLargeBodyRoundTrip checks a body above the inline threshold is relayed
// intact and captured on disk.
func TestLargeBodyRoundTrip(t *testing.T) {
	payload := strings.Repeat("abcdefghij", 5000) // 50 KB

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != payload {
			t.Errorf("backend received %d bytes, want %d", len(body), len(payload))
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, payload)
	}))
	defer backend.Close()

	st, err := store.New(store.Options{DataDir: t.TempDir(), MaxInlineBodySize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	target, _ := url.Parse(backend.URL)
	front := httptest.NewServer(New(Config{Target: target, PublicURL: "http://debug.local"}, st, discardLogger()))
	defer front.Close()

	resp, err := http.Post(front.URL+"/echo", "text/plain", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != payload {
		t.Errorf("client received %d bytes, want %d", len(got), len(payload))
	}

	id := resp.Header.Get(DebugURLHeader)
	id = id[strings.LastIndex(id, "/")+1:]
	entry := waitForEntry(t, st, id)

	if !entry.RequestSpilled || !entry.ResponseSpilled {
		t.Fatal("both bodies should have spilled to disk")
	}
	if entry.RequestBodySize != int64(len(payload)) || entry.ResponseBodySize != int64(len(payload)) {
		t.Errorf("sizes = %d/%d, want %d", entry.RequestBodySize, entry.ResponseBodySize, len(payload))
	}
	for _, side := range []store.Side{store.SideRequest, store.SideResponse} {
		rc, size, err := st.Body(id, side)
		if err != nil {
			t.Fatalf("Body(%s): %v", side, err)
		}
		content, _ := io.ReadAll(rc)
		_ = rc.Close()
		if size != int64(len(payload)) || string(content) != payload {
			t.Errorf("%s body file holds %d bytes, want %d", side, len(content), len(payload))
		}
	}
}

// TestSSEIsForwardedAndCaptured covers the other streaming format.
func TestSSEIsForwardedAndCaptured(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
				"message": map[string]string{"content": fmt.Sprintf("chunk-%d", i)},
			}))
			w.(http.Flusher).Flush()
		}
	}))
	defer backend.Close()

	front, st, _ := setup(t, backend.URL)
	resp, err := http.Get(front.URL + "/api/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	id := resp.Header.Get(DebugURLHeader)
	id = id[strings.LastIndex(id, "/")+1:]
	entry := waitForEntry(t, st, id)

	if entry.ResponseFormat != store.FormatSSE {
		t.Errorf("response_format = %q, want sse", entry.ResponseFormat)
	}
	if got, want := OllamaPreview(entry.ResponseFormat, entry.ResponseBody), "chunk-0chunk-1chunk-2"; got != want {
		t.Errorf("reconstructed message = %q, want %q", got, want)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
