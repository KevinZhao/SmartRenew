package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KevinZhao/SmartRenew/auth"
)

func boolPtr(b bool) *bool { return &b }

func TestAuthIsEnabledDefaultsToTrue(t *testing.T) {
	tests := []struct {
		name string
		cfg  *AuthConfig
		want bool
	}{
		{"nil config", nil, true},
		{"field omitted", &AuthConfig{}, true},
		{"explicitly true", &AuthConfig{Enabled: boolPtr(true)}, true},
		{"explicitly false", &AuthConfig{Enabled: boolPtr(false)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAuthEnabledDefaultsToTrueWhenJSONOmitsIt(t *testing.T) {
	// Regression guard: a config without an "auth" block must still enforce
	// login, not silently leave the app open.
	var cfg Config
	if err := json.Unmarshal([]byte(`{"listen_addr":":5000"}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.Auth.IsEnabled() {
		t.Fatal("auth defaulted to disabled when the config omits the auth block")
	}
}

func TestParseSessionTTL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty defaults to 12h", "", 12 * time.Hour},
		{"valid duration", "2h30m", 2*time.Hour + 30*time.Minute},
		{"invalid falls back", "not-a-duration", 12 * time.Hour},
		{"zero falls back", "0s", 12 * time.Hour},
		{"negative falls back", "-1h", 12 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &AuthConfig{SessionTTL: tc.in}
			if got := a.ParseSessionTTL(); got != tc.want {
				t.Fatalf("ParseSessionTTL(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
	if got := (*AuthConfig)(nil).ParseSessionTTL(); got != 12*time.Hour {
		t.Fatalf("nil ParseSessionTTL() = %v, want 12h", got)
	}
}

func TestParseLockoutDuration(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty defaults to 15m", "", 15 * time.Minute},
		{"valid duration", "30m", 30 * time.Minute},
		{"invalid falls back", "bogus", 15 * time.Minute},
		{"zero falls back", "0", 15 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &AuthConfig{LockoutDuration: tc.in}
			if got := a.ParseLockoutDuration(); got != tc.want {
				t.Fatalf("ParseLockoutDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMaxAttempts(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"unset defaults to 5", 0, 5},
		{"custom value", 10, 10},
		{"negative falls back", -3, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &AuthConfig{MaxLoginAttempts: tc.in}
			if got := a.MaxAttempts(); got != tc.want {
				t.Fatalf("MaxAttempts(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateAuth(t *testing.T) {
	users := []auth.User{{Username: "alice", Password: "pw"}}
	tests := []struct {
		name    string
		cfg     AuthConfig
		wantErr bool
	}{
		{"enabled with users", AuthConfig{Users: users}, false},
		{"enabled without users", AuthConfig{}, true},
		{"disabled without users is fine", AuthConfig{Enabled: boolPtr(false)}, false},
		{"invalid session_ttl", AuthConfig{Users: users, SessionTTL: "abc"}, true},
		{"invalid lockout_duration", AuthConfig{Users: users, LockoutDuration: "abc"}, true},
		{"valid durations", AuthConfig{Users: users, SessionTTL: "8h", LockoutDuration: "5m"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuth(&tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestResolveUserPasswords(t *testing.T) {
	hash := "pbkdf2-sha256$600000$c2FsdA$a2V5"
	t.Setenv("SMARTRENEW_USER_ALICE_PASSWORD_HASH", hash)
	t.Setenv("SMARTRENEW_USER_OPS_TEAM_PASSWORD_HASH", "ops-hash")
	t.Setenv("SMARTRENEW_USER_BOB_PASSWORD_HASH", "should-not-be-used")

	a := &AuthConfig{Users: []auth.User{
		{Username: "alice"},
		{Username: "ops-team"},
		{Username: "bob", PasswordHash: "existing-hash"},
		{Username: "carol", Password: "plain"},
		{Username: "dave"}, // no env var set
	}}
	resolveUserPasswords(a)

	if a.Users[0].PasswordHash != hash {
		t.Errorf("alice hash = %q, want %q", a.Users[0].PasswordHash, hash)
	}
	if a.Users[1].PasswordHash != "ops-hash" {
		t.Errorf("ops-team hash = %q, want ops-hash (dashes should map to underscores)", a.Users[1].PasswordHash)
	}
	if a.Users[2].PasswordHash != "existing-hash" {
		t.Errorf("bob hash = %q — env must not override an explicit hash", a.Users[2].PasswordHash)
	}
	if a.Users[3].PasswordHash != "" || a.Users[3].Password != "plain" {
		t.Errorf("carol changed: %+v — an explicit plaintext password must be left alone", a.Users[3])
	}
	if a.Users[4].PasswordHash != "" {
		t.Errorf("dave hash = %q, want empty", a.Users[4].PasswordHash)
	}
}

// --- Load() integration ---

// writeConfig writes cfg JSON to a temp dir and points Load() at it.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("SMARTRENEW_CONFIG_FILE", path)
	return dir
}

const validAccounts = `"accounts":[{"alias":"a1","account_id":"111122223333","access_key":"AKIAX","secret_key":"s","regions":["us-east-1"]}]`

func TestLoadAuthFromConfig(t *testing.T) {
	writeConfig(t, `{`+validAccounts+`,"auth":{"users":[{"username":"alice","password":"pw"}],"session_ttl":"4h","max_login_attempts":7,"lockout_duration":"20m","cookie_secure":true}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.IsEnabled() {
		t.Error("auth should be enabled by default")
	}
	if len(cfg.Auth.Users) != 1 || cfg.Auth.Users[0].Username != "alice" {
		t.Fatalf("users = %+v, want one user alice", cfg.Auth.Users)
	}
	if got := cfg.Auth.ParseSessionTTL(); got != 4*time.Hour {
		t.Errorf("session TTL = %v, want 4h", got)
	}
	if got := cfg.Auth.MaxAttempts(); got != 7 {
		t.Errorf("max attempts = %d, want 7", got)
	}
	if got := cfg.Auth.ParseLockoutDuration(); got != 20*time.Minute {
		t.Errorf("lockout = %v, want 20m", got)
	}
	if !cfg.Auth.CookieSecure {
		t.Error("cookie_secure = false, want true")
	}
}

func TestLoadFailsWhenAuthEnabledWithoutUsers(t *testing.T) {
	writeConfig(t, `{`+validAccounts+`}`)
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with auth enabled and no users — the app would start unprotected")
	}
}

func TestLoadAllowsAuthDisabledWithoutUsers(t *testing.T) {
	writeConfig(t, `{`+validAccounts+`,"auth":{"enabled":false}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.IsEnabled() {
		t.Fatal("auth should be disabled")
	}
}

func TestLoadUsersFromUsersFile(t *testing.T) {
	dir := writeConfig(t, `{`+validAccounts+`,"auth":{"users":[{"username":"in-config","password":"pw"}],"users_file":"USERS_PATH"}}`)
	usersPath := filepath.Join(dir, "users.json")
	if err := os.WriteFile(usersPath, []byte(`[{"username":"from-file","password":"pw"}]`), 0o600); err != nil {
		t.Fatalf("write users: %v", err)
	}
	// Rewrite the config with the real path now that it is known.
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{`+validAccounts+`,"auth":{"users":[{"username":"in-config","password":"pw"}],"users_file":"`+usersPath+`"}}`), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Auth.Users) != 1 || cfg.Auth.Users[0].Username != "from-file" {
		t.Fatalf("users = %+v, want users_file to replace the inline list", cfg.Auth.Users)
	}
}

func TestLoadUsersFileEnvOverridesConfig(t *testing.T) {
	dir := writeConfig(t, `{`+validAccounts+`,"auth":{"users":[{"username":"in-config","password":"pw"}]}}`)
	envPath := filepath.Join(dir, "env-users.json")
	if err := os.WriteFile(envPath, []byte(`[{"username":"from-env","password":"pw"}]`), 0o600); err != nil {
		t.Fatalf("write users: %v", err)
	}
	t.Setenv("SMARTRENEW_USERS_FILE", envPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Auth.Users) != 1 || cfg.Auth.Users[0].Username != "from-env" {
		t.Fatalf("users = %+v, want SMARTRENEW_USERS_FILE to win", cfg.Auth.Users)
	}
}

func TestLoadFailsOnUnreadableUsersFile(t *testing.T) {
	dir := writeConfig(t, `{`+validAccounts+`}`)
	t.Setenv("SMARTRENEW_USERS_FILE", filepath.Join(dir, "missing.json"))
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with a missing users file")
	}
}

func TestLoadFailsOnMalformedUsersFile(t *testing.T) {
	dir := writeConfig(t, `{`+validAccounts+`}`)
	bad := filepath.Join(dir, "users.json")
	if err := os.WriteFile(bad, []byte(`{not-an-array}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("SMARTRENEW_USERS_FILE", bad)
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with a malformed users file")
	}
}

func TestLoadResolvesUserHashFromEnv(t *testing.T) {
	writeConfig(t, `{`+validAccounts+`,"auth":{"users":[{"username":"alice"}]}}`)
	t.Setenv("SMARTRENEW_USER_ALICE_PASSWORD_HASH", "pbkdf2-sha256$600000$c2FsdA$a2V5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Users[0].PasswordHash == "" {
		t.Fatal("password hash was not resolved from the environment")
	}
}
