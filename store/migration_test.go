package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// 构造一个“旧版本”数据库：RFC3339 时间 + 旧 notify_log 结构
func seedLegacyDB(t *testing.T, path string) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE reservations (
			id TEXT PRIMARY KEY, account_alias TEXT NOT NULL, account_id TEXT NOT NULL,
			region TEXT NOT NULL, type TEXT NOT NULL, resource_id TEXT NOT NULL,
			instance_type TEXT DEFAULT '', platform TEXT DEFAULT '', quantity INTEGER DEFAULT 1,
			used_count INTEGER DEFAULT 0, start_time TEXT, end_time TEXT,
			status TEXT DEFAULT 'active', description TEXT DEFAULT '',
			updated_at TEXT DEFAULT (datetime('now')), hourly_rate REAL DEFAULT 0, equiv_cores REAL DEFAULT 0
		);
		CREATE TABLE notify_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT, reservation_id TEXT NOT NULL,
			level TEXT NOT NULL, notified_at TEXT DEFAULT (datetime('now')),
			UNIQUE(reservation_id, level)
		);`)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// 旧格式: RFC3339 带 T 和 Z
	future := now.Add(10 * 24 * time.Hour).Format(time.RFC3339)
	pastToday := now.Add(-3 * time.Hour).Format(time.RFC3339)
	// 旧格式且带 +08:00 偏移（CSV 导入产生）
	loc, _ := time.LoadLocation("Asia/Shanghai")
	offsetFuture := now.Add(20 * 24 * time.Hour).In(loc).Format(time.RFC3339)

	ins := func(id, end string) {
		_, err := db.Exec(`INSERT INTO reservations
			(id, account_alias, account_id, region, type, resource_id, start_time, end_time, status, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			"111122223333_us-east-1_"+id, "acct", "111122223333", "us-east-1", "odcr", id,
			now.Add(-100*24*time.Hour).Format(time.RFC3339), end, "active", now.Format(time.RFC3339))
		if err != nil {
			t.Fatal(err)
		}
	}
	ins("future", future)
	ins("expired-today", pastToday)
	ins("offset-future", offsetFuture)
	if _, err := db.Exec(`INSERT INTO notify_log (reservation_id, level) VALUES (?,?)`,
		"111122223333_us-east-1_future", "critical"); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyDBMigratesCorrectly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)

	// 打开 = 触发 migrate()
	s, err := New(path)
	if err != nil {
		t.Fatalf("New on legacy db: %v", err)
	}
	defer s.Close()

	rows, err := s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (migration lost data)", len(rows))
	}

	// 1) 时间必须能解析回来（不再是零值）
	for _, r := range rows {
		if r.EndTime.IsZero() {
			t.Errorf("%s: EndTime 迁移后仍为零值", r.ResourceID)
		}
		if r.UpdatedAt.IsZero() {
			t.Errorf("%s: UpdatedAt 迁移后仍为零值", r.ResourceID)
		}
	}

	// 2) 告警窗口必须正确排除当天已过期项
	alerts, err := s.GetAlerts(30, 30, 14, 7, 3, 1)
	if err != nil {
		t.Fatalf("GetAlerts: %v", err)
	}
	got := map[string]bool{}
	for _, a := range alerts {
		got[a.ResourceID] = true
		if a.DaysLeft < 0 {
			t.Errorf("%s: days_left=%d", a.ResourceID, a.DaysLeft)
		}
	}
	if got["expired-today"] {
		t.Error("当天已过期项迁移后仍出现在告警中")
	}
	if !got["future"] {
		t.Error("未过期项迁移后从告警中消失")
	}
	if !got["offset-future"] {
		t.Error("带 +08:00 偏移的未过期项迁移后从告警中消失")
	}
	t.Logf("alerts after migration: %v", got)

	// 3) 旧 notify_log 记录不应压制当前周期（end_time 为空）
	var fut time.Time
	for _, r := range rows {
		if r.ResourceID == "future" {
			fut = r.EndTime
		}
	}
	notified, err := s.HasNotified("111122223333_us-east-1_future", "t14", fut)
	if err != nil {
		t.Fatal(err)
	}
	if notified {
		t.Error("旧 notify_log 记录压制了新周期的告警")
	}

	// 4) 重复打开（幂等）
	s.Close()
	s2, err := New(path)
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	defer s2.Close()
	rows2, err := s2.List("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 3 {
		t.Fatalf("二次迁移后 %d 行, want 3", len(rows2))
	}
}
