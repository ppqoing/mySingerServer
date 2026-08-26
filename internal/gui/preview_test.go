package gui

import (
	"context"
	"errors"
	"testing"
	"time"

	"dedup/internal/proto"
)

type fakePreviewTransport struct {
	online  bool
	sent    chan proto.LocalRequest
	sendErr error
}

func (transport *fakePreviewTransport) IsOnline(string) bool {
	return transport.online
}

func (transport *fakePreviewTransport) Send(_ string, msgType uint8, value any) error {
	if transport.sendErr != nil {
		return transport.sendErr
	}
	if msgType != proto.MsgLocalRequest {
		return errors.New("unexpected message type")
	}
	request, ok := value.(*proto.LocalRequest)
	if !ok {
		return errors.New("unexpected message value")
	}
	transport.sent <- *request
	return nil
}

func previewTestRequest() proto.LocalImagePreviewRequest {
	return proto.LocalImagePreviewRequest{
		MaxWidth: 320, MaxHeight: 320, Format: "jpeg", Quality: 80,
		Sha512: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd" +
			"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
	}
}

func TestPreviewBrokerPairsResponseByMachineAndRequestID(t *testing.T) {
	transport := &fakePreviewTransport{online: true, sent: make(chan proto.LocalRequest, 1)}
	broker := NewPreviewBroker(transport)
	result := make(chan proto.LocalResponse, 1)
	go func() {
		response, _ := broker.Preview(context.Background(), "machine-a", previewTestRequest())
		result <- response
	}()
	sent := <-transport.sent
	if sent.Operation != proto.LocalOperationPreviewImage || sent.RequestID == "" || len(sent.Payload) == 0 {
		t.Fatalf("sent request=%#v", sent)
	}
	var decoded proto.LocalImagePreviewRequest
	if err := proto.DecodeLocalImagePreviewPayload(sent.Payload, &decoded); err != nil || decoded.Sha512 != previewTestRequest().Sha512 {
		t.Fatalf("sent payload decoded=%#v err=%v", decoded, err)
	}
	if !broker.Dispatch("machine-a", &proto.LocalResponse{RequestID: sent.RequestID, OK: true, Payload: []byte{1}}) {
		t.Fatal("response not claimed")
	}
	if got := <-result; !got.OK {
		t.Fatalf("response=%#v", got)
	}
}

func TestPreviewBrokerConsumesOnlyItsOwnResponses(t *testing.T) {
	transport := &fakePreviewTransport{online: true, sent: make(chan proto.LocalRequest, 1)}
	broker := NewPreviewBroker(transport)
	if broker.Dispatch("machine-a", &proto.FilesystemBrowseResponse{RequestID: "other"}) {
		t.Fatal("foreign message type claimed")
	}
	if broker.Dispatch("machine-a", &proto.LocalResponse{RequestID: "unrelated"}) {
		t.Fatal("unpaired local response claimed")
	}
	result := make(chan proto.LocalResponse, 1)
	go func() {
		response, _ := broker.Preview(context.Background(), "machine-a", previewTestRequest())
		result <- response
	}()
	sent := <-transport.sent
	if broker.Dispatch("machine-b", &proto.LocalResponse{RequestID: sent.RequestID, OK: true}) {
		t.Fatal("wrong machine response claimed")
	}
	select {
	case response := <-result:
		t.Fatalf("wrong machine paired response=%#v", response)
	case <-time.After(50 * time.Millisecond):
	}
	if !broker.Dispatch("machine-a", &proto.LocalResponse{RequestID: sent.RequestID, OK: true}) {
		t.Fatal("correct response not claimed")
	}
	<-result
}

func TestPreviewBrokerCancellationClearsPendingAndIgnoresLateResponse(t *testing.T) {
	transport := &fakePreviewTransport{online: true, sent: make(chan proto.LocalRequest, 1)}
	broker := NewPreviewBroker(transport)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := broker.Preview(ctx, "machine-a", previewTestRequest())
		result <- err
	}()
	sent := <-transport.sent
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Preview error=%v, want context cancellation", err)
	}
	if broker.Dispatch("machine-a", &proto.LocalResponse{RequestID: sent.RequestID, OK: true}) {
		t.Fatal("late response claimed after cancellation")
	}
}

func TestPreviewBrokerFailMachineReturnsDisconnectImmediately(t *testing.T) {
	transport := &fakePreviewTransport{online: true, sent: make(chan proto.LocalRequest, 1)}
	broker := NewPreviewBroker(transport)
	result := make(chan proto.LocalResponse, 1)
	go func() {
		response, _ := broker.Preview(context.Background(), "machine-a", previewTestRequest())
		result <- response
	}()
	<-transport.sent
	broker.FailMachine("machine-a")
	select {
	case response := <-result:
		if response.ErrorCode != "agent_disconnected" {
			t.Fatalf("response=%#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not release pending preview")
	}
}

func TestPreviewBrokerDoesNotSendToOfflineAgent(t *testing.T) {
	transport := &fakePreviewTransport{online: false, sent: make(chan proto.LocalRequest, 1)}
	broker := NewPreviewBroker(transport)
	_, err := broker.Preview(context.Background(), "machine-a", previewTestRequest())
	if !errors.Is(err, ErrPreviewAgentOffline) {
		t.Fatalf("offline preview error=%v", err)
	}
	select {
	case request := <-transport.sent:
		t.Fatalf("offline preview sent request=%#v", request)
	default:
	}
}

func TestPreviewBrokerRejectsInvalidRequestBeforeSend(t *testing.T) {
	transport := &fakePreviewTransport{online: true, sent: make(chan proto.LocalRequest, 1)}
	broker := NewPreviewBroker(transport)
	if _, err := broker.Preview(context.Background(), "machine-a", proto.LocalImagePreviewRequest{}); err == nil {
		t.Fatal("invalid preview request reached transport")
	}
	select {
	case request := <-transport.sent:
		t.Fatalf("invalid preview sent request=%#v", request)
	default:
	}
}
