package updater

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func testRequest(operation Operation) Request {
	request := Request{Version: ProtocolVersion, ID: "550e8400-e29b-41d4-a716-446655440000", Operation: operation, Source: "cli"}
	if mutatingOperation(operation) {
		request.OperationID = request.ID
	}
	return request
}

func TestRequestValidationRejectsUnknownAndUnsafeInputs(t *testing.T) {
	tests := []Request{
		{Version: ProtocolVersion, ID: testRequest(OperationGetStatus).ID, Operation: "run_shell"},
		{Version: ProtocolVersion, ID: testRequest(OperationRestartComponent).ID, Operation: OperationRestartComponent, OperationID: testRequest(OperationRestartComponent).ID, Component: Component("../../systemd"), Source: "cli"},
		testRequest(OperationRestartComponent),
		{Version: ProtocolVersion, ID: testRequest(OperationInstallComponent).ID, Operation: OperationInstallComponent, OperationID: testRequest(OperationInstallComponent).ID, Component: ComponentCore, Source: "cli"},
	}
	for _, request := range tests {
		if err := request.Validate(); err == nil {
			t.Fatalf("unsafe request was accepted: %+v", request)
		}
	}
}

func TestFrameRoundTripAndUnknownFields(t *testing.T) {
	request := testRequest(OperationGetStatus)
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, request, MaxFrameSize); err != nil {
		t.Fatal(err)
	}
	var decoded Request
	if err := ReadFrame(&buffer, &decoded, MaxFrameSize); err != nil {
		t.Fatal(err)
	}
	if decoded.Fingerprint() != request.Fingerprint() {
		t.Fatal("decoded request differs")
	}
	var unknown bytes.Buffer
	if err := WriteFrame(&unknown, map[string]any{"protocol_version": 1, "request_id": request.ID, "operation": "GetStatus", "path": "/tmp/x"}, MaxFrameSize); err != nil {
		t.Fatal(err)
	}
	if err := ReadFrame(&unknown, &decoded, MaxFrameSize); err == nil {
		t.Fatal("unknown path field was accepted")
	}
}

func TestRequestUsesLaravelCompatibleEnvelope(t *testing.T) {
	request := testRequest(OperationRestartComponent)
	request.Component = ComponentSubs
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	payload, ok := envelope["payload"].(map[string]any)
	if !ok || payload["component_id"] != string(ComponentSubs) || payload["operation_id"] != request.ID {
		t.Fatalf("unexpected payload: %s", encoded)
	}
	for _, forbidden := range []string{"target_release_id", "preflight_token", "url", "path", "command"} {
		if _, exists := payload[forbidden]; exists {
			t.Fatalf("forbidden protocol field %q exists", forbidden)
		}
	}
}

func TestFrameSizeLimit(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, strings.Repeat("x", MaxFrameSize), MaxFrameSize); err == nil {
		t.Fatal("oversized frame was accepted")
	}
	var invalid [4]byte
	binary.BigEndian.PutUint32(invalid[:], MaxFrameSize+1)
	if err := ReadFrame(bytes.NewReader(invalid[:]), &map[string]any{}, MaxFrameSize); err == nil {
		t.Fatal("oversized length prefix was accepted")
	}
}
