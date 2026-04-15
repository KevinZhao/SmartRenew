package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"smartrenew/model"
)

type LarkNotifier struct {
	WebhookURL string
	client     *http.Client
}

func NewLark(webhookURL string) *LarkNotifier {
	return &LarkNotifier{
		WebhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (l *LarkNotifier) Name() string { return "lark" }

func (l *LarkNotifier) Send(alerts []model.Alert) error {
	if len(alerts) == 0 {
		return nil
	}

	content := buildLarkContent(alerts)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     content,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal lark payload: %w", err)
	}

	resp, err := l.client.Post(l.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send lark: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// Check HTTP status first (proxy errors return non-JSON bodies)
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if resp.StatusCode != http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&result) // best-effort
		return fmt.Errorf("lark HTTP %d: %s", resp.StatusCode, result.Msg)
	}
	// Lark returns 200 even for app-level errors; check the JSON body
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("lark response decode: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("lark error code %d: %s", result.Code, result.Msg)
	}
	return nil
}

func buildLarkContent(alerts []model.Alert) map[string]any {
	var criticals, warnings, others []model.Alert
	for _, a := range alerts {
		switch a.Level {
		case model.LevelCritical:
			criticals = append(criticals, a)
		case model.LevelWarning:
			warnings = append(warnings, a)
		default:
			others = append(others, a)
		}
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("**SmartRenew Alert** - %d items expiring soon\n", len(alerts)))

	if len(criticals) > 0 {
		lines = append(lines, "**Urgent (<=3 days):**")
		for _, a := range criticals {
			lines = append(lines, formatAlertLine(a))
		}
		lines = append(lines, "")
	}
	if len(warnings) > 0 {
		lines = append(lines, "**Warning (<=7 days):**")
		for _, a := range warnings {
			lines = append(lines, formatAlertLine(a))
		}
		lines = append(lines, "")
	}
	if len(others) > 0 {
		lines = append(lines, "**Attention (<=30 days):**")
		for _, a := range others {
			lines = append(lines, formatAlertLine(a))
		}
	}

	return map[string]any{
		"header": map[string]any{
			"title": map[string]any{
				"tag":     "plain_text",
				"content": "SmartRenew Resource Expiry Alert",
			},
			"template": "red",
		},
		"elements": []map[string]any{
			{
				"tag":     "markdown",
				"content": strings.Join(lines, "\n"),
			},
		},
	}
}

func formatAlertLine(a model.Alert) string {
	return fmt.Sprintf("- **%dd** | %s | %s | %s | %s (%s)",
		a.DaysLeft,
		strings.ToUpper(string(a.Type)),
		a.InstanceType,
		a.AccountAlias,
		a.ResourceID,
		a.Region,
	)
}
