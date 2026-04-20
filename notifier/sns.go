package notifier

import (
	"context"
	"fmt"
	"strings"
	"time"

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

func (s *SNSNotifier) publish(subject, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// SNS Subject: max 100 chars, no control chars / newlines
	if len(subject) > 100 {
		subject = subject[:100]
	}

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
