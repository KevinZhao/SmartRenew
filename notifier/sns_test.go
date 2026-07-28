package notifier

import (
	"strings"
	"testing"
	"time"

	"github.com/KevinZhao/SmartRenew/model"
)

func TestTruncateMessageLeavesSmallBodiesAlone(t *testing.T) {
	body := "short message"
	got := truncateMessage(body)
	if got != body {
		t.Fatalf("truncateMessage altered a small body: %q", got)
	}
}

func TestTruncateMessageRespectsSNSLimit(t *testing.T) {
	// SNS rejects a Publish whose message exceeds 256 KiB; without truncation
	// the whole notification is lost and no alert reaches anyone.
	body := strings.Repeat("x", snsMaxMessageBytes+10_000)
	got := truncateMessage(body)

	if len(got) > snsMaxMessageBytes {
		t.Fatalf("truncated body is %d bytes, want <= %d", len(got), snsMaxMessageBytes)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncated body does not tell the reader that content was dropped")
	}
}

func TestTruncateMessageKeepsValidUTF8(t *testing.T) {
	// A byte-slice cut can land mid-rune; the result must still be valid UTF-8.
	body := strings.Repeat("资源即将到期", snsMaxMessageBytes/6)
	got := truncateMessage(body)

	if len(got) > snsMaxMessageBytes {
		t.Fatalf("truncated body is %d bytes, want <= %d", len(got), snsMaxMessageBytes)
	}
	if !utf8Valid(got) {
		t.Error("truncation produced invalid UTF-8 — a multi-byte rune was split")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestBuildExpiryTextGroupsByLevel(t *testing.T) {
	now := time.Now()
	alerts := []model.Alert{
		{Reservation: model.Reservation{ID: "a", ResourceID: "cr-a", InstanceType: "p5.48xlarge",
			AccountAlias: "prod", Region: "us-east-1", Type: model.TypeCB, EndTime: now.Add(2 * 24 * time.Hour)},
			DaysLeft: 2, Level: model.LevelCritical},
		{Reservation: model.Reservation{ID: "b", ResourceID: "cr-b", InstanceType: "m5.large",
			AccountAlias: "dev", Region: "us-west-2", Type: model.TypeRI, EndTime: now.Add(20 * 24 * time.Hour)},
			DaysLeft: 20, Level: model.LevelNormal},
	}

	text := buildExpiryText(alerts)
	for _, want := range []string{"cr-a", "cr-b", "p5.48xlarge", "prod", "us-east-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("expiry text missing %q\n%s", want, text)
		}
	}
}

func TestBuildExpiryTextHandlesManyAlerts(t *testing.T) {
	// The realistic path to hitting the SNS limit: a large multi-account estate.
	now := time.Now()
	alerts := make([]model.Alert, 5000)
	for i := range alerts {
		alerts[i] = model.Alert{
			Reservation: model.Reservation{
				ID: "id", ResourceID: strings.Repeat("r", 40), InstanceType: "p5.48xlarge",
				AccountAlias: strings.Repeat("acct", 10), Region: "ap-northeast-1",
				Type: model.TypeODCR, EndTime: now.Add(time.Duration(i) * time.Hour),
			},
			DaysLeft: i / 24, Level: model.LevelNormal,
		}
	}

	body := truncateMessage(buildExpiryText(alerts))
	if len(body) > snsMaxMessageBytes {
		t.Fatalf("body is %d bytes after truncation, want <= %d — Publish would be rejected",
			len(body), snsMaxMessageBytes)
	}
}
