package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
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

	"github.com/KevinZhao/SmartRenew/auth"
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
	hashPassword := flag.String("hash-password", "", "print a PBKDF2 hash for the given password and exit (for auth.users[].password_hash)")
	flag.Parse()

	if *hashPassword != "" {
		h, err := auth.HashPassword(*hashPassword)
		if err != nil {
			log.Fatalf("hash password: %v", err)
		}
		fmt.Println(h)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Auto-resolve missing account IDs via STS GetCallerIdentity. A failure on a
	// notifier-only account is logged and tolerated; a failure on an account we
	// actually sync is fatal.
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	degraded, err := config.ResolveAccountIDs(resolveCtx, cfg.Accounts,
		func(ctx context.Context, accessKey, secretKey, region string) (string, error) {
			return provider.ResolveAccountID(ctx, aws.Config{
				Region:      region,
				Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
			})
		})
	resolveCancel()
	if err != nil {
		log.Fatalf("%v", err)
	}
	if len(degraded) > 0 {
		slog.Warn("starting with degraded notifier accounts", "aliases", degraded)
	}

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
			// Credentials are not verified here; a stale key surfaces as a send
			// error per cycle rather than blocking startup.
			for _, alias := range degraded {
				if alias == nc.AccountAlias {
					slog.Warn("sns notifier uses an account whose credentials could not be verified; "+
						"sends will likely fail until the key is rotated",
						"account_alias", alias, "topic", nc.TopicARN)
				}
			}
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

	// Build auth (static users from config). Fails fast on bad hashes.
	var handlerOpts []handler.Option
	if cfg.Auth.IsEnabled() {
		authn, err := auth.NewAuthenticator(cfg.Auth.Users)
		if err != nil {
			log.Fatalf("init auth: %v", err)
		}
		sessions := auth.NewSessionStore(cfg.Auth.ParseSessionTTL())
		limiter := auth.NewLoginLimiter(cfg.Auth.MaxAttempts(), cfg.Auth.ParseLockoutDuration())
		// Per-username limiter: looser threshold so a third party spraying a
		// known username cannot trivially lock the real user out, while still
		// capping brute force that rotates X-Forwarded-For.
		userLimiter := auth.NewLoginLimiter(cfg.Auth.MaxAttempts()*4, cfg.Auth.ParseLockoutDuration())
		handlerOpts = append(handlerOpts, handler.WithAuth(authn, sessions, limiter, userLimiter, cfg.Auth.CookieSecure))
		slog.Info("auth enabled",
			"users", authn.Usernames(),
			"session_ttl", cfg.Auth.ParseSessionTTL().String(),
			"max_login_attempts", cfg.Auth.MaxAttempts(),
			"lockout", cfg.Auth.ParseLockoutDuration().String(),
			"cookie_secure", cfg.Auth.CookieSecure)
	} else {
		slog.Warn("auth disabled — UI and API are open to anyone who can reach the port")
	}

	h := handler.New(db, sc, cfg, frontendFS, handlerOpts...)

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
