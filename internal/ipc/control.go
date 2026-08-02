//go:build windows

package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
)

// ControlRequest is a single-shot command from the CLI, as opposed to an MCP
// session. The connection carries one request and one response, then closes.
type ControlRequest struct {
	Op string `json:"op"`

	// Fields used by later phases (cred.*) are decoded lazily from Raw so this
	// struct does not have to know about every op.
	Raw json.RawMessage `json:"-"`
}

// ControlResponse is the reply to a ControlRequest.
type ControlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`

	Data any `json:"data,omitempty"`
}

// ControlHandler answers one control request.
type ControlHandler func(ctx context.Context, req ControlRequest) ControlResponse

// ServeControl decodes the request already peeked from conn, runs the handler,
// and writes one JSON line back.
func ServeControl(ctx context.Context, conn net.Conn, firstLine []byte, h ControlHandler) error {
	var req ControlRequest
	if err := json.Unmarshal(firstLine, &req); err != nil {
		return writeJSONLine(conn, ControlResponse{
			OK:    false,
			Code:  "INVALID_ARGUMENT",
			Error: "control frame is not valid JSON",
		})
	}
	req.Raw = append(json.RawMessage(nil), firstLine...)

	return writeJSONLine(conn, h(ctx, req))
}

// SendControl opens a connection, sends one request, and reads one response.
func SendControl(ctx context.Context, pipePath string, req any) (*ControlResponse, error) {
	conn, err := Dial(ctx, pipePath)
	if err != nil {
		return nil, dialDiagnostic(pipePath, err)
	}
	defer conn.Close()

	if err := writeJSONLine(conn, req); err != nil {
		return nil, err
	}

	var resp ControlResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
