package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/handler"
	"github.com/KevinZhao/SmartRenew/notifier"
	"github.com/KevinZhao/SmartRenew/scheduler"
	"github.com/KevinZhao/SmartRenew/store"
)

//go:embed frontend
var frontendFiles embed.FS

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	slog.Info("config loaded", "accounts", len(cfg.Accounts), "listen", cfg.ListenAddr)

	db, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}
	defer db.Close()

	// Build notifiers
	var notifiers []notifier.Notifier
	for _, nc := range cfg.Notifiers {
		if !nc.Enabled {
			continue
		}
		switch nc.Type {
		case "lark":
			notifiers = append(notifiers, notifier.NewLark(nc.WebhookURL))
			slog.Info("notifier enabled", "type", "lark")
		default:
			slog.Warn("unknown notifier type, skipped", "type", nc.Type)
		}
	}

	sc := scheduler.New(cfg, db, notifiers)

	// Embed frontend
	frontendFS, err := fs.Sub(frontendFiles, "frontend")
	if err != nil {
		log.Fatalf("frontend embed: %v", err)
	}
	h := handler.New(db, sc, cfg, frontendFS)

	// Start scheduler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc.StartCron(ctx)

	// Start HTTP server with timeouts
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Minute, // generous for long sync
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		slog.Info("SmartRenew running", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}
