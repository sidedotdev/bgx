package bgx_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	lg "github.com/ehsanul/libghostty-vt-static"
	bgx "github.com/sidedotdev/bgx"
	"github.com/sidedotdev/bgx/daemon"
	"github.com/sidedotdev/bgx/vt"
)

type testTerminal struct {
	input  io.Reader
	output bytes.Buffer
	resize chan struct{}

	mu       sync.Mutex
	cols     uint16
	rows     uint16
	sizeErr  error
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

func (t *testTerminal) ReadContext(ctx context.Context, p []byte) (int, error) {
	if input, ok := t.input.(interface {
		ReadContext(context.Context, []byte) (int, error)
	}); ok {
		return input.ReadContext(ctx, p)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return t.Read(p)
}

type contextPipeReader struct {
	*io.PipeReader
}

func (r *contextPipeReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	closeErr := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		closeErr <- r.CloseWithError(ctx.Err())
	})
	n, err := r.Read(p)
	if !stop() {
		if cancelErr := <-closeErr; cancelErr != nil && !errors.Is(cancelErr, io.ErrClosedPipe) {
			err = errors.Join(err, cancelErr)
		}
	}
	return n, err
}

func (t *testTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.output.Write(p)
}

func (t *testTerminal) Size() (cols, rows uint16, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows, t.sizeErr
}

func (t *testTerminal) setSize(cols, rows uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols, t.rows = cols, rows
}

func (t *testTerminal) ResizeEvents(ctx context.Context) <-chan struct{} {
	events := make(chan struct{})
	go func() {
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.resize:
				select {
				case events <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events
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
		defer func() {
			if err := serverConn.Close(); err != nil {
				serverErr <- err
			}
		}()
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
	t.Cleanup(func() {
		if err := terminalInputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := terminalInputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{terminalInputReader})

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
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	terminalInputReader, terminalInputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := terminalInputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := terminalInputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{terminalInputReader})

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

	output, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}

	const message = "\r\nDetached from session\r\n"
	beforeMessage, found := strings.CutSuffix(output, message)
	if !found {
		t.Fatalf("terminal output = %q, want suffix %q", output, message)
	}
	assertTerminalStatePreserved(t, beforeMessage, 100, 30)
}

func TestClientAttachDetachInstructionsReserveRowAndRenderHint(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	terminalInputReader, terminalInputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := terminalInputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := terminalInputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{terminalInputReader})

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(
			context.Background(),
			terminal,
			bgx.WithAttachMode(bgx.AttachModeIsolated),
			bgx.WithDetachInstructions(),
		)
	}()

	// The bottom row is reserved for the hint, so the session is told a
	// one-row-shorter size.
	expectResizeFrame(t, frames, 23, 80)

	sessionLines := []string{"FINAL-TOP", "FINAL-MIDDLE", "FINAL-BOTTOM"}
	payload := []byte("FINAL-TOP\r\nFINAL-MIDDLE\r\nFINAL-BOTTOM\x1b[H")
	if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, payload); err != nil {
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
	for _, line := range sessionLines {
		if !strings.Contains(output, line) {
			t.Fatalf("terminal output %q does not contain session line %q", output, line)
		}
	}
	if !strings.Contains(output, `detach: ctrl+\`) {
		t.Fatalf("terminal output %q does not contain the detach hint", output)
	}
	if !strings.HasSuffix(output, "Session ended\r\n") {
		t.Fatalf("terminal output = %q, want session-ended message after final state", output)
	}
	assertLifecycleRestoresHistoryAndPrintsFinalState(
		t,
		output,
		80,
		24,
		sessionLines,
		"Session ended",
	)
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
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})

	closed := make(chan struct{})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return &closeTrackingStream{
			ReadWriteCloser: clientConn,
			closed:          closed,
		}, nil
	}

	serverReady := make(chan struct{})
	serverErr := make(chan error, 1)
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
		if _, _, err := daemon.ReadFrame(br); err != nil {
			serverErr <- err
			return
		}
		close(serverReady)
		serverErr <- nil
	}()

	terminalInputReader, terminalInputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := terminalInputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := terminalInputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{terminalInputReader})

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
	if err := <-serverErr; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("attach server: %v", err)
	}

	_, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
}

type failingTerminal struct {
	*testTerminal
	writeErr   error
	restoreErr error
}

func (t *failingTerminal) Write(p []byte) (int, error) {
	if t.writeErr != nil {
		return 0, t.writeErr
	}
	return t.testTerminal.Write(p)
}

func (t *failingTerminal) Restore() error {
	if err := t.testTerminal.Restore(); err != nil {
		return err
	}
	return t.restoreErr
}

type resizeWriteFailureStream struct {
	reader   *bytes.Reader
	writeErr error
	writes   int
	closed   bool
}

func newResizeWriteFailureStream(writeErr error) *resizeWriteFailureStream {
	return &resizeWriteFailureStream{
		reader:   bytes.NewReader([]byte("{\"ok\":true}\n")),
		writeErr: writeErr,
	}
}

func (s *resizeWriteFailureStream) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *resizeWriteFailureStream) Write(p []byte) (int, error) {
	s.writes++
	if s.writes == 1 {
		return len(p), nil
	}
	return 0, s.writeErr
}

func (s *resizeWriteFailureStream) Close() error {
	s.closed = true
	return nil
}

func TestClientAttachReportsTerminalWriteFailure(t *testing.T) {
	writeErr := errors.New("terminal output failed")
	stream := newResizeWriteFailureStream(errors.New("resize should not be reached"))
	terminal := &failingTerminal{
		testTerminal: newTestTerminal(bytes.NewReader(nil)),
		writeErr:     writeErr,
	}
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}

	err := bgx.NewClient(dial).Attach(
		context.Background(),
		terminal,
		bgx.WithAttachMode(bgx.AttachModeIsolated),
	)
	if !errors.Is(err, writeErr) {
		t.Fatalf("Attach error = %v, want terminal write error", err)
	}
	_, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
}

func TestClientAttachReportsTerminalRestoreFailure(t *testing.T) {
	restoreErr := errors.New("terminal restore failed")
	stream := newResizeWriteFailureStream(errors.New("resize failed"))
	terminal := &failingTerminal{
		testTerminal: newTestTerminal(bytes.NewReader(nil)),
		restoreErr:   restoreErr,
	}
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, restoreErr) {
		t.Fatalf("Attach error = %v, want terminal restore error", err)
	}
}

func TestClientAttachReportsInitialResizeFailure(t *testing.T) {
	resizeErr := errors.New("resize frame failed")
	stream := newResizeWriteFailureStream(resizeErr)
	terminal := newTestTerminal(bytes.NewReader(nil))
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, resizeErr) {
		t.Fatalf("Attach error = %v, want resize frame error", err)
	}
	if !stream.closed {
		t.Fatal("attach stream was not closed")
	}
}

func TestClientAttachReportsMalformedDaemonFrame(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}

	serverErr := make(chan error, 1)
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
		if _, _, err := daemon.ReadFrame(br); err != nil {
			serverErr <- err
			return
		}
		if _, err := serverConn.Write([]byte{byte(daemon.FrameOutput)}); err != nil {
			serverErr <- err
			return
		}
		serverErr <- serverConn.Close()
	}()

	terminalInputReader, terminalInputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := terminalInputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := terminalInputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{terminalInputReader})
	err := bgx.NewClient(dial).Attach(
		context.Background(),
		terminal,
		bgx.WithAttachMode(bgx.AttachModeIsolated),
	)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Attach error = %v, want unexpected EOF", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("attach server: %v", err)
	}

	output, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
	if strings.Contains(output, "\x1bc") {
		t.Fatalf("terminal output = %q, remote frame failure must not fully reset terminal", output)
	}
	if !strings.Contains(output, "\x1b[?25h\x1b[0m") {
		t.Fatalf("terminal output = %q, want cursor and style cleanup", output)
	}
}

func TestClientAttachDetachIsSuccessful(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	terminal := newTestTerminal(bytes.NewReader([]byte{0x1C}))
	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if err != nil {
		t.Fatalf("Attach after detach: %v", err)
	}

	expectResizeFrame(t, frames, 24, 80)
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}
}

type sequencedSizeTerminal struct {
	*testTerminal

	sizeMu    sync.Mutex
	sizeCalls int
	sizes     []struct {
		cols uint16
		rows uint16
		err  error
	}
}

func (t *sequencedSizeTerminal) Size() (cols, rows uint16, err error) {
	t.sizeMu.Lock()
	defer t.sizeMu.Unlock()
	index := t.sizeCalls
	t.sizeCalls++
	if index >= len(t.sizes) {
		index = len(t.sizes) - 1
	}
	size := t.sizes[index]
	return size.cols, size.rows, size.err
}

func TestClientAttachSkipsUnavailableSizeUntilValidResize(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := &sequencedSizeTerminal{
		testTerminal: newTestTerminal(&contextPipeReader{inputReader}),
		sizes: []struct {
			cols uint16
			rows uint16
			err  error
		}{
			{err: bgx.ErrTerminalSizeUnavailable},
			{cols: 100, rows: 30},
		},
	}

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(
			context.Background(),
			terminal,
			bgx.WithAttachMode(bgx.AttachModeIsolated),
			bgx.WithDetachInstructions(),
		)
	}()

	select {
	case terminal.resize <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("resize event was not consumed")
	}
	expectResizeFrame(t, frames, 29, 100)

	if _, err := inputWriter.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	frame := nextFrame(t, frames)
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
}

func TestClientAttachReportsActionableSizeError(t *testing.T) {
	sizeErr := errors.New("read terminal dimensions")
	stream := newResizeWriteFailureStream(errors.New("resize should not be sent"))
	terminal := newTestTerminal(bytes.NewReader(nil))
	terminal.sizeErr = sizeErr
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, sizeErr) {
		t.Fatalf("Attach error = %v, want terminal size error", err)
	}
	output, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
	assertTerminalStatePreserved(t, output, 80, 24)
}

func TestClientAttachDetachInstructionsSizeFailureRestoresDisplay(t *testing.T) {
	sizeErr := errors.New("read terminal dimensions")
	stream := newResizeWriteFailureStream(errors.New("resize should not be sent"))
	terminal := newTestTerminal(bytes.NewReader(nil))
	terminal.sizeErr = sizeErr
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}

	err := bgx.NewClient(dial).Attach(
		context.Background(),
		terminal,
		bgx.WithAttachMode(bgx.AttachModeIsolated),
		bgx.WithDetachInstructions(),
	)
	if !errors.Is(err, sizeErr) {
		t.Fatalf("Attach error = %v, want terminal size error", err)
	}
	output, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
	assertTerminalStatePreserved(t, output, 80, 24)
}

type blockingResizeTerminal struct {
	*testTerminal

	sizeMu      sync.Mutex
	sizeCalls   int
	sizeStarted chan struct{}
	releaseSize chan struct{}
	restored    chan struct{}
	sizeErr     error
}

func (t *blockingResizeTerminal) Size() (cols, rows uint16, err error) {
	t.sizeMu.Lock()
	t.sizeCalls++
	call := t.sizeCalls
	t.sizeMu.Unlock()
	if call > 1 {
		select {
		case <-t.sizeStarted:
		default:
			close(t.sizeStarted)
		}
		<-t.releaseSize
		return 0, 0, t.sizeErr
	}
	return 80, 24, nil
}

func (t *blockingResizeTerminal) Restore() error {
	if err := t.testTerminal.Restore(); err != nil {
		return err
	}
	close(t.restored)
	return nil
}

func TestClientAttachWaitsForResizeWorkerBeforeRestore(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := &blockingResizeTerminal{
		testTerminal: newTestTerminal(&contextPipeReader{inputReader}),
		sizeStarted:  make(chan struct{}),
		releaseSize:  make(chan struct{}),
		restored:     make(chan struct{}),
	}

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
	}()

	expectResizeFrame(t, frames, 24, 80)
	select {
	case terminal.resize <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("resize event was not consumed")
	}
	select {
	case <-terminal.sizeStarted:
	case <-time.After(time.Second):
		t.Fatal("resize worker did not enter Size")
	}

	if err := daemon.WriteFrame(serverConn, daemon.FrameEnded, nil); err != nil {
		t.Fatalf("write ended frame: %v", err)
	}
	select {
	case <-terminal.restored:
		t.Fatal("terminal restored while resize worker was still active")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case err := <-result:
		t.Fatalf("Attach returned before resize worker stopped: %v", err)
	default:
	}

	close(terminal.releaseSize)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after resize worker stopped")
	}
	select {
	case <-terminal.restored:
	default:
		t.Fatal("terminal was not restored")
	}
}

type blockingInputTerminal struct {
	*testTerminal

	inputStarted chan struct{}
	inputExited  chan struct{}
	restored     chan struct{}
}

func (t *blockingInputTerminal) ReadContext(ctx context.Context, _ []byte) (int, error) {
	close(t.inputStarted)
	<-ctx.Done()
	close(t.inputExited)
	return 0, ctx.Err()
}

func (t *blockingInputTerminal) Restore() error {
	select {
	case <-t.inputExited:
	default:
		return errors.New("terminal restored before input worker exited")
	}
	if err := t.testTerminal.Restore(); err != nil {
		return err
	}
	close(t.restored)
	return nil
}

func TestClientAttachWaitsForBlockedInputBeforeRestore(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)
	terminal := &blockingInputTerminal{
		testTerminal: newTestTerminal(bytes.NewReader(nil)),
		inputStarted: make(chan struct{}),
		inputExited:  make(chan struct{}),
		restored:     make(chan struct{}),
	}

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
	}()

	expectResizeFrame(t, frames, 24, 80)
	select {
	case <-terminal.inputStarted:
	case <-time.After(time.Second):
		t.Fatal("input worker did not start")
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
	select {
	case <-terminal.inputExited:
	default:
		t.Fatal("input worker was not joined")
	}
	select {
	case <-terminal.restored:
	default:
		t.Fatal("terminal was not restored")
	}
}

type closeErrorStream struct {
	io.ReadWriteCloser
	err error
}

func (s *closeErrorStream) Close() error {
	return errors.Join(s.ReadWriteCloser.Close(), s.err)
}

func TestClientAttachDetachRetainsRestoreFailure(t *testing.T) {
	restoreErr := errors.New("restore failed")
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)
	terminal := &failingTerminal{
		testTerminal: newTestTerminal(bytes.NewReader([]byte{0x1C})),
		restoreErr:   restoreErr,
	}

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, restoreErr) {
		t.Fatalf("Attach error = %v, want restore failure", err)
	}

	expectResizeFrame(t, frames, 24, 80)
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}
}

func TestClientAttachDetachRetainsUnexpectedStreamCloseFailure(t *testing.T) {
	closeErr := errors.New("close failed")
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return &closeErrorStream{
			ReadWriteCloser: clientConn,
			err:             closeErr,
		}, nil
	}
	frames := attachFrameServer(t, serverConn)
	terminal := newTestTerminal(bytes.NewReader([]byte{0x1C}))

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Attach error = %v, want stream close failure", err)
	}

	expectResizeFrame(t, frames, 24, 80)
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}
}

type closedResizeEventsTerminal struct {
	*blockingInputTerminal
}

func (t *closedResizeEventsTerminal) ResizeEvents(context.Context) <-chan struct{} {
	events := make(chan struct{})
	close(events)
	return events
}

func TestClientAttachContinuesAfterResizeEventsClose(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)
	terminal := &closedResizeEventsTerminal{
		blockingInputTerminal: &blockingInputTerminal{
			testTerminal: newTestTerminal(bytes.NewReader(nil)),
			inputStarted: make(chan struct{}),
			inputExited:  make(chan struct{}),
			restored:     make(chan struct{}),
		},
	}

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
	}()

	expectResizeFrame(t, frames, 24, 80)
	select {
	case <-terminal.inputStarted:
	case <-time.After(time.Second):
		t.Fatal("input worker did not start")
	}
	select {
	case err := <-result:
		t.Fatalf("Attach returned after resize events closed: %v", err)
	default:
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
}

type shortWriteTerminal struct {
	*testTerminal

	mu            sync.Mutex
	short         bool
	writeObserved chan struct{}
	attempts      []string
}

func (t *shortWriteTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.attempts = append(t.attempts, string(p))
	short := t.short
	t.mu.Unlock()
	if t.writeObserved != nil {
		select {
		case t.writeObserved <- struct{}{}:
		default:
		}
	}
	if short {
		n := len(p) - 1
		if _, err := t.testTerminal.Write(p[:n]); err != nil {
			return 0, err
		}
		return n, nil
	}
	return t.testTerminal.Write(p)
}

func (t *shortWriteTerminal) attemptedWrite(want string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, attempt := range t.attempts {
		if attempt == want {
			return true
		}
	}
	return false
}

func (t *shortWriteTerminal) armShortWrites() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.short = true
}

func TestClientAttachReportsInitialTerminalShortWrite(t *testing.T) {
	stream := newResizeWriteFailureStream(errors.New("resize should not be reached"))
	terminal := &shortWriteTerminal{
		testTerminal: newTestTerminal(bytes.NewReader(nil)),
		short:        true,
	}
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}

	err := bgx.NewClient(dial).Attach(
		context.Background(),
		terminal,
		bgx.WithAttachMode(bgx.AttachModeIsolated),
	)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Attach error = %v, want short write", err)
	}
	if !terminal.attemptedWrite("\x1b[?1049l") {
		t.Fatal("Attach did not attempt to restore the normal screen after partial entry")
	}
}

func TestClientAttachReportsStreamedTerminalShortWrite(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)
	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := &shortWriteTerminal{
		testTerminal:  newTestTerminal(&contextPipeReader{inputReader}),
		writeObserved: make(chan struct{}, 4),
	}

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(
			context.Background(),
			terminal,
			bgx.WithAttachMode(bgx.AttachModeIsolated),
		)
	}()

	expectResizeFrame(t, frames, 24, 80)
	for i := 0; i < 2; i++ {
		select {
		case <-terminal.writeObserved:
		case <-time.After(time.Second):
			t.Fatal("initial attach rendering did not complete")
		}
	}
	terminal.armShortWrites()

	if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, []byte("output")); err != nil {
		t.Fatalf("write output frame: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Attach error = %v, want short write", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after short terminal write")
	}
}

func TestClientAttachRetainsResizeFailureWhenSessionEndWins(t *testing.T) {
	resizeErr := errors.New("resize failed")
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := &blockingResizeTerminal{
		testTerminal: newTestTerminal(&contextPipeReader{inputReader}),
		sizeStarted:  make(chan struct{}),
		releaseSize:  make(chan struct{}),
		restored:     make(chan struct{}),
		sizeErr:      resizeErr,
	}

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
	}()

	expectResizeFrame(t, frames, 24, 80)
	select {
	case terminal.resize <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("resize event was not consumed")
	}
	select {
	case <-terminal.sizeStarted:
	case <-time.After(time.Second):
		t.Fatal("resize worker did not enter Size")
	}
	if err := daemon.WriteFrame(serverConn, daemon.FrameEnded, nil); err != nil {
		t.Fatalf("write ended frame: %v", err)
	}
	close(terminal.releaseSize)

	select {
	case err := <-result:
		if !errors.Is(err, resizeErr) {
			t.Fatalf("Attach error = %v, want resize failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after resize worker stopped")
	}
}

type detachErrorTerminal struct {
	*testTerminal
	err error
}

func (t *detachErrorTerminal) ReadContext(_ context.Context, p []byte) (int, error) {
	p[0] = 0x1C
	return 1, t.err
}

func (t *detachErrorTerminal) Read(p []byte) (int, error) {
	p[0] = 0x1C
	return 1, t.err
}

func TestClientAttachDetachRetainsSimultaneousReadFailure(t *testing.T) {
	readErr := errors.New("terminal read failed")
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)
	terminal := &detachErrorTerminal{
		testTerminal: newTestTerminal(bytes.NewReader(nil)),
		err:          readErr,
	}

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, readErr) {
		t.Fatalf("Attach error = %v, want terminal read failure", err)
	}

	expectResizeFrame(t, frames, 24, 80)
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}
}

type closeEOFStream struct {
	response *bytes.Reader
	closed   chan struct{}
	once     sync.Once
	readErr  error

	mu     sync.Mutex
	writes bytes.Buffer
}

func newCloseEOFStream() *closeEOFStream {
	return &closeEOFStream{
		response: bytes.NewReader([]byte("{\"ok\":true}\n")),
		closed:   make(chan struct{}),
	}
}

func (s *closeEOFStream) Read(p []byte) (int, error) {
	if s.response.Len() > 0 {
		return s.response.Read(p)
	}
	<-s.closed
	if s.readErr != nil {
		return 0, errors.Join(io.EOF, s.readErr)
	}
	return 0, io.EOF
}

func (s *closeEOFStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes.Write(p)
}

func (s *closeEOFStream) Close() error {
	s.once.Do(func() {
		close(s.closed)
	})
	return nil
}

func TestClientAttachDetachIgnoresFrameEOFCausedByClose(t *testing.T) {
	stream := newCloseEOFStream()
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}
	terminal := newTestTerminal(bytes.NewReader([]byte{0x1C}))

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if err != nil {
		t.Fatalf("Attach after detach: %v", err)
	}

	stream.mu.Lock()
	written := append([]byte(nil), stream.writes.Bytes()...)
	stream.mu.Unlock()
	lineEnd := bytes.IndexByte(written, '\n')
	if lineEnd < 0 {
		t.Fatal("attach request was not terminated")
	}
	frames := bufio.NewReader(bytes.NewReader(written[lineEnd+1:]))
	tag, _, err := daemon.ReadFrame(frames)
	if err != nil {
		t.Fatalf("read resize frame: %v", err)
	}
	if tag != daemon.FrameResize {
		t.Fatalf("first frame tag = %v, want FrameResize", tag)
	}
	tag, _, err = daemon.ReadFrame(frames)
	if err != nil {
		t.Fatalf("read detach frame: %v", err)
	}
	if tag != daemon.FrameDetach {
		t.Fatalf("second frame tag = %v, want FrameDetach", tag)
	}
}

func TestClientAttachDetachRetainsUnexpectedSiblingOfExpectedCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return &closeErrorStream{
			ReadWriteCloser: clientConn,
			err:             errors.Join(net.ErrClosed, closeErr),
		}, nil
	}
	frames := attachFrameServer(t, serverConn)
	terminal := newTestTerminal(bytes.NewReader([]byte{0x1C}))

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Attach error = %v, want unexpected close failure", err)
	}

	expectResizeFrame(t, frames, 24, 80)
	frame := nextFrame(t, frames)
	if frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}
}

func TestClientAttachDetachRetainsUnexpectedSiblingOfShutdownFrameEOF(t *testing.T) {
	readErr := errors.New("frame read failed")
	stream := newCloseEOFStream()
	stream.readErr = readErr
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}
	terminal := newTestTerminal(bytes.NewReader([]byte{0x1C}))

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
	if !errors.Is(err, readErr) {
		t.Fatalf("Attach error = %v, want unexpected frame read failure", err)
	}
}
func TestClientAttachDisconnectClearsDetachInstructionsWithoutReset(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{inputReader})

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(
			context.Background(),
			terminal,
			bgx.WithAttachMode(bgx.AttachModeIsolated),
			bgx.WithDetachInstructions(),
		)
	}()

	expectResizeFrame(t, frames, 23, 80)
	sessionLines := []string{"DISCONNECT-TOP", "DISCONNECT-MIDDLE", "DISCONNECT-BOTTOM"}
	payload := []byte("DISCONNECT-TOP\r\nDISCONNECT-MIDDLE\r\nDISCONNECT-BOTTOM\x1b[H")
	if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, payload); err != nil {
		t.Fatalf("write output frame: %v", err)
	}
	if err := serverConn.Close(); err != nil {
		t.Fatalf("close attach server: %v", err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Attach error = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Attach did not return after remote disconnect")
	}

	output, entered, restored := terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
	if strings.Contains(output, "\x1bc") {
		t.Fatalf("terminal output = %q, remote disconnect must not fully reset terminal", output)
	}
	leaveAlt := strings.LastIndex(output, "\x1b[?1049l")
	if leaveAlt < 0 {
		t.Fatalf("terminal output = %q, want alternate screen restored", output)
	}
	for _, line := range sessionLines {
		if !strings.Contains(output[leaveAlt:], line) {
			t.Fatalf("terminal output after alternate screen = %q, want line %q", output[leaveAlt:], line)
		}
	}
	if !strings.Contains(output[leaveAlt:], "\x1b[?25h\x1b[0m") {
		t.Fatalf("terminal output = %q, want cursor and style cleanup", output)
	}
	if !strings.HasSuffix(output, "Disconnected from session\r\n") {
		t.Fatalf("terminal output = %q, want disconnect message after final state", output)
	}
	assertLifecycleRestoresHistoryAndPrintsFinalState(
		t,
		output,
		80,
		24,
		sessionLines,
		"Disconnected from session",
	)
}

func assertLifecycleRestoresHistoryAndPrintsFinalState(
	t *testing.T,
	output string,
	cols, physicalRows uint16,
	sessionLines []string,
	outcome string,
) {
	t.Helper()
	emulated, err := lg.NewTerminal(
		lg.WithSize(cols, physicalRows),
		lg.WithMaxScrollback(uint(physicalRows)+50),
	)
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	defer emulated.Close()

	for i := 0; i < int(physicalRows)+10; i++ {
		emulated.VTWrite([]byte(fmt.Sprintf("history-%03d\r\n", i)))
	}
	const restoredText = "shell prompt> draft"
	emulated.VTWrite([]byte(restoredText))
	emulated.VTWrite([]byte(output))

	selection, err := emulated.SelectAll()
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	history, err := emulated.SelectionFormatString(
		lg.WithSelection(selection),
		lg.WithSelectionFormat(lg.FormatterFormatPlain),
		lg.WithSelectionTrim(false),
		lg.WithSelectionUnwrap(false),
	)
	if err != nil {
		t.Fatalf("SelectionFormatString: %v", err)
	}

	restoredAt := strings.Index(history, restoredText)
	if restoredAt < 0 {
		t.Fatalf("pre-attach terminal content was lost; history=%q output=%q", history, output)
	}
	finalState := strings.Join(sessionLines, "\n") + "\n" + outcome
	finalStateAt := strings.LastIndex(history, finalState)
	if finalStateAt <= restoredAt {
		t.Fatalf("complete final session state and lifecycle outcome were not preserved together after restored history; want %q in history=%q output=%q", finalState, history, output)
	}
}

func assertTerminalStatePreserved(
	t *testing.T,
	output string,
	cols, rows uint16,
) {
	t.Helper()
	emulated, err := lg.NewTerminal(
		lg.WithSize(cols, rows),
		lg.WithMaxScrollback(uint(rows)+20),
	)
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	defer emulated.Close()
	for i := 0; i < int(rows)+10; i++ {
		emulated.VTWrite([]byte(fmt.Sprintf("history-%03d\r\n", i)))
	}
	emulated.VTWrite([]byte("\x1b[32mshell prompt> draft\x1b[5D"))

	scrollbackRows, err := emulated.ScrollbackRows()
	if err != nil {
		t.Fatalf("ScrollbackRows: %v", err)
	}
	if scrollbackRows == 0 {
		t.Fatal("test setup did not create terminal scrollback")
	}
	// This binding measures positive deltas away from the live bottom.
	emulated.ScrollViewportDelta(5)
	viewportActive, err := emulated.ViewportActive()
	if err != nil {
		t.Fatalf("ViewportActive: %v", err)
	}
	if !viewportActive {
		t.Fatal("test setup did not move the viewport away from the bottom")
	}

	type terminalState struct {
		visible        string
		history        string
		scrollbackRows uint
		scrollbar      lg.Scrollbar
		viewportActive bool
	}
	snapshot := func() terminalState {
		scrollbarBeforeSelection, err := emulated.Scrollbar()
		if err != nil {
			t.Fatalf("Scrollbar before selection: %v", err)
		}
		viewportBeforeSelection, err := emulated.ViewportActive()
		if err != nil {
			t.Fatalf("ViewportActive before selection: %v", err)
		}
		selection, err := emulated.SelectAll()
		if err != nil {
			t.Fatalf("SelectAll: %v", err)
		}
		history, err := emulated.SelectionFormatString(
			lg.WithSelection(selection),
			lg.WithSelectionFormat(lg.FormatterFormatPlain),
			lg.WithSelectionTrim(false),
			lg.WithSelectionUnwrap(false),
		)
		if err != nil {
			t.Fatalf("SelectionFormatString: %v", err)
		}
		scrollbarAfterSelection, err := emulated.Scrollbar()
		if err != nil {
			t.Fatalf("Scrollbar after selection: %v", err)
		}
		viewportAfterSelection, err := emulated.ViewportActive()
		if err != nil {
			t.Fatalf("ViewportActive after selection: %v", err)
		}
		if scrollbarAfterSelection != scrollbarBeforeSelection ||
			viewportAfterSelection != viewportBeforeSelection {
			t.Fatal("formatting terminal history changed the viewport")
		}

		formatter, err := lg.NewFormatter(
			emulated,
			lg.WithFormatterFormat(lg.FormatterFormatVT),
			lg.WithFormatterExtraStyle(true),
			lg.WithFormatterExtraCursor(true),
		)
		if err != nil {
			t.Fatalf("NewFormatter: %v", err)
		}
		visible, err := formatter.Format()
		formatter.Close()
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		scrollbackRows, err := emulated.ScrollbackRows()
		if err != nil {
			t.Fatalf("ScrollbackRows: %v", err)
		}
		scrollbar, err := emulated.Scrollbar()
		if err != nil {
			t.Fatalf("Scrollbar: %v", err)
		}
		viewportActive, err := emulated.ViewportActive()
		if err != nil {
			t.Fatalf("ViewportActive: %v", err)
		}
		return terminalState{
			visible:        string(visible),
			history:        history,
			scrollbackRows: scrollbackRows,
			scrollbar:      scrollbar,
			viewportActive: viewportActive,
		}
	}

	before := snapshot()
	emulated.VTWrite([]byte(output))
	after := snapshot()
	if after != before {
		t.Fatalf("terminal state after attach differs from pre-attach state:\nbefore %#v\nafter  %#v\nattach output %q", before, after, output)
	}
}

// TestClientAttachConcurrentOutputAndResizeKeepsSnapshotConsistent interleaves
// streamed output with terminal resizes and verifies the session-end repaint
// still reflects the last displayed output, i.e. the display model and the
// snapshot model stayed in lockstep.
func TestClientAttachConcurrentOutputAndResizeKeepsSnapshotConsistent(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Errorf("close attach server: %v", err)
		}
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{inputReader})

	result := make(chan error, 1)
	go func() {
		result <- bgx.NewClient(dial).Attach(
			context.Background(),
			terminal,
			bgx.WithAttachMode(bgx.AttachModeIsolated),
		)
	}()

	// The initial resize frame confirms the handshake ack has been written, so
	// the server's output frames below cannot interleave with it on the pipe.
	nextFrame(t, frames)
	go func() {
		for range frames {
		}
	}()

	sizes := []struct{ cols, rows uint16 }{{100, 30}, {60, 20}, {80, 24}}
	for i := 0; i < 120; i++ {
		payload := []byte(fmt.Sprintf("line-%03d\r\n", i))
		if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, payload); err != nil {
			t.Fatalf("write output frame: %v", err)
		}
		size := sizes[i%len(sizes)]
		terminal.setSize(size.cols, size.rows)
		select {
		case terminal.resize <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatal("resize event was not consumed")
		}
	}
	if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, []byte("final-marker")); err != nil {
		t.Fatalf("write final output frame: %v", err)
	}
	if err := daemon.WriteFrame(serverConn, daemon.FrameEnded, nil); err != nil {
		t.Fatalf("write ended frame: %v", err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Attach did not return after session end")
	}

	output, _, _ := terminal.snapshot()
	leaveAlt := strings.LastIndex(output, "\x1b[?1049l")
	if leaveAlt < 0 {
		t.Fatalf("attach never left the alternate screen; got %q", output)
	}
	if !strings.Contains(output[leaveAlt:], "final-marker") {
		t.Fatalf("final snapshot diverged from displayed session output; got %q", output[leaveAlt:])
	}
}
func TestClientAttachSessionEndPrintsOutcomeAfterWrittenRows(t *testing.T) {
	fullHeightLines := make([]string, 24)
	for i := range fullHeightLines {
		fullHeightLines[i] = fmt.Sprintf("FULL-%02d", i+1)
	}

	tests := []struct {
		name         string
		payload      string
		sessionLines []string
	}{
		{
			name:         "short output drops untouched trailing rows",
			payload:      "SHORT-TOP\r\nSHORT-BOTTOM\x1b[H",
			sessionLines: []string{"SHORT-TOP", "SHORT-BOTTOM"},
		},
		{
			name:         "touched empty rows are retained",
			payload:      "TOUCHED-TOP\r\n\r\n\x1b[H",
			sessionLines: []string{"TOUCHED-TOP", "", ""},
		},
		{
			name:         "full-height output scrolls the outcome below the snapshot",
			payload:      strings.Join(fullHeightLines, "\r\n") + "\x1b[H",
			sessionLines: fullHeightLines,
		},
	}

	for _, mode := range []bgx.AttachMode{bgx.AttachModeIsolated, bgx.AttachModeNative} {
		for _, tt := range tests {
			t.Run(string(mode)+"/"+tt.name, func(t *testing.T) {
				clientConn, serverConn := net.Pipe()
				t.Cleanup(func() {
					if err := serverConn.Close(); err != nil {
						t.Errorf("close attach server: %v", err)
					}
				})
				dial := func(context.Context) (io.ReadWriteCloser, error) {
					return clientConn, nil
				}
				frames := attachFrameServer(t, serverConn)

				inputReader, inputWriter := io.Pipe()
				t.Cleanup(func() {
					if err := inputReader.Close(); err != nil {
						t.Errorf("close terminal input reader: %v", err)
					}
					if err := inputWriter.Close(); err != nil {
						t.Errorf("close terminal input writer: %v", err)
					}
				})
				terminal := newTestTerminal(&contextPipeReader{inputReader})

				result := make(chan error, 1)
				go func() {
					result <- bgx.NewClient(dial).Attach(
						context.Background(),
						terminal,
						bgx.WithAttachMode(mode),
					)
				}()

				expectResizeFrame(t, frames, 24, 80)
				if err := daemon.WriteFrame(serverConn, daemon.FrameOutput, []byte(tt.payload)); err != nil {
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
				if !strings.HasSuffix(output, "Session ended\r\n") {
					t.Fatalf("terminal output = %q, want session-ended message after final state", output)
				}
				if mode == bgx.AttachModeNative {
					assertNativeOutputNeverIsolatesOrResets(t, output)
				}
				assertLifecycleRestoresHistoryAndPrintsFinalState(
					t,
					output,
					80,
					24,
					tt.sessionLines,
					"Session ended",
				)
			})
		}
	}
}

// assertNativeOutputNeverIsolatesOrResets checks that a native attachment
// never took over the alternate screen on its own initiative, reset the
// terminal, or cleared the outer scrollback.
func assertNativeOutputNeverIsolatesOrResets(t *testing.T, output string) {
	t.Helper()
	for _, forbidden := range []string{"\x1b[?1049h", "\x1b[?47h", "\x1b[?1047h", "\x1bc", "\x1b[3J"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("native attach wrote %q; output %q", forbidden, output)
		}
	}
}

// nativeAttachHarness runs a native-mode attachment against a frame server
// and returns the streamed client frames plus the terminal input writer.
type nativeAttachHarness struct {
	serverConn net.Conn
	frames     <-chan frameMsg
	terminal   *testTerminal
	input      *io.PipeWriter
	result     chan error
}

func startNativeAttach(t *testing.T, options ...bgx.AttachOption) *nativeAttachHarness {
	t.Helper()
	return startModeAttach(t, bgx.AttachModeNative, options...)
}

func startModeAttach(t *testing.T, mode bgx.AttachMode, options ...bgx.AttachOption) *nativeAttachHarness {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		// Tests may already have closed the server side to simulate a
		// disconnect, so a close error here is expected noise.
		_ = serverConn.Close() //nolint:errcheck
	})
	dial := func(context.Context) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	frames := attachFrameServer(t, serverConn)

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		if err := inputReader.Close(); err != nil {
			t.Errorf("close terminal input reader: %v", err)
		}
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close terminal input writer: %v", err)
		}
	})
	terminal := newTestTerminal(&contextPipeReader{inputReader})

	h := &nativeAttachHarness{
		serverConn: serverConn,
		frames:     frames,
		terminal:   terminal,
		input:      inputWriter,
		result:     make(chan error, 1),
	}
	go func() {
		h.result <- bgx.NewClient(dial).Attach(
			context.Background(),
			terminal,
			append([]bgx.AttachOption{bgx.WithAttachMode(mode)}, options...)...,
		)
	}()
	return h
}

func (h *nativeAttachHarness) writeOutput(t *testing.T, payload string) {
	t.Helper()
	if err := daemon.WriteFrame(h.serverConn, daemon.FrameOutput, []byte(payload)); err != nil {
		t.Fatalf("write output frame: %v", err)
	}
}

func (h *nativeAttachHarness) waitForOutput(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		output, _, _ := h.terminal.snapshot()
		if strings.Contains(output, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal output %q never contained %q", output, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *nativeAttachHarness) wait(t *testing.T, wantErr error) string {
	t.Helper()
	select {
	case err := <-h.result:
		if wantErr == nil && err != nil {
			t.Fatalf("Attach: %v", err)
		}
		if wantErr != nil && !errors.Is(err, wantErr) {
			t.Fatalf("Attach error = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return")
	}
	output, entered, restored := h.terminal.snapshot()
	if !entered || !restored {
		t.Fatalf("raw lifecycle entered=%v restored=%v, want both true", entered, restored)
	}
	return output
}

// nativeModeReset is the client's withdrawal of session input-reporting modes
// whenever the outer terminal stops receiving forwarded session bytes.
const nativeModeReset = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[?1l"

// nativeEntry is what the client writes before the first forwarded bytes on an
// 80x24 terminal: the pre-attach screen is scrolled into scrollback and the
// cursor homed, without touching the alternate screen.
func nativeEntry(rows int) string {
	return fmt.Sprintf("\x1b[%d;1H", rows) + strings.Repeat("\r\n", rows) + "\x1b[H"
}

func TestClientAttachNativeForwardsRawBytesAndDetaches(t *testing.T) {
	h := startNativeAttach(t)
	// Native presentation advertises the full terminal height.
	expectResizeFrame(t, h.frames, 24, 80)

	before, _, _ := h.terminal.snapshot()
	if before != "" {
		t.Fatalf("native attach wrote %q before any session output", before)
	}

	const payload = "raw \x1b[31mred\x1b[0m\r\n\x1b[?1000h\x1b[?2004hprompt> "
	h.writeOutput(t, payload)
	h.waitForOutput(t, "prompt> ")

	if _, err := h.input.Write([]byte("typed")); err != nil {
		t.Fatalf("write terminal input: %v", err)
	}
	frame := nextFrame(t, h.frames)
	if frame.tag != daemon.FrameInput || string(frame.payload) != "typed" {
		t.Fatalf("frame = tag %v payload %q, want FrameInput %q", frame.tag, frame.payload, "typed")
	}

	if _, err := h.input.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	if frame := nextFrame(t, h.frames); frame.tag != daemon.FrameDetach {
		t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
	}
	output := h.wait(t, nil)

	assertNativeOutputNeverIsolatesOrResets(t, output)
	wantPrefix := nativeEntry(24) + payload
	if !strings.HasPrefix(output, wantPrefix) {
		t.Fatalf("native output = %q, want raw session bytes after entry %q", output, wantPrefix)
	}
	cleanup := output[len(wantPrefix):]
	for _, want := range []string{
		"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1006l", "\x1b[?1004l", "\x1b[?2004l", "\x1b[?1l",
		"\x1b[?25h\x1b[0m", "\x1b7\x1b[r\x1b8",
	} {
		if !strings.Contains(cleanup, want) {
			t.Fatalf("native cleanup %q lacks %q", cleanup, want)
		}
	}
	if strings.Contains(cleanup, "\x1b[?1049l") {
		t.Fatalf("native cleanup %q left an alternate screen the session never entered", cleanup)
	}
	if !strings.HasSuffix(output, "Detached from session\r\n") {
		t.Fatalf("native output = %q, want detach message last", output)
	}
	assertLifecycleRestoresHistoryAndPrintsFinalState(
		t,
		output,
		80,
		24,
		[]string{"raw red", "prompt> "},
		"Detached from session",
	)
}

func TestClientAttachNativeLeavesSessionAlternateScreenOnEnd(t *testing.T) {
	h := startNativeAttach(t)
	expectResizeFrame(t, h.frames, 24, 80)

	h.writeOutput(t, "shell$ \x1b[?1049h\x1b[2J\x1b[HTUI\x1b[?25l")
	h.waitForOutput(t, "TUI")
	if err := daemon.WriteFrame(h.serverConn, daemon.FrameEnded, nil); err != nil {
		t.Fatalf("write ended frame: %v", err)
	}
	output := h.wait(t, nil)

	// The session's own alternate-screen switch is forwarded verbatim, so
	// cleanup must leave it and re-show the cursor before the outcome.
	afterContent := output[strings.LastIndex(output, "TUI"):]
	leave := strings.Index(afterContent, "\x1b[?1049l")
	if leave < 0 {
		t.Fatalf("native cleanup %q did not leave the session's alternate screen", afterContent)
	}
	if !strings.Contains(afterContent[leave:], "\x1b[?25h\x1b[0m") {
		t.Fatalf("native cleanup %q did not restore the cursor", afterContent)
	}
	if !strings.HasSuffix(output, "\r\nSession ended\r\n") {
		t.Fatalf("native output = %q, want session-ended message last", output)
	}
	for _, forbidden := range []string{"\x1bc", "\x1b[3J"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("native attach wrote %q", forbidden)
		}
	}
}

func TestClientAttachNativeResizeAdvertisesFullHeight(t *testing.T) {
	h := startNativeAttach(t, bgx.WithDetachInstructions())
	// The hint is one-time, not a reserved row, so no row is withheld.
	expectResizeFrame(t, h.frames, 24, 80)

	h.terminal.setSize(100, 30)
	select {
	case h.terminal.resize <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("resize event was not consumed")
	}
	expectResizeFrame(t, h.frames, 30, 100)

	if err := h.serverConn.Close(); err != nil {
		t.Fatalf("close attach server: %v", err)
	}
	output := h.wait(t, io.EOF)
	// Nothing was presented, so the terminal is untouched apart from the
	// outcome line.
	if output != "\r\nDisconnected from session\r\n" {
		t.Fatalf("native output = %q, want only the disconnect message", output)
	}
}

// renderTranscript replays raw client output onto a fresh terminal and returns
// the visible rows with trailing blanks trimmed, plus the styled VT rendering
// so tests can check that no client-side styling survives.
func renderTranscript(t *testing.T, transcript string, cols, rows uint16) (plain []string, styled string) {
	t.Helper()
	term, err := lg.NewTerminal(lg.WithSize(cols, rows))
	if err != nil {
		t.Fatalf("new terminal: %v", err)
	}
	defer term.Close()
	term.VTWrite([]byte(transcript))
	format := func(format lg.FormatterFormat) string {
		f, err := lg.NewFormatter(term, lg.WithFormatterFormat(format))
		if err != nil {
			t.Fatalf("new formatter: %v", err)
		}
		defer f.Close()
		s, err := f.FormatString()
		if err != nil {
			t.Fatalf("format screen: %v", err)
		}
		return s
	}
	lines := strings.Split(format(lg.FormatterFormatPlain), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, format(lg.FormatterFormatVT)
}

func TestClientAttachNativeHintClearedOnEndAndDisconnect(t *testing.T) {
	tests := []struct {
		name    string
		output  []string
		finish  func(t *testing.T, h *nativeAttachHarness) string
		message string
		// want is the rendered screen after the attachment finished, given the
		// pre-attach screen was scrolled away by native entry.
		want []string
	}{
		{
			name:   "immediate end after hint leaves no hint on the outcome row",
			output: []string{"line-1\r\nline-2\r\n"},
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := daemon.WriteFrame(h.serverConn, daemon.FrameEnded, nil); err != nil {
					t.Fatalf("write ended frame: %v", err)
				}
				return h.wait(t, nil)
			},
			message: "Session ended",
			want:    []string{"line-1", "line-2", "", "Session ended"},
		},
		{
			name: "cursor-addressed output past the hint keeps the session content",
			// The hint lands on row 4 (cursor waits on row 3); the session then
			// draws on rows 6 and 4 by absolute addressing, so row 4 must not be
			// erased while nothing else of the hint remains.
			output: []string{"line-1\r\nline-2\r\n", "\x1b[6;1Hstatus\x1b[4;1H\x1b[2Koverwrote-hint"},
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := h.serverConn.Close(); err != nil {
					t.Fatalf("close attach server: %v", err)
				}
				return h.wait(t, io.EOF)
			},
			message: "Disconnected from session",
			want:    []string{"line-1", "line-2", "", "overwrote-hint", "", "status", "Disconnected from session"},
		},
		{
			name:   "cursor-addressed output below the hint still clears the hint row",
			output: []string{"line-1\r\nline-2\r\n", "\x1b[8;1Hfooter"},
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := daemon.WriteFrame(h.serverConn, daemon.FrameEnded, nil); err != nil {
					t.Fatalf("write ended frame: %v", err)
				}
				return h.wait(t, nil)
			},
			message: "Session ended",
			want:    []string{"line-1", "line-2", "", "", "", "", "", "footer", "Session ended"},
		},
		{
			name: "partial overwrite without an erase keeps only the session's characters",
			// The session writes over the start of the hint row without
			// erasing it, so the hint's tail and background would otherwise
			// survive next to the session's text.
			output: []string{"line-1\r\nline-2\r\n", "\x1b[4;1Hpartial"},
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := h.serverConn.Close(); err != nil {
					t.Fatalf("close attach server: %v", err)
				}
				return h.wait(t, io.EOF)
			},
			message: "Disconnected from session",
			want:    []string{"line-1", "line-2", "", "partial", "Disconnected from session"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startNativeAttach(t, bgx.WithDetachInstructions())
			expectResizeFrame(t, h.frames, 24, 80)
			h.writeOutput(t, tt.output[0])
			h.waitForOutput(t, `detach: ctrl+\`)
			for _, payload := range tt.output[1:] {
				h.writeOutput(t, payload)
				h.waitForOutput(t, payload)
			}
			output := tt.finish(t, h)

			if !strings.HasSuffix(output, tt.message+"\r\n") {
				t.Fatalf("native output = %q, want outcome message last", output)
			}
			assertNativeOutputNeverIsolatesOrResets(t, output)
			got, styled := renderTranscript(t, output, 80, 24)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("rendered screen:\n%q\nwant:\n%q\ntranscript %q", got, tt.want, output)
			}
			if strings.Contains(styled, "48;5;236") {
				t.Fatalf("hint background survived on the final screen: %q", styled)
			}
		})
	}
}

// TestClientAttachNativeHintBeforeAlternateScreenIsCleanedUp draws the hint on
// the primary buffer, has the session enter its alternate screen, and checks
// that every lifecycle exit restores the session's primary content with no
// trace of the hint, a clean outcome line, and intact scrollback.
func TestClientAttachNativeHintBeforeAlternateScreenIsCleanedUp(t *testing.T) {
	const primary = "shell$ ls\r\nfile-a\r\nfile-b\r\nshell$ vim"
	const enterAlt = "\x1b[?1049h\x1b[2J\x1b[HEDITOR\x1b[?25l"
	tests := []struct {
		name string
		// exitAlt is session output leaving the alternate screen before the
		// lifecycle event, or empty when the session is still in it.
		exitAlt string
		finish  func(t *testing.T, h *nativeAttachHarness) string
		message string
	}{
		{
			name: "detach while in alternate screen",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if _, err := h.input.Write([]byte{0x1C}); err != nil {
					t.Fatalf("write detach key: %v", err)
				}
				if frame := nextFrame(t, h.frames); frame.tag != daemon.FrameDetach {
					t.Fatalf("frame tag = %v, want FrameDetach", frame.tag)
				}
				return h.wait(t, nil)
			},
			message: "Detached from session",
		},
		{
			name: "session end while in alternate screen",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := daemon.WriteFrame(h.serverConn, daemon.FrameEnded, nil); err != nil {
					t.Fatalf("write ended frame: %v", err)
				}
				return h.wait(t, nil)
			},
			message: "Session ended",
		},
		{
			name: "disconnect while in alternate screen",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := h.serverConn.Close(); err != nil {
					t.Fatalf("close attach server: %v", err)
				}
				return h.wait(t, io.EOF)
			},
			message: "Disconnected from session",
		},
		{
			name:    "session end after leaving alternate screen itself",
			exitAlt: "\x1b[?1049l\x1b[?25h",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := daemon.WriteFrame(h.serverConn, daemon.FrameEnded, nil); err != nil {
					t.Fatalf("write ended frame: %v", err)
				}
				return h.wait(t, nil)
			},
			message: "Session ended",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startNativeAttach(t, bgx.WithDetachInstructions())
			expectResizeFrame(t, h.frames, 24, 80)
			h.writeOutput(t, primary)
			h.waitForOutput(t, `detach: ctrl+\`)
			h.writeOutput(t, enterAlt)
			h.waitForOutput(t, "EDITOR")
			if tt.exitAlt != "" {
				h.writeOutput(t, tt.exitAlt)
				h.waitForOutput(t, tt.exitAlt)
			}
			output := tt.finish(t, h)

			if !strings.HasSuffix(output, tt.message+"\r\n") {
				t.Fatalf("native output = %q, want outcome message last", output)
			}
			// The session's own alternate-screen entry is forwarded verbatim,
			// so only the client's own take-over is forbidden here.
			if strings.Count(output, "\x1b[?1049h") != 1 {
				t.Fatalf("native output = %q, want exactly the session's alternate-screen entry", output)
			}
			for _, forbidden := range []string{"\x1bc", "\x1b[3J"} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("native attach wrote %q", forbidden)
				}
			}
			got, styled := renderTranscript(t, output, 80, 24)
			// The primary buffer is back with the shell's content, the hint
			// row (row 5) is blank again, and the outcome follows the prompt.
			want := []string{"shell$ ls", "file-a", "file-b", "shell$ vim", tt.message}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("rendered screen:\n%q\nwant:\n%q\ntranscript %q", got, want, output)
			}
			if strings.Contains(styled, "48;5;236") || strings.Contains(styled, "EDITOR") {
				t.Fatalf("alternate screen or hint styling survived on the final screen: %q", styled)
			}
			assertLifecycleRestoresHistoryAndPrintsFinalState(
				t,
				output,
				80,
				24,
				[]string{"shell$ ls", "file-a", "file-b", "shell$ vim"},
				tt.message,
			)
		})
	}
}

func TestClientAttachNativeShowsDetachHintOnceWithoutScrollRegion(t *testing.T) {
	h := startNativeAttach(t, bgx.WithDetachInstructions())
	expectResizeFrame(t, h.frames, 24, 80)

	h.writeOutput(t, "line-1\r\nline-2\r\n")
	h.waitForOutput(t, `detach: ctrl+\`)
	h.writeOutput(t, "line-3\r\n")
	h.waitForOutput(t, "line-3")
	if _, err := h.input.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	output := h.wait(t, nil)

	if got := strings.Count(output, `detach: ctrl+\`); got != 1 {
		t.Fatalf("detach hint shown %d times, want exactly once; output %q", got, output)
	}
	// The hint is drawn on the first row the session has not reached (the
	// cursor waits on row 3 after two lines), with the cursor saved and
	// restored around it.
	wantHint := "\x1b7\x1b[4;1H\x1b[48;5;236;38;5;250m\x1b[2K detach: ctrl+\\ \x1b[0m\x1b8"
	if !strings.Contains(output, wantHint) {
		t.Fatalf("native output %q lacks one-time hint %q", output, wantHint)
	}
	if !strings.Contains(output, "\x1b]0;") {
		t.Fatalf("native output %q lacks the best-effort title hint", output)
	}
	// Session bytes keep flowing raw after the hint; nothing confines them.
	if !strings.Contains(output, wantHint+"line-3\r\n") {
		t.Fatalf("native output %q did not forward output raw after the hint", output)
	}
	// The only scroll-region write is the full reset during cleanup.
	if strings.Count(output, "\x1b[r") != 1 || strings.Contains(output, ";24r") {
		t.Fatalf("native output %q set a scroll region", output)
	}
	assertNativeOutputNeverIsolatesOrResets(t, output)
}

// daemonSnapshot renders the output frame a daemon sends on attach or
// resynchronization for a session that has produced sessionOutput.
func daemonSnapshot(t *testing.T, sessionOutput string) string {
	t.Helper()
	term, err := vt.New(80, 24)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	defer term.Close()
	if _, err := term.Write([]byte(sessionOutput)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	snapshot, err := term.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return string(snapshot)
}

func TestClientAttachAutoTransitionsResizeAndForwardRawBeforeSwitch(t *testing.T) {
	h := startModeAttach(t, bgx.AttachModeAuto, bgx.WithDetachInstructions())
	// Nothing is reserved until the first frame reveals the active screen.
	expectResizeFrame(t, h.frames, 24, 80)

	h.writeOutput(t, daemonSnapshot(t, "shell$ "))
	h.waitForOutput(t, "shell$ ")

	// Bytes up to the alternate-screen entry stream raw; the entry itself is
	// replaced by the protected isolated screen with a reserved hint row.
	h.writeOutput(t, "ls\r\n\x1b[?1049h\x1b[2J\x1b[HEDITOR")
	expectResizeFrame(t, h.frames, 23, 80)
	h.waitForOutput(t, "EDITOR")
	output, _, _ := h.terminal.snapshot()
	entered := strings.Index(output, "\x1b[?1049h\x1b[2J\x1b[H")
	if entered < 0 || !strings.HasSuffix(output[:entered], "ls\r\n"+nativeModeReset) {
		t.Fatalf("output %q: raw bytes before the switch were not forwarded ahead of isolation", output)
	}
	if strings.Contains(output[:entered], "EDITOR") {
		t.Fatalf("output %q forwarded alternate-screen bytes raw", output)
	}
	if !strings.Contains(output[entered:], "\x1b[1;23r") {
		t.Fatalf("output %q did not reserve the hint row while isolated", output)
	}

	// Leaving the alternate screen returns to native at full height and
	// repaints the primary screen from the model instead of forwarding.
	h.writeOutput(t, "\x1b[?1049lshell$ ")
	expectResizeFrame(t, h.frames, 24, 80)
	h.waitForOutput(t, "\x1b[?1049l\x1b[r\x1b[2J\x1b[H\x1b[0m")

	if _, err := h.input.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	output = h.wait(t, nil)
	if got := strings.Count(output, "\x1b[?1049h"); got != 1 {
		t.Fatalf("alternate screen entered %d times, want once; output %q", got, output)
	}
	for _, forbidden := range []string{"\x1bc", "\x1b[3J"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("auto attach wrote %q", forbidden)
		}
	}
	// Cleanup follows the presentation at exit: native mode restoration
	// without leaving an alternate screen the terminal is no longer on.
	leftAlt := strings.LastIndex(output, "\x1b[?1049l")
	if !strings.Contains(output[leftAlt:], "\x1b7\x1b[r\x1b8") {
		t.Fatalf("output %q lacks native cleanup after returning to the primary screen", output)
	}
	if !strings.HasSuffix(output, "Detached from session\r\n") {
		t.Fatalf("output %q lacks the detach outcome", output)
	}
	got, styled := renderTranscript(t, output, 80, 24)
	want := []string{"shell$ ls", "shell$", "Detached from session"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rendered screen:\n%q\nwant:\n%q\ntranscript %q", got, want, output)
	}
	if strings.Contains(styled, "EDITOR") || strings.Contains(styled, "48;5;236") {
		t.Fatalf("alternate screen or hint styling survived on the final screen: %q", styled)
	}
	assertLifecycleRestoresHistoryAndPrintsFinalState(t, output, 80, 24, []string{"shell$ ls", "shell$ "}, "Detached from session")
}

func TestClientAttachAutoStartedInAlternateScreenRestoresPrimaryOnExit(t *testing.T) {
	h := startModeAttach(t, bgx.AttachModeAuto, bgx.WithDetachInstructions())
	expectResizeFrame(t, h.frames, 24, 80)

	h.writeOutput(t, daemonSnapshot(t, "shell$ ls\r\nfile-a\r\nshell$ vim\x1b[?1049h\x1b[2J\x1b[HEDITOR"))
	expectResizeFrame(t, h.frames, 23, 80)
	h.waitForOutput(t, "EDITOR")
	output, _, _ := h.terminal.snapshot()
	if !strings.HasPrefix(output, "\x1b[?1049h\x1b[2J\x1b[H") {
		t.Fatalf("output %q did not isolate up front for a session on the alternate screen", output)
	}

	h.writeOutput(t, "\x1b[?1049l\r\nshell$ ")
	expectResizeFrame(t, h.frames, 24, 80)
	h.waitForOutput(t, "shell$ vim")

	if err := h.serverConn.Close(); err != nil {
		t.Fatalf("close server conn: %v", err)
	}
	output = h.wait(t, io.EOF)
	if !strings.HasSuffix(output, "Disconnected from session\r\n") {
		t.Fatalf("output %q lacks the disconnect outcome", output)
	}
	got, styled := renderTranscript(t, output, 80, 24)
	want := []string{"shell$ ls", "file-a", "shell$ vim", "shell$", "Disconnected from session"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rendered screen:\n%q\nwant:\n%q\ntranscript %q", got, want, output)
	}
	if strings.Contains(styled, "EDITOR") {
		t.Fatalf("alternate screen survived on the final screen: %q", styled)
	}
	assertLifecycleRestoresHistoryAndPrintsFinalState(
		t, output, 80, 24, []string{"shell$ ls", "file-a", "shell$ vim", "shell$ "}, "Disconnected from session",
	)
}

// physicalModes replays a client transcript onto a fresh terminal and returns
// the explicit set/reset sequences describing its resulting tracked modes.
func physicalModes(t *testing.T, transcript string) string {
	t.Helper()
	term, err := vt.New(80, 24)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	defer term.Close()
	if _, err := term.Write([]byte(transcript)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return string(term.ModeSequences())
}

func TestClientAttachAutoIsolationWithdrawsNativelyForwardedModes(t *testing.T) {
	const enableModes = "\x1b[?1h\x1b[?1000h\x1b[?1002h\x1b[?1006h\x1b[?1004h\x1b[?2004h"
	const wantReset = "\x1b[?1l\x1b[?1000l\x1b[?1002l\x1b[?1004l\x1b[?1006l\x1b[?2004l"
	tests := []struct {
		name   string
		finish func(t *testing.T, h *nativeAttachHarness) string
	}{
		{
			name: "detach",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if _, err := h.input.Write([]byte{0x1C}); err != nil {
					t.Fatalf("write detach key: %v", err)
				}
				return h.wait(t, nil)
			},
		},
		{
			name: "session end",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := daemon.WriteFrame(h.serverConn, daemon.FrameEnded, nil); err != nil {
					t.Fatalf("write ended frame: %v", err)
				}
				return h.wait(t, nil)
			},
		},
		{
			name: "disconnect",
			finish: func(t *testing.T, h *nativeAttachHarness) string {
				if err := h.serverConn.Close(); err != nil {
					t.Fatalf("close server conn: %v", err)
				}
				return h.wait(t, io.EOF)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startModeAttach(t, bgx.AttachModeAuto)
			expectResizeFrame(t, h.frames, 24, 80)
			h.writeOutput(t, daemonSnapshot(t, "shell$ "))
			h.waitForOutput(t, "shell$ ")

			// A primary-buffer program enables reporting modes, which stream
			// raw to the physical terminal, then a TUI takes the alternate
			// screen and the attachment ends while isolated.
			h.writeOutput(t, enableModes+"picker> ")
			h.waitForOutput(t, "picker> ")
			if !strings.Contains(physicalModes(t, h.terminal.output.String()), "\x1b[?1000h") {
				t.Fatal("modes were not forwarded natively before isolation")
			}
			h.writeOutput(t, "\x1b[?1049h\x1b[2J\x1b[HEDITOR")
			h.waitForOutput(t, "EDITOR")

			output := tt.finish(t, h)
			if strings.Contains(output, "\x1bc") || strings.Contains(output, "\x1b[3J") {
				t.Fatalf("cleanup reset the terminal or cleared scrollback: %q", output)
			}
			modes := physicalModes(t, output)
			if !strings.Contains(modes, "\x1b[?25h") {
				t.Fatalf("cursor left hidden after %s; modes %q output %q", tt.name, modes, output)
			}
			for _, want := range strings.SplitAfter(wantReset, "l") {
				if want == "" {
					continue
				}
				if !strings.Contains(modes, want) {
					t.Fatalf("physical terminal modes %q after %s lack %q; output %q", modes, tt.name, want, output)
				}
			}
		})
	}
}

func TestClientAttachAutoReturnToNativeRestoresSessionModes(t *testing.T) {
	h := startModeAttach(t, bgx.AttachModeAuto)
	expectResizeFrame(t, h.frames, 24, 80)
	h.writeOutput(t, daemonSnapshot(t, "shell$ "))
	h.waitForOutput(t, "shell$ ")

	h.writeOutput(t, "\x1b[?2004h\x1b[?1h\x1b[?1049h\x1b[2J\x1b[HEDITOR")
	h.waitForOutput(t, "EDITOR")
	isolated, _, _ := h.terminal.snapshot()
	if modes := physicalModes(t, isolated); strings.Contains(modes, "\x1b[?2004h") || strings.Contains(modes, "\x1b[?1h") {
		t.Fatalf("session modes stayed enabled while isolated: %q", modes)
	}

	// The session keeps bracketed paste and application cursor keys enabled
	// when it leaves the alternate screen, so native presentation must bring
	// the physical terminal back into agreement with it.
	h.writeOutput(t, "\x1b[?1049lshell$ ")
	h.waitForOutput(t, "\x1b[?1049l\x1b[r\x1b[2J\x1b[H\x1b[0m")
	deadline := time.Now().Add(2 * time.Second)
	for {
		native, _, _ := h.terminal.snapshot()
		modes := physicalModes(t, native)
		if strings.Contains(modes, "\x1b[?2004h") && strings.Contains(modes, "\x1b[?1h") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session modes not restored after returning to native: %q output %q", modes, native)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := h.input.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	output := h.wait(t, nil)
	if modes := physicalModes(t, output); strings.Contains(modes, "\x1b[?2004h") || strings.Contains(modes, "\x1b[?1h") {
		t.Fatalf("native detach left session modes enabled: %q", modes)
	}
}

// assertUnrestrictedScrolling replays a transcript and then writes past the
// bottom row, which only scrolls the whole screen when no DECSTBM region was
// left behind.
func assertUnrestrictedScrolling(t *testing.T, transcript string) {
	t.Helper()
	term, err := vt.New(80, 24)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	defer term.Close()
	if _, err := term.Write([]byte(transcript + "\x1b[24;1Hbottom-probe\r\nnext-probe")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	screen, err := term.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen: %v", err)
	}
	if !strings.Contains(string(screen), "bottom-probe") || !strings.Contains(string(screen), "next-probe") {
		t.Fatalf("scrolling is restricted after cleanup; screen %q transcript %q", screen, transcript)
	}
}

func TestClientAttachIsolatedDetachAfterNativePhaseRestoresCursorAndScrolling(t *testing.T) {
	h := startModeAttach(t, bgx.AttachModeAuto, bgx.WithDetachInstructions())
	expectResizeFrame(t, h.frames, 24, 80)
	h.writeOutput(t, daemonSnapshot(t, "shell$ "))
	h.waitForOutput(t, "shell$ ")

	// The primary-buffer program hides the cursor before a TUI takes the
	// alternate screen, and the user detaches while it is showing.
	h.writeOutput(t, "\x1b[?25lls\r\n")
	h.waitForOutput(t, "ls\r\n")
	h.writeOutput(t, "\x1b[?1049h\x1b[2J\x1b[HEDITOR")
	expectResizeFrame(t, h.frames, 23, 80)
	h.waitForOutput(t, "EDITOR")

	if _, err := h.input.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	output := h.wait(t, nil)
	if !strings.HasSuffix(output, "Detached from session\r\n") {
		t.Fatalf("output %q lacks the detach outcome", output)
	}
	if !strings.Contains(physicalModes(t, output), "\x1b[?25h") {
		t.Fatalf("cursor left hidden after isolated detach; output %q", output)
	}
	assertUnrestrictedScrolling(t, output)
	got, styled := renderTranscript(t, output, 80, 24)
	if len(got) == 0 || got[0] != "shell$ ls" || got[len(got)-1] != "Detached from session" {
		t.Fatalf("rendered screen %q, want the primary content first and the outcome last", got)
	}
	if strings.Contains(styled, "EDITOR") || strings.Contains(styled, "48;5;236") {
		t.Fatalf("alternate screen or hint styling survived on the final screen: %q", styled)
	}
	// The session left the cursor on the row below "ls", so the outcome is
	// printed from that preserved position.
	assertLifecycleRestoresHistoryAndPrintsFinalState(t, output, 80, 24, []string{"shell$ ls", ""}, "Detached from session")
}

func TestClientAttachIsolatedDetachReleasesReservedScrollRegion(t *testing.T) {
	h := startModeAttach(t, bgx.AttachModeIsolated, bgx.WithDetachInstructions())
	expectResizeFrame(t, h.frames, 23, 80)
	h.writeOutput(t, daemonSnapshot(t, "shell$ "))
	h.waitForOutput(t, "\x1b[1;23r")

	if _, err := h.input.Write([]byte{0x1C}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	output := h.wait(t, nil)
	if !strings.HasSuffix(output, "Detached from session\r\n") {
		t.Fatalf("output %q lacks the detach outcome", output)
	}
	assertUnrestrictedScrolling(t, output)
	assertLifecycleRestoresHistoryAndPrintsFinalState(t, output, 80, 24, nil, "Detached from session")
}

type peerDetachEOFStream struct {
	*closeEOFStream
	writeReleased chan struct{}
	releaseOnce   sync.Once
}

func (s *peerDetachEOFStream) Write(p []byte) (int, error) {
	n, err := s.closeEOFStream.Write(p)
	if len(p) == 5 && p[0] == byte(daemon.FrameDetach) {
		// The peer can close after receiving detach, before Write returns.
		if closeErr := s.closeEOFStream.Close(); closeErr != nil {
			return n, closeErr
		}
		<-s.writeReleased
	}
	return n, err
}

func (s *peerDetachEOFStream) Close() error {
	s.releaseOnce.Do(func() { close(s.writeReleased) })
	return s.closeEOFStream.Close()
}

func TestClientAttachDetachHandlesPeerEOFBeforeWriteReturns(t *testing.T) {
	unexpected := errors.New("unexpected peer read failure")
	for _, readErr := range []error{nil, unexpected} {
		name := "EOF"
		if readErr != nil {
			name = "EOF with unrelated error"
		}
		t.Run(name, func(t *testing.T) {
			stream := &peerDetachEOFStream{
				closeEOFStream: newCloseEOFStream(),
				writeReleased:  make(chan struct{}),
			}
			stream.readErr = readErr
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
				return stream, nil
			}).Attach(ctx, newTestTerminal(bytes.NewReader([]byte{0x1C})))
			if ctx.Err() != nil {
				t.Fatalf("Attach did not complete before timeout: %v", err)
			}
			if readErr == nil && err != nil {
				t.Fatalf("Attach after detach: %v", err)
			}
			if readErr != nil && !errors.Is(err, readErr) {
				t.Fatalf("Attach error = %v, want %v", err, readErr)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("Attach retained expected detach EOF: %v", err)
			}
		})
	}
}
