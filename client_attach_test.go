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

	assertTerminalStatePreserved(t, output, 100, 30)
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

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
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
	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
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

	err := bgx.NewClient(dial).Attach(context.Background(), terminal)
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
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
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
			bgx.WithDetachInstructions(),
		)
	}()

	expectResizeFrame(t, frames, 23, 80)
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
	repaint := strings.LastIndex(output, "\x1b[2J\x1b[H\x1b[0m")
	if leaveAlt < 0 || repaint < leaveAlt {
		t.Fatalf("terminal output = %q, want final session state repainted after leaving alternate screen", output)
	}
	if !strings.Contains(output[repaint:], "\x1b[?25h\x1b[0m") {
		t.Fatalf("terminal output = %q, want cursor and style cleanup", output)
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
		result <- bgx.NewClient(dial).Attach(context.Background(), terminal)
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
