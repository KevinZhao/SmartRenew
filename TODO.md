# SmartRenew TODO — Code Review Findings

> Generated: 2026-04-13  
> Last verified: 2026-04-14

## MEDIUM

- [x] `scheduler/scheduler.go:84-105` — Check-send-mark non-atomic → `MarkNotifiedBatch` 事务化批量标记
- [x] `handler/api.go:18` — Concrete types prevent handler unit testing → 定义 `ReservationStore` + `Syncer` 接口

## LOW

- [ ] `handler/api.go:125` — CSV import `ReadAll` buffers entire file in memory (32MB 上限已足够)
- [ ] `go.mod:1` — Bare module path `smartrenew`, should be fully qualified (推仓库时再改)
- [x] All packages — Global `log.Printf` → migrated to `log/slog` structured logging
