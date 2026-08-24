// Package proxy relays traffic to the configured target and captures every
// exchange into the store. The relayed traffic is never altered, apart from the
// correlation header added to the response.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/DrSmithFr/http-debug/internal/store"
)

// DebugURLHeader carries the URL of the detail page for the current exchange.
// A header is used rather than a cookie because the clients that matter here
// are programs — Open WebUI, curl, an SDK — which surface response headers in
// their logs but ignore cookies.
const DebugURLHeader = "X-Debug-Url"

// DebugURLCookie is the optional cookie holding the same URL, for the cases
// where the client is a browser and reading headers is impractical.
const DebugURLCookie = "x_debug_url"

// Config holds the proxy settings.
type Config struct {
	// Target is the upstream every request is relayed to.
	Target *url.URL
	// PublicURL is the base used to build the correlation header.
	PublicURL string
	// SetDebugCookie also sets the detail URL as a cookie on the response.
	SetDebugCookie bool
}

// Proxy is the HTTP handler relaying and capturing traffic.
type Proxy struct {
	cfg    Config
	store  *store.Store
	log    *slog.Logger
	rp     *httputil.ReverseProxy
	client *http.Client
}

type ctxKey struct{}

// exchange links an in-flight request to the entry capturing it.
type exchange struct {
	id      string
	startAt time.Time
}

// New builds the reverse proxy.
func New(cfg Config, st *store.Store, log *slog.Logger) *Proxy {
	p := &Proxy{
		cfg:   cfg,
		store: st,
		log:   log,
		// No client timeout: a long generation is a normal case here, and the
		// pending sweep is what bounds a request that never completes.
		client: &http.Client{},
	}
	p.rp = &httputil.ReverseProxy{
		Director: p.direct,
		// ReverseProxy buffers responses by default, which breaks streaming as
		// seen by the client. Its auto-detection only covers text/event-stream
		// and would leave Ollama's NDJSON buffered, so flushing is forced.
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.handleError,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return p
}

func (p *Proxy) direct(r *http.Request) {
	r.URL.Scheme = p.cfg.Target.Scheme
	r.URL.Host = p.cfg.Target.Host
	r.URL.Path = joinPath(p.cfg.Target.Path, r.URL.Path)
	r.Host = p.cfg.Target.Host
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Reading the body consumes it, so it is read in full and re-injected
	// before the relay can happen.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	target := *p.cfg.Target
	target.Path = joinPath(p.cfg.Target.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	entry := &store.Entry{
		Method:         r.Method,
		URL:            target.String(),
		Status:         store.StatusPending,
		RequestHeaders: r.Header.Clone(),
		RequestFormat:  detectFormat(r.Header.Get("Content-Type"), body),
		IsOllama:       isOllamaPath(r.URL.Path),
		StartedAt:      time.Now(),
	}
	id, err := p.store.Put(entry)
	if err != nil {
		p.log.Error("capture request", "error", err)
		p.rp.ServeHTTP(w, r)
		return
	}
	if err := p.store.SetRequestBody(id, body); err != nil {
		p.log.Error("store request body", "id", id, "error", err)
	}

	ctx := context.WithValue(r.Context(), ctxKey{}, &exchange{id: id, startAt: entry.StartedAt})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	ex, ok := exchangeFrom(resp.Request.Context())
	if !ok {
		return nil
	}

	ttfb := time.Since(ex.startAt).Milliseconds()
	format := detectFormat(resp.Header.Get("Content-Type"), nil)
	if err := p.store.Update(ex.id, func(e *store.Entry) {
		e.StatusCode = resp.StatusCode
		e.ResponseHeaders = resp.Header.Clone()
		e.ResponseFormat = format
		e.TTFBMs = &ttfb
	}); err != nil {
		p.log.Warn("update entry", "id", ex.id, "error", err)
	}

	detail := p.DetailURL(ex.id)
	resp.Header.Set(DebugURLHeader, detail)
	if p.cfg.SetDebugCookie {
		resp.Header.Add("Set-Cookie", (&http.Cookie{
			Name:  DebugURLCookie,
			Value: detail,
			Path:  "/",
		}).String())
	}

	resp.Body = newCaptureReader(resp.Body, p.store, ex.id, format, func(streamMs *int64) {
		p.finalize(ex, streamMs, nil)
	})
	return nil
}

func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	if ex, ok := exchangeFrom(r.Context()); ok {
		p.finalize(ex, nil, err)
	}
	p.log.Warn("relay failed", "url", r.URL.String(), "error", err)
	w.WriteHeader(http.StatusBadGateway)
	_, _ = fmt.Fprintf(w, "debugproxy: relay to %s failed: %v\n", p.cfg.Target, err)
}

func (p *Proxy) finalize(ex *exchange, streamMs *int64, relayErr error) {
	err := p.store.Finalize(ex.id, func(e *store.Entry) {
		e.StreamMs = streamMs
		if relayErr != nil {
			e.Status = store.StatusError
			e.Error = relayErr.Error()
		}
	})
	if err != nil {
		p.log.Warn("finalize entry", "id", ex.id, "error", err)
	}
}

// DetailURL builds the address of the detail page for an entry.
func (p *Proxy) DetailURL(id string) string {
	return strings.TrimSuffix(p.cfg.PublicURL, "/") + "/r/" + id
}

func exchangeFrom(ctx context.Context) (*exchange, bool) {
	ex, ok := ctx.Value(ctxKey{}).(*exchange)
	return ex, ok
}

// joinPath concatenates the target base path with the incoming path, keeping
// exactly one slash between them.
func joinPath(base, path string) string {
	switch {
	case base == "" || base == "/":
		return path
	case path == "" || path == "/":
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}
