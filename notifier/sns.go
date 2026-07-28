package notifier

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"github.com/KevinZhao/SmartRenew/model"
)

// SNSNotifier publishes alerts to an SNS topic (typically with email subscribers).
// Uses plain-text format since the SNS email delivery renders HTML inconsistently
// across mail clients.
type SNSNotifier struct {
	client   *sns.Client
	topicARN string
}

func NewSNS(awsCfg aws.Config, topicARN string) *SNSNotifier {
	return &SNSNotifier{
		client:   sns.NewFromConfig(awsCfg),
		topicARN: topicARN,
	}
}

func (s *SNSNotifier) Name() string { return "sns" }

func (s *SNSNotifier) Send(alerts []model.Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	subject := fmt.Sprintf("[SmartRenew] %d resources expiring within 30 days", len(alerts))
	return s.publish(subject, buildExpiryText(alerts))
}

func (s *SNSNotifier) SendGPUAlerts(items []model.GPUCoverage) error {
	if len(items) == 0 {
		return nil
	}
	subject := fmt.Sprintf("[SmartRenew] %d GPU instances on-demand", len(items))
	return s.publish(subject, buildGPUODText(items))
}

// snsMaxMessageBytes is the SNS Publish limit for the message body (256 KiB),
// minus a small margin for the request envelope. Exceeding it makes Publish
// fail outright, so a long alert list must be truncated rather than dropped.
const snsMaxMessageBytes = 256*1024 - 1024

const snsTruncationNotice = "\n\n... message truncated: too many alerts to fit in one notification. See the SmartRenew UI for the full list.\n"

// truncateMessage caps body at the SNS limit, cutting on a rune boundary so the
// result stays valid UTF-8, and appends a notice so the reader knows the list
// is incomplete.
func truncateMessage(body string) string {
	if len(body) <= snsMaxMessageBytes {
		return body
	}
	keep := snsMaxMessageBytes - len(snsTruncationNotice)
	// Back off to a rune boundary: a raw byte cut can split a multi-byte rune.
	for keep > 0 && !utf8.RuneStart(body[keep]) {
		keep--
	}
	return body[:keep] + snsTruncationNotice
}

// truncateSubject caps the subject at the SNS limit of 100 characters, on a
// rune boundary.
func truncateSubject(subject string) string {
	const max = 100
	if len(subject) <= max {
		return subject
	}
	keep := max
	for keep > 0 && !utf8.RuneStart(subject[keep]) {
		keep--
	}
	return subject[:keep]
}

func (s *SNSNotifier) publish(subject, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// SNS Subject: max 100 chars, no control chars / newlines
	subject = truncateSubject(subject)
	body = truncateMessage(body)

	_, err := s.client.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(s.topicARN),
		Subject:  aws.String(subject),
		Message:  aws.String(body),
	})
	if err != nil {
		return fmt.Errorf("sns publish: %w", err)
	}
	return nil
}

func buildExpiryText(alerts []model.Alert) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("SmartRenew Alert — %d resources expiring within 30 days\n", len(alerts)))
	b.WriteString(strings.Repeat("=", 70) + "\n\n")

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

	writeGroup := func(title string, items []model.Alert) {
		if len(items) == 0 {
			return
		}
		b.WriteString(title + "\n")
		b.WriteString(strings.Repeat("-", len(title)) + "\n")
		for _, a := range items {
			b.WriteString(fmt.Sprintf("  %2dd | %-12s | %-20s | %-20s | %s (%s) | ends %s\n",
				a.DaysLeft,
				strings.ToUpper(string(a.Type)),
				a.InstanceType,
				a.AccountAlias,
				a.ResourceID,
				a.Region,
				a.EndTime.Format("2006-01-02"),
			))
		}
		b.WriteString("\n")
	}

	writeGroup("URGENT (≤3 days)", criticals)
	writeGroup("WARNING (≤7 days)", warnings)
	writeGroup("ATTENTION (≤30 days)", others)

	b.WriteString("—\nSent by SmartRenew\n")
	return b.String()
}

func buildGPUODText(items []model.GPUCoverage) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("SmartRenew GPU On-Demand Alert — %d GPU instances uncovered\n", len(items)))
	b.WriteString(strings.Repeat("=", 70) + "\n\n")
	b.WriteString("The following GPU instances are running without SP / CB / RI coverage:\n\n")
	for _, g := range items {
		b.WriteString(fmt.Sprintf("  %-22s | %-18s | %-20s | %s/%s\n",
			g.InstanceID,
			g.InstanceType,
			g.AccountAlias,
			g.Region,
			g.AZ,
		))
	}
	b.WriteString("\n—\nSent by SmartRenew\n")
	return b.String()
}
