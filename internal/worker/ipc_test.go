package worker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

type frameBody struct {
	ID   int    `msgpack:"id"`
	Text string `msgpack:"text"`
}

func TestFrameRoundTrip(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	writer := NewIPCConn(left)
	reader := NewIPCConn(right)

	const count = 24
	errs := make(chan error, count)
	var writers sync.WaitGroup
	for i := 0; i < count; i++ {
		writers.Add(1)
		go func(id int) {
			defer writers.Done()
			errs <- writer.Write(MsgResult, frameBody{ID: id, Text: fmt.Sprintf("frame-%d", id)})
		}(i)
	}

	seen := make(map[int]string, count)
	for range count {
		env, err := reader.Read()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if env.Type != MsgResult {
			t.Fatalf("type = %q, want %q", env.Type, MsgResult)
		}
		body, err := DecodeBody[frameBody](env)
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		seen[body.ID] = body.Text
	}
	writers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	if len(seen) != count {
		t.Fatalf("decoded %d frames, want %d", len(seen), count)
	}
	for i := 0; i < count; i++ {
		if got, want := seen[i], fmt.Sprintf("frame-%d", i); got != want {
			t.Fatalf("frame %d = %q, want %q", i, got, want)
		}
	}
}

func TestFrameRejectsZeroOversizeAndMalformed(t *testing.T) {
	t.Run("write rejects empty type", func(t *testing.T) {
		var stream bytes.Buffer
		if err := NewIPCConn(&stream).Write("", frameBody{ID: 1, Text: "body"}); err == nil || !strings.Contains(err.Error(), "type") {
			t.Fatalf("Write empty type error = %v, want type error", err)
		}
	})

	t.Run("read rejects zero header without body", func(t *testing.T) {
		stream := bytes.NewBuffer([]byte{0, 0, 0, 0})
		if _, err := NewIPCConn(stream).Read(); err == nil || !strings.Contains(err.Error(), "length") {
			t.Fatalf("Read zero length error = %v, want length error", err)
		}
	})

	t.Run("read rejects oversize header without body", func(t *testing.T) {
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, MaxFrameBytes+1)
		stream := bytes.NewBuffer(header)
		if _, err := NewIPCConn(stream).Read(); err == nil || !strings.Contains(err.Error(), "length") {
			t.Fatalf("Read oversize length error = %v, want length error", err)
		}
	})

	t.Run("read rejects malformed msgpack", func(t *testing.T) {
		stream := bytes.NewBuffer([]byte{0, 0, 0, 1, 0xc1})
		if _, err := NewIPCConn(stream).Read(); err == nil || !strings.Contains(err.Error(), "envelope") {
			t.Fatalf("Read malformed msgpack error = %v, want envelope error", err)
		}
	})

	t.Run("read rejects empty type", func(t *testing.T) {
		stream := bytes.NewBuffer([]byte{0, 0, 0, 14, 0x82, 0xa4, 't', 'y', 'p', 'e', 0xa0, 0xa4, 'b', 'o', 'd', 'y', 0xc4, 0x00})
		if _, err := NewIPCConn(stream).Read(); err == nil || !strings.Contains(err.Error(), "type") {
			t.Fatalf("Read empty type error = %v, want type error", err)
		}
	})

	t.Run("read distinguishes truncated header", func(t *testing.T) {
		_, err := NewIPCConn(bytes.NewBuffer([]byte{0, 0})).Read()
		if err == nil || !strings.Contains(err.Error(), "header") || !strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
			t.Fatalf("Read truncated header error = %v, want wrapped unexpected EOF", err)
		}
	})

	t.Run("decode body rejects nil and malformed", func(t *testing.T) {
		if _, err := DecodeBody[frameBody](nil); err == nil {
			t.Fatal("DecodeBody(nil) unexpectedly succeeded")
		}
		if _, err := DecodeBody[frameBody](&Envelope{Type: MsgJob, Body: []byte{0xc1}}); err == nil {
			t.Fatal("DecodeBody(malformed) unexpectedly succeeded")
		}
	})
}

func TestFrameLengthAcceptsOnlyUint32InRange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value uint32
		valid bool
	}{
		{"zero", 0, false},
		{"maximum", uint32(MaxFrameBytes), true},
		{"maximum plus one", uint32(MaxFrameBytes) + 1, false},
		{"high bit", 0x80000000, false},
		{"uint32 maximum", 0xffffffff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validFrameLengthUint32(tc.value)
			if (err == nil) != tc.valid {
				t.Fatalf("validFrameLengthUint32(%#x) error = %v, want valid=%t", tc.value, err, tc.valid)
			}
		})
	}
}

func TestIPCConnHonorsConfiguredFrameMaximum(t *testing.T) {
	var stream bytes.Buffer
	conn := NewIPCConnWithMax(&stream, 64)
	err := conn.Write(
		MsgResult,
		frameBody{ID: 1, Text: strings.Repeat("x", 128)},
	)
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("configured maximum Write error = %v, want length rejection", err)
	}
}

// Break caught: a lease message can bypass the configured IPC frame ceiling
// merely because it uses a newly added message type.
func TestIOLeaseFrameHonorsConfiguredMaximum(t *testing.T) {
	var stream bytes.Buffer
	err := NewIPCConnWithMax(&stream, 128).Write(MsgIOLeaseAcquire, IOLeaseAcquireMsg{
		JobID: 1, RequestID: 2, TaskID: strings.Repeat("t", 256),
		InstanceID: "instance", DiskKey: "disk", Class: 1, WantBytes: 1 << 20,
	})
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("oversized lease frame error = %v, want length rejection", err)
	}
}

func TestIPCReadDistinguishesCleanEOFTruncatedBodyAndIncompatibleBody(t *testing.T) {
	t.Run("clean EOF", func(t *testing.T) {
		_, err := NewIPCConn(bytes.NewBuffer(nil)).Read()
		if !errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("clean EOF error = %v", err)
		}
	})
	t.Run("truncated body", func(t *testing.T) {
		var stream bytes.Buffer
		if err := binary.Write(&stream, binary.BigEndian, uint32(5)); err != nil {
			t.Fatal(err)
		}
		stream.Write([]byte{0x81, 0xa1})
		_, err := NewIPCConn(&stream).Read()
		if !errors.Is(err, io.ErrUnexpectedEOF) ||
			!strings.Contains(err.Error(), "payload") {
			t.Fatalf("truncated body error = %v", err)
		}
	})
	t.Run("valid but incompatible body", func(t *testing.T) {
		body, err := msgpack.Marshal(map[string]any{
			"id": "not-an-integer", "text": "valid msgpack",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecodeBody[frameBody](&Envelope{Type: MsgJob, Body: body})
		if err == nil || !strings.Contains(err.Error(), "decode body") {
			t.Fatalf("incompatible body error = %v", err)
		}
	})
}
