package bgx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/sidedotdev/bgx/daemon"
)

// Dialer opens a fresh stream for one client operation.
type Dialer func(context.Context) (io.ReadWriteCloser, error)

// Info describes a session at a point in time.
type Info = daemon.Info

// ExitResult is returned by operations that wait for a session to exit.
type ExitResult struct {
	Info     *Info
	ExitCode int
}

// ResponseError reports an error returned by the session daemon.
type ResponseError struct {
	Operation string
	Message   string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Operation, e.Message)
}

// ProtocolError reports a malformed or incomplete daemon response.
type ProtocolError struct {
	Operation string
	Err       error
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%s: invalid response: %v", e.Operation, e.Err)
}

func (e *ProtocolError) Unwrap() error {
	return e.Err
}

// Client performs one operation per stream obtained from its Dialer.
type Client struct {
	dial Dialer
}

// NewClient creates a client that obtains a fresh stream for every operation.
func NewClient(dialer Dialer) *Client {
	return &Client{dial: dialer}
}

// Info returns the current session information.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	resp, err := c.request(ctx, daemon.Request{Op: "info"})
	if err != nil {
		return nil, err
	}
	if resp.Info == nil {
		return nil, missingResponseField("info", "info")
	}
	return resp.Info, nil
}

// Wait waits for the session to exit.
func (c *Client) Wait(ctx context.Context) (*ExitResult, error) {
	return c.exitOperation(ctx, "wait")
}

// Kill terminates the session and waits for it to exit.
func (c *Client) Kill(ctx context.Context) (*ExitResult, error) {
	return c.exitOperation(ctx, "kill")
}

// Send writes raw bytes to the session PTY.
func (c *Client) Send(ctx context.Context, input []byte) error {
	_, err := c.request(ctx, daemon.Request{Op: "send", Input: input})
	return err
}

// History returns the session's raw scrollback bytes.
func (c *Client) History(ctx context.Context) ([]byte, error) {
	resp, err := c.request(ctx, daemon.Request{Op: "history"})
	if err != nil {
		return nil, err
	}
	return resp.History, nil
}

func (c *Client) exitOperation(ctx context.Context, operation string) (*ExitResult, error) {
	resp, err := c.request(ctx, daemon.Request{Op: operation})
	if err != nil {
		return nil, err
	}
	if resp.Info == nil {
		return nil, missingResponseField(operation, "info")
	}
	if resp.ExitCode == nil {
		return nil, missingResponseField(operation, "exit_code")
	}
	return &ExitResult{Info: resp.Info, ExitCode: *resp.ExitCode}, nil
}

func (c *Client) request(ctx context.Context, req daemon.Request) (resp daemon.Response, retErr error) {
	if c == nil || c.dial == nil {
		return daemon.Response{}, errors.New("bgx: client has no dialer")
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return daemon.Response{}, fmt.Errorf("%s: dial: %w", req.Op, err)
	}
	if conn == nil {
		return daemon.Response{}, &ProtocolError{
			Operation: req.Op,
			Err:       errors.New("dialer returned a nil stream"),
		}
	}

	var closeOnce sync.Once
	var closeMu sync.Mutex
	var closeErr error
	closeConn := func() {
		closeOnce.Do(func() {
			err := conn.Close()
			closeMu.Lock()
			closeErr = err
			closeMu.Unlock()
		})
	}
	stopCancellation := context.AfterFunc(ctx, closeConn)
	defer func() {
		stopCancellation()
		closeConn()
		closeMu.Lock()
		retErr = errors.Join(retErr, closeErr)
		closeMu.Unlock()
	}()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return daemon.Response{}, ctxErr
		}
		return daemon.Response{}, &ProtocolError{
			Operation: req.Op,
			Err:       fmt.Errorf("write request: %w", err),
		}
	}

	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return daemon.Response{}, ctxErr
		}
		return daemon.Response{}, &ProtocolError{
			Operation: req.Op,
			Err:       fmt.Errorf("decode response: %w", err),
		}
	}
	if !resp.OK {
		return daemon.Response{}, &ResponseError{
			Operation: req.Op,
			Message:   resp.Error,
		}
	}
	return resp, nil
}

func missingResponseField(operation, field string) error {
	return &ProtocolError{
		Operation: operation,
		Err:       fmt.Errorf("missing %s", field),
	}
}
