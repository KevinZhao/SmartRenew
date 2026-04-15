package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"smartrenew/model"
)

type Store struct {
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
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
		CREATE TABLE IF NOT EXISTS org_accounts (
			account_id TEXT PRIMARY KEY,
			account_name TEXT NOT NULL DEFAULT '',
			email TEXT DEFAULT '',
			status TEXT DEFAULT '',
			joined_method TEXT DEFAULT '',
			joined_at TEXT DEFAULT '',
			tag TEXT DEFAULT '',
			updated_at TEXT DEFAULT (datetime('now'))
		);
	`)
	return err
}

func (s *Store) Upsert(r model.Reservation) error {
	_, err := s.db.Exec(`
		INSERT INTO reservations (id, account_alias, account_id, region, type, resource_id,
			instance_type, platform, quantity, start_time, end_time, status, description, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, end_time=excluded.end_time, quantity=excluded.quantity,
			description=excluded.description, updated_at=datetime('now')`,
		r.ID, r.AccountAlias, r.AccountID, r.Region, string(r.Type), r.ResourceID,
		r.InstanceType, r.Platform, r.Quantity,
		r.StartTime.Format(time.RFC3339), r.EndTime.Format(time.RFC3339),
		r.Status, r.Description)
	return err
}

func (s *Store) List(typeFilter, accountFilter string) ([]model.Reservation, error) {
	query := "SELECT id, account_alias, account_id, region, type, resource_id, instance_type, platform, quantity, start_time, end_time, status, description, updated_at FROM reservations WHERE 1=1"
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
		platform, quantity, start_time, end_time, status, description, updated_at
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

func (s *Store) MarkNotified(reservationID string, level model.AlertLevel) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO notify_log (reservation_id, level) VALUES (?, ?)",
		reservationID, string(level))
	return err
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
			&typ, &r.ResourceID, &r.InstanceType, &r.Platform, &r.Quantity,
			&startStr, &endStr, &r.Status, &r.Description, &updatedStr)
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

// UpsertOrgAccount inserts or updates an org account, preserving the existing tag.
func (s *Store) UpsertOrgAccount(a model.OrgAccount) error {
	_, err := s.db.Exec(`
		INSERT INTO org_accounts (account_id, account_name, email, status, joined_method, joined_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(account_id) DO UPDATE SET
			account_name=excluded.account_name, email=excluded.email,
			status=excluded.status, joined_method=excluded.joined_method,
			joined_at=excluded.joined_at, updated_at=datetime('now')`,
		a.AccountID, a.AccountName, a.Email, a.Status, a.JoinedMethod,
		a.JoinedAt.Format(time.RFC3339))
	return err
}

// ListOrgAccounts returns all org accounts.
func (s *Store) ListOrgAccounts() ([]model.OrgAccount, error) {
	rows, err := s.db.Query(`SELECT account_id, account_name, email, status, joined_method, joined_at, tag, updated_at FROM org_accounts ORDER BY account_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("query org_accounts: %w", err)
	}
	defer rows.Close()

	var results []model.OrgAccount
	for rows.Next() {
		var a model.OrgAccount
		var joinedStr, updatedStr string
		if err := rows.Scan(&a.AccountID, &a.AccountName, &a.Email, &a.Status, &a.JoinedMethod, &joinedStr, &a.Tag, &updatedStr); err != nil {
			return nil, fmt.Errorf("scan org_account: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, joinedStr); err == nil {
			a.JoinedAt = t
		}
		if t, err := time.Parse(time.RFC3339, updatedStr); err == nil {
			a.UpdatedAt = t
		}
		results = append(results, a)
	}
	return results, rows.Err()
}

// UpdateOrgAccountTag updates the tag for a specific org account.
func (s *Store) UpdateOrgAccountTag(accountID, tag string) error {
	res, err := s.db.Exec(`UPDATE org_accounts SET tag = ?, updated_at = datetime('now') WHERE account_id = ?`, tag, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("account %s not found", accountID)
	}
	return nil
}
