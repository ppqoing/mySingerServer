package elevation

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	ProtocolVersion uint16 = 1
	MaxPayloadSize         = 256 << 10
	MaxFrameSize           = MaxPayloadSize + 4096
)

type Action string

const (
	ActionWriteHelperConfig Action = "write_helper_config"
	ActionInstallHelperTask Action = "install_helper_task"
	ActionRemoveHelperTask  Action = "remove_helper_task"
)

const (
	ErrorCodeInvalidRequest   = "invalid_request"
	ErrorCodeUnauthorizedPeer = "unauthorized_peer"
	ErrorCodeTimeout          = "timeout"
	ErrorCodeUACCancelled     = "uac_cancelled"
	ErrorCodeWriteFailed      = "write_failed"
	ErrorCodeSaveVerifyFailed = "save_verify_failed"
	ErrorCodeTaskFailed       = "task_failed"
	ErrorCodeUnavailable      = "unavailable"
	ErrorCodeInternal         = "internal_error"
)

type Request struct {
	Version uint16 `msgpack:"version"`
	Nonce   string `msgpack:"nonce"`
	Action  Action `msgpack:"action"`
	Payload []byte `msgpack:"payload"`
}

type Response struct {
	Version      uint16 `msgpack:"version"`
	Nonce        string `msgpack:"nonce"`
	OK           bool   `msgpack:"ok"`
	ErrorCode    string `msgpack:"error_code"`
	ErrorSummary string `msgpack:"error_summary"`
}

type OneShotGuard struct {
	mu       sync.Mutex
	expected string
	used     bool
}

func NewNonce() (string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", errors.New("elevation: generate nonce failed")
	}
	return hex.EncodeToString(value), nil
}

func ValidateNonce(value string) error {
	if len(value) != 64 {
		return errors.New("elevation: nonce must have 64 characters")
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("elevation: nonce must be lowercase hexadecimal")
		}
	}
	return nil
}

func NewOneShotGuard(expected string) (*OneShotGuard, error) {
	if err := ValidateNonce(expected); err != nil {
		return nil, err
	}
	return &OneShotGuard{expected: expected}, nil
}

func (guard *OneShotGuard) Consume(nonce string) error {
	if guard == nil {
		return errors.New("elevation: one-shot guard is required")
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.used {
		return errors.New("elevation: one-shot nonce was already consumed")
	}
	guard.used = true
	if nonce != guard.expected {
		return errors.New("elevation: nonce mismatch")
	}
	return nil
}

func ValidateRequest(request Request) error {
	if request.Version != ProtocolVersion {
		return errors.New("elevation: unsupported protocol version")
	}
	if err := ValidateNonce(request.Nonce); err != nil {
		return err
	}
	switch request.Action {
	case ActionWriteHelperConfig, ActionInstallHelperTask, ActionRemoveHelperTask:
	default:
		return errors.New("elevation: action is not allowed")
	}
	if len(request.Payload) > MaxPayloadSize {
		return errors.New("elevation: payload exceeds limit")
	}
	return nil
}

func ValidateResponse(expectedNonce string, response Response) error {
	if err := ValidateNonce(expectedNonce); err != nil {
		return err
	}
	if response.Version != ProtocolVersion {
		return errors.New("elevation: response version mismatch")
	}
	if response.Nonce != expectedNonce {
		return errors.New("elevation: response nonce mismatch")
	}
	if response.OK {
		if response.ErrorCode != "" || response.ErrorSummary != "" {
			return errors.New("elevation: successful response contains an error")
		}
		return nil
	}
	if !allowedErrorCode(response.ErrorCode) {
		return errors.New("elevation: response error code is not allowed")
	}
	if !safeErrorSummary(response.ErrorSummary) {
		return errors.New("elevation: response error summary is unsafe")
	}
	return nil
}

func allowedErrorCode(value string) bool {
	switch value {
	case ErrorCodeInvalidRequest,
		ErrorCodeUnauthorizedPeer,
		ErrorCodeTimeout,
		ErrorCodeUACCancelled,
		ErrorCodeWriteFailed,
		ErrorCodeSaveVerifyFailed,
		ErrorCodeTaskFailed,
		ErrorCodeUnavailable,
		ErrorCodeInternal:
		return true
	default:
		return false
	}
}

func safeErrorSummary(value string) bool {
	if value == "" || len(value) > 160 || !utf8.ValidString(value) {
		return false
	}
	if strings.ContainsAny(value, `\\/:=@`) || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	lower := strings.ToLower(value)
	for _, secretMarker := range []string{"password", "passwd", "postgres", "dsn", "pgpassword", "secret", "token"} {
		if strings.Contains(lower, secretMarker) {
			return false
		}
	}
	return true
}

func EncodeRequestFrame(request Request) ([]byte, error) {
	if err := ValidateRequest(request); err != nil {
		return nil, err
	}
	return encodeFrame(request)
}

func EncodeResponseFrame(response Response) ([]byte, error) {
	if err := ValidateResponse(response.Nonce, response); err != nil {
		return nil, err
	}
	return encodeFrame(response)
}

func encodeFrame(value any) ([]byte, error) {
	body, err := msgpack.Marshal(value)
	if err != nil {
		return nil, errors.New("elevation: encode message failed")
	}
	if len(body) == 0 || len(body) > MaxFrameSize {
		return nil, errors.New("elevation: encoded frame exceeds limit")
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	return frame, nil
}

func DecodeSingleRequestFrame(frame []byte) (Request, error) {
	var request Request
	if err := decodeSingleFrame(frame, &request); err != nil {
		return Request{}, err
	}
	if err := ValidateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

func DecodeSingleResponseFrame(frame []byte, expectedNonce string) (Response, error) {
	var response Response
	if err := decodeSingleFrame(frame, &response); err != nil {
		return Response{}, err
	}
	if err := ValidateResponse(expectedNonce, response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func decodeSingleFrame(frame []byte, destination any) error {
	if len(frame) < 4 {
		return errors.New("elevation: truncated frame header")
	}
	length := int(binary.BigEndian.Uint32(frame[:4]))
	if length <= 0 || length > MaxFrameSize {
		return errors.New("elevation: invalid frame length")
	}
	if len(frame) != 4+length {
		return errors.New("elevation: truncated or trailing frame data")
	}
	return strictDecodeBody(frame[4:], destination)
}

func ReadRequestFrame(reader io.Reader) (Request, error) {
	body, err := readFrameBody(reader)
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := strictDecodeBody(body, &request); err != nil {
		return Request{}, err
	}
	if err := ValidateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

func ReadResponseFrame(reader io.Reader, expectedNonce string) (Response, error) {
	body, err := readFrameBody(reader)
	if err != nil {
		return Response{}, err
	}
	var response Response
	if err := strictDecodeBody(body, &response); err != nil {
		return Response{}, err
	}
	if err := ValidateResponse(expectedNonce, response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func readFrameBody(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("elevation: reader is required")
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, errors.New("elevation: truncated frame header")
	}
	length := int(binary.BigEndian.Uint32(header))
	if length <= 0 || length > MaxFrameSize {
		return nil, errors.New("elevation: invalid frame length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, errors.New("elevation: truncated frame body")
	}
	return body, nil
}

func WriteRequestFrame(writer io.Writer, request Request) error {
	frame, err := EncodeRequestFrame(request)
	if err != nil {
		return err
	}
	return writeAll(writer, frame)
}

func WriteResponseFrame(writer io.Writer, response Response) error {
	frame, err := EncodeResponseFrame(response)
	if err != nil {
		return err
	}
	return writeAll(writer, frame)
}

func strictDecodeBody(body []byte, destination any) error {
	reader := bytes.NewReader(body)
	decoder := msgpack.NewDecoder(reader)
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return errors.New("elevation: strict message decode failed")
	}
	if reader.Len() != 0 {
		return errors.New("elevation: trailing message data")
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	if writer == nil {
		return errors.New("elevation: writer is required")
	}
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return errors.New("elevation: frame write failed")
		}
		if count <= 0 || count > len(data) {
			return fmt.Errorf("elevation: invalid frame write count")
		}
		data = data[count:]
	}
	return nil
}
