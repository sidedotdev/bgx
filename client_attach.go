package bgx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sidedotdev/bgx/daemon"
	"github.com/sidedotdev/bgx/vt"
)

type attachConfig struct {
	mode                   AttachMode
	showDetachInstructions bool
}

type attachStreamCloser struct {
	once sync.Once
	err  error
}

func (c *attachStreamCloser) close(conn io.Closer) {
	c.once.Do(func() {
		c.err = conn.Close()
	})
}

// AttachOption configures an interactive attachment.
type AttachOption func(*attachConfig)

// WithDetachInstructions reserves the terminal's bottom row for the detach
// key hint when its dimensions are available.
func WithDetachInstructions() AttachOption {
	return func(cfg *attachConfig) {
		cfg.showDetachInstructions = true
	}
}

// WithAttachMode selects the presentation mode; the zero value means
// AttachModeAuto. Unknown modes make Attach fail with an AttachOptionsError
// before it touches the terminal or the session.
func WithAttachMode(mode AttachMode) AttachOption {
	return func(cfg *attachConfig) {
		cfg.mode = mode
	}
}

// Attach connects terminal to the session until it ends, the user detaches, the
// terminal input closes, or ctx is canceled.
func (c *Client) Attach(ctx context.Context, terminal Terminal, options ...AttachOption) (retErr error) {
	const operation = "attach"
	if terminal == nil {
		return errors.New("attach: terminal is nil")
	}
	if c == nil || c.dial == nil {
		return errors.New("bgx: client has no dialer")
	}

	var cfg attachConfig
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	mode, err := cfg.mode.normalized()
	if err != nil {
		return &AttachOptionsError{Err: err}
	}
	cfg.mode = mode

	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("%s: dial: %w", operation, err)
	}
	if conn == nil {
		return &ProtocolError{
			Operation: operation,
			Err:       errors.New("dialer returned a nil stream"),
		}
	}

	var streamCloser attachStreamCloser
	stopCancellation := context.AfterFunc(ctx, func() {
		streamCloser.close(conn)
	})
	defer func() {
		stopCancellation()
		streamCloser.close(conn)
		closeErr := withoutExpectedAttachShutdownErrors(streamCloser.err, false)
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("attach: close stream: %w", closeErr))
		}
	}()

	if err := json.NewEncoder(conn).Encode(daemon.Request{Op: operation}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &ProtocolError{
			Operation: operation,
			Err:       fmt.Errorf("write request: %w", err),
		}
	}

	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &ProtocolError{
			Operation: operation,
			Err:       fmt.Errorf("decode response: %w", err),
		}
	}
	var resp daemon.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return &ProtocolError{
			Operation: operation,
			Err:       fmt.Errorf("decode response: %w", err),
		}
	}
	if !resp.OK {
		return &ResponseError{
			Operation: operation,
			Message:   resp.Error,
		}
	}

	err = runTerminalAttach(ctx, conn, br, func() {
		streamCloser.close(conn)
	}, terminal, cfg)
	return withoutAttachDetached(err)
}

var errAttachDetached = errors.New("attach detached")

func withoutAttachDetached(err error) error {
	if err == nil || err == errAttachDetached {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	children := joined.Unwrap()
	remaining := make([]error, 0, len(children))
	for _, child := range children {
		if child = withoutAttachDetached(child); child != nil {
			remaining = append(remaining, child)
		}
	}
	return errors.Join(remaining...)
}

type prunedAttachError struct {
	message string
	err     error
}

func (e *prunedAttachError) Error() string {
	return e.message
}

func (e *prunedAttachError) Unwrap() error {
	return e.err
}

func withoutExpectedAttachShutdownErrors(err error, includeEOF bool) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		remaining := make([]error, 0, len(children))
		for _, child := range children {
			if child = withoutExpectedAttachShutdownErrors(child, includeEOF); child != nil {
				remaining = append(remaining, child)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := withoutExpectedAttachShutdownErrors(wrapped.Unwrap(), includeEOF)
		if child == nil {
			return nil
		}
		return &prunedAttachError{message: err.Error(), err: child}
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		includeEOF && errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func finalAttachScreenPrefix(snapshot attachSnapshot) string {
	var b strings.Builder
	b.WriteString("\x1b[?1049l\x1b[r")
	if snapshot.physicalRows > 0 {
		fmt.Fprintf(&b, "\x1b[%d;1H", snapshot.physicalRows)
		b.WriteString(strings.Repeat("\r\n", int(snapshot.physicalRows)))
	}
	b.WriteString("\x1b[H\x1b[0m")
	return b.String()
}

func finalAttachOutcomePrefix(snapshot attachSnapshot) string {
	return "\x1b[?25h\x1b[0m" + finalAttachOutcomePosition(snapshot)
}

// finalAttachOutcomePosition moves the cursor to the first row below the
// session's final output so the outcome line never overwrites it; when the
// screen is full or its geometry unknown, the outcome scrolls in below. The
// target row is erased first, since a native detach hint may occupy it.
func finalAttachOutcomePosition(snapshot attachSnapshot) string {
	if snapshot.cols == 0 || snapshot.rows == 0 || snapshot.physicalRows == 0 {
		return "\r\n"
	}
	if snapshot.writtenRows < snapshot.physicalRows {
		return fmt.Sprintf("\x1b[%d;1H\x1b[2K", snapshot.writtenRows+1)
	}
	return fmt.Sprintf("\x1b[%d;1H\r\n", snapshot.physicalRows)
}

func runTerminalAttach(
	ctx context.Context,
	conn io.Writer,
	frames io.Reader,
	closeStream func(),
	terminal Terminal,
	cfg attachConfig,
) (retErr error) {
	if err := terminal.EnterRaw(); err != nil {
		return fmt.Errorf("attach: enter raw mode: %w", err)
	}
	defer func() {
		if err := terminal.Restore(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("attach: restore terminal: %w", err))
		}
	}()

	var outMu sync.Mutex
	writeOut := func(s string) error {
		outMu.Lock()
		defer outMu.Unlock()
		n, err := io.WriteString(terminal, s)
		if err != nil {
			return fmt.Errorf("attach: write terminal: %w", err)
		}
		if n != len(s) {
			return fmt.Errorf("attach: write terminal: %w", io.ErrShortWrite)
		}
		return nil
	}
	writeBytes := func(p []byte) error {
		outMu.Lock()
		defer outMu.Unlock()
		n, err := terminal.Write(p)
		if err != nil {
			return fmt.Errorf("attach: write terminal: %w", err)
		}
		if n != len(p) {
			return fmt.Errorf("attach: write terminal: %w", io.ErrShortWrite)
		}
		return nil
	}

	term, err := vt.New(vt.DefaultCols, vt.DefaultRows)
	if err != nil {
		return fmt.Errorf("attach: initialize terminal state: %w", err)
	}
	defer term.Close()
	models := newAttachModels(term, cfg, writeOut, writeBytes)

	var detached, sessionEnded atomic.Bool
	var remoteStreamTerminated bool
	defer func() {
		outcome := attachOutcomeAborted
		switch {
		case detached.Load():
			outcome = attachOutcomeDetached
		case sessionEnded.Load():
			outcome = attachOutcomeSessionEnded
		case remoteStreamTerminated:
			outcome = attachOutcomeDisconnected
		}
		if err := models.finish(outcome); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	if err := models.start(); err != nil {
		return err
	}

	var writeMu sync.Mutex
	send := func(tag daemon.FrameTag, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := daemon.WriteFrame(conn, tag, payload); err != nil {
			return fmt.Errorf("attach: send frame: %w", err)
		}
		return nil
	}
	sendSize := func() error {
		cols, rows, err := terminal.Size()
		if errors.Is(err, ErrTerminalSizeUnavailable) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("attach: read terminal size: %w", err)
		}
		if cols == 0 || rows == 0 {
			return nil
		}
		rows, err = models.applyResize(cols, rows)
		if err != nil {
			return err
		}
		if rows == 0 {
			return nil
		}
		return send(daemon.FrameResize, daemon.EncodeResize(rows, cols))
	}
	if err := sendSize(); err != nil {
		return err
	}

	attachCtx, stopAttach := context.WithCancel(ctx)
	defer stopAttach()

	resizeErr := make(chan error, 1)
	resizeStopped := make(chan struct{})
	resizes := terminal.ResizeEvents(attachCtx)
	go func() {
		defer close(resizeStopped)
		for range resizes {
			if err := sendSize(); err != nil {
				resizeErr <- err
				return
			}
		}
	}()

	frameErr := make(chan error, 1)
	go func() {
		for {
			tag, payload, err := daemon.ReadFrame(frames)
			if err != nil {
				frameErr <- fmt.Errorf("attach: read frame: %w", err)
				return
			}
			switch tag {
			case daemon.FrameOutput:
				var resized bool
				resized, err = models.applyOutput(payload)
				if err == nil && resized {
					err = sendSize()
				}
			case daemon.FrameResize:
				err = sendSize()
			case daemon.FrameEnded:
				sessionEnded.Store(true)
				frameErr <- nil
				return
			}
			if err != nil {
				frameErr <- err
				return
			}
		}
	}()

	inputErr := make(chan error, 1)
	go func() {
		err := runAttachInput(attachCtx, terminal, send)
		if errors.Is(err, errAttachDetached) {
			detached.Store(true)
		}
		inputErr <- err
	}()

	viewErr := models.viewErrs

	var result, frameResult error
	var resizeErrSelected, frameDone, inputDone bool
	select {
	case <-ctx.Done():
		result = ctx.Err()
	case err := <-resizeErr:
		result = err
		resizeErrSelected = true
	case frameResult = <-frameErr:
		frameDone = true
	case err := <-inputErr:
		result = err
		inputDone = true
	case err := <-viewErr:
		result = err
	}

	ctxErr := ctx.Err()
	shuttingDown := ctxErr != nil
	if shuttingDown {
		selectedErr := withoutExpectedAttachShutdownErrors(result, true)
		result = ctxErr
		if selectedErr != nil {
			result = errors.Join(result, selectedErr)
		}
	}

	joinWorkerError := func(err error, shuttingDown bool) {
		err = withoutExpectedAttachShutdownErrors(err, shuttingDown)
		if err != nil {
			result = errors.Join(result, err)
		}
	}
	if !frameDone {
		select {
		case frameResult = <-frameErr:
			frameDone = true
		default:
		}
	}
	if !inputDone {
		select {
		case err := <-inputErr:
			joinWorkerError(err, shuttingDown)
			inputDone = true
		default:
		}
	}
	remoteStreamTerminated = frameDone && !shuttingDown

	stopAttach()
	closeStream()
	<-resizeStopped
	if !resizeErrSelected {
		select {
		case err := <-resizeErr:
			joinWorkerError(err, true)
		default:
		}
	}

	if !frameDone {
		frameResult = <-frameErr
	}
	if !inputDone {
		joinWorkerError(<-inputErr, true)
	}
	// The peer may acknowledge detach by closing before the detach write returns.
	joinWorkerError(frameResult, shuttingDown || !frameDone || detached.Load())
	return result
}
