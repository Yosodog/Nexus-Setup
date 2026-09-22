package updater

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// Request is the only data accepted by the privileged Unix socket. Its fields
// are deliberately identifiers and enums, never commands, paths, URLs, unit
// names, or environment variables.
type Request struct {
	Version       uint16            `json:"-"`
	ID            string            `json:"-"`
	Operation     Operation         `json:"-"`
	Component     Component         `json:"-"`
	OperationID   string            `json:"-"`
	Source        string            `json:"-"`
	Configuration map[string]string `json:"-"`
}

// Response is a bounded, sanitized protocol response. Data is produced by
// fixed handlers and never contains command output or secrets.
type Response struct {
	Version     uint16          `json:"-"`
	ID          string          `json:"-"`
	Accepted    bool            `json:"-"`
	OperationID string          `json:"-"`
	Data        json.RawMessage `json:"-"`
	Error       *ProtocolError  `json:"-"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type requestEnvelope struct {
	ProtocolVersion uint16          `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	Operation       Operation       `json:"operation"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

type requestPayload struct {
	Component     Component         `json:"component_id,omitempty"`
	OperationID   string            `json:"operation_id,omitempty"`
	Source        string            `json:"source,omitempty"`
	Configuration map[string]string `json:"configuration,omitempty"`
}

func (request Request) MarshalJSON() ([]byte, error) {
	payload := requestPayload{
		Component:     request.Component,
		OperationID:   request.OperationID,
		Source:        request.Source,
		Configuration: request.Configuration,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(requestEnvelope{
		ProtocolVersion: request.Version,
		RequestID:       request.ID,
		Operation:       request.Operation,
		Payload:         payloadBytes,
	})
}

func (request *Request) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope requestEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("request contains multiple JSON values")
	}
	request.Version = envelope.ProtocolVersion
	request.ID = envelope.RequestID
	request.Operation = envelope.Operation
	if len(envelope.Payload) == 0 {
		return nil
	}
	payloadDecoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	payloadDecoder.DisallowUnknownFields()
	var payload requestPayload
	if err := payloadDecoder.Decode(&payload); err != nil {
		return err
	}
	if err := payloadDecoder.Decode(&trailing); err != io.EOF {
		return errors.New("request payload contains multiple JSON values")
	}
	request.Component = payload.Component
	request.OperationID = payload.OperationID
	request.Source = payload.Source
	request.Configuration = payload.Configuration
	return nil
}

type responseEnvelope struct {
	ProtocolVersion uint16          `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	OK              bool            `json:"ok"`
	Data            json.RawMessage `json:"data"`
	ErrorCode       string          `json:"error_code,omitempty"`
	ErrorMessage    string          `json:"error_message,omitempty"`
}

func (response Response) MarshalJSON() ([]byte, error) {
	data := response.Data
	if len(data) == 0 {
		if response.Accepted {
			data, _ = json.Marshal(map[string]any{"operation_id": response.OperationID, "status": "accepted"})
		} else {
			data = []byte("{}")
		}
	}
	envelope := responseEnvelope{ProtocolVersion: response.Version, RequestID: response.ID, OK: response.Error == nil, Data: data}
	if response.Error != nil {
		envelope.ErrorCode = response.Error.Code
		envelope.ErrorMessage = response.Error.Message
	}
	return json.Marshal(envelope)
}

func (response *Response) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope responseEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("response contains multiple JSON values")
	}
	response.Version = envelope.ProtocolVersion
	response.ID = envelope.RequestID
	response.Data = envelope.Data
	response.Accepted = false
	var payloadMap map[string]any
	if len(envelope.Data) > 0 && json.Unmarshal(envelope.Data, &payloadMap) == nil {
		if status, ok := payloadMap["status"].(string); ok && status == "accepted" {
			response.Accepted = true
		}
		if operationID, ok := payloadMap["operation_id"].(string); ok {
			response.OperationID = operationID
		}
	}
	if !envelope.OK {
		response.Error = &ProtocolError{Code: envelope.ErrorCode, Message: envelope.ErrorMessage}
	}
	return nil
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// Validate enforces the protocol boundary and operation-specific fields.
func (r Request) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", r.Version)
	}
	if !uuidPattern.MatchString(r.ID) || strings.ToLower(r.ID) != r.ID {
		return errors.New("request id must be a canonical UUID")
	}
	if !validOperation(r.Operation) {
		return fmt.Errorf("unsupported operation %q", r.Operation)
	}
	if r.Component != "" && !validComponent(r.Component) {
		return fmt.Errorf("unsupported component %q", r.Component)
	}
	if r.Source != "cli" && r.Source != "gui" {
		return errors.New("source must be cli or gui")
	}

	if componentOperation(r.Operation) && r.Component == "" {
		return errors.New("component is required for this operation")
	}
	if !componentOperation(r.Operation) && r.Component != "" {
		return errors.New("component is not accepted for this operation")
	}
	if err := validateComponentConfiguration(r.Operation, r.Component, r.Configuration); err != nil {
		return err
	}
	if r.Operation == OperationGetOperation && (!uuidPattern.MatchString(r.OperationID) || strings.ToLower(r.OperationID) != r.OperationID) {
		return errors.New("operation id must be a canonical UUID")
	}
	if mutatingOperation(r.Operation) && (!uuidPattern.MatchString(r.OperationID) || strings.ToLower(r.OperationID) != r.OperationID) {
		return errors.New("operation id must be a canonical UUID for mutating operations")
	}
	if r.Operation != OperationGetOperation && !mutatingOperation(r.Operation) && r.OperationID != "" {
		return errors.New("operation id is not accepted for this operation")
	}
	if r.Component == ComponentCore && r.Operation == OperationInstallComponent {
		return errors.New("Core is installed by nexus install and updated by nexus update")
	}
	if (r.Operation == OperationEnableComponent || r.Operation == OperationDisableComponent || r.Operation == OperationRestartComponent) && r.Component == ComponentCore {
		if r.Operation != OperationRestartComponent {
			return errors.New("Core cannot be enabled or disabled as a component")
		}
	}

	return nil
}

// Fingerprint returns a stable digest of the safe request fields. It is used
// to make retries idempotent without persisting arbitrary request input.
func (r Request) Fingerprint() string {
	b, _ := json.Marshal(struct {
		Operation           Operation
		Component           Component
		OperationID         string
		Source              string
		ConfigurationDigest string
	}{
		Operation:           r.Operation,
		Component:           r.Component,
		OperationID:         r.OperationID,
		Source:              r.Source,
		ConfigurationDigest: configurationDigest(r.Configuration),
	})
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func configurationDigest(configuration map[string]string) string {
	if len(configuration) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(configuration)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validateComponentConfiguration(operation Operation, component Component, configuration map[string]string) error {
	acceptsConfiguration := operation == OperationInstallComponent
	if !acceptsConfiguration && len(configuration) > 0 {
		return errors.New("configuration is accepted only for component installation")
	}
	if !acceptsConfiguration {
		return nil
	}
	if len(configuration) > 8 {
		return errors.New("component configuration contains too many fields")
	}

	allowed := map[string]bool{}
	switch component {
	case ComponentSubs:
		if len(configuration) != 0 {
			return errors.New("local Subs installation reuses Core configuration")
		}
		return nil
	case ComponentDiscord:
		allowed["bot_token"] = true
		allowed["client_id"] = true
		allowed["guild_id"] = true
		for _, field := range []string{"bot_token", "client_id", "guild_id"} {
			if configuration[field] == "" {
				return fmt.Errorf("Discord component configuration field %q is required", field)
			}
		}
	default:
		return errors.New("component does not accept installation configuration")
	}

	total := 0
	for key, value := range configuration {
		if !allowed[key] {
			return fmt.Errorf("unsupported component configuration field %q", key)
		}
		if value == "" || len(value) > 512 || strings.IndexFunc(value, func(character rune) bool {
			return character < 0x20 || character == 0x7f
		}) >= 0 {
			return fmt.Errorf("component configuration field %q is invalid", key)
		}
		total += len(value)
	}
	if total > 48*1024 {
		return errors.New("component configuration is too large")
	}
	if clientID := configuration["client_id"]; clientID != "" && !regexp.MustCompile(`^[0-9]{17,20}$`).MatchString(clientID) {
		return errors.New("Discord client id is invalid")
	}
	if guildID := configuration["guild_id"]; guildID != "" && !regexp.MustCompile(`^[0-9]{17,20}$`).MatchString(guildID) {
		return errors.New("Discord guild id is invalid")
	}
	return nil
}

func (r Request) IdempotencyID() string {
	if mutatingOperation(r.Operation) {
		return r.OperationID
	}
	return r.ID
}

func encodeJSON(value any, max int) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, fmt.Errorf("encoded message exceeds %d bytes", max)
	}
	return b, nil
}

// WriteFrame writes exactly one big-endian length-prefixed JSON message.
func WriteFrame(w io.Writer, value any, max int) error {
	if max <= 0 || max > MaxTransportFrameSize {
		return errors.New("invalid frame limit")
	}
	b, err := encodeJSON(value, max)
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b) > max {
		return errors.New("invalid frame size")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(b)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one bounded JSON message and rejects trailing JSON values.
func ReadFrame(r io.Reader, destination any, max int) error {
	if max <= 0 || max > MaxTransportFrameSize {
		return errors.New("invalid frame limit")
	}
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 || length > uint32(max) {
		return fmt.Errorf("invalid frame length %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values in frame")
		}
		return err
	}
	return nil
}

func serveRequest(conn net.Conn, handler func(Request) Response) error {
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReaderSize(conn, MaxFrameSize+4)
	var request Request
	if err := ReadFrame(reader, &request, MaxFrameSize); err != nil {
		return WriteFrame(conn, Response{Version: ProtocolVersion, Error: &ProtocolError{Code: "invalid_request", Message: safeProtocolMessage(err)}}, MaxResponseSize)
	}
	response := handler(request)
	return WriteFrame(conn, response, MaxResponseSize)
}

func safeProtocolMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 256 {
		return message[:256]
	}
	return message
}
