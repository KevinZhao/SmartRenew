package notifier

import "github.com/KevinZhao/SmartRenew/model"

type Notifier interface {
	Name() string
	Send(alerts []model.Alert) error
	SendGPUAlerts(items []model.GPUCoverage) error
}
