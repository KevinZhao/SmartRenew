package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Account struct {
	Alias     string   `json:"alias"`
	AccountID string   `json:"account_id"`
	AccessKey string   `json:"access_key"`
	SecretKey string   `json:"secret_key"`
	Regions   []string `json:"regions"`
	// SkipTypes lists reservation types to skip fetching for this account,
	// typically used when the IAM principal lacks permissions for a service.
	// Accepted values: rds_ri, cache_ri, redshift_ri, opensearch_ri, memorydb_ri, bedrock_pt.
	SkipTypes []string `json:"skip_types"`
}

// ShouldSkip reports whether the given type identifier is in SkipTypes.
func (a Account) ShouldSkip(typeID string) bool {
	for _, t := range a.SkipTypes {
		if t == typeID {
			return true
		}
	}
	return false
}

type NotifyConfig struct {
	Enabled    bool   `json:"enabled"`
	Type       string `json:"type"`        // "lark" | "sns"
	WebhookURL string `json:"webhook_url"` // lark only
	// SNS-specific fields
	TopicARN     string `json:"topic_arn"`     // SNS topic to publish to
	Region       string `json:"region"`        // SNS region, e.g. ap-northeast-1
	AccountAlias string `json:"account_alias"` // which account's AKSK to use when publishing
}

type Config struct {
	Accounts      []Account      `json:"accounts"`
	AccountsFile  string         `json:"accounts_file"` // optional: load accounts from separate file
	Notifiers     []NotifyConfig `json:"notifiers"`
	RemindDays    []int          `json:"remind_days"`
	SyncInterval  string         `json:"sync_interval"`  // e.g. "6h", "30m"
	AlertInterval string         `json:"alert_interval"` // e.g. "1h", "15m"
	ListenAddr    string         `json:"listen_addr"`
	DBPath        string         `json:"db_path"`
	// GPUCardCounts overrides built-in instance_type → GPU card count map.
	// Keys can be full instance_type ("p5.48xlarge") or family ("p5").
	GPUCardCounts map[string]int `json:"gpu_card_counts"`
}

func DefaultConfig() *Config {
	return &Config{
		RemindDays:    []int{30, 14, 7, 3, 1},
		SyncInterval:  "6h",
		AlertInterval: "1h",
		ListenAddr:    ":5000",
		DBPath:        "/data/smartrenew.db",
	}
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	path := os.Getenv("SMARTRENEW_CONFIG_FILE")
	if path == "" {
		path = "config.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Load accounts from file. Priority: SMARTRENEW_ACCOUNTS_FILE env > accounts_file in config.
	// Env var overrides (replaces) the config field, not appends.
	accountsPath := cfg.AccountsFile
	if envFile := os.Getenv("SMARTRENEW_ACCOUNTS_FILE"); envFile != "" {
		accountsPath = envFile
	}
	if accountsPath != "" {
		resolved, err := filepath.Abs(accountsPath)
		if err != nil {
			return nil, fmt.Errorf("resolve accounts path %s: %w", accountsPath, err)
		}
		acctData, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("read accounts file %s: %w", resolved, err)
		}
		var accounts []Account
		if err := json.Unmarshal(acctData, &accounts); err != nil {
			return nil, fmt.Errorf("parse accounts file: %w", err)
		}
		cfg.Accounts = accounts // replace, not append
	}

	resolveCredentials(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func validate(cfg *Config) error {
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("no accounts configured (set in config or via accounts_file / SMARTRENEW_ACCOUNTS_FILE)")
	}
	for i, a := range cfg.Accounts {
		if a.Alias == "" {
			return fmt.Errorf("account[%d]: alias is required", i)
		}
		// account_id is optional — will be auto-resolved via STS GetCallerIdentity at startup.
		if a.AccessKey == "" || a.SecretKey == "" {
			return fmt.Errorf("account[%d] %q: access_key and secret_key are required", i, a.Alias)
		}
		if len(a.Regions) == 0 {
			return fmt.Errorf("account[%d] %q: at least one region is required", i, a.Alias)
		}
	}
	for i, n := range cfg.Notifiers {
		if !n.Enabled {
			continue
		}
		switch n.Type {
		case "lark":
			if n.WebhookURL == "" {
				return fmt.Errorf("notifier[%d] lark: webhook_url is required when enabled", i)
			}
		case "sns":
			if n.TopicARN == "" {
				return fmt.Errorf("notifier[%d] sns: topic_arn is required when enabled", i)
			}
			if n.Region == "" {
				return fmt.Errorf("notifier[%d] sns: region is required when enabled", i)
			}
		}
	}
	for _, d := range cfg.RemindDays {
		if d <= 0 {
			return fmt.Errorf("remind_days must be positive integers, got %d", d)
		}
	}
	if cfg.SyncInterval != "" {
		if _, err := time.ParseDuration(cfg.SyncInterval); err != nil {
			return fmt.Errorf("invalid sync_interval %q: %w", cfg.SyncInterval, err)
		}
	}
	if cfg.AlertInterval != "" {
		if _, err := time.ParseDuration(cfg.AlertInterval); err != nil {
			return fmt.Errorf("invalid alert_interval %q: %w", cfg.AlertInterval, err)
		}
	}
	return nil
}

// MaxRemindDays returns the largest value in RemindDays.
func (c *Config) MaxRemindDays() int {
	largest := 30
	for _, d := range c.RemindDays {
		if d > largest {
			largest = d
		}
	}
	return largest
}

// ParseSyncInterval returns the sync interval as a duration, defaulting to 6h.
func (c *Config) ParseSyncInterval() time.Duration {
	if d, err := time.ParseDuration(c.SyncInterval); err == nil && d > 0 {
		return d
	}
	return 6 * time.Hour
}

// ParseAlertInterval returns the alert check interval as a duration, defaulting to 1h.
func (c *Config) ParseAlertInterval() time.Duration {
	if d, err := time.ParseDuration(c.AlertInterval); err == nil && d > 0 {
		return d
	}
	return 1 * time.Hour
}

// resolveCredentials fills empty AccessKey/SecretKey from environment variables.
// Pattern: SMARTRENEW_<ALIAS_UPPER>_ACCESS_KEY / SMARTRENEW_<ALIAS_UPPER>_SECRET_KEY
func resolveCredentials(cfg *Config) {
	for i := range cfg.Accounts {
		acct := &cfg.Accounts[i]
		if acct.AccessKey != "" && acct.SecretKey != "" {
			continue
		}
		envPrefix := "SMARTRENEW_" + strings.ToUpper(strings.ReplaceAll(acct.Alias, "-", "_"))
		if acct.AccessKey == "" {
			acct.AccessKey = os.Getenv(envPrefix + "_ACCESS_KEY")
		}
		if acct.SecretKey == "" {
			acct.SecretKey = os.Getenv(envPrefix + "_SECRET_KEY")
		}
		if acct.AccessKey != "" && acct.SecretKey != "" {
			slog.Info("account credentials resolved from env", "alias", acct.Alias, "prefix", envPrefix+"_*")
		}
	}
}
