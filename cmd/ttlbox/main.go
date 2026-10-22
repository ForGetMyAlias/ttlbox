package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ttlbox/internal/config"
	"ttlbox/internal/httpapi"
	"ttlbox/internal/ratelimit"
	"ttlbox/internal/service"
	"ttlbox/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := storage.NewFS(cfg.DataDir, cfg.MaxTotal)
	if err != nil {
		return err
	}
	svc := service.New(store, cfg.DefaultTTL, cfg.MaxTTL)

	opts := httpapi.Options{
		BaseURL: cfg.BaseURL, Token: cfg.Token, MaxBody: cfg.MaxSize,
		TrustProxy: cfg.TrustProxy, Log: log,
	}
	if cfg.UploadRate > 0 {
		opts.UploadLimit = ratelimit.New(cfg.UploadRate, cfg.UploadRate)
		go opts.UploadLimit.GC(ctx)
	}
	if cfg.DownloadRate > 0 {
		opts.DownloadLimit = ratelimit.New(cfg.DownloadRate, cfg.DownloadRate)
		go opts.DownloadLimit.GC(ctx)
	}
	if cfg.Token == "" {
		log.Warn("UPLOAD_TOKEN is empty: anyone who can reach this server can upload")
	}

	go func() {
		for {
			if n, err := svc.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.Error("sweep", "err", err)
			} else if n > 0 {
				log.Info("swept", "removed", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(svc, opts),
		ReadHeaderTimeout: 5 * time.Second,  // slowloris
		ReadTimeout:       10 * time.Minute, // bounds slow uploads (~85 KB/s floor at 50 MB)
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
