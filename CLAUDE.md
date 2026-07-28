# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is SmartRenew

AWS reservation lifecycle manager. Syncs Savings Plans (SP), Capacity Blocks (CB), On-Demand Capacity Reservations (ODCR), and Reserved Instances (RI) across multiple AWS accounts/regions, stores them in SQLite, and sends Lark webhook alerts when resources approach expiry. Each AWS account is configured with its own AKSK (Access Key / Secret Key) — no Organization or STS AssumeRole dependency.

## Build & Run

```bash
# Build (requires CGO for sqlite3)
CGO_ENABLED=1 go build -o smartrenew .

# Run locally via Docker
cp config.example.json config.json  # edit with real credentials
./run-local.sh [config.json path] [data dir]

# Run binary directly
SMARTRENEW_CONFIG_FILE=config.json ./smartrenew

# Docker build
docker build -t smartrenew .
```

## Test

```bash
go test ./...              # run all tests
go test -race ./...        # with race detector
go test -cover ./...       # with coverage
go test ./store/...        # single package
```

Tests exist for `auth/`, `config/`, `csvutil/`, `handler/`, `model/`, `notifier/`,
`scheduler/` and `store/`. `provider/` is only lightly covered (the AWS API calls
themselves are untested).

### Timestamp storage

SQLite has no date type and compares TEXT lexicographically, so every timestamp
column is stored as UTC in a single format (`2006-01-02 15:04:05`, see
`store.sqlTimeLayout`) and range queries wrap columns in `julianday()`.

Do NOT store RFC3339: its `T` separator (0x54) sorts after the space (0x20) that
`datetime('now')` emits, so a same-day expiry compares as greater than "now" and
already-expired resources leak into alerts. Offsets like `+08:00` break ordering
the same way. `store.migrate()` normalises legacy rows written in RFC3339.

## Config

JSON config loaded from `SMARTRENEW_CONFIG_FILE` env (default: `config.json`). Accounts can be split to a separate file via `accounts_file` field or `SMARTRENEW_ACCOUNTS_FILE` env. Each account requires its own access_key/secret_key. See `config.example.json` for schema.

## Architecture

```
main.go          — entry point, wires all components, embeds frontend/, starts HTTP + cron
config/          — JSON config loading, validation, env overrides
provider/        — AWS API calls (EC2 + SavingsPlans SDKs), returns []model.Reservation
store/           — SQLite persistence (WAL mode), schema auto-migration, upsert/query/notify-log
scheduler/       — Cron loop (sync every 6h, alert check every 1h), dedup via notify_log table
auth/            — password hashing (PBKDF2), in-memory sessions, static user list, login rate limiting
handler/         — HTTP API (net/http ServeMux, Go 1.22+ method routing) + auth middleware + embedded SPA
notifier/        — Notifier interface + Lark webhook implementation
model/           — Shared types: Reservation, Alert, AlertLevel, ResourceType constants
csvutil/         — CSV import/export for reservations
frontend/        — Static SPA (Vue 3 CDN, single index.html + app.js), embedded via go:embed
deploy/k8s/      — Kubernetes manifests (namespace, deployment, service, ingress, pvc, configmap, secret)
```

### Key data flow

1. `provider.SyncAccount` calls AWS APIs per account (using direct AKSK) → `[]model.Reservation`
2. `scheduler.SyncAll` delete-then-repopulates per account in SQLite
3. `scheduler.CheckAndNotify` queries expiring reservations, dedup via `notify_log` table, sends to all enabled notifiers
4. `handler` serves REST API + embedded Vue SPA

### Resource types

| Const | AWS Resource | API | Scope |
|-------|-------------|-----|-------|
| `sp` | Savings Plans | `DescribeSavingsPlans` | Global (first region) |
| `cb` | Capacity Blocks | `DescribeCapacityReservations` | Per-region |
| `odcr` | On-Demand Capacity Reservations | `DescribeCapacityReservations` | Per-region |
| `ri` | EC2 Reserved Instances | `DescribeReservedInstances` | Per-region |
| `rds_ri` | RDS Reserved Instances | `DescribeReservedDBInstances` | Per-region |
| `cache_ri` | ElastiCache Reserved Nodes | `DescribeReservedCacheNodes` | Per-region |
| `redshift_ri` | Redshift Reserved Nodes | `DescribeReservedNodes` | Per-region |
| `opensearch_ri` | OpenSearch Reserved Instances | `DescribeReservedInstances` | Per-region |
| `memorydb_ri` | MemoryDB Reserved Nodes | `DescribeReservedNodes` | Per-region |
| `bedrock_pt` | Bedrock Provisioned Throughput | `ListProvisionedModelThroughputs` | Per-region |

### API endpoints

All endpoints except `/api/health`, `/api/login`, `/api/me` and the login page
assets require a valid session cookie (see Auth below).

- `GET /api/reservations?type=&account=` — list with optional filters
- `GET /api/alerts` — expiring resources within `max(remind_days)` window
- `POST /api/sync` — trigger manual sync in background, returns 202 (409 if already running)
- `GET /api/sync/status` — poll background sync progress
- `GET /api/export` — CSV download
- `POST /api/import` — CSV upload (multipart)
- `GET /api/gpu-coverage?account=` — GPU instance coverage list
- `GET /api/health` — SQLite ping (public: k8s probes)
- `POST /api/login` — `{username, password}` → session cookie
- `POST /api/logout` — revoke session
- `GET /api/me` — current auth state (401 when unauthenticated)
- `GET /` — embedded Vue SPA (redirects to `/login.html` when unauthenticated)

## Auth

Static username/password login — no user management, users are fixed in config.

```bash
# Generate a password hash for auth.users[].password_hash
./smartrenew -hash-password 'your-password'
```

Config lives under the `auth` key (see `config.example.json`). Users can also be
loaded from a separate file via `auth.users_file` / `SMARTRENEW_USERS_FILE` (used
by `deploy/k8s/secret.yaml`), or a single hash injected via
`SMARTRENEW_USER_<USERNAME_UPPER>_PASSWORD_HASH`.

- `auth.enabled` defaults to **true**; startup fails if no users are configured.
- Passwords are stored as PBKDF2-SHA256, 600k iterations (`auth/password.go`).
  A plaintext `password` field is accepted for dev and hashed at startup.
- Sessions are in-memory (`auth/session.go`), so a restart logs everyone out.
  Single replica only — `deployment.yaml` uses `strategy: Recreate`.
- Failed logins are throttled per client IP *and* per username
  (`auth/ratelimit.go`); the username limiter is what stops brute force that
  rotates `X-Forwarded-For`.
- State-changing requests (`POST`) additionally require a same-origin
  `Origin`/`Referer`, since `SameSite=Lax` ignores ports and sibling subdomains.
- Set `auth.cookie_secure: true` when serving over HTTPS.

## Known Issues

See `TODO.md` for prioritized code review findings (CRITICAL/HIGH/MEDIUM/LOW).
