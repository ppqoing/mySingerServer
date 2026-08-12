package proto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/nodectl"
)

// These cases fail if local control messages permit more than the fixed
// 4 MiB boundary, unknown commands, or topics that cannot be stable keys.
func TestLocalEnvelopeRejectsOversizedPayloadAndUnknownOperation(t *testing.T) {
	oversized := make([]byte, LocalPayloadMaxBytes+1)
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"request payload", (LocalRequest{RequestID: "request-1", Operation: LocalOperationStatusGet, Payload: oversized}).Validate(), LocalPayloadTooLargeErrorCode},
		{"response payload", (LocalResponse{RequestID: "request-1", Payload: oversized}).Validate(), LocalPayloadTooLargeErrorCode},
		{"event payload", (LocalEvent{Sequence: 1, Topic: "analysis.progress", Payload: oversized}).Validate(), LocalPayloadTooLargeErrorCode},
		{"unknown operation", (LocalRequest{RequestID: "request-1", Operation: "local.unknown"}).Validate(), UnsupportedOperationErrorCode},
		{"whitespace topic", (LocalEvent{Sequence: 1, Topic: " analysis.progress"}).Validate(), InvalidLocalTopicErrorCode},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatalf("Validate() succeeded, want %q", tt.want)
			}
			if tt.err.Error() != tt.want {
				t.Fatalf("Validate() error = %q, want %q", tt.err, tt.want)
			}
		})
	}

	if err := (LocalEvent{Sequence: 1, Topic: "analysis.progress"}).Validate(); err != nil {
		t.Fatalf("valid LocalEvent.Validate(): %v", err)
	}
	if err := (LocalRequest{RequestID: "request-2", Operation: LocalOperationShutdown}).Validate(); err != nil {
		t.Fatalf("known LocalRequest.Validate(): %v", err)
	}
}

func TestLocalLifecyclePayloadsRoundTripWithoutNewMessageTypes(t *testing.T) {
	status := nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: "node-" + strings.Repeat("1", 64), PID: 17,
		ExecutablePath: `C:\agent.exe`, ConfigSHA256: strings.Repeat("a", 64),
		Lifecycle: "running", ServiceReady: true, Ready: true, SyncHealthy: true,
	}
	tests := []any{
		LocalStatusGetResponse{Status: status},
		LocalConfigGetResponse{CanonicalJSON: []byte("{\n}\n"), SHA256: strings.Repeat("b", 64)},
		LocalConfigRequest{CanonicalJSON: []byte("{\n}\n")},
		LocalConfigValidateResponse{Valid: true, SHA256: strings.Repeat("c", 64), RestartRequired: true},
		LocalConfigSaveResponse{SHA256: strings.Repeat("d", 64), RestartRequired: true},
		LocalShutdownResponse{Accepted: true},
	}
	for _, want := range tests {
		encoded, err := msgpack.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %T: %v", want, err)
		}
		got := reflect.New(reflect.TypeOf(want)).Interface()
		if err := msgpack.Unmarshal(encoded, got); err != nil {
			t.Fatalf("unmarshal %T: %v", want, err)
		}
		if !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), want) {
			t.Fatalf("round trip %T = %#v, want %#v", want, got, want)
		}
	}
}
