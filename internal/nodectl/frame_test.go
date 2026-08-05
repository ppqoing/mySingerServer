package nodectl

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func TestFrameRoundTripRequest(t *testing.T) {
	// This catches a wire-format change that stops a valid command crossing a local pipe.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	want := Request{Version: ProtocolVersion, RequestID: "request-1", Command: CommandStatus}
	done := make(chan error, 1)
	go func() { done <- WriteFrame(left, want) }()
	var got Request
	if err := ReadFrame(right, &got); err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestFrameRoundTripsMaximumWorkerSnapshot(t *testing.T) {
	// This catches accidental frame growth or a lower worker bound than the frozen protocol permits.
	status := validAgentStatus()
	status.WorkerExpected = 1024
	status.WorkerReady = 1024
	status.Workers = make([]WorkerStatus, 1024)
	for i := range status.Workers {
		status.Workers[i] = WorkerStatus{Index: i, PID: i + 100, Ready: true, CurrentTaskSummary: "scan"}
	}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() { done <- WriteFrame(left, status) }()
	var got Status
	if err := ReadFrame(right, &got); err != nil {
		t.Fatalf("ReadFrame(max worker status) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteFrame(max worker status) error = %v", err)
	}
	if len(got.Workers) != 1024 || got.Workers[1023].Index != 1023 {
		t.Fatalf("worker snapshot = len %d last %#v, want 1024 workers through index 1023", len(got.Workers), got.Workers[len(got.Workers)-1])
	}
}

func TestFrameRejectsZeroAndOversizedDeclaredLengths(t *testing.T) {
	// This catches allocating or accepting an invalid frame before MessagePack decoding begins.
	for _, declared := range []uint32{0, MaxFrameSize + 1} {
		t.Run("declared length", func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			done := make(chan error, 1)
			go func() {
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], declared)
				_, err := left.Write(header[:])
				done <- err
			}()
			var got Request
			if err := ReadFrame(right, &got); err == nil {
				t.Fatalf("ReadFrame(length=%d) error = nil, want rejection", declared)
			}
			if err := <-done; err != nil {
				t.Fatalf("header write error = %v", err)
			}
		})
	}
}

func TestFrameRejectsTruncatedPayload(t *testing.T) {
	// This catches treating a partial payload as a complete control message.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], 8)
		if _, err := left.Write(header[:]); err != nil {
			done <- err
			return
		}
		if _, err := left.Write([]byte{0x82, 0xa1, 'x'}); err != nil {
			done <- err
			return
		}
		done <- left.Close()
	}()
	var got Request
	if err := ReadFrame(right, &got); err == nil {
		t.Fatal("ReadFrame(truncated) error = nil, want rejection")
	}
	if err := <-done; err != nil && err != io.ErrClosedPipe {
		t.Fatalf("truncated write error = %v", err)
	}
}

func TestFrameIgnoresUnknownAdditionalFields(t *testing.T) {
	// This catches an incompatible decoder that rejects future fields instead of preserving v1 compatibility.
	payload, err := msgpack.Marshal(map[string]any{
		"version":        ProtocolVersion,
		"request_id":     "request-1",
		"command":        string(CommandStatus),
		"future_feature": "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadFrame(bytes.NewReader(prefixed(payload)), &got); err != nil {
		t.Fatalf("ReadFrame(unknown field) error = %v", err)
	}
	if got.Command != CommandStatus || got.RequestID != "request-1" {
		t.Fatalf("decoded request = %#v, want valid request fields", got)
	}
}

func TestFrameRejectsDecodedInvalidResponse(t *testing.T) {
	// This catches a decoder that accepts an invalid response shape after MessagePack parsing succeeds.
	payload, err := msgpack.Marshal(Response{
		Version:   ProtocolVersion,
		RequestID: "request-1",
		OK:        false,
		Status:    ptrStatus(validAgentStatus()),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := ReadFrame(bytes.NewReader(prefixed(payload)), &got); err == nil {
		t.Fatal("ReadFrame(invalid response) error = nil, want validation error")
	}
}

func TestWriteFrameRejectsPayloadLargerThanBound(t *testing.T) {
	// This catches writing a length prefix before discovering that an encoded payload exceeds 1 MiB.
	var output bytes.Buffer
	if err := WriteFrame(&output, map[string]string{"payload": string(bytes.Repeat([]byte{'x'}, MaxFrameSize))}); err == nil {
		t.Fatal("WriteFrame(oversized) error = nil, want rejection")
	}
	if output.Len() != 0 {
		t.Fatalf("WriteFrame(oversized) wrote %d bytes, want 0", output.Len())
	}
}

func prefixed(payload []byte) []byte {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	return append(header[:], payload...)
}
