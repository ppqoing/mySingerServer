package proto

import (
	"testing"
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
