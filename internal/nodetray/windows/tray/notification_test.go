package tray

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type recordedNotification struct {
	title string
	body  string
}

type recordingSink struct {
	values []recordedNotification
	err    error
}

func (s *recordingSink) Send(title, body string) error {
	s.values = append(s.values, recordedNotification{title: title, body: body})
	return s.err
}

func TestNotifierAllowsOnlyFixedAttentionCodesAndIgnoresRawSummary(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	sink := &recordingSink{}
	notifier := NewNotifier(func() time.Time { return now }, sink)

	allowed := []Event{
		{Component: "agent", Code: CodeStartFailed},
		{Component: "agent", Code: CodeUnexpectedExit},
		{Component: "worker", Code: CodeWorkersNotReady},
		{Component: "config", Code: CodeConfigCorrupt},
		{Component: "config", Code: CodeConfigDrift},
		{Component: "helper", Code: CodeUACRequired, Summary: "password=hunter2 postgres://user:pass@db/db C:\\secret\\helper.json\r\n"},
	}
	for _, event := range allowed {
		sent, err := notifier.Notify(event)
		if err != nil || !sent {
			t.Fatalf("Notify(%+v) = sent %v, err %v", event, sent, err)
		}
		now = now.Add(time.Second)
	}
	if len(sink.values) != len(allowed) {
		t.Fatalf("sink count = %d, want %d", len(sink.values), len(allowed))
	}
	for _, value := range sink.values {
		combined := value.title + " " + value.body
		for _, forbidden := range []string{"hunter2", "postgres://", "user:pass", `C:\\secret`} {
			if strings.Contains(combined, forbidden) {
				t.Fatalf("notification leaked %q in %q", forbidden, combined)
			}
		}
		if strings.ContainsAny(value.title, "\r\n") || strings.ContainsAny(value.body, "\r\n") {
			t.Fatalf("notification contains a control line break: title=%q body=%q", value.title, value.body)
		}
		if len([]rune(value.title)) > 63 || len([]rune(value.body)) > 255 {
			t.Fatalf("notification exceeds Windows limits: title=%d body=%d", len([]rune(value.title)), len([]rune(value.body)))
		}
	}

	if sent, err := notifier.Notify(Event{Component: "agent", Code: "normal-refresh", Summary: "anything"}); sent || !errors.Is(err, ErrUnsupportedNotification) {
		t.Fatalf("unknown notification = sent %v, err %v", sent, err)
	}
	if len(sink.values) != len(allowed) {
		t.Fatal("unknown notification reached sink")
	}
}

func TestNotifierDeduplicatesComponentAndCodeForThirtySeconds(t *testing.T) {
	now := time.Unix(100, 0)
	sink := &recordingSink{}
	notifier := NewNotifier(func() time.Time { return now }, sink)
	event := Event{Component: "agent", Code: CodeStartFailed}

	if sent, err := notifier.Notify(event); err != nil || !sent {
		t.Fatalf("first notify = sent %v, err %v", sent, err)
	}
	now = now.Add(29 * time.Second)
	if sent, err := notifier.Notify(event); err != nil || sent {
		t.Fatalf("29-second notify = sent %v, err %v", sent, err)
	}
	if sent, err := notifier.Notify(Event{Component: "helper", Code: CodeStartFailed}); err != nil || !sent {
		t.Fatalf("different component notify = sent %v, err %v", sent, err)
	}
	now = now.Add(time.Second)
	if sent, err := notifier.Notify(event); err != nil || !sent {
		t.Fatalf("30-second notify = sent %v, err %v", sent, err)
	}
	if len(sink.values) != 3 {
		t.Fatalf("sink count = %d, want 3", len(sink.values))
	}
}

func TestNotifierPropagatesSinkFailureWithoutRecordingDeduplication(t *testing.T) {
	now := time.Unix(200, 0)
	sink := &recordingSink{err: errors.New("sink unavailable")}
	notifier := NewNotifier(func() time.Time { return now }, sink)
	event := Event{Component: "agent", Code: CodeUnexpectedExit}

	if sent, err := notifier.Notify(event); sent || err == nil || err.Error() != "notification_delivery_failed" {
		t.Fatalf("failed delivery = sent %v, err %v", sent, err)
	}
	sink.err = nil
	if sent, err := notifier.Notify(event); err != nil || !sent {
		t.Fatalf("retry after failed delivery = sent %v, err %v", sent, err)
	}
}
