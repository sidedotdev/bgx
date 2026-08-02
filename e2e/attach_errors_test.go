package e2e

// These tests exercise attach's error contract against teardown races by
// standing in for a session daemon on its unix socket, since a real session
// cannot deterministically disappear between the liveness check and the attach
// dial or handshake.

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	t.Cleanup(func() { ln.Close() })
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

	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, ok := readRequest(t, bufio.NewReader(conn))
		if !ok || req.Op != "info" {
			t.Errorf("first request = %+v, want info", req)
			return
		}
		// Unlink the socket before answering so the attach dial that follows
		// the liveness check deterministically fails.
		ln.Close()
		os.Remove(sockPath)
		_ = json.NewEncoder(conn).Encode(daemon.Response{
			OK:   true,
			Info: &daemon.Info{ID: id, Running: true},
		})
	}()

	res := bgxIn(t, dir, "attach", id)
	<-served
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
	if msg, _ := errObj["error"].(string); !strings.Contains(msg, "does not exist") {
		t.Fatalf("error = %q, want mention of not existing", msg)
	}
}

func TestAttachNegativeHandshakeReportsAttachFailed(t *testing.T) {
	dir := runDir(t)
	const id = "closing"
	ln, _ := fakeSessionSocket(t, dir, id)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, ok := readRequest(t, bufio.NewReader(conn))
				if !ok {
					return
				}
				switch req.Op {
				case "info":
					_ = json.NewEncoder(conn).Encode(daemon.Response{
						OK:   true,
						Info: &daemon.Info{ID: id, Running: true},
					})
				case "attach":
					_ = json.NewEncoder(conn).Encode(daemon.Response{
						OK:    false,
						Error: "session has ended",
					})
				default:
					t.Errorf("unexpected request op %q", req.Op)
				}
			}(conn)
		}
	}()

	res := bgxIn(t, dir, "attach", id)
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
	if msg, _ := errObj["error"].(string); !strings.Contains(msg, "session has ended") {
		t.Fatalf("error = %q, want the daemon's handshake message", msg)
	}
}
