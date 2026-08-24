// Command debugproxy relays HTTP traffic to a target URL, captures every
// exchange, and exposes it in a live web interface.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DrSmithFr/http-debug/internal/proxy"
	"github.com/DrSmithFr/http-debug/internal/store"
	"github.com/DrSmithFr/http-debug/internal/web"
)

// version is stamped at build time with -ldflags "-X main.version=…".
var version = "dev"

// config is the full runtime configuration, read from the environment.
type config struct {
	TargetURL         string
	ListenAddr        string
	WebAddr           string
	PublicURL         string
	DataDir           string
	MaxInlineBodySize int64
	MaxEntries        int
	PendingTimeout    time.Duration
	SetDebugCookie    bool
}

func loadConfig() (config, error) {
	cfg := config{
		TargetURL:         os.Getenv("TARGET_URL"),
		ListenAddr:        envString("LISTEN_ADDR", ":8080"),
		WebAddr:           envString("WEB_ADDR", ":8081"),
		DataDir:           envString("DATA_DIR", "/data"),
		MaxInlineBodySize: 1 << 20,
		MaxEntries:        1000,
		PendingTimeout:    5 * time.Minute,
	}

	if cfg.TargetURL == "" {
		return cfg, errors.New("TARGET_URL is required")
	}
	target, err := url.Parse(cfg.TargetURL)
	if err != nil || !target.IsAbs() || target.Host == "" {
		return cfg, fmt.Errorf("TARGET_URL must be an absolute URL, got %q", cfg.TargetURL)
	}

	cfg.PublicURL = envString("PUBLIC_URL", defaultPublicURL(cfg.WebAddr))
	if cfg.MaxInlineBodySize, err = envInt64("MAX_INLINE_BODY_SIZE", cfg.MaxInlineBodySize); err != nil {
		return cfg, err
	}
	if cfg.MaxEntries, err = envInt("MAX_ENTRIES", cfg.MaxEntries); err != nil {
		return cfg, err
	}
	if cfg.PendingTimeout, err = envDuration("PENDING_TIMEOUT", cfg.PendingTimeout); err != nil {
		return cfg, err
	}
	if cfg.SetDebugCookie, err = envBool("SET_DEBUG_COOKIE", false); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	target, err := url.Parse(cfg.TargetURL)
	if err != nil {
		return err
	}

	st, err := store.New(store.Options{
		DataDir:           cfg.DataDir,
		MaxInlineBodySize: cfg.MaxInlineBodySize,
		MaxEntries:        cfg.MaxEntries,
	})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	px := proxy.New(proxy.Config{
		Target:         target,
		PublicURL:      cfg.PublicURL,
		SetDebugCookie: cfg.SetDebugCookie,
	}, st, log)

	ui := web.New(st, px, cfg.TargetURL, log)

	proxySrv := &http.Server{Addr: cfg.ListenAddr, Handler: px}
	webSrv := &http.Server{Addr: cfg.WebAddr, Handler: ui.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sweepPending(ctx, st, cfg.PendingTimeout, log)

	errs := make(chan error, 2)
	go serve(proxySrv, "proxy", cfg.ListenAddr, errs, log)
	go serve(webSrv, "web", cfg.WebAddr, errs, log)

	log.Info("debugproxy started",
		"version", version,
		"target", cfg.TargetURL,
		"proxy", cfg.ListenAddr,
		"web", cfg.WebAddr,
		"public_url", cfg.PublicURL,
		"data_dir", cfg.DataDir,
	)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errs:
		stop()
		shutdown(proxySrv, webSrv)
		return err
	}
	shutdown(proxySrv, webSrv)
	return nil
}

func serve(srv *http.Server, name, addr string, errs chan<- error, log *slog.Logger) {
	log.Debug("listening", "server", name, "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("%s server on %s: %w", name, addr, err)
	}
}

func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}

// sweepPending periodically closes entries whose response never arrived, so
// they cannot pile up in memory unnoticed.
func sweepPending(ctx context.Context, st *store.Store, timeout time.Duration, log *slog.Logger) {
	interval := timeout / 2
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := st.SweepPending(timeout); n > 0 {
				log.Warn("closed stale pending entries", "count", n, "timeout", timeout)
			}
		}
	}
}

// defaultPublicURL guesses the browsable address of the UI from its listen
// address, which is correct for the common case of a locally published port.
func defaultPublicURL(webAddr string) string {
	host, port, found := strings.Cut(webAddr, ":")
	if !found {
		return "http://localhost" + webAddr
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" {
		host = "localhost"
	}
	return "http://" + host + ":" + port
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	v, err := envInt64(name, int64(fallback))
	return int(v), err
}

func envInt64(name string, fallback int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return fallback, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return n, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback, fmt.Errorf("%s must be a positive duration such as 30s or 5m, got %q", name, raw)
	}
	return d, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return b, nil
}
