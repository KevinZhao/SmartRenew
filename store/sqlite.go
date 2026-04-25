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
	return err
}

func (s *Store) Upsert(r model.Reservation) error {
	_, err := s.db.Exec(`
		INSERT INTO reservations (id, account_alias, account_id, region, type, resource_id,
			instance_type, platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, end_time=excluded.end_time, quantity=excluded.quantity,
			used_count=excluded.used_count, description=excluded.description,
			hourly_rate=excluded.hourly_rate, equiv_cores=excluded.equiv_cores,
			updated_at=datetime('now')`,
		r.ID, r.AccountAlias, r.AccountID, r.Region, string(r.Type), r.ResourceID,
		r.InstanceType, r.Platform, r.Quantity, r.UsedCount,
		r.StartTime.Format(time.RFC3339), r.EndTime.Format(time.RFC3339),
		r.Status, r.Description, r.HourlyRate, r.EquivCores)
	return err
}

func (s *Store) List(typeFilter, accountFilter string) ([]model.Reservation, error) {
	query := "SELECT id, account_alias, account_id, region, type, resource_id, instance_type, platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, updated_at FROM reservations WHERE 1=1"
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

func (s *Store) GetAlerts(maxDays int) ([]model.Alert, error) {
	query := `SELECT id, account_alias, account_id, region, type, resource_id, instance_type,
		platform, quantity, used_count, start_time, end_time, status, description, hourly_rate, equiv_cores, updated_at
		FROM reservations
		WHERE end_time > datetime('now')
		  AND end_time <= datetime('now', '+' || ? || ' days')
		  AND status NOT IN ('cancelled', 'expired', 'failed', 'deleted', 'retired')
		ORDER BY end_time ASC`

	rows, err := s.queryReservations(query, maxDays)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var alerts []model.Alert
	for _, r := range rows {
		daysLeft := int(r.EndTime.Sub(now).Hours() / 24)
		alerts = append(alerts, model.Alert{
			Reservation: r,
			DaysLeft:    daysLeft,
			Level:       model.CalcAlertLevel(daysLeft),
		})
	}
	return alerts, nil
}

func (s *Store) HasNotified(reservationID string, level model.AlertLevel) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM notify_log WHERE reservation_id = ? AND level = ?",
		reservationID, string(level)).Scan(&count)
	return count > 0, err
}

// MarkNotifiedBatch marks multiple alerts as notified in a single transaction.
func (s *Store) MarkNotifiedBatch(alerts []model.Alert) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT OR IGNORE INTO notify_log (reservation_id, level) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, a := range alerts {
		if _, err := stmt.Exec(a.ID, string(a.Level)); err != nil {
			return fmt.Errorf("mark %s/%s: %w", a.ID, a.Level, err)
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
			&startStr, &endStr, &r.Status, &r.Description, &r.HourlyRate, &r.EquivCores, &updatedStr)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Type = model.ResourceType(typ)
		if t, err := time.Parse(time.RFC3339, startStr); err == nil {
			r.StartTime = t
		} else if startStr != "" {
			slog.Warn("parse start_time", "value", startStr, "id", r.ID, "err", err)
		}
		if t, err := time.Parse(time.RFC3339, endStr); err == nil {
			r.EndTime = t
		} else if endStr != "" {
			slog.Warn("parse end_time", "value", endStr, "id", r.ID, "err", err)
		}
		if t, err := time.Parse(time.RFC3339, updatedStr); err == nil {
			r.UpdatedAt = t
		} else if updatedStr != "" {
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
		if t, err := time.Parse(time.RFC3339, updatedStr); err == nil {
			g.UpdatedAt = t
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
