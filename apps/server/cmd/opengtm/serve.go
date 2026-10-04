package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/pluginrun"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
	"github.com/debpalash/OpenGTM/apps/server/internal/server"
)

func init() {
	register("serve", "run the HTTP server: API gateway, web UI and live events", runServe)
}

// roleFlags adds the flags every long-running role shares.
func roleFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "", "path to opengtm.yaml (default $"+config.ConfigEnv+")")
	return fs, path
}

// roleLogger installs a JSON slog logger at the configured level.
func roleLogger(cfg config.Config, role string) *slog.Logger {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("role", role, "version", version)
	slog.SetDefault(log)
	return log
}

func runServe(ctx context.Context, args []string) error {
	fs, cfgPath := roleFlags("serve")
	listen := fs.String("listen", "", "listen address (overrides OPENGTM_LISTEN)")
	withWorker := fs.Bool("worker", false, "also run the queue worker in this process (lite profile)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	log := roleLogger(cfg, "serve")

	pool, err := db.Open(ctx, cfg.DatabaseURL, db.Options{
		AppName:  "opengtm-serve",
		MaxConns: int32(16 + 2*cfg.Worker.Concurrency),
		Lazy:     true,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	az, err := authz.New(cfg.LegacyAPIURL, authz.Options{})
	if err != nil {
		return err
	}

	hub := progress.NewHub(pool, log)
	go hub.Run(ctx)

	catalog := pluginrun.LoadCatalog(cfg.Plugins)
	pluginrun.LogCatalog(log, catalog)

	closing := make(chan struct{})
	handler, err := server.New(server.Deps{
		Config: cfg, Pool: pool, Authz: az, Hub: hub, Logger: log,
		Version: version, Closing: closing,
		Routes: []func(*http.ServeMux){pluginrun.NewAPI(pool, catalog, az).Mount},
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// No WriteTimeout: SSE streams and long FastAPI requests are open-ended;
		// the proxy bounds the wait for upstream headers instead.
	}
	srv.RegisterOnShutdown(func() { close(closing) })
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("serve: listen %s: %w", cfg.Listen, err)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	workerDone := make(chan error, 1)
	if *withWorker {
		go func() {
			workerDone <- queue.RunWorker(ctx, queue.Env{Pool: pool, Config: cfg, Logger: log})
		}()
	} else {
		workerDone <- nil
	}
	log.Info("serving", "addr", ln.Addr().String(), "legacy_api", cfg.LegacyAPIURL,
		"web_dir", cfg.WebDir, "worker", *withWorker)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown incomplete", "err", err)
	}
	if err := <-workerDone; err != nil {
		log.Error("worker stopped with error", "err", err)
	}
	log.Info("stopped")
	return nil
}
