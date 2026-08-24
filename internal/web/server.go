// Package web serves the REST API, the event stream and the static UI.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DrSmithFr/http-debug/internal/proxy"
	"github.com/DrSmithFr/http-debug/internal/store"
)

//go:embed static
var staticFS embed.FS

const (
	defaultLimit = 100
	maxLimit     = 500
	// heartbeatInterval keeps idle SSE connections from being closed by
	// intermediaries.
	heartbeatInterval = 25 * time.Second
)

// Server wires the HTTP routes of the debug interface.
type Server struct {
	store  *store.Store
	proxy  *proxy.Proxy
	log    *slog.Logger
	target string
}

// New builds the handler tree.
func New(st *store.Store, px *proxy.Proxy, target string, log *slog.Logger) *Server {
	return &Server{store: st, proxy: px, log: log, target: target}
}

// Handler returns the router serving the API and the UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/requests", s.listRequests)
	mux.HandleFunc("POST /api/requests", s.replayRequest)
	mux.HandleFunc("DELETE /api/requests", s.clearRequests)
	mux.HandleFunc("GET /api/requests/{id}", s.getRequest)
	mux.HandleFunc("GET /api/requests/{id}/body/{side}", s.getBody)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/config", s.getConfig)

	ui, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(ui))
	// The detail page is a client-side route; it resolves to the same document.
	mux.HandleFunc("GET /r/{id}", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
	mux.Handle("GET /", files)
	return mux
}

// --- REST ---

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, maxLimit)
	}

	var before time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before must be a unix timestamp in milliseconds")
			return
		}
		before = time.UnixMilli(ms)
	}

	entries, err := s.store.List(limit, before)
	if err != nil {
		s.fail(w, "list requests", err)
		return
	}
	rows := make([]listRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, newListRow(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rows})
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) {
	entry, err := s.store.Get(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		s.fail(w, "get request", err)
		return
	}
	writeJSON(w, http.StatusOK, newDetail(entry))
}

func (s *Server) getBody(w http.ResponseWriter, r *http.Request) {
	side := store.Side(r.PathValue("side"))
	if side != store.SideRequest && side != store.SideResponse {
		writeError(w, http.StatusBadRequest, `side must be "request" or "response"`)
		return
	}
	id := r.PathValue("id")
	entry, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		s.fail(w, "get request", err)
		return
	}

	rc, size, err := s.store.Body(id, side)
	if err != nil {
		s.fail(w, "open body", err)
		return
	}
	defer func() { _ = rc.Close() }()

	// The original Content-Type is restored so the browser renders the payload
	// the way the real client received it.
	headers := entry.ResponseHeaders
	if side == store.SideRequest {
		headers = entry.RequestHeaders
	}
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, rc); err != nil {
		s.log.Warn("stream body", "id", id, "error", err)
	}
}

func (s *Server) replayRequest(w http.ResponseWriter, r *http.Request) {
	var req proxy.ReplayRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	id, err := s.proxy.Replay(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) clearRequests(w http.ResponseWriter, _ *http.Request) {
	if err := s.store.Clear(); err != nil {
		s.fail(w, "clear history", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"target_url": s.target})
}

// --- Event stream ---

// events serves the single Server-Sent Events stream shared by every client.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	ch, unsubscribe := s.store.Events().Subscribe()
	defer unsubscribe()

	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(eventPayload(ev))
			if err != nil {
				s.log.Warn("encode event", "error", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// eventPayload keeps full bodies off the stream: the detail view fetches them
// over the REST routes instead.
func eventPayload(ev store.Event) any {
	switch ev.Type {
	case store.EventCreated:
		return newListRow(ev.Entry)
	case store.EventUpdated:
		return map[string]any{
			"id":          ev.ID,
			"status":      ev.Entry.Status,
			"status_code": ev.Entry.StatusCode,
			"ttfb_ms":     ev.Entry.TTFBMs,
			"stream_ms":   ev.Entry.StreamMs,
			"total_ms":    ev.Entry.TotalMs,
		}
	case store.EventDelta:
		return map[string]any{"id": ev.ID, "chunk": ev.Chunk}
	default:
		return map[string]any{"id": ev.ID}
	}
}

// --- Payloads ---

type listRow struct {
	ID             string       `json:"id"`
	Method         string       `json:"method"`
	URL            string       `json:"url"`
	Status         store.Status `json:"status"`
	StatusCode     int          `json:"status_code,omitempty"`
	Error          string       `json:"error,omitempty"`
	RequestFormat  store.Format `json:"request_format,omitempty"`
	ResponseFormat store.Format `json:"response_format,omitempty"`
	IsOllama       bool         `json:"is_ollama"`
	IsReplay       bool         `json:"is_replay"`
	StartedAt      int64        `json:"started_at"`
	TTFBMs         *int64       `json:"ttfb_ms"`
	StreamMs       *int64       `json:"stream_ms"`
	TotalMs        *int64       `json:"total_ms"`
}

func newListRow(e *store.Entry) listRow {
	return listRow{
		ID: e.ID, Method: e.Method, URL: e.URL,
		Status: e.Status, StatusCode: e.StatusCode, Error: e.Error,
		RequestFormat: e.RequestFormat, ResponseFormat: e.ResponseFormat,
		IsOllama: e.IsOllama, IsReplay: e.IsReplay,
		StartedAt: e.StartedAt.UnixMilli(),
		TTFBMs:    e.TTFBMs, StreamMs: e.StreamMs, TotalMs: e.TotalMs,
	}
}

// bodyPayload carries a body inline when it stayed under the inline threshold.
// Past it, only the size and the URL of the raw payload are reported.
type bodyPayload struct {
	Format    store.Format `json:"format,omitempty"`
	Size      int64        `json:"size"`
	Body      string       `json:"body,omitempty"`
	Truncated bool         `json:"truncated"`
	Binary    bool         `json:"binary"`
	URL       string       `json:"url"`
}

type detail struct {
	listRow
	FinishedAt      *int64            `json:"finished_at,omitempty"`
	RequestHeaders  map[string]string `json:"request_headers"`
	ResponseHeaders map[string]string `json:"response_headers"`
	Request         bodyPayload       `json:"request"`
	Response        bodyPayload       `json:"response"`
	OllamaPreview   string            `json:"ollama_preview,omitempty"`
}

func newDetail(e *store.Entry) detail {
	d := detail{
		listRow:         newListRow(e),
		RequestHeaders:  flattenHeader(e.RequestHeaders),
		ResponseHeaders: flattenHeader(e.ResponseHeaders),
		Request:         newBodyPayload(e, store.SideRequest),
		Response:        newBodyPayload(e, store.SideResponse),
	}
	if e.FinishedAt != nil {
		ms := e.FinishedAt.UnixMilli()
		d.FinishedAt = &ms
	}
	if e.IsOllama {
		d.OllamaPreview = proxy.OllamaPreview(e.ResponseFormat, e.ResponseBody)
	}
	return d
}

func newBodyPayload(e *store.Entry, side store.Side) bodyPayload {
	body, spilled, size, format := e.ResponseBody, e.ResponseSpilled, e.ResponseBodySize, e.ResponseFormat
	if side == store.SideRequest {
		body, spilled, size, format = e.RequestBody, e.RequestSpilled, e.RequestBodySize, e.RequestFormat
	}
	p := bodyPayload{
		Format:    format,
		Size:      size,
		Truncated: spilled,
		URL:       fmt.Sprintf("/api/requests/%s/body/%s", e.ID, side),
	}
	if !utf8.Valid(body) {
		p.Binary = true
		return p
	}
	p.Body = string(body)
	return p
}

func flattenHeader(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		out[name] = strings.Join(values, ", ")
	}
	return out
}

// --- Helpers ---

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, what+" failed")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
