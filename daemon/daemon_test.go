package daemon

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startSession runs Serve for cmd in the background and returns the socket and
// retention directory once the socket is ready.
func startSession(t *testing.T, id string, cmd []string, md map[string]string) (socketPath, retentionDir string, errCh chan error) {
	t.Helper()
	_, socketPath, retentionDir, errCh = startSessionObjMeta(t, id, cmd, md)
	return socketPath, retentionDir, errCh
}

// startSessionObj is like startSession but also returns the *Session so
// white-box tests can inspect its internal state.
func startSessionObj(t *testing.T, id string, cmd []string, md map[string]string) (s *Session, socketPath string, errCh chan error) {
	t.Helper()
	s, socketPath, _, errCh = startSessionObjMeta(t, id, cmd, md)
	return s, socketPath, errCh
}

func startSessionObjMeta(t *testing.T, id string, cmd []string, md map[string]string) (s *Session, socketPath, retentionDir string, errCh chan error) {
	t.Helper()
	// A short base keeps the socket path under the platform's sun_path limit;
	// t.TempDir embeds the (long) test name and can overflow it.
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath = filepath.Join(dir, "sock")
	retentionDir = filepath.Join(dir, "ended")
	cfg := Config{
		ID:           id,
		Command:      cmd,
		Metadata:     md,
		SocketPath:   socketPath,
		RetentionDir: retentionDir,
	}
	s, err = newSession(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	errCh = make(chan error, 1)
	go func() { errCh <- s.run() }()
	waitForSocket(t, socketPath)
	return s, socketPath, retentionDir, errCh
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s never became ready", path)
}

func roundTrip(t *testing.T, socketPath string, req Request) Response {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestWaitReturnsZeroExitCode(t *testing.T) {
	socketPath, _, errCh := startSession(t, "ok", []string{"sh", "-c", "sleep 0.1; exit 0"}, nil)

	resp := roundTrip(t, socketPath, Request{Op: "wait"})
	if !resp.OK || resp.ExitCode == nil || *resp.ExitCode != 0 {
		t.Fatalf("wait: got %+v", resp)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestWaitReturnsNonzeroExitCode(t *testing.T) {
	socketPath, _, errCh := startSession(t, "fail", []string{"sh", "-c", "sleep 0.1; exit 3"}, nil)

	resp := roundTrip(t, socketPath, Request{Op: "wait"})
	if resp.ExitCode == nil || *resp.ExitCode != 3 {
		t.Fatalf("wait: got %+v", resp)
	}
	<-errCh
}

func TestInfoReportsRunningThenKilled(t *testing.T) {
	md := map[string]string{"role": "worker"}
	socketPath, retentionDir, errCh := startSession(t, "long", []string{"sleep", "30"}, md)

	resp := roundTrip(t, socketPath, Request{Op: "info"})
	if resp.Info == nil || !resp.Info.Running {
		t.Fatalf("info while running: %+v", resp)
	}
	if resp.Info.Pid <= 0 {
		t.Fatalf("expected a pid, got %d", resp.Info.Pid)
	}
	if resp.Info.Metadata["role"] != "worker" {
		t.Fatalf("metadata not echoed: %+v", resp.Info.Metadata)
	}

	kill := roundTrip(t, socketPath, Request{Op: "kill"})
	if kill.Info == nil || kill.Info.Running || !kill.Info.Killed {
		t.Fatalf("kill info: %+v", kill.Info)
	}
	if kill.Info.ExitCode == nil || *kill.Info.ExitCode != 128+1 {
		t.Fatalf("expected SIGHUP exit code 129, got %+v", kill.Info.ExitCode)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("serve: %v", err)
	}

	// The ended record must be persisted under the global namespace.
	record := RecordPath(retentionDir, "long")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("record not persisted: %v", err)
	}
}

func TestSendReachesPTYAndPersistsHistory(t *testing.T) {
	socketPath, retentionDir, errCh := startSession(t, "echoer", []string{"cat"}, nil)

	resp := roundTrip(t, socketPath, Request{Op: "send", Input: []byte("ping\n")})
	if !resp.OK {
		t.Fatalf("send: %+v", resp)
	}

	// cat echoes the input back through the PTY into the scrollback store.
	if !waitForOutput(t, socketPath) {
		t.Fatalf("session produced no output after send")
	}

	roundTrip(t, socketPath, Request{Op: "kill"})
	if err := <-errCh; err != nil {
		t.Fatalf("serve: %v", err)
	}

	history, err := os.ReadFile(HistoryPath(retentionDir, "echoer"))
	if err != nil {
		t.Fatalf("history not persisted: %v", err)
	}
	if !bytes.Contains(history, []byte("ping")) {
		t.Fatalf("history missing sent input: %q", history)
	}
}

func waitForOutput(t *testing.T, socketPath string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := roundTrip(t, socketPath, Request{Op: "info"})
		if resp.Info != nil && resp.Info.OutputBytes > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestSocketRemovedAfterExit(t *testing.T) {
	socketPath, _, errCh := startSession(t, "quick", []string{"true"}, nil)
	if err := <-errCh; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket not removed after exit: stat err = %v", err)
	}
}

func TestLargeSendDeliveredInOrder(t *testing.T) {
	socketPath, retentionDir, errCh := startSession(t, "bulk", []string{"cat"}, nil)

	// Many newline-terminated lines exercise the queue and short-write handling
	// without tripping the terminal's canonical line-length limit.
	const lines = 4000
	var payload bytes.Buffer
	for i := 0; i < lines; i++ {
		payload.WriteString("ping\n")
	}
	if resp := roundTrip(t, socketPath, Request{Op: "send", Input: payload.Bytes()}); !resp.OK {
		t.Fatalf("send: %+v", resp)
	}
	// Ctrl-D at the start of a line makes cat see EOF and exit cleanly.
	roundTrip(t, socketPath, Request{Op: "send", Input: []byte{0x04}})

	if err := <-errCh; err != nil {
		t.Fatalf("serve: %v", err)
	}

	history, err := os.ReadFile(HistoryPath(retentionDir, "bulk"))
	if err != nil {
		t.Fatalf("history not persisted: %v", err)
	}
	// cat echoes every line back, so each sent line appears at least once; no
	// lines should be dropped by the input queue.
	if got := bytes.Count(history, []byte("ping")); got < lines {
		t.Fatalf("history missing sent lines: got %d occurrences, want >= %d", got, lines)
	}
}

func TestInputQueueCapBoundsRetainedBytes(t *testing.T) {
	// A child that never reads its stdin makes every PTY write block, so queued
	// input accumulates until the cap rejects further bytes.
	s, socketPath, errCh := startSessionObj(t, "stuck", []string{"sleep", "30"}, nil)

	const sends = 8
	const chunk = 64 << 10 // 512KiB total, double the 256KiB cap
	payload := bytes.Repeat([]byte("a"), chunk)
	for i := 0; i < sends; i++ {
		if resp := roundTrip(t, socketPath, Request{Op: "send", Input: payload}); !resp.OK {
			t.Fatalf("send %d: %+v", i, resp)
		}
	}

	// Once writes block, the queued bytes still in inputBuf must stay bounded by
	// the cap rather than growing without limit across repeated sends.
	if got := s.retainedInput(); got > ptyInputCap {
		t.Fatalf("retained input %d exceeds cap %d", got, ptyInputCap)
	}

	roundTrip(t, socketPath, Request{Op: "kill"})
	if err := <-errCh; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestUnknownOp(t *testing.T) {
	socketPath, _, errCh := startSession(t, "u", []string{"sleep", "30"}, nil)
	resp := roundTrip(t, socketPath, Request{Op: "bogus"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("expected error for unknown op, got %+v", resp)
	}
	roundTrip(t, socketPath, Request{Op: "kill"})
	<-errCh
}
