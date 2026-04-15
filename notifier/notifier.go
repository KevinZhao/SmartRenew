package notifier

import "smartrenew/model"

// Notifier is the abstraction for all notification channels.
type Notifier interface {
	Name() string
	Send(alerts []model.Alert) error
}
