package gui

import (
	"context"
	"errors"
	"sync"

	"dedup/internal/proto"
)

var ErrPreviewAgentOffline = errors.New("preview agent offline")

type previewTransport interface {
	IsOnline(machineID string) bool
	Send(machineID string, msgType uint8, value any) error
}

type previewPending struct {
	machineID string
	result    chan proto.LocalResponse
}

// PreviewBroker pairs agent local.preview.image responses with their
// originating HTTP request over the manager channel.
type PreviewBroker struct {
	transport previewTransport

	mu      sync.Mutex
	pending map[string]previewPending
}

func NewPreviewBroker(transport previewTransport) *PreviewBroker {
	return &PreviewBroker{
		transport: transport,
		pending:   make(map[string]previewPending),
	}
}

func (broker *PreviewBroker) Preview(
	ctx context.Context,
	machineID string,
	request proto.LocalImagePreviewRequest,
) (proto.LocalResponse, error) {
	if broker.transport == nil || !broker.transport.IsOnline(machineID) {
		return proto.LocalResponse{}, ErrPreviewAgentOffline
	}
	if err := request.Validate(); err != nil {
		return proto.LocalResponse{}, err
	}
	payload, err := proto.EncodeLocalPayload(request)
	if err != nil {
		return proto.LocalResponse{}, err
	}
	requestID, err := filesystemBrowseRequestID()
	if err != nil {
		return proto.LocalResponse{}, err
	}
	key := previewKey(machineID, requestID)
	result := make(chan proto.LocalResponse, 1)
	broker.mu.Lock()
	broker.pending[key] = previewPending{machineID: machineID, result: result}
	broker.mu.Unlock()
	localRequest := proto.LocalRequest{
		RequestID: requestID,
		Operation: proto.LocalOperationPreviewImage,
		Payload:   payload,
	}
	if err := broker.transport.Send(machineID, proto.MsgLocalRequest, &localRequest); err != nil {
		broker.removePending(key)
		return proto.LocalResponse{}, err
	}

	select {
	case response := <-result:
		return response, nil
	case <-ctx.Done():
		broker.removePending(key)
		return proto.LocalResponse{}, ctx.Err()
	}
}

// Dispatch claims only responses that pair with a pending preview request so
// other manager-channel local operations can add their own brokers later.
func (broker *PreviewBroker) Dispatch(machineID string, message any) bool {
	response, ok := message.(*proto.LocalResponse)
	if !ok || response == nil || response.RequestID == "" {
		return false
	}
	key := previewKey(machineID, response.RequestID)
	broker.mu.Lock()
	pending, ok := broker.pending[key]
	if ok {
		delete(broker.pending, key)
	}
	broker.mu.Unlock()
	if !ok {
		return false
	}
	pending.result <- *response
	return true
}

func (broker *PreviewBroker) FailMachine(machineID string) {
	broker.mu.Lock()
	failed := make([]previewPending, 0)
	for key, pending := range broker.pending {
		if pending.machineID == machineID {
			delete(broker.pending, key)
			failed = append(failed, pending)
		}
	}
	broker.mu.Unlock()
	for _, pending := range failed {
		pending.result <- proto.LocalResponse{ErrorCode: "agent_disconnected"}
	}
}

func (broker *PreviewBroker) removePending(key string) {
	broker.mu.Lock()
	delete(broker.pending, key)
	broker.mu.Unlock()
}

func previewKey(machineID, requestID string) string {
	return machineID + "\x00" + requestID
}
