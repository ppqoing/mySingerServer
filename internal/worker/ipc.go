package worker

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
)

const MaxFrameBytes = 16 << 20

type IPCConn struct {
	rwc           io.ReadWriter
	maxFrameBytes int
	mu            sync.Mutex
}

func NewIPCConn(rwc io.ReadWriter) *IPCConn {
	return NewIPCConnWithMax(rwc, MaxFrameBytes)
}

func NewIPCConnWithMax(rwc io.ReadWriter, maxFrameBytes int) *IPCConn {
	if maxFrameBytes <= 0 {
		maxFrameBytes = MaxFrameBytes
	}
	if maxFrameBytes > MaxFrameBytes {
		maxFrameBytes = MaxFrameBytes
	}
	return &IPCConn{rwc: rwc, maxFrameBytes: maxFrameBytes}
}

func (c *IPCConn) Write(msgType string, body any) error {
	if msgType == "" {
		return fmt.Errorf("worker IPC: message type is required")
	}
	bodyBytes, err := msgpack.Marshal(body)
	if err != nil {
		return fmt.Errorf("worker IPC: marshal body: %w", err)
	}
	payload, err := msgpack.Marshal(Envelope{Type: msgType, Body: bodyBytes})
	if err != nil {
		return fmt.Errorf("worker IPC: marshal envelope: %w", err)
	}
	if err := validFrameLengthMax(len(payload), c.maxFrameBytes); err != nil {
		return err
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writeFull(c.rwc, header[:]); err != nil {
		return fmt.Errorf("worker IPC: write header: %w", err)
	}
	if err := writeFull(c.rwc, payload); err != nil {
		return fmt.Errorf("worker IPC: write payload: %w", err)
	}
	if flusher, ok := c.rwc.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return fmt.Errorf("worker IPC: flush: %w", err)
		}
	}
	return nil
}

func (c *IPCConn) Read() (*Envelope, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.rwc, header[:]); err != nil {
		return nil, fmt.Errorf("worker IPC: read header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if err := validFrameLengthUint32Max(length, c.maxFrameBytes); err != nil {
		return nil, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.rwc, payload); err != nil {
		return nil, fmt.Errorf("worker IPC: read payload: %w", err)
	}
	var env Envelope
	if err := msgpack.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("worker IPC: decode envelope: %w", err)
	}
	if env.Type == "" {
		return nil, fmt.Errorf("worker IPC: message type is required")
	}
	return &env, nil
}

func DecodeBody[T any](env *Envelope) (T, error) {
	var body T
	if env == nil {
		return body, fmt.Errorf("worker IPC: decode body: nil envelope")
	}
	if err := msgpack.Unmarshal(env.Body, &body); err != nil {
		return body, fmt.Errorf("worker IPC: decode body: %w", err)
	}
	return body, nil
}

func validFrameLength(length int) error {
	return validFrameLengthMax(length, MaxFrameBytes)
}

func validFrameLengthMax(length, maximum int) error {
	if length == 0 || length > maximum {
		return fmt.Errorf("worker IPC: invalid frame length %d", length)
	}
	return nil
}

func validFrameLengthUint32(length uint32) error {
	return validFrameLengthUint32Max(length, MaxFrameBytes)
}

func validFrameLengthUint32Max(length uint32, maximum int) error {
	if length == 0 || uint64(length) > uint64(maximum) {
		return fmt.Errorf("worker IPC: invalid frame length %d", length)
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
