package gui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	"dedup/internal/proto"
)

var ErrFilesystemAgentOffline = errors.New("filesystem agent offline")

type filesystemBrowseTransport interface {
	IsOnline(machineID string) bool
	Send(machineID string, msgType uint8, value any) error
}

type filesystemBrowsePending struct {
	machineID string
	result    chan proto.FilesystemBrowseResponse
}

// FilesystemBroker pairs agent browse responses with their originating HTTP request.
type FilesystemBroker struct {
	transport filesystemBrowseTransport

	mu      sync.Mutex
	pending map[string]filesystemBrowsePending
}

func NewFilesystemBroker(transport filesystemBrowseTransport) *FilesystemBroker {
	return &FilesystemBroker{
		transport: transport,
		pending:   make(map[string]filesystemBrowsePending),
	}
}

func (broker *FilesystemBroker) Browse(
	ctx context.Context,
	machineID string,
	request proto.FilesystemBrowseRequest,
) (proto.FilesystemBrowseResponse, error) {
	if broker.transport == nil || !broker.transport.IsOnline(machineID) {
		return proto.FilesystemBrowseResponse{}, ErrFilesystemAgentOffline
	}
	requestID, err := filesystemBrowseRequestID()
	if err != nil {
		return proto.FilesystemBrowseResponse{}, err
	}
	request.RequestID = requestID
	if err := request.Validate(); err != nil {
		return proto.FilesystemBrowseResponse{}, err
	}
	key := filesystemBrowseKey(machineID, requestID)
	result := make(chan proto.FilesystemBrowseResponse, 1)
	broker.mu.Lock()
	broker.pending[key] = filesystemBrowsePending{machineID: machineID, result: result}
	broker.mu.Unlock()
	if err := broker.transport.Send(machineID, proto.MsgFilesystemBrowse, &request); err != nil {
		broker.removePending(key)
		return proto.FilesystemBrowseResponse{}, err
	}

	select {
	case response := <-result:
		return response, nil
	case <-ctx.Done():
		broker.removePending(key)
		return proto.FilesystemBrowseResponse{}, ctx.Err()
	}
}

func (broker *FilesystemBroker) Dispatch(machineID string, message any) bool {
	response, ok := message.(*proto.FilesystemBrowseResponse)
	if !ok || response == nil || response.RequestID == "" {
		return false
	}
	key := filesystemBrowseKey(machineID, response.RequestID)
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

func (broker *FilesystemBroker) FailMachine(machineID string) {
	broker.mu.Lock()
	failed := make([]filesystemBrowsePending, 0)
	for key, pending := range broker.pending {
		if pending.machineID == machineID {
			delete(broker.pending, key)
			failed = append(failed, pending)
		}
	}
	broker.mu.Unlock()
	for _, pending := range failed {
		pending.result <- proto.FilesystemBrowseResponse{ErrorCode: "agent_disconnected"}
	}
}

func (broker *FilesystemBroker) removePending(key string) {
	broker.mu.Lock()
	delete(broker.pending, key)
	broker.mu.Unlock()
}

func filesystemBrowseRequestID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func filesystemBrowseKey(machineID, requestID string) string {
	return machineID + "\x00" + requestID
}
