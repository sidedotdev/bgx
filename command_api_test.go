package bgx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandAPIsForLiveSession(t *testing.T) {
	ctx := context.Background()
	id := "commandapi/live"

	if _, err := Run(id, []string{"cat"}, RunSpec{
		Metadata: map[string]string{"api": "live"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { killSession(t, id) })

	infoResult, err := Info(ctx, id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !infoResult.Exists || infoResult.SessionInfo == nil {
		t.Fatalf("Info result = %#v, want existing session", infoResult)
	}
	if !infoResult.Running || infoResult.Metadata["api"] != "live" {
		t.Fatalf("Info session = %#v, want running session with metadata", infoResult.SessionInfo)
	}

	sendResult, err := Send(ctx, id, []byte("command-api"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sendResult.ID != id || !sendResult.Sent {
		t.Fatalf("Send result = %#v, want id %q sent", sendResult, id)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		history, historyErr := History(ctx, id)
		if historyErr == nil && strings.Contains(string(history), "command-api") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("History never contained sent input; history=%q err=%v", history, historyErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	killResult, err := Kill(ctx, id)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killResult.Exists || killResult.SessionInfo == nil {
		t.Fatalf("Kill result = %#v, want existing session", killResult)
	}
	if killResult.Running || !killResult.Killed {
		t.Fatalf("Kill session = %#v, want killed ended session", killResult.SessionInfo)
	}
}

func TestCommandAPIsUseEndedSessionRecords(t *testing.T) {
	ctx := context.Background()
	id := "commandapi/ended"

	if _, err := Run(id, []string{"sh", "-c", "printf ended-output; exit 7"}, RunSpec{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitEnded(t, id)

	infoResult, err := Info(ctx, id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !infoResult.Exists || infoResult.SessionInfo == nil || infoResult.Running {
		t.Fatalf("Info result = %#v, want existing ended session", infoResult)
	}
	if infoResult.ExitCode == nil || *infoResult.ExitCode != 7 {
		t.Fatalf("Info exit code = %v, want 7", infoResult.ExitCode)
	}

	waitResult, err := Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waitResult.Info == nil || waitResult.ExitCode != 7 {
		t.Fatalf("Wait result = %#v, want exit code 7 with session info", waitResult)
	}

	killResult, err := Kill(ctx, id)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killResult.Exists || killResult.SessionInfo == nil || killResult.Running {
		t.Fatalf("Kill result = %#v, want existing ended session", killResult)
	}

	history, err := History(ctx, id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !strings.Contains(string(history), "ended-output") {
		t.Fatalf("History = %q, want ended-output", history)
	}
}

func TestCommandAPIsForMissingSession(t *testing.T) {
	ctx := context.Background()
	id := "commandapi/missing"

	infoResult, err := Info(ctx, id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if infoResult.Exists || infoResult.SessionInfo != nil {
		t.Fatalf("Info result = %#v, want non-existing session", infoResult)
	}

	operations := []struct {
		name string
		call func() error
	}{
		{name: "Wait", call: func() error {
			_, err := Wait(ctx, id)
			return err
		}},
		{name: "Kill", call: func() error {
			_, err := Kill(ctx, id)
			return err
		}},
		{name: "Send", call: func() error {
			_, err := Send(ctx, id, []byte("input"))
			return err
		}},
		{name: "History", call: func() error {
			_, err := History(ctx, id)
			return err
		}},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.call()
			var notFound *SessionNotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("%s error = %T %v, want *SessionNotFoundError", operation.name, err, err)
			}
			if notFound.ID != id {
				t.Fatalf("%s error id = %q, want %q", operation.name, notFound.ID, id)
			}
		})
	}
}

func TestListAndVersionAPIs(t *testing.T) {
	keepID := "commandapi/list-keep"
	dropID := "commandapi/list-drop"

	if _, err := Run(keepID, []string{"sleep", "30"}, RunSpec{
		Metadata: map[string]string{"group": "keep"},
	}); err != nil {
		t.Fatalf("Run keep session: %v", err)
	}
	t.Cleanup(func() { killSession(t, keepID) })

	if _, err := Run(dropID, []string{"sh", "-c", "exit 0"}, RunSpec{
		Metadata: map[string]string{"group": "drop"},
	}); err != nil {
		t.Fatalf("Run drop session: %v", err)
	}
	waitEnded(t, dropID)

	sessions := List(ListOptions{Metadata: map[string]string{"group": "keep"}})
	if _, ok := findSession(sessions, keepID); !ok {
		t.Fatalf("List missing matching session %q: %#v", keepID, sessions)
	}
	if _, ok := findSession(sessions, dropID); ok {
		t.Fatalf("List included non-matching session %q: %#v", dropID, sessions)
	}

	versionInfo := Version()
	if versionInfo.Version == "" {
		t.Fatal("Version result has an empty version")
	}
	if versionInfo.SocketDir == "" {
		t.Fatal("Version result has an empty socket directory")
	}
	if versionInfo.RetentionDir == "" {
		t.Fatal("Version result has an empty retention directory")
	}
}
func TestCommandAPIsPreserveContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := "commandapi/canceled"

	operations := []struct {
		name string
		call func() error
	}{
		{name: "Info", call: func() error {
			_, err := Info(ctx, id)
			return err
		}},
		{name: "Wait", call: func() error {
			_, err := Wait(ctx, id)
			return err
		}},
		{name: "Kill", call: func() error {
			_, err := Kill(ctx, id)
			return err
		}},
		{name: "Send", call: func() error {
			_, err := Send(ctx, id, []byte("input"))
			return err
		}},
		{name: "History", call: func() error {
			_, err := History(ctx, id)
			return err
		}},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s error = %T %v, want context.Canceled", operation.name, err, err)
			}
		})
	}
}

func TestSendPreservesDaemonResponseError(t *testing.T) {
	id := "commandapi/response-error"
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}

	path := socketPath(id)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove stale socket: %v", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove socket: %v", err)
		}
	})

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}

		if _, readErr := bufio.NewReader(conn).ReadBytes('\n'); readErr != nil {
			serverErr <- errors.Join(readErr, conn.Close())
			return
		}
		_, writeErr := fmt.Fprintln(conn, `{"ok":false,"error":"permission denied"}`)
		serverErr <- errors.Join(writeErr, conn.Close())
	}()

	_, err = Send(context.Background(), id, []byte("input"))
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("Send error = %T %v, want *ResponseError", err, err)
	}
	if responseErr.Operation != "send" || responseErr.Message != "permission denied" {
		t.Fatalf("ResponseError = %#v", responseErr)
	}
	if serverErr := <-serverErr; serverErr != nil {
		t.Fatalf("fake daemon: %v", serverErr)
	}
}

func TestSessionNotFoundErrorWrapsDialCause(t *testing.T) {
	_, err := Send(context.Background(), "commandapi/wrapped-missing", nil)

	var notFound *SessionNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Send error = %T %v, want *SessionNotFoundError", err, err)
	}
	if notFound.Operation != "send" {
		t.Fatalf("SessionNotFoundError operation = %q, want send", notFound.Operation)
	}
	if errors.Unwrap(notFound) == nil {
		t.Fatal("SessionNotFoundError does not wrap its dial cause")
	}
}
func TestWaitPreservesContextErrorWhileAwaitingEndedRecord(t *testing.T) {
	id := "commandapi/wait-canceled"
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}

	path := socketPath(id)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove stale socket: %v", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove socket: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err = Wait(ctx, id)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %T %v, want context.DeadlineExceeded", err, err)
	}
}

func TestIsSessionUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "EOF", err: fmt.Errorf("decode response: %w", io.EOF), want: true},
		{name: "broken pipe", err: fmt.Errorf("write request: %w", syscall.EPIPE), want: true},
		{name: "connection reset", err: fmt.Errorf("read response: %w", syscall.ECONNRESET), want: true},
		{name: "other error", err: errors.New("invalid response"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isSessionUnavailable(test.err); got != test.want {
				t.Fatalf("isSessionUnavailable(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}
