package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DrSmithFr/http-debug/internal/store"
)

// ReplayRequest is a request composed in the UI. It is sent by the server
// rather than the browser, which sidesteps cross-origin restrictions and allows
// targeting an arbitrary URL instead of only the configured target.
type ReplayRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// headers that describe the previous transfer and must not be replayed as-is.
var strippedHeaders = map[string]bool{
	"content-length":    true,
	"connection":        true,
	"keep-alive":        true,
	"transfer-encoding": true,
	"upgrade":           true,
	"host":              true,
}

// Replay sends a composed request and captures it like any other exchange. It
// returns as soon as the entry exists, so the UI can follow a streamed response
// live over the event stream.
func (p *Proxy) Replay(req ReplayRequest) (string, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if !target.IsAbs() || target.Host == "" {
		return "", errors.New("url must be absolute")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", target.Scheme)
	}

	body := []byte(req.Body)
	header := make(http.Header)
	for name, values := range req.Headers {
		if strippedHeaders[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			header.Add(name, v)
		}
	}

	entry := &store.Entry{
		Method:         method,
		URL:            target.String(),
		Status:         store.StatusPending,
		RequestHeaders: header.Clone(),
		RequestFormat:  detectFormat(header.Get("Content-Type"), body),
		IsOllama:       isOllamaPath(target.Path),
		IsReplay:       true,
		StartedAt:      time.Now(),
	}
	id, err := p.store.Put(entry)
	if err != nil {
		return "", err
	}
	if err := p.store.SetRequestBody(id, body); err != nil {
		p.log.Error("store replay body", "id", id, "error", err)
	}

	go p.send(&exchange{id: id, startAt: entry.StartedAt}, method, target, header, body)
	return id, nil
}

func (p *Proxy) send(ex *exchange, method string, target *url.URL, header http.Header, body []byte) {
	// Detached from the API request: the caller has already been answered.
	httpReq, err := http.NewRequestWithContext(context.Background(), method, target.String(), strings.NewReader(string(body)))
	if err != nil {
		p.finalize(ex, nil, err)
		return
	}
	httpReq.Header = header

	resp, err := p.client.Do(httpReq)
	if err != nil {
		p.finalize(ex, nil, err)
		return
	}

	ttfb := time.Since(ex.startAt).Milliseconds()
	format := detectFormat(resp.Header.Get("Content-Type"), nil)
	if err := p.store.Update(ex.id, func(e *store.Entry) {
		e.StatusCode = resp.StatusCode
		e.ResponseHeaders = resp.Header.Clone()
		e.ResponseFormat = format
		e.TTFBMs = &ttfb
	}); err != nil {
		p.log.Warn("update replay entry", "id", ex.id, "error", err)
	}

	capture := newCaptureReader(resp.Body, p.store, ex.id, format, func(streamMs *int64) {
		p.finalize(ex, streamMs, nil)
	})
	if _, err := io.Copy(io.Discard, capture); err != nil {
		p.log.Warn("read replay response", "id", ex.id, "error", err)
	}
	_ = capture.Close()
}
