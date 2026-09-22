package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

type Client struct {
	SocketPath string
	Timeout    time.Duration
}

func NewClient(socketPath string) (Client, error) {
	if socketPath == "" {
		return Client{}, errors.New("socket path is required")
	}
	return Client{SocketPath: socketPath, Timeout: 30 * time.Second}, nil
}

func (client Client) Call(ctx context.Context, request Request) (Response, error) {
	if err := request.Validate(); err != nil {
		return Response{}, err
	}
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "unix", client.SocketPath)
	if err != nil {
		return Response{}, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(timeout))
	}
	if err := WriteFrame(connection, request, MaxFrameSize); err != nil {
		return Response{}, err
	}
	var response Response
	if err := ReadFrame(connection, &response, MaxResponseSize); err != nil {
		return Response{}, err
	}
	if response.Version != ProtocolVersion {
		return Response{}, fmt.Errorf("unsupported response protocol version %d", response.Version)
	}
	if response.ID != "" && response.ID != request.ID {
		return Response{}, errors.New("response request id does not match")
	}
	if response.Error != nil {
		return response, response.Error
	}
	return response, nil
}

func DecodeResponseData[T any](response Response, destination *T) error {
	if len(response.Data) == 0 {
		return errors.New("response does not contain data")
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("response contains multiple JSON values")
		}
		return err
	}
	return nil
}
