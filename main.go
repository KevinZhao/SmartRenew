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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/handler"
	"github.com/KevinZhao/SmartRenew/notifier"
	"github.com/KevinZhao/SmartRenew/provider"
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

	// Auto-resolve missing account IDs via STS GetCallerIdentity.
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.AccountID != "" || len(a.Regions) == 0 {
			continue
		}
		awsCfg := aws.Config{
			Region:      a.Regions[0],
			Credentials: credentials.NewStaticCredentialsProvider(a.AccessKey, a.SecretKey, ""),
		}
		id, err := provider.ResolveAccountID(resolveCtx, awsCfg)
		if err != nil {
			log.Fatalf("resolve account_id for %q: %v", a.Alias, err)
		}
		a.AccountID = id
		slog.Info("account_id resolved via STS", "alias", a.Alias, "account_id", id)
	}
	resolveCancel()

	// Apply GPU card count overrides (config → provider registry).
	if len(cfg.GPUCardCounts) > 0 {
		provider.SetGPUCardOverrides(cfg.GPUCardCounts)
		slog.Info("gpu card count overrides applied", "entries", len(cfg.GPUCardCounts))
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
		case "sns":
			acct := findAccount(cfg.Accounts, nc.AccountAlias)
			if acct == nil {
				log.Fatalf("sns notifier: account_alias %q not found in configured accounts", nc.AccountAlias)
			}
			awsCfg := aws.Config{
				Region:      nc.Region,
				Credentials: credentials.NewStaticCredentialsProvider(acct.AccessKey, acct.SecretKey, ""),
			}
			notifiers = append(notifiers, notifier.NewSNS(awsCfg, nc.TopicARN))
			slog.Info("notifier enabled", "type", "sns", "topic", nc.TopicARN, "region", nc.Region, "via_account", nc.AccountAlias)
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

func findAccount(accounts []config.Account, alias string) *config.Account {
	for i := range accounts {
		if accounts[i].Alias == alias {
			return &accounts[i]
		}
	}
	return nil
}
