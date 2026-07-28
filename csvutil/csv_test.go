package csvutil

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/KevinZhao/SmartRenew/model"
)

func TestExportImportRoundTrip(t *testing.T) {
	start := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	end := time.Date(2027, 3, 1, 10, 30, 0, 0, time.UTC)
	rows := []model.Reservation{{
		ID: "111122223333_us-east-1_cr-1", AccountAlias: "prod", AccountID: "111122223333",
		Region: "us-east-1", Type: model.TypeODCR, ResourceID: "cr-1",
		InstanceType: "p5.48xlarge", Platform: "Linux/UNIX", Quantity: 4, UsedCount: 2,
		StartTime: start, EndTime: end, Status: "active", Description: "ODCR - us-east-1a",
	}}

	var buf bytes.Buffer
	if err := Export(&buf, rows); err != nil {
		t.Fatalf("Export: %v", err)
	}

	got, err := Import(&buf)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	g := got[0]
	if g.ResourceID != "cr-1" || g.AccountID != "111122223333" || g.Region != "us-east-1" {
		t.Errorf("identity fields lost: %+v", g)
	}
	if g.Quantity != 4 || g.UsedCount != 2 {
		t.Errorf("quantity/used_count = %d/%d, want 4/2", g.Quantity, g.UsedCount)
	}
	if !g.StartTime.Equal(start) || !g.EndTime.Equal(end) {
		t.Errorf("times = %v/%v, want %v/%v", g.StartTime, g.EndTime, start, end)
	}
	if g.ID != "111122223333_us-east-1_cr-1" {
		t.Errorf("ID = %q, want the composite id", g.ID)
	}
}

func TestImportPreservesInstantForOffsetTimestamps(t *testing.T) {
	// A CSV carrying +08:00 timestamps must yield the same instant as the
	// equivalent UTC value. Getting this wrong is what let expired resources
	// slip into the alert window.
	csv := "account_id,region,resource_id,end_time\n" +
		"111122223333,us-east-1,cr-1,2026-09-01T18:00:00+08:00\n"
	got, err := Import(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	want := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if !got[0].EndTime.Equal(want) {
		t.Fatalf("EndTime = %v, want the same instant as %v", got[0].EndTime.UTC(), want)
	}
}

func TestImportSkipsRowsMissingIdentity(t *testing.T) {
	csv := "account_id,region,resource_id\n" +
		",us-east-1,cr-1\n" + // no account_id
		"111122223333,us-east-1,\n" + // no resource_id
		"111122223333,us-east-1,cr-ok\n"
	got, err := Import(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 1 || got[0].ResourceID != "cr-ok" {
		t.Fatalf("got %d rows, want only cr-ok", len(got))
	}
}

func TestImportHandlesReorderedAndExtraColumns(t *testing.T) {
	csv := "extra,resource_id,account_id,region,quantity\n" +
		"ignored,cr-1,111122223333,us-west-2,7\n"
	got, err := Import(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Quantity != 7 || got[0].Region != "us-west-2" {
		t.Errorf("column mapping wrong: %+v", got[0])
	}
}

func TestImportDefaultsAndBadValues(t *testing.T) {
	csv := "account_id,region,resource_id,quantity,used_count,end_time\n" +
		"111122223333,us-east-1,cr-1,not-a-number,also-bad,not-a-date\n"
	got, err := Import(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Quantity != 1 {
		t.Errorf("Quantity = %d, want the default 1 when unparseable", got[0].Quantity)
	}
	if got[0].UsedCount != 0 {
		t.Errorf("UsedCount = %d, want 0 when unparseable", got[0].UsedCount)
	}
	if !got[0].EndTime.IsZero() {
		t.Errorf("EndTime = %v, want zero when unparseable", got[0].EndTime)
	}
}

func TestImportEmptyInput(t *testing.T) {
	for _, in := range []string{"", "account_id,region,resource_id\n"} {
		got, err := Import(strings.NewReader(in))
		if err != nil {
			t.Fatalf("Import(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Errorf("Import(%q) returned %d rows, want 0", in, len(got))
		}
	}
}

func TestExportWritesHeaderAndAllRows(t *testing.T) {
	rows := []model.Reservation{
		{AccountID: "1", ResourceID: "a", Region: "us-east-1"},
		{AccountID: "2", ResourceID: "b", Region: "us-west-2"},
	}
	var buf bytes.Buffer
	if err := Export(&buf, rows); err != nil {
		t.Fatalf("Export: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + 2 rows)", len(lines))
	}
	if !strings.HasPrefix(lines[0], "account_alias,account_id,region") {
		t.Errorf("unexpected header: %q", lines[0])
	}
}

func TestExportQuotesFieldsContainingCommas(t *testing.T) {
	rows := []model.Reservation{{
		AccountID: "1", ResourceID: "a", Description: "ODCR, us-east-1a, spare",
	}}
	var buf bytes.Buffer
	if err := Export(&buf, rows); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Round-tripping is the real requirement: the comma must not split a column.
	got, err := Import(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 1 || got[0].Description != "ODCR, us-east-1a, spare" {
		t.Fatalf("description did not survive the round trip: %+v", got)
	}
}
