//go:build windows

package bgx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/daemon"
	"golang.org/x/sys/windows"
)

func TestProcessTerminalReadContextCancellationWithBlockedInput(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create input pipe: %v", err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close input pipe: %v", err)
		}
		if err := writer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close input writer: %v", err)
		}
	})

	terminal := &ProcessTerminal{in: input}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := terminal.ReadContext(ctx, make([]byte, 1))
		result <- err
	}()

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadContext error = %v, want context.Canceled", err)
		}
		if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			t.Fatalf("ReadContext error = %v, want operation-aborted detail normalized", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadContext did not return after cancellation")
	}
}

func TestClientAttachSessionEndCancelsBlockedProcessTerminalInput(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create input pipe: %v", err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close input pipe: %v", err)
		}
		if err := writer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close input writer: %v", err)
		}
	})

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}

	serverReady := make(chan struct{})
	serverErr := make(chan error, 1)
	sendEnded := make(chan struct{})
	go func() {
		br := bufio.NewReader(serverConn)
		if _, err := br.ReadBytes('\n'); err != nil {
			serverErr <- err
			return
		}
		if err := json.NewEncoder(serverConn).Encode(map[string]any{"ok": true}); err != nil {
			serverErr <- err
			return
		}
		close(serverReady)
		<-sendEnded
		serverErr <- daemon.WriteFrame(serverConn, daemon.FrameEnded, nil)
	}()

	terminal := &ProcessTerminal{in: input, out: &bytes.Buffer{}}
	result := make(chan error, 1)
	go func() {
		result <- NewClient(dial).Attach(context.Background(), terminal)
	}()

	select {
	case <-serverReady:
	case <-time.After(time.Second):
		t.Fatal("attach handshake did not complete")
	}
	close(sendEnded)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Attach after session end: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after session end")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("attach server: %v", err)
	}
}

func TestWithoutExpectedWindowsCancellationErrorRetainsUnexpectedSibling(t *testing.T) {
	unexpected := errors.New("read cleanup failed")
	err := withoutExpectedWindowsCancellationError(errors.Join(
		windows.ERROR_OPERATION_ABORTED,
		unexpected,
	))

	if !errors.Is(err, unexpected) {
		t.Fatalf("pruned error = %v, want unexpected sibling", err)
	}
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		t.Fatalf("pruned error = %v, want operation-aborted leaf removed", err)
	}
}
