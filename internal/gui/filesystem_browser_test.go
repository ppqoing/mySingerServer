package gui

import (
	"context"
	"errors"
	"testing"
	"time"

	"dedup/internal/proto"
)

type fakeFilesystemTransport struct {
	online  bool
	sent    chan proto.FilesystemBrowseRequest
	sendErr error
}

func (transport *fakeFilesystemTransport) IsOnline(string) bool {
	return transport.online
}

func (transport *fakeFilesystemTransport) Send(_ string, msgType uint8, value any) error {
	if transport.sendErr != nil {
		return transport.sendErr
	}
	if msgType != proto.MsgFilesystemBrowse {
		return errors.New("unexpected message type")
	}
	request, ok := value.(*proto.FilesystemBrowseRequest)
	if !ok {
		return errors.New("unexpected message value")
	}
	transport.sent <- *request
	return nil
}

func TestFilesystemBrokerPairsResponseByMachineAndRequestID(t *testing.T) {
	transport := &fakeFilesystemTransport{online: true, sent: make(chan proto.FilesystemBrowseRequest, 1)}
	broker := NewFilesystemBroker(transport)
	result := make(chan proto.FilesystemBrowseResponse, 1)
	go func() {
		response, _ := broker.Browse(context.Background(), "machine-a", proto.FilesystemBrowseRequest{Path: `D:\Media`, Limit: 200})
		result <- response
	}()
	sent := <-transport.sent
	if !broker.Dispatch("machine-a", &proto.FilesystemBrowseResponse{RequestID: sent.RequestID, CurrentPath: `D:\Media`}) {
		t.Fatal("response not claimed")
	}
	if got := <-result; got.CurrentPath != `D:\Media` {
		t.Fatalf("response=%#v", got)
	}
}

func TestFilesystemBrokerConsumesWrongMachineResponseWithoutPairing(t *testing.T) {
	transport := &fakeFilesystemTransport{online: true, sent: make(chan proto.FilesystemBrowseRequest, 1)}
	broker := NewFilesystemBroker(transport)
	result := make(chan proto.FilesystemBrowseResponse, 1)
	go func() {
		response, _ := broker.Browse(context.Background(), "machine-a", proto.FilesystemBrowseRequest{Path: `D:\Media`, Limit: 200})
		result <- response
	}()
	sent := <-transport.sent
	if !broker.Dispatch("machine-b", &proto.FilesystemBrowseResponse{RequestID: sent.RequestID}) {
		t.Fatal("wrong machine browse response was not consumed")
	}
	select {
	case response := <-result:
		t.Fatalf("wrong machine paired response=%#v", response)
	case <-time.After(50 * time.Millisecond):
	}
	if !broker.Dispatch("machine-a", &proto.FilesystemBrowseResponse{RequestID: sent.RequestID, CurrentPath: `D:\Media`}) {
		t.Fatal("correct response not claimed")
	}
	<-result
}

func TestFilesystemBrokerCancellationClearsPendingAndIgnoresLateResponse(t *testing.T) {
	transport := &fakeFilesystemTransport{online: true, sent: make(chan proto.FilesystemBrowseRequest, 1)}
	broker := NewFilesystemBroker(transport)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := broker.Browse(ctx, "machine-a", proto.FilesystemBrowseRequest{Path: `D:\Media`, Limit: 200})
		result <- err
	}()
	sent := <-transport.sent
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Browse error=%v, want context cancellation", err)
	}
	if !broker.Dispatch("machine-a", &proto.FilesystemBrowseResponse{RequestID: sent.RequestID}) {
		t.Fatal("late browse response was not consumed after cancellation")
	}
}

func TestFilesystemBrokerFailMachineReturnsDisconnectImmediately(t *testing.T) {
	transport := &fakeFilesystemTransport{online: true, sent: make(chan proto.FilesystemBrowseRequest, 1)}
	broker := NewFilesystemBroker(transport)
	result := make(chan proto.FilesystemBrowseResponse, 1)
	go func() {
		response, _ := broker.Browse(context.Background(), "machine-a", proto.FilesystemBrowseRequest{Path: `D:\Media`, Limit: 200})
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
		t.Fatal("disconnect did not release pending browse")
	}
}

func TestFilesystemBrokerDoesNotSendToOfflineAgent(t *testing.T) {
	transport := &fakeFilesystemTransport{online: false, sent: make(chan proto.FilesystemBrowseRequest, 1)}
	broker := NewFilesystemBroker(transport)
	_, err := broker.Browse(context.Background(), "machine-a", proto.FilesystemBrowseRequest{Path: `D:\Media`, Limit: 200})
	if err == nil {
		t.Fatal("offline browse did not fail")
	}
	select {
	case request := <-transport.sent:
		t.Fatalf("offline browse sent request=%#v", request)
	default:
	}
}
