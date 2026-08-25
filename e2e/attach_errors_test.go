package e2e

// These tests exercise attach's error contract against teardown races by
// standing in for a session daemon on its unix socket, since a real session
// cannot deterministically disappear between the liveness check and the attach
// dial or handshake.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sidedotdev/bgx/daemon"
)

// fakeSessionSocket listens where the CLI expects the session's daemon socket
// for id inside dir's isolated environment.
func fakeSessionSocket(t *testing.T, dir, id string) (net.Listener, string) {
	t.Helper()
	sockDir := filepath.Join(dir, "bgx", "run")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("make socket dir: %v", err)
	}
	sockPath := filepath.Join(sockDir, id+".sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close fake session listener: %v", err)
		}
	})
	return ln, sockPath
}

// readRequest consumes one JSON request line from an accepted connection.
func readRequest(t *testing.T, br *bufio.Reader) (daemon.Request, bool) {
	t.Helper()
	line, err := br.ReadBytes('\n')
	if err != nil {
		return daemon.Request{}, false
	}
	var req daemon.Request
	if err := json.Unmarshal(line, &req); err != nil {
		t.Errorf("decode request %q: %v", line, err)
		return daemon.Request{}, false
	}
	return req, true
}

func TestAttachDialFailureAfterLivenessCheckReportsNotFound(t *testing.T) {
	dir := runDir(t)
	const id = "phantom"
	ln, sockPath := fakeSessionSocket(t, dir, id)

	served := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			served <- fmt.Errorf("accept info request: %w", err)
			return
		}

		var serveErr error
		defer func() {
			served <- errors.Join(serveErr, conn.Close())
		}()

		req, ok := readRequest(t, bufio.NewReader(conn))
		if !ok || req.Op != "info" {
			serveErr = fmt.Errorf("first request = %+v, want info", req)
			return
		}
		// Unlink the socket before answering so the attach dial that follows
		// the liveness check deterministically fails.
		if err := ln.Close(); err != nil {
			serveErr = fmt.Errorf("close listener before response: %w", err)
			return
		}
		if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			serveErr = fmt.Errorf("remove socket before response: %w", err)
			return
		}
		if err := json.NewEncoder(conn).Encode(daemon.Response{
			OK:   true,
			Info: &daemon.Info{ID: id, Running: true},
		}); err != nil {
			serveErr = fmt.Errorf("encode info response: %w", err)
		}
	}()

	res := bgxIn(t, dir, "attach", id)
	if err := <-served; err != nil {
		t.Fatalf("serve fake session: %v", err)
	}
	if res.exitCode == 0 {
		t.Fatalf("attach succeeded despite dial failure; stdout=%q", res.stdout)
	}
	if res.stdout != "" {
		t.Fatalf("attach error leaked to stdout: %q", res.stdout)
	}
	errObj := decodeJSON(t, res.stderr)
	if errObj["code"] != "session_not_found" {
		t.Fatalf("code = %v, want session_not_found; stderr=%q", errObj["code"], res.stderr)
	}
	msg, ok := errObj["error"].(string)
	if !ok {
		t.Fatalf("error = %T, want string; stderr=%q", errObj["error"], res.stderr)
	}
	if !strings.Contains(msg, "does not exist") {
		t.Fatalf("error = %q, want mention of not existing", msg)
	}
}

func TestAttachNegativeHandshakeReportsAttachFailed(t *testing.T) {
	dir := runDir(t)
	const id = "closing"
	ln, _ := fakeSessionSocket(t, dir, id)

	served := make(chan error, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				served <- fmt.Errorf("accept request: %w", err)
				continue
			}
			go func(conn net.Conn) {
				var serveErr error
				defer func() {
					served <- errors.Join(serveErr, conn.Close())
				}()

				req, ok := readRequest(t, bufio.NewReader(conn))
				if !ok {
					serveErr = errors.New("read request")
					return
				}
				switch req.Op {
				case "info":
					if err := json.NewEncoder(conn).Encode(daemon.Response{
						OK:   true,
						Info: &daemon.Info{ID: id, Running: true},
					}); err != nil {
						serveErr = fmt.Errorf("encode info response: %w", err)
					}
				case "attach":
					if err := json.NewEncoder(conn).Encode(daemon.Response{
						OK:    false,
						Error: "session has ended",
					}); err != nil {
						serveErr = fmt.Errorf("encode attach response: %w", err)
					}
				default:
					serveErr = fmt.Errorf("unexpected request op %q", req.Op)
				}
			}(conn)
		}
	}()

	res := bgxIn(t, dir, "attach", id)
	for i := 0; i < 2; i++ {
		if err := <-served; err != nil {
			t.Fatalf("serve fake session: %v", err)
		}
	}
	if res.exitCode == 0 {
		t.Fatalf("attach succeeded despite refused handshake; stdout=%q", res.stdout)
	}
	if res.stdout != "" {
		t.Fatalf("attach error leaked to stdout: %q", res.stdout)
	}
	errObj := decodeJSON(t, res.stderr)
	if errObj["code"] != "attach_failed" {
		t.Fatalf("code = %v, want attach_failed; stderr=%q", errObj["code"], res.stderr)
	}
	msg, ok := errObj["error"].(string)
	if !ok {
		t.Fatalf("error = %T, want string; stderr=%q", errObj["error"], res.stderr)
	}
	if !strings.Contains(msg, "session has ended") {
		t.Fatalf("error = %q, want the daemon's handshake message", msg)
	}
}
func TestAttachDisconnectClearsDetachInstructionsWithoutReset(t *testing.T) {
	dir := runDir(t)
	const id = "disconnected"
	ln, _ := fakeSessionSocket(t, dir, id)

	disconnect := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		infoConn, err := ln.Accept()
		if err != nil {
			served <- fmt.Errorf("accept info request: %w", err)
			return
		}
		req, ok := readRequest(t, bufio.NewReader(infoConn))
		if !ok || req.Op != "info" {
			served <- errors.Join(
				fmt.Errorf("first request = %+v, want info", req),
				infoConn.Close(),
			)
			return
		}
		if err := json.NewEncoder(infoConn).Encode(daemon.Response{
			OK:   true,
			Info: &daemon.Info{ID: id, Running: true},
		}); err != nil {
			served <- errors.Join(fmt.Errorf("encode info response: %w", err), infoConn.Close())
			return
		}
		if err := infoConn.Close(); err != nil {
			served <- fmt.Errorf("close info connection: %w", err)
			return
		}

		attachConn, err := ln.Accept()
		if err != nil {
			served <- fmt.Errorf("accept attach request: %w", err)
			return
		}
		var serveErr error
		defer func() {
			served <- errors.Join(serveErr, attachConn.Close())
		}()

		req, ok = readRequest(t, bufio.NewReader(attachConn))
		if !ok || req.Op != "attach" {
			serveErr = fmt.Errorf("second request = %+v, want attach", req)
			return
		}
		if err := json.NewEncoder(attachConn).Encode(daemon.Response{OK: true}); err != nil {
			serveErr = fmt.Errorf("encode attach response: %w", err)
			return
		}
		if err := daemon.WriteFrame(attachConn, daemon.FrameOutput, []byte("connected")); err != nil {
			serveErr = fmt.Errorf("write attach output: %w", err)
			return
		}

		<-disconnect
	}()

	c := startAttachE2EClient(
		t,
		dir,
		id,
		&pty.Winsize{Rows: 12, Cols: 40},
		"--show-detach-instructions",
	)
	defer closePTY(t, c.ptmx)

	c.waitFor(t, "connected")
	c.waitFor(t, "detach: ctrl+\\")

	close(disconnect)
	select {
	case <-c.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not exit when the daemon disconnected")
	}
	select {
	case err := <-c.readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
	if err := c.cmd.Wait(); err == nil {
		t.Fatal("attach client succeeded despite daemon disconnect")
	}
	if err := <-served; err != nil {
		t.Fatalf("serve fake session: %v", err)
	}

	out := c.output()
	if strings.Contains(out, "\x1bc") {
		t.Fatalf("daemon disconnect performed a full terminal reset; got %q", out)
	}
	if !strings.Contains(out, "\x1b[?25h\x1b[0m") {
		t.Fatalf("daemon disconnect did not reset the cursor and style; got %q", out)
	}
	if !strings.Contains(out, "\r\nDisconnected from session\r\n") {
		t.Fatalf("daemon disconnect did not print its outcome after the final state; got %q", out)
	}
	if rows := hintRows(renderScreen(t, out, 40, 12)); len(rows) != 0 {
		t.Fatalf("detach hint still rendered on rows %v after daemon disconnect", rows)
	}
}
