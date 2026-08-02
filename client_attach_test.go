package bgx_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	bgx "github.com/sidedotdev/bgx"
	"github.com/sidedotdev/bgx/daemon"
)

type testTerminal struct {
	input  io.Reader
	output bytes.Buffer
	resize chan struct{}

	mu       sync.Mutex
	cols     uint16
	rows     uint16
	entered  bool
	restored bool
}

func newTestTerminal(input io.Reader) *testTerminal {
	return &testTerminal{
		input:  input,
		resize: make(chan struct{}),
		cols:   80,
		rows:   24,
	}
}

func (t *testTerminal) Read(p []byte) (int, error) {
	return t.input.Read(p)
}

func (t *testTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.output.Write(p)
}

func (t *testTerminal) Size() (cols, rows uint16, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows, nil
}

func (t *testTerminal) setSize(cols, rows uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols, t.rows = cols, rows
}

func (t *testTerminal) ResizeEvents(context.Context) <-chan struct{} {
	return t.resize
}

func (t *testTerminal) EnterRaw() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entered = true
	return nil
}

func (t *testTerminal) Restore() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restored = true
	return nil
}

func (t *testTerminal) snapshot() (string, bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.output.String(), t.entered, t.restored
}

func TestClientAttachIgnoresUnknownDaemonFrames(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	closed := make(chan struct{})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return &closeTrackingStream{
			ReadWriteCloser: clientConn,
			closed:          closed,
		}, nil
	}

	serverErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)

		line, err := br.ReadBytes('\n')
		if err != nil {
			serverErr <- err
			return
		}
		var req map[string]any
		if err := json.Unmarshal(line, &req); err != nil {
			serverErr <- err
			return
		}
		if req["op"] != "attach" {
			serverErr <- &unexpectedValueError{got: req["op"], want: "attach"}
			return
		}
		if err := json.NewEncoder(serverConn).Encode(map[string]any{"ok": true}); err != nil {
			serverErr <- err
			return
		}

		tag, payload, err := daemon.ReadFrame(br)
		if err != nil {
			serverErr <- err
			return
		}
		if tag != daemon.FrameResize {
			serverErr <- &unexpectedValueError{got: tag, want: daemon.FrameResize}
			return
		}
		size, ok := daemon.DecodeResize(payload)
		if !ok || size.Cols != 80 || size.Rows != 24 {
			serverErr <- &unexpectedValueError{got: size, want: daemon.ResizePayload{Cols: 80, Rows: 24}}
			return
		}

		if err := daemon.WriteFrame(serverConn, daemon.FrameTag(255), []byte("future")); err != nil {
			serverErr <- err
			return
		}
		if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, []byte("visible")); err != nil {
			serverErr <- err
			return
		}
		if err := daemon.WriteFrame(serverConn, daemon.FrameEnded, nil); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	terminalInputReader, terminalInputWriter := io.Pipe()
	defer terminalInputReader.Close()
	defer terminalInputWriter.Close()
	terminal := newTestTerminal(terminalInputReader)

	if err := bgx.NewClient(dial).Attach(context.Background(), terminal); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("attach server: %v", err)
	}
	waitForClosed(t, closed)

	output, entered, restored := terminal.snapshot()
	if !entered {
		t.Fatal("terminal did not enter raw mode")
	}
	if !restored {
		t.Fatal("terminal raw mode was not restored")
	}
	if !bytes.Contains([]byte(output), []byte("visible")) {
		t.Fatalf("terminal output %q does not contain post-unknown-frame output", output)
	}
}

type frameMsg struct {
	tag     daemon.FrameTag
	payload []byte
}

// attachFrameServer accepts the attach handshake on serverConn and streams the
// client's subsequent frames until the connection closes.
func attachFrameServer(t *testing.T, serverConn net.Conn) <-chan frameMsg {
	t.Helper()
	frames := make(chan frameMsg, 16)
	go func() {
		defer close(frames)
		br := bufio.NewReader(serverConn)
		if _, err := br.ReadBytes('\n'); err != nil {
			t.Errorf("attach request: %v", err)
			return
		}
		if err := json.NewEncoder(serverConn).Encode(map[string]any{"ok": true}); err != nil {
			t.Errorf("attach ack: %v", err)
			return
		}
		for {
			tag, payload, err := daemon.ReadFrame(br)
			if err != nil {
				return
			}
			frames <- frameMsg{tag: tag, payload: payload}
		}
	}()
	return frames
}

func nextFrame(t *testing.T, frames <-chan frameMsg) frameMsg {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("frame stream closed early")
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a frame")
		return frameMsg{}
	}
}

func expectResizeFrame(t *testing.T, frames <-chan frameMsg, rows, cols uint16) {
	t.Helper()
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameResize {
		t.Fatalf("frame tag = %v, want FrameResize", frame.tag)
	}
	size, ok := daemon.DecodeResize(frame.payload)
	if !ok || size.Rows != rows || size.Cols != cols {
		t.Fatalf("resize payload = %+v (ok=%v), want rows=%d cols=%d", size, ok, rows, cols)
	}
}

func TestClientAttachForwardsInputResizeAndDetach(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	terminalInputReader, terminalInputWriter := io.Pipe()
	defer terminalInputReader.Close()
	defer terminalInputWriter.Close()
	terminal := newTestTerminal(terminalInputReader)

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
	}()

	expectResizeFrame(t, frames, 24, 80)

	if _, err := terminalInputWriter.Write([]byte("hello")); err != nil {
		t.Fatalf("write terminal input: %v", err)
	}
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameInput || !bytes.Equal(frame.payload, []byte("hello")) {
		t.Fatalf("frame = tag %v payload %q, want FrameInput %q", frame.tag, frame.payload, "hello")
	}

	terminal.setSize(100, 30)
	select {
	case terminal.resize <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("resize event was not consumed")
	}
	expectResizeFrame(t, frames, 30, 100)

	// Ctrl+\ as the raw control byte is the detach key.
	if _, err := terminalInputWriter.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	frame = nextFrame(t, frames)
	if frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after detach")
	}

	_, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
}

func TestClientAttachDetachInstructionsReserveRowAndRenderHint(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	terminalInputReader, terminalInputWriter := io.Pipe()
	defer terminalInputReader.Close()
	defer terminalInputWriter.Close()
	terminal := newTestTerminal(terminalInputReader)

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal, bgx.WithDetachInstructions())
	}()

	// The bottom row is reserved for the hint, so the session is told a
	// one-row-shorter size.
	expectResizeFrame(t, frames, 23, 80)

	if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, []byte("hi")); err != nil {
		t.Fatalf("write output frame: %v", err)
	}
	if err := daemon.WriteFrame(serverConn, daemon.FrameEnded, nil); err != nil {
		t.Fatalf("write ended frame: %v", err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after session end")
	}

	output, _, _ := terminal.snapshot()
	if !strings.Contains(output, "hi") {
		t.Fatalf("terminal output %q does not contain the session output", output)
	}
	if !strings.Contains(output, `detach: ctrl+\`) {
		t.Fatalf("terminal output %q does not contain the detach hint", output)
	}
}

type unexpectedValueError struct {
	got  any
	want any
}

func (e *unexpectedValueError) Error() string {
	return "unexpected value"
}

type closeTrackingStream struct {
	io.ReadWriteCloser
	once   sync.Once
	closed chan struct{}
}

func (s *closeTrackingStream) Close() error {
	err := s.ReadWriteCloser.Close()
	s.once.Do(func() {
		close(s.closed)
	})
	return err
}

func TestClientAttachCancellationClosesStream(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	closed := make(chan struct{})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return &closeTrackingStream{
			ReadWriteCloser: clientConn,
			closed:          closed,
		}, nil
	}

	serverReady := make(chan struct{})
	go func() {
		br := bufio.NewReader(serverConn)
		_, _ = br.ReadBytes('\n')
		_ = json.NewEncoder(serverConn).Encode(map[string]any{"ok": true})
		_, _, _ = daemon.ReadFrame(br)
		close(serverReady)
	}()

	terminalInputReader, terminalInputWriter := io.Pipe()
	defer terminalInputReader.Close()
	defer terminalInputWriter.Close()
	terminal := newTestTerminal(terminalInputReader)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(ctx, terminal)
	}()

	select {
	case <-serverReady:
	case <-time.After(time.Second):
		t.Fatal("attach handshake did not complete")
	}
	cancel()

	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Attach error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after cancellation")
	}
	waitForClosed(t, closed)

	_, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
}
