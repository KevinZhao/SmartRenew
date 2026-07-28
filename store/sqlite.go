package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KevinZhao/SmartRenew/model"
	_ "github.com/mattn/go-sqlite3"
)

// sqlTimeLayout is the format used for every timestamp column. SQLite has no
// date type and compares TEXT lexicographically, so all timestamps are stored
// as UTC in this single format — that makes string ordering agree with
// chronological ordering and matches what datetime() produces.
//
// RFC3339 must NOT be used here: its 'T' separator (0x54) sorts after the space
// (0x20) that datetime('now') emits, so a same-day timestamp would compare as
// greater than "now" no matter the actual time. Offsets like +08:00 break the
// ordering the same way.
const sqlTimeLayout = "2006-01-02 15:04:05"

// formatTime renders t for storage: UTC, in sqlTimeLayout. The zero time maps
// to an empty string so "no expiry" is distinguishable from a real date and is
// never matched by a range query.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(sqlTimeLayout)
}

// parseTime reads a stored timestamp. It accepts the canonical layout and, for
// rows written by older versions, RFC3339.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(sqlTimeLayout, s); err == nil {
		return t.UTC(), nil
	}
	// Legacy rows (and SQLite's own sub-second variants) may carry RFC3339.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

type Store struct {
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	// Serialize writers at the driver level — SQLite allows one writer at a time,
	// and multiple open conns can cause SQLITE_BUSY even with busy_timeout.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS reservations (
			id TEXT PRIMARY KEY,
			account_alias TEXT NOT NULL,
			account_id TEXT NOT NULL,
			region TEXT NOT NULL,
			type TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			instance_type TEXT DEFAULT '',
			platform TEXT DEFAULT '',
			quantity INTEGER DEFAULT 1,
			used_count INTEGER DEFAULT 0,
			start_time TEXT,
			end_time TEXT,
			status TEXT DEFAULT 'active',
			description TEXT DEFAULT '',
			updated_at TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS notify_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reservation_id TEXT NOT NULL,
			level TEXT NOT NULL,
			notified_at TEXT DEFAULT (datetime('now')),
			UNIQUE(reservation_id, level)
		);
	`)
	if err != nil {
		return err
	}
	// Best-effort column adds for upgrades from older schema; idempotent —
	// fails with "duplicate column" on subsequent runs, which we intentionally ignore.
	_, _ = s.db.Exec("ALTER TABLE reservations ADD COLUMN used_count INTEGER DEFAULT 0")
	_, _ = s.db.Exec("ALTER TABLE reservations ADD COLUMN hourly_rate REAL DEFAULT 0")
	_, _ = s.db.Exec("ALTER TABLE reservations ADD COLUMN equiv_cores REAL DEFAULT 0")
	_, _ = s.db.Exec("ALTER TABLE reservations ADD COLUMN capacity_unit TEXT DEFAULT ''")

	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS gpu_coverage (
			id TEXT PRIMARY KEY,
			account_alias TEXT NOT NULL,
			account_id TEXT NOT NULL,
			region TEXT NOT NULL,
			az TEXT DEFAULT '',
			instance_id TEXT NOT NULL,
			instance_type TEXT NOT NULL,
			coverage TEXT NOT NULL,
			coverage_ref TEXT DEFAULT '',
			sp_rate REAL DEFAULT 0,
			updated_at TEXT DEFAULT (datetime('now'))
		);
	`)
	if err != nil {
		return err
	}

	// notify_log gained an end_time column so that a renewed resource (same
	// reservation id, later expiry) alerts again instead of being deduped
	// against the previous cycle forever.
	_, _ = s.db.Exec("ALTER TABLE notify_log ADD COLUMN end_time TEXT DEFAULT ''")
	if err := s.migrateNotifyLogKey(); err != nil {
		return err
	}

	return s.normaliseStoredTimestamps()
}

// migrateNotifyLogKey replaces the old UNIQUE(reservation_id, level) index with
// one that also covers end_time. Rows written before this migration keep an
// empty end_time, so they no longer suppress the current cycle.
func (s *Store) migrateNotifyLogKey() error {
	var indexSQL string
	err := s.db.QueryRow(`
		SELECT COALESCE(group_concat(sql, ';'), '') FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'notify_log' AND sql IS NOT NULL`).Scan(&indexSQL)
	if err != nil {
		return fmt.Errorf("inspect notify_log indexes: %w", err)
	}
	if strings.Contains(indexSQL, "end_time") {
		return nil // already migrated
	}

	// The old uniqueness came from an inline UNIQUE() constraint, which cannot
	// be dropped in place — rebuild the table.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS notify_log_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reservation_id TEXT NOT NULL,
			level TEXT NOT NULL,
			end_time TEXT DEFAULT '',
			notified_at TEXT DEFAULT (datetime('now')),
			UNIQUE(reservation_id, level, end_time)
		);
		INSERT OR IGNORE INTO notify_log_new (reservation_id, level, end_time, notified_at)
			SELECT reservation_id, level, COALESCE(end_time, ''), notified_at FROM notify_log;
		DROP TABLE notify_log;
		ALTER TABLE notify_log_new RENAME TO notify_log;
	`)
	if err != nil {
		return fmt.Errorf("migrate notify_log key: %w", err)
	}
	slog.Info("notify_log migrated to include end_time in the dedup key")
	return nil
}

// normaliseStoredTimestamps rewrites timestamps left by older versions (RFC3339
// with a 'T' separator, possibly with a non-UTC offset) into the canonical
// space-separated UTC layout, so range queries order correctly.
func (s *Store) normaliseStoredTimestamps() error {
	type target struct {
		table string
		cols  []string
	}
	targets := []target{
		{"reservations", []string{"start_time", "end_time", "updated_at"}},
		{"gpu_coverage", []string{"updated_at"}},
	}
	for _, tg := range targets {
		for _, col := range tg.cols {
			// strftime() parses ISO-8601 (including the 'T' form and offsets)
			// and re-renders it in the canonical layout, normalising to UTC.
			q := fmt.Sprintf(
				`UPDATE %s SET %s = strftime('%%Y-%%m-%%d %%H:%%M:%%S', %s)
				 WHERE %s LIKE '%%T%%' AND strftime('%%Y-%%m-%%d %%H:%%M:%%S', %s) IS NOT NULL`,
				tg.table, col, col, col, col)
			res, err := s.db.Exec(q)
			if err != nil {
				return fmt.Errorf("normalise %s.%s: %w", tg.table, col, err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				slog.Info("normalised legacy timestamps", "table", tg.table, "column", col, "rows", n)
			}
		}
	}
	return nil
}

func (s *Store) Upsert(r model.Reservation) error {
	_, err := s.db.Exec(`
		INSERT INTO reservations (id, account_alias, account_id, region, type, resource_id,
			instance_type, platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, capacity_unit, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, end_time=excluded.end_time, quantity=excluded.quantity,
			used_count=excluded.used_count, description=excluded.description,
			hourly_rate=excluded.hourly_rate, equiv_cores=excluded.equiv_cores,
			capacity_unit=excluded.capacity_unit,
			updated_at=datetime('now')`,
		r.ID, r.AccountAlias, r.AccountID, r.Region, string(r.Type), r.ResourceID,
		r.InstanceType, r.Platform, r.Quantity, r.UsedCount,
		formatTime(r.StartTime), formatTime(r.EndTime),
		r.Status, r.Description, r.HourlyRate, r.EquivCores, r.CapacityUnit)
	return err
}

// upsertTx is Upsert against an open transaction, used by ReplaceAccount.
func upsertTx(tx *sql.Tx, r model.Reservation) error {
	_, err := tx.Exec(`
		INSERT INTO reservations (id, account_alias, account_id, region, type, resource_id,
			instance_type, platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, capacity_unit, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, end_time=excluded.end_time, quantity=excluded.quantity,
			used_count=excluded.used_count, description=excluded.description,
			hourly_rate=excluded.hourly_rate, equiv_cores=excluded.equiv_cores,
			capacity_unit=excluded.capacity_unit,
			updated_at=datetime('now')`,
		r.ID, r.AccountAlias, r.AccountID, r.Region, string(r.Type), r.ResourceID,
		r.InstanceType, r.Platform, r.Quantity, r.UsedCount,
		formatTime(r.StartTime), formatTime(r.EndTime),
		r.Status, r.Description, r.HourlyRate, r.EquivCores, r.CapacityUnit)
	return err
}

// ReplaceAccount atomically swaps all reservations for one account: the delete
// and the re-population happen in a single transaction, so readers never see a
// partially-populated (or empty) account mid-sync.
func (s *Store) ReplaceAccount(accountID string, items []model.Reservation) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM reservations WHERE account_id = ?", accountID); err != nil {
		return fmt.Errorf("delete account %s: %w", accountID, err)
	}
	for _, it := range items {
		if err := upsertTx(tx, it); err != nil {
			return fmt.Errorf("upsert %s: %w", it.ID, err)
		}
	}
	return tx.Commit()
}

// ReplaceGPUCoverage atomically swaps all GPU coverage rows for one account.
func (s *Store) ReplaceGPUCoverage(accountID string, items []model.GPUCoverage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM gpu_coverage WHERE account_id = ?", accountID); err != nil {
		return fmt.Errorf("delete gpu_coverage %s: %w", accountID, err)
	}
	for _, g := range items {
		if _, err := tx.Exec(`
			INSERT INTO gpu_coverage (id, account_alias, account_id, region, az, instance_id,
				instance_type, coverage, coverage_ref, sp_rate, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
			ON CONFLICT(id) DO UPDATE SET
				coverage=excluded.coverage, coverage_ref=excluded.coverage_ref,
				sp_rate=excluded.sp_rate, updated_at=datetime('now')`,
			g.ID, g.AccountAlias, g.AccountID, g.Region, g.AZ, g.InstanceID,
			g.InstanceType, string(g.Coverage), g.CoverageRef, g.SPRate); err != nil {
			return fmt.Errorf("upsert gpu_coverage %s: %w", g.ID, err)
		}
	}
	return tx.Commit()
}

func (s *Store) List(typeFilter, accountFilter string) ([]model.Reservation, error) {
	query := "SELECT id, account_alias, account_id, region, type, resource_id, instance_type, platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, capacity_unit, updated_at FROM reservations WHERE 1=1"
	args := []any{}
	if typeFilter != "" {
		query += " AND type = ?"
		args = append(args, typeFilter)
	}
	if accountFilter != "" {
		query += " AND account_id = ?"
		args = append(args, accountFilter)
	}
	query += " ORDER BY end_time ASC"
	return s.queryReservations(query, args...)
}

// GetAlerts returns reservations expiring within maxDays. remindDays are the
// configured reminder points, used to tag each alert with the threshold it has
// just crossed; pass nil to leave Threshold unset.
func (s *Store) GetAlerts(maxDays int, remindDays ...int) ([]model.Alert, error) {
	query := `SELECT id, account_alias, account_id, region, type, resource_id, instance_type,
		platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, capacity_unit, updated_at
		FROM reservations
		WHERE end_time != ''
		  AND julianday(end_time) > julianday('now')
		  AND julianday(end_time) <= julianday('now', '+' || ? || ' days')
		  AND status NOT IN ('cancelled', 'expired', 'failed', 'deleted', 'retired')
		ORDER BY julianday(end_time) ASC`

	rows, err := s.queryReservations(query, maxDays)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var alerts []model.Alert
	for _, r := range rows {
		alerts = append(alerts, model.NewAlert(r, now, remindDays))
	}
	return alerts, nil
}

// HasNotified reports whether this exact reminder was already sent: same
// resource, same reminder key (see model.Alert.NotifyKey) and same expiry date.
//
// Including endTime means a renewed resource (same id, later expiry) alerts
// again on its next cycle; including the reminder key means each configured
// remind_days value fires once rather than only the first of a level.
func (s *Store) HasNotified(reservationID, notifyKey string, endTime time.Time) (bool, error) {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM notify_log WHERE reservation_id = ? AND level = ? AND end_time = ?",
		reservationID, notifyKey, formatTime(endTime)).Scan(&count)
	return count > 0, err
}

// PruneNotifyLog deletes notify_log entries whose expiry is further in the past
// than retain, keeping the table from growing without bound. Rows with no
// end_time (written before the schema gained the column) are left alone: their
// age is unknown and they no longer suppress current alerts anyway.
func (s *Store) PruneNotifyLog(retain time.Duration) (int64, error) {
	cutoff := formatTime(time.Now().UTC().Add(-retain))
	res, err := s.db.Exec(
		`DELETE FROM notify_log
		 WHERE end_time != '' AND julianday(end_time) < julianday(?)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune notify_log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // deletion succeeded; count unavailable
	}
	return n, nil
}

// MarkNotifiedBatch marks multiple alerts as notified in a single transaction.
func (s *Store) MarkNotifiedBatch(alerts []model.Alert) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT OR IGNORE INTO notify_log (reservation_id, level, end_time) VALUES (?, ?, ?)")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, a := range alerts {
		if _, err := stmt.Exec(a.ID, a.NotifyKey(), formatTime(a.EndTime)); err != nil {
			return fmt.Errorf("mark %s/%s: %w", a.ID, a.NotifyKey(), err)
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteByAccountID(accountID string) error {
	_, err := s.db.Exec("DELETE FROM reservations WHERE account_id = ?", accountID)
	return err
}

func (s *Store) queryReservations(query string, args ...any) ([]model.Reservation, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var results []model.Reservation
	for rows.Next() {
		var r model.Reservation
		var typ, startStr, endStr, updatedStr string
		err := rows.Scan(&r.ID, &r.AccountAlias, &r.AccountID, &r.Region,
			&typ, &r.ResourceID, &r.InstanceType, &r.Platform, &r.Quantity, &r.UsedCount,
			&startStr, &endStr, &r.Status, &r.Description, &r.HourlyRate, &r.EquivCores, &r.CapacityUnit, &updatedStr)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Type = model.ResourceType(typ)
		if t, err := parseTime(startStr); err == nil {
			r.StartTime = t
		} else {
			slog.Warn("parse start_time", "value", startStr, "id", r.ID, "err", err)
		}
		if t, err := parseTime(endStr); err == nil {
			r.EndTime = t
		} else {
			slog.Warn("parse end_time", "value", endStr, "id", r.ID, "err", err)
		}
		if t, err := parseTime(updatedStr); err == nil {
			r.UpdatedAt = t
		} else {
			slog.Warn("parse updated_at", "value", updatedStr, "id", r.ID, "err", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return results, nil
}

func (s *Store) Ping() error {
	return s.db.Ping()
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) UpsertGPUCoverage(g model.GPUCoverage) error {
	_, err := s.db.Exec(`
		INSERT INTO gpu_coverage (id, account_alias, account_id, region, az, instance_id,
			instance_type, coverage, coverage_ref, sp_rate, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			coverage=excluded.coverage, coverage_ref=excluded.coverage_ref,
			sp_rate=excluded.sp_rate, updated_at=datetime('now')`,
		g.ID, g.AccountAlias, g.AccountID, g.Region, g.AZ, g.InstanceID,
		g.InstanceType, string(g.Coverage), g.CoverageRef, g.SPRate)
	return err
}

func (s *Store) ListGPUCoverage(accountFilter string) ([]model.GPUCoverage, error) {
	query := `SELECT id, account_alias, account_id, region, az, instance_id,
		instance_type, coverage, coverage_ref, sp_rate, updated_at
		FROM gpu_coverage WHERE 1=1`
	args := []any{}
	if accountFilter != "" {
		query += " AND account_id = ?"
		args = append(args, accountFilter)
	}
	query += " ORDER BY coverage ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query gpu_coverage: %w", err)
	}
	defer rows.Close()

	var results []model.GPUCoverage
	for rows.Next() {
		var g model.GPUCoverage
		var coverage, updatedStr string
		err := rows.Scan(&g.ID, &g.AccountAlias, &g.AccountID, &g.Region, &g.AZ,
			&g.InstanceID, &g.InstanceType, &coverage, &g.CoverageRef, &g.SPRate, &updatedStr)
		if err != nil {
			return nil, fmt.Errorf("scan gpu_coverage: %w", err)
		}
		g.Coverage = model.CoverageType(coverage)
		if t, err := parseTime(updatedStr); err == nil {
			g.UpdatedAt = t
		} else {
			slog.Warn("parse gpu_coverage updated_at", "value", updatedStr, "id", g.ID, "err", err)
		}
		results = append(results, g)
	}
	return results, rows.Err()
}

func (s *Store) DeleteGPUCoverageByAccountID(accountID string) error {
	_, err := s.db.Exec("DELETE FROM gpu_coverage WHERE account_id = ?", accountID)
	return err
}

// pruneMaxAccounts caps the keep list below SQLite's default parameter limit
// (SQLITE_LIMIT_VARIABLE_NUMBER, typically 999) with a safety margin.
const pruneMaxAccounts = 900

// PruneAccounts deletes all reservations and gpu_coverage rows whose
// account_id is not in the provided keep set. Used to clean up data for
// accounts that were removed from configuration. Returns the number of
// rows deleted across both tables. When keep is empty the call is a no-op
// to guard against accidental full-wipe.
func (s *Store) PruneAccounts(keep []string) (int64, error) {
	if len(keep) == 0 {
		return 0, nil
	}
	if len(keep) > pruneMaxAccounts {
		return 0, fmt.Errorf("prune: too many accounts (%d), limit %d", len(keep), pruneMaxAccounts)
	}
	var sb strings.Builder
	args := make([]any, 0, len(keep))
	for i, id := range keep {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('?')
		args = append(args, id)
	}
	placeholders := sb.String()
	var total int64
	for _, table := range []string{"reservations", "gpu_coverage"} {
		res, err := s.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE account_id NOT IN (%s)", table, placeholders),
			args...)
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			slog.Warn("prune rows affected", "table", table, "err", err)
			continue
		}
		total += n
	}
	return total, nil
}
