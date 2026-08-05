package tray

import (
	"errors"
	"sync"
	"time"
)

const (
	CodeStartFailed     = "start_failed"
	CodeUnexpectedExit  = "unexpected_exit"
	CodeWorkersNotReady = "workers_not_ready"
	CodeConfigCorrupt   = "config_corrupt"
	CodeConfigDrift     = "config_drift"
	CodeUACRequired     = "uac_required"
)

var ErrUnsupportedNotification = errors.New("unsupported_notification")

type Event struct {
	Component string
	Code      string
	Summary   string
}

type NotificationSink interface {
	Send(title, body string) error
}

type Notifier struct {
	mu   sync.Mutex
	now  func() time.Time
	sink NotificationSink
	last map[string]time.Time
}

func NewNotifier(now func() time.Time, sink NotificationSink) *Notifier {
	if now == nil {
		now = time.Now
	}
	return &Notifier{now: now, sink: sink, last: make(map[string]time.Time)}
}

func (n *Notifier) Notify(event Event) (bool, error) {
	title, body, ok := notificationText(event)
	if !ok {
		return false, ErrUnsupportedNotification
	}
	if n == nil || n.sink == nil {
		return false, errors.New("notification_delivery_failed")
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	key := event.Component + "\x00" + event.Code
	if previous, found := n.last[key]; found && now.Sub(previous) < 30*time.Second {
		return false, nil
	}
	if err := n.sink.Send(safeMenuText(title, 63), safeMenuText(body, 255)); err != nil {
		return false, errors.New("notification_delivery_failed")
	}
	n.last[key] = now
	return true, nil
}

func notificationText(event Event) (string, string, bool) {
	switch event.Code {
	case CodeStartFailed:
		if event.Component != "agent" && event.Component != "helper" {
			return "", "", false
		}
		return "组件启动失败", "请打开节点控制台查看状态并重试。", true
	case CodeUnexpectedExit:
		if event.Component != "agent" && event.Component != "helper" {
			return "", "", false
		}
		return "组件异常退出", "请打开节点控制台查看状态。", true
	case CodeWorkersNotReady:
		if event.Component != "worker" {
			return "", "", false
		}
		return "Worker 尚未就绪", "Worker 长时间未达到预期数量，请检查 Agent 状态。", true
	case CodeConfigCorrupt:
		if event.Component != "config" {
			return "", "", false
		}
		return "配置损坏", "配置无法安全读取，请在节点控制台中恢复或重新保存。", true
	case CodeConfigDrift:
		if event.Component != "config" {
			return "", "", false
		}
		return "运行配置需要同步", "已保存配置与运行状态不一致，请打开节点控制台处理。", true
	case CodeUACRequired:
		if event.Component != "helper" {
			return "", "", false
		}
		return "需要管理员权限", "手动启动删除 Helper 需要确认 Windows 管理员授权。", true
	default:
		return "", "", false
	}
}
