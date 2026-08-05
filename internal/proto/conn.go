package proto

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxFrameSize follows architecture-plan v1.2 and the M1 detail document.
const MaxFrameSize = 16 << 20

const DefaultWriteTimeout = 30 * time.Second

var ErrFrameTooLarge = errors.New("proto: frame length invalid or exceeds 16MB")

type envelope struct {
	Type uint8              `msgpack:"t"`
	Body msgpack.RawMessage `msgpack:"b"`
}

// Conn reads and writes length-prefixed msgpack frames. Writes may be made
// concurrently; reads must be made by one goroutine.
type Conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
	wm sync.Mutex
	wt time.Duration
}

func NewConn(nc net.Conn) *Conn {
	return &Conn{
		nc: nc,
		r:  bufio.NewReaderSize(nc, 64<<10),
		w:  bufio.NewWriterSize(nc, 256<<10),
		wt: DefaultWriteTimeout,
	}
}

func (c *Conn) Close() error                       { return c.nc.Close() }
func (c *Conn) RemoteAddr() net.Addr               { return c.nc.RemoteAddr() }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.nc.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.nc.SetWriteDeadline(t) }

func (c *Conn) SetWriteTimeout(timeout time.Duration) {
	c.wm.Lock()
	c.wt = timeout
	c.wm.Unlock()
}

func (c *Conn) WriteFrame(msgType uint8, value any) error {
	payload, err := EncodeFramePayload(msgType, value)
	if err != nil {
		return err
	}

	c.wm.Lock()
	defer c.wm.Unlock()
	if c.wt > 0 {
		if err := c.nc.SetWriteDeadline(time.Now().Add(c.wt)); err != nil {
			return err
		}
		defer func() {
			_ = c.nc.SetWriteDeadline(time.Time{})
		}()
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := c.w.Write(header[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	return c.w.Flush()
}

// EncodeFramePayload applies the exact body, envelope, and size validation
// used by Conn.WriteFrame, without performing network I/O.
func EncodeFramePayload(msgType uint8, value any) ([]byte, error) {
	body, err := msgpack.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("proto: marshal body type=%d: %w", msgType, err)
	}
	payload, err := msgpack.Marshal(envelope{
		Type: msgType,
		Body: msgpack.RawMessage(body),
	})
	if err != nil {
		return nil, fmt.Errorf("proto: marshal envelope: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	return payload, nil
}

func (c *Conn) ReadFrame() (uint8, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > MaxFrameSize {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, err
	}
	var env envelope
	if err := msgpack.Unmarshal(payload, &env); err != nil {
		return 0, nil, fmt.Errorf("proto: bad envelope: %w", err)
	}
	if len(env.Body) == 0 {
		return 0, nil, fmt.Errorf("proto: empty body for type=%d", env.Type)
	}
	return env.Type, env.Body, nil
}

// Heartbeat sends Ping frames until the context is cancelled or writing fails.
func Heartbeat(ctx context.Context, conn *Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := conn.WriteFrame(MsgPing, &Ping{TS: now.UnixMilli()}); err != nil {
				return
			}
		}
	}
}
