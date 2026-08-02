package bgx_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	bgx "github.com/sidedotdev/bgx"
)

type scriptedDialer struct {
	t         *testing.T
	mu        sync.Mutex
	responses []string
	requests  []map[string]any
	closed    []<-chan struct{}
}

func (d *scriptedDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	if len(d.responses) == 0 {
		d.mu.Unlock()
		return nil, errors.New("unexpected dial")
	}
	response := d.responses[0]
	d.responses = d.responses[1:]
	d.mu.Unlock()

	client, server := net.Pipe()
	closed := make(chan struct{})
	d.mu.Lock()
	d.closed = append(d.closed, closed)
	d.mu.Unlock()

	go func() {
		defer close(closed)
		defer server.Close()

		line, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			d.t.Errorf("read request: %v", err)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(line, &request); err != nil {
			d.t.Errorf("decode request: %v", err)
			return
		}
		d.mu.Lock()
		d.requests = append(d.requests, request)
		d.mu.Unlock()

		if _, err := io.WriteString(server, response); err != nil {
			d.t.Errorf("write response: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, server)
	}()

	return client, nil
}

func (d *scriptedDialer) snapshot() ([]map[string]any, []<-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.requests...), append([]<-chan struct{}(nil), d.closed...)
}

func waitForClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("client did not close operation stream")
	}
}

func TestClientOperationsUseFreshStreams(t *testing.T) {
	dialer := &scriptedDialer{
		t: t,
		responses: []string{
			"{\"ok\":true,\"info\":{\"id\":\"job\",\"running\":true}}\n",
			"{\"ok\":true,\"info\":{\"id\":\"job\",\"running\":false},\"exit_code\":7}\n",
			"{\"ok\":true,\"info\":{\"id\":\"job\",\"running\":false},\"exit_code\":137}\n",
			"{\"ok\":true}\n",
			"{\"ok\":true,\"history\":\"aGVsbG8A/w==\"}\n",
		},
	}
	client := bgx.NewClient(dialer.Dial)
	ctx := context.Background()

	info, err := client.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ID != "job" || !info.Running {
		t.Fatalf("Info = %#v", info)
	}

	wait, err := client.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if wait.ExitCode != 7 || wait.Info == nil || wait.Info.ID != "job" {
		t.Fatalf("Wait = %#v", wait)
	}

	killed, err := client.Kill(ctx)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if killed.ExitCode != 137 || killed.Info == nil || killed.Info.ID != "job" {
		t.Fatalf("Kill = %#v", killed)
	}

	if err := client.Send(ctx, []byte{0, 1, 0xff}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	history, err := client.History(ctx)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if string(history) != "hello\x00\xff" {
		t.Fatalf("History = %q", history)
	}

	requests, streams := dialer.snapshot()
	if len(requests) != 5 {
		t.Fatalf("requests = %d, want 5", len(requests))
	}
	wantOps := []string{"info", "wait", "kill", "send", "history"}
	for i, want := range wantOps {
		if got := requests[i]["op"]; got != want {
			t.Errorf("request %d op = %#v, want %q", i, got, want)
		}
		waitForClosed(t, streams[i])
	}
	if got := requests[3]["input"]; got != "AAH/" {
		t.Errorf("send input = %#v, want base64 payload", got)
	}
}

func TestClientReturnsDialFailure(t *testing.T) {
	want := errors.New("transport unavailable")
	client := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
		return nil, want
	})

	_, err := client.Info(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Info error = %v, want wrapped dial failure", err)
	}
}

func TestClientReturnsResponseError(t *testing.T) {
	dialer := &scriptedDialer{
		t:         t,
		responses: []string{"{\"ok\":false,\"error\":\"permission denied\"}\n"},
	}
	client := bgx.NewClient(dialer.Dial)

	err := client.Send(context.Background(), []byte("input"))
	var responseErr *bgx.ResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("Send error = %T %v, want *bgx.ResponseError", err, err)
	}
	if responseErr.Operation != "send" || responseErr.Message != "permission denied" {
		t.Fatalf("ResponseError = %#v", responseErr)
	}
}

func TestClientRejectsMalformedAndIncompleteResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		call     func(*bgx.Client) error
	}{
		{
			name:     "malformed JSON",
			response: "not-json\n",
			call: func(client *bgx.Client) error {
				_, err := client.Info(context.Background())
				return err
			},
		},
		{
			name:     "info missing result",
			response: "{\"ok\":true}\n",
			call: func(client *bgx.Client) error {
				_, err := client.Info(context.Background())
				return err
			},
		},
		{
			name:     "wait missing exit code",
			response: "{\"ok\":true,\"info\":{\"id\":\"job\"}}\n",
			call: func(client *bgx.Client) error {
				_, err := client.Wait(context.Background())
				return err
			},
		},
		{
			name:     "kill missing info",
			response: "{\"ok\":true,\"exit_code\":137}\n",
			call: func(client *bgx.Client) error {
				_, err := client.Kill(context.Background())
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer := &scriptedDialer{t: t, responses: []string{tt.response}}
			err := tt.call(bgx.NewClient(dialer.Dial))
			var protocolErr *bgx.ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("error = %T %v, want *bgx.ProtocolError", err, err)
			}
		})
	}
}
func TestClientCancellationClosesOperationStream(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	dialed := make(chan struct{})
	peerClosed := make(chan struct{})
	client := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
		close(dialed)
		return clientConn, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Info(ctx)
		result <- err
	}()

	<-dialed
	if _, err := bufio.NewReader(serverConn).ReadBytes('\n'); err != nil {
		t.Fatalf("read request: %v", err)
	}
	go func() {
		_, _ = io.Copy(io.Discard, serverConn)
		close(peerClosed)
	}()

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Info error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Info did not return after context cancellation")
	}
	select {
	case <-peerClosed:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not close operation stream")
	}
}

func TestClientClosesStreamOnResponseFailure(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{name: "malformed", response: "not-json\n"},
		{name: "daemon error", response: "{\"ok\":false,\"error\":\"denied\"}\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer := &scriptedDialer{t: t, responses: []string{tt.response}}
			client := bgx.NewClient(dialer.Dial)

			if _, err := client.Info(context.Background()); err == nil {
				t.Fatal("Info returned nil error")
			}

			_, streams := dialer.snapshot()
			if len(streams) != 1 {
				t.Fatalf("streams = %d, want 1", len(streams))
			}
			waitForClosed(t, streams[0])
		})
	}
}

func TestClientRejectsNilDialerStream(t *testing.T) {
	client := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
		return nil, nil
	})

	_, err := client.Info(context.Background())
	if err == nil {
		t.Fatal("Info returned nil error")
	}
	var protocolErr *bgx.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("Info error = %T %v, want *bgx.ProtocolError", err, err)
	}
}
