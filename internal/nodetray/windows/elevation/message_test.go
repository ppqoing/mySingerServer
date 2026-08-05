package elevation

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

const testNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMessageValidatesFrozenRequestContract(t *testing.T) {
	valid := Request{
		Version: ProtocolVersion,
		Nonce:   testNonce,
		Action:  ActionWriteHelperConfig,
		Payload: []byte("payload"),
	}
	if err := ValidateRequest(valid); err != nil {
		t.Fatalf("ValidateRequest(valid): %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "version", mutate: func(request *Request) { request.Version++ }},
		{name: "empty nonce", mutate: func(request *Request) { request.Nonce = "" }},
		{name: "uppercase nonce", mutate: func(request *Request) { request.Nonce = strings.ToUpper(testNonce) }},
		{name: "short nonce", mutate: func(request *Request) { request.Nonce = testNonce[:62] }},
		{name: "unknown action", mutate: func(request *Request) { request.Action = "run_anything" }},
		{name: "oversize payload", mutate: func(request *Request) { request.Payload = make([]byte, MaxPayloadSize+1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.mutate(&request)
			if err := ValidateRequest(request); err == nil {
				t.Fatal("ValidateRequest accepted an invalid request")
			}
		})
	}
}

func TestMessageGeneratesUnpredictableOneUseNonce(t *testing.T) {
	first, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce(first): %v", err)
	}
	second, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce(second): %v", err)
	}
	if first == second {
		t.Fatal("NewNonce reused a nonce")
	}
	for _, nonce := range []string{first, second} {
		if len(nonce) != 64 || strings.ToLower(nonce) != nonce {
			t.Fatalf("nonce %q is not 64 lowercase hexadecimal characters", nonce)
		}
		if err := ValidateNonce(nonce); err != nil {
			t.Fatalf("ValidateNonce(%q): %v", nonce, err)
		}
	}

	guard, err := NewOneShotGuard(first)
	if err != nil {
		t.Fatalf("NewOneShotGuard: %v", err)
	}
	if err := guard.Consume(first); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if err := guard.Consume(first); err == nil {
		t.Fatal("replayed nonce was accepted")
	}
	if err := guard.Consume(second); err == nil {
		t.Fatal("mismatched nonce was accepted")
	}
}

func TestMessageStrictFramesRejectUnknownTruncatedOversizeAndSecondFrame(t *testing.T) {
	request := Request{Version: ProtocolVersion, Nonce: testNonce, Action: ActionRemoveHelperTask}
	frame, err := EncodeRequestFrame(request)
	if err != nil {
		t.Fatalf("EncodeRequestFrame: %v", err)
	}
	decoded, err := DecodeSingleRequestFrame(frame)
	if err != nil {
		t.Fatalf("DecodeSingleRequestFrame(valid): %v", err)
	}
	if decoded.Action != ActionRemoveHelperTask || decoded.Nonce != testNonce {
		t.Fatalf("decoded request mismatch: %#v", decoded)
	}

	unknownBody, err := msgpack.Marshal(map[string]any{
		"version": ProtocolVersion,
		"nonce":   testNonce,
		"action":  string(ActionRemoveHelperTask),
		"payload": []byte(nil),
		"path":    `C:\\secret\\helper.json`,
	})
	if err != nil {
		t.Fatalf("marshal unknown body: %v", err)
	}
	unknownFrame := make([]byte, 4+len(unknownBody))
	binary.BigEndian.PutUint32(unknownFrame, uint32(len(unknownBody)))
	copy(unknownFrame[4:], unknownBody)
	if _, err := DecodeSingleRequestFrame(unknownFrame); err == nil {
		t.Fatal("unknown request field was accepted")
	}
	if _, err := DecodeSingleRequestFrame(frame[:len(frame)-1]); err == nil {
		t.Fatal("truncated frame was accepted")
	}
	zero := make([]byte, 4)
	if _, err := DecodeSingleRequestFrame(zero); err == nil {
		t.Fatal("zero-length frame was accepted")
	}
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, uint32(MaxFrameSize+1))
	if _, err := DecodeSingleRequestFrame(oversize); err == nil {
		t.Fatal("oversize frame was accepted")
	}
	if _, err := DecodeSingleRequestFrame(append(append([]byte(nil), frame...), frame...)); err == nil {
		t.Fatal("second request frame was accepted")
	}
}

func TestMessageResponseMustMatchVersionNonceAndStableErrorShape(t *testing.T) {
	success := Response{Version: ProtocolVersion, Nonce: testNonce, OK: true}
	frame, err := EncodeResponseFrame(success)
	if err != nil {
		t.Fatalf("EncodeResponseFrame: %v", err)
	}
	if _, err := DecodeSingleResponseFrame(frame, testNonce); err != nil {
		t.Fatalf("DecodeSingleResponseFrame(valid): %v", err)
	}

	invalid := []Response{
		{Version: ProtocolVersion + 1, Nonce: testNonce, OK: true},
		{Version: ProtocolVersion, Nonce: strings.Repeat("a", 64), OK: true},
		{Version: ProtocolVersion, Nonce: testNonce, OK: true, ErrorCode: ErrorCodeInternal},
		{Version: ProtocolVersion, Nonce: testNonce, OK: false},
		{Version: ProtocolVersion, Nonce: testNonce, OK: false, ErrorCode: "invented", ErrorSummary: "failed"},
		{Version: ProtocolVersion, Nonce: testNonce, OK: false, ErrorCode: ErrorCodeInternal, ErrorSummary: `C:\\secret\\helper.json password=hunter2`},
	}
	for index, response := range invalid {
		if err := ValidateResponse(testNonce, response); err == nil {
			t.Fatalf("invalid response %d was accepted: %#v", index, response)
		}
	}

	unknownBody, err := msgpack.Marshal(map[string]any{
		"version":       ProtocolVersion,
		"nonce":         testNonce,
		"ok":            true,
		"error_code":    "",
		"error_summary": "",
		"environment":   []string{"PASSWORD=hunter2"},
	})
	if err != nil {
		t.Fatalf("marshal response unknown body: %v", err)
	}
	var framed bytes.Buffer
	if err := binary.Write(&framed, binary.BigEndian, uint32(len(unknownBody))); err != nil {
		t.Fatalf("write length: %v", err)
	}
	framed.Write(unknownBody)
	if _, err := DecodeSingleResponseFrame(framed.Bytes(), testNonce); err == nil {
		t.Fatal("unknown response field was accepted")
	}
}
