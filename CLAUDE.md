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

No tests exist yet — test files need to be created.

## Config

JSON config loaded from `SMARTRENEW_CONFIG_FILE` env (default: `config.json`). Accounts can be split to a separate file via `accounts_file` field or `SMARTRENEW_ACCOUNTS_FILE` env. Each account requires its own access_key/secret_key. See `config.example.json` for schema.

## Architecture

```
main.go          — entry point, wires all components, embeds frontend/, starts HTTP + cron
config/          — JSON config loading, validation, env overrides
provider/        — AWS API calls (EC2 + SavingsPlans SDKs), returns []model.Reservation
store/           — SQLite persistence (WAL mode), schema auto-migration, upsert/query/notify-log
scheduler/       — Cron loop (sync every 6h, alert check every 1h), dedup via notify_log table
handler/         — HTTP API (net/http ServeMux, Go 1.22+ method routing) + embedded SPA
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

- `GET /api/reservations?type=&account=` — list with optional filters
- `GET /api/alerts` — expiring resources within `max(remind_days)` window
- `POST /api/sync` — trigger manual sync (5min timeout)
- `GET /api/export` — CSV download
- `POST /api/import` — CSV upload (multipart)
- `GET /api/health` — SQLite ping
- `GET /` — embedded Vue SPA

## Known Issues

See `TODO.md` for prioritized code review findings (CRITICAL/HIGH/MEDIUM/LOW).
