package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jsxd3v/veilox/internal/budget"
	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/proxy"
	"github.com/jsxd3v/veilox/internal/store"
)

// Hooks for tests.
var (
	onListen      = func(string) {}
	serveSignals  = func(c chan<- os.Signal) { signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) }
	shutdownGrace = 30 * time.Second
)

func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Log.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Log.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func runServe(args []string, _, stderr io.Writer) error {
	fs := newFlags("serve", stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "path to veilox.yaml")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Apps) == 0 {
		return fmt.Errorf("%s has no apps; create a key with `veilox keygen <app-name>`", *cfgPath)
	}
	log := newLogger(cfg, stderr)
	rt, err := proxy.NewRuntime(cfg)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.Resolve(cfg.Storage.Path))
	if err != nil {
		return err
	}
	st.StartWriter(func(err error) { log.Error("could not save a request record", "error", err) })
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing the database", "error", err)
		}
	}()
	bt, err := budget.New(context.Background(), cfg.Location, nil, st.SpendByApp)
	if err != nil {
		return fmt.Errorf("loading today's spend: %w", err)
	}

	srv := proxy.New(rt, st, bt, log)
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		// No WriteTimeout: answers can stream for many minutes. The proxy sets a deadline on reading
		// each request body, and server.upstream_timeout caps each provider call.
		IdleTimeout: 2 * time.Minute,
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	go prune(ctx, st, cfg.Storage.RetentionDays, log)

	logStartup(log, cfg, rt, ln.Addr().String())
	onListen(ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	sigs := make(chan os.Signal, 4)
	serveSignals(sigs)
	defer signal.Stop(sigs)
	for {
		select {
		case err := <-errc:
			return err
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				reload(*cfgPath, cfg, srv, log)
				continue
			}
			log.Info("shutting down; waiting for open requests", "grace", shutdownGrace.String())
			sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			err := httpSrv.Shutdown(sctx)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				log.Warn("grace period over; closing the remaining connections")
				_ = httpSrv.Close()
				return nil
			}
			return err
		}
	}
}

// reload re-reads veilox.yaml and prices.yaml. A broken file keeps the running config.
func reload(path string, running *config.Config, srv *proxy.Server, log *slog.Logger) {
	cfg, err := config.Load(path)
	if err == nil && len(cfg.Apps) == 0 {
		err = errors.New("no apps")
	}
	var rt *proxy.Runtime
	if err == nil {
		rt, err = proxy.NewRuntime(cfg)
	}
	if err != nil {
		log.Error("reload failed; keeping the previous config", "error", err)
		return
	}
	if cfg.Server.Listen != running.Server.Listen || cfg.Storage.Path != running.Storage.Path || cfg.Server.Timezone != running.Server.Timezone {
		log.Warn("server.listen, server.timezone and storage.path only change on restart")
		cfg.Server.Listen, cfg.Storage.Path, cfg.Server.Timezone, cfg.Location = running.Server.Listen, running.Storage.Path, running.Server.Timezone, running.Location
	}
	srv.Reload(rt)
	log.Info("config reloaded", "apps", len(cfg.Apps), "priced_models", rt.Prices.Len())
}

func logStartup(log *slog.Logger, cfg *config.Config, rt *proxy.Runtime, addr string) {
	log.Info("veilox listening", "addr", addr, "version", Version, "apps", len(cfg.Apps),
		"detectors", rt.Engine.Types(), "names", len(cfg.Names), "priced_models", rt.Prices.Len())
	if rt.Prices.Len() == 0 {
		log.Warn("no prices loaded; costs will show as unknown", "pricing_file", cfg.Pricing.File)
	}
	for _, name := range []string{config.OpenAI, config.Anthropic} {
		p := cfg.Providers[name]
		if os.Getenv(p.APIKeyEnv) == "" {
			log.Warn("provider key not set; calls to this provider will fail", "provider", name, "env", p.APIKeyEnv)
		}
	}
}

// prune deletes old request rows at startup and then once a day.
func prune(ctx context.Context, st *store.Store, days int, log *slog.Logger) {
	if days <= 0 {
		return
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		n, err := st.Prune(ctx, time.Now().AddDate(0, 0, -days))
		if err != nil {
			log.Error("retention cleanup failed", "error", err)
		} else if n > 0 {
			log.Info("retention cleanup", "deleted_rows", n, "retention_days", days)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
