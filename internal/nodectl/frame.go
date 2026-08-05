package nodectl

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

const MaxFrameSize = 1024 * 1024

type validatable interface {
	Validate() error
}

// WriteFrame MessagePack-encodes value and writes it as one bounded, big-endian
// length-prefixed frame. It does not write anything when encoding exceeds the
// protocol limit.
func WriteFrame(w io.Writer, value any) error {
	payload, err := msgpack.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode control frame: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameSize {
		return fmt.Errorf("control frame size %d outside 1..%d", len(payload), MaxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(w, header[:]); err != nil {
		return fmt.Errorf("write control frame header: %w", err)
	}
	if err := writeAll(w, payload); err != nil {
		return fmt.Errorf("write control frame payload: %w", err)
	}
	return nil
}

// ReadFrame reads exactly one bounded, big-endian length-prefixed MessagePack
// frame and validates decoded protocol values.
func ReadFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return fmt.Errorf("read control frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > MaxFrameSize {
		return fmt.Errorf("control frame declared size %d outside 1..%d", length, MaxFrameSize)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read control frame payload: %w", err)
	}
	if err := msgpack.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("decode control frame: %w", err)
	}
	if checked, ok := value.(validatable); ok {
		if err := checked.Validate(); err != nil {
			return fmt.Errorf("validate control frame: %w", err)
		}
	}
	return nil
}

func writeAll(w io.Writer, value []byte) error {
	for len(value) > 0 {
		n, err := w.Write(value)
		if n > 0 {
			value = value[n:]
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
