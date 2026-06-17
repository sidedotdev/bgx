package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/scrollback"
	"github.com/sidedotdev/bgx/vt"
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

// TestAttachSnapshotStreamCoversEntireOutput is a race torture test for the
// attach handoff. A client that joins a live session gets a point-in-time
// rendered snapshot followed by the raw output stream; if the snapshot and the
// stream subscription are not captured atomically, output produced in the gap
// is either lost (in neither) or duplicated (in both). Many clients attach at
// staggered moments while a producer floods the PTY, and each client's snapshot
// plus stream must exactly tile the session's complete output: the stream must
// be a clean suffix of the full output and the snapshot must equal the
// rendering of the preceding prefix. Both checks are verified against the full
// scrollback history, which retains every byte here.
func TestAttachSnapshotStreamCoversEntireOutput(t *testing.T) {
	const (
		clients  = 24
		lines    = 8000
		sentinel = "ZZSENTINELZZ"
	)
	sentinelBytes := []byte(sentinel)
	// A deliberately slow shell loop dribbles distinct, sequentially numbered
	// lines, keeping output in flight throughout every client's attach handoff
	// while the low instantaneous rate keeps each client's bounded backlog from
	// overflowing (which would disconnect it — a separate concern). The final
	// sentinel line marks end of output, and the trailing sleep keeps the
	// session alive so clients drain in full before it is killed (output dropped
	// during session shutdown is likewise a separate concern).
	shCmd := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do printf 'ln%%05d\n' "$i"; i=$((i+1)); done; printf '%s\n'; sleep 30`, lines, sentinel)
	s, socketPath, _, errCh := startTortureSession(t, "torture", []string{"sh", "-c", shCmd})

	type capture struct {
		snapshot []byte
		stream   []byte
	}
	caps := make([]capture, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			caps[i].snapshot, caps[i].stream = attachCapture(t, socketPath, sentinelBytes)
		}()
		// Stagger so attaches land at many different points in the stream.
		time.Sleep(time.Millisecond)
	}
	wg.Wait()

	// Snapshot the complete output as ground truth while the session is still
	// alive (the history op waits for all buffered writes), then stop it.
	resp := roundTrip(t, socketPath, Request{Op: "history"})
	if !resp.OK {
		t.Fatalf("history op failed: %q", resp.Error)
	}
	full := resp.History
	s.kill()
	<-errCh
	if !bytes.Contains(full, sentinelBytes) {
		t.Fatalf("session output (%d bytes) never contained the sentinel", len(full))
	}

	for i, c := range caps {
		assertAttachTiles(t, i, full, c.snapshot, c.stream)
	}
}

// startTortureSession runs a session whose scrollback head is large enough to
// retain the entire run, making its history the exact ground truth for every
// byte the PTY emitted.
func startTortureSession(t *testing.T, id string, cmd []string) (s *Session, socketPath, retentionDir string, errCh chan error) {
	t.Helper()
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
		SocketPath:   socketPath,
		RetentionDir: retentionDir,
		Scrollback:   scrollback.Config{HeadSize: 16 << 20, TailSize: 1 << 20},
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

// attachCapture performs the attach handshake and records the snapshot (the
// first Output frame) and the subsequent raw stream, stopping once the final
// sentinel line has fully arrived so the captured stream ends exactly where the
// session output does.
func attachCapture(t *testing.T, socketPath string, sentinel []byte) (snapshot, stream []byte) {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Errorf("dial: %v", err)
		return nil, nil
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	if err := json.NewEncoder(conn).Encode(Request{Op: "attach"}); err != nil {
		t.Errorf("attach encode: %v", err)
		return nil, nil
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Errorf("attach handshake: %v", err)
		return nil, nil
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil || !resp.OK {
		t.Errorf("attach response: err=%v line=%q", err, line)
		return nil, nil
	}

	gotSnapshot := false
	for {
		tag, payload, err := ReadFrame(br)
		if err != nil {
			break
		}
		if tag != FrameOutput {
			continue
		}
		if !gotSnapshot {
			gotSnapshot = true
			snapshot = payload
			if bytes.Contains(snapshot, sentinel) {
				break
			}
			continue
		}
		stream = append(stream, payload...)
		// The sentinel is the last line, so once it and a following newline are
		// present the stream has captured everything up to end of output.
		if i := bytes.Index(stream, sentinel); i >= 0 && bytes.IndexByte(stream[i+len(sentinel):], '\n') >= 0 {
			break
		}
	}
	_ = WriteFrame(conn, FrameDetach, nil)
	return snapshot, stream
}

// assertAttachTiles verifies a single client's snapshot and stream tile the full
// output exactly: the stream is a clean suffix and the snapshot equals the
// rendering of the preceding prefix.
func assertAttachTiles(t *testing.T, idx int, full, snapshot, stream []byte) {
	t.Helper()
	if len(stream) > len(full) || !bytes.Equal(full[len(full)-len(stream):], stream) {
		t.Errorf("client %d: streamed output is not a suffix of the session output (stream=%dB, full=%dB); output was lost or duplicated during the attach handoff", idx, len(stream), len(full))
		return
	}
	prefix := full[:len(full)-len(stream)]
	want := renderTorture(t, prefix)
	if !bytes.Equal(want, snapshot) {
		t.Errorf("client %d: snapshot does not equal the rendering of the %dB pre-attach prefix (snapshot=%dB, want=%dB); output was lost or duplicated during the attach handoff", idx, len(prefix), len(snapshot), len(want))
	}
}

// renderTorture renders data through a fresh terminal sized like the session's,
// reproducing the snapshot the daemon would have sent at that point in the
// stream.
func renderTorture(t *testing.T, data []byte) []byte {
	t.Helper()
	term, err := vt.New(defaultCols, defaultRows)
	if err != nil {
		t.Fatalf("vt new: %v", err)
	}
	defer term.Close()
	if _, err := term.Write(data); err != nil {
		t.Fatalf("vt write: %v", err)
	}
	dump, err := term.DumpScreen()
	if err != nil {
		t.Fatalf("dump screen: %v", err)
	}
	return dump
}
