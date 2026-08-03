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
	"sync"
	"sync/atomic"

	"github.com/sidedotdev/bgx/daemon"
)

type attachConfig struct {
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

	if err := writeOut("\x1b[2J\x1b[H"); err != nil {
		return err
	}

	var view *attachView
	var detached, sessionEnded atomic.Bool
	var remoteStreamTerminated bool
	defer func() {
		if view != nil {
			if err := view.close(); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
		if (sessionEnded.Load() || remoteStreamTerminated) && !detached.Load() {
			if view != nil {
				if row := view.reservedRow(); row > 0 {
					if err := writeOut(fmt.Sprintf("\x1b7\x1b[r\x1b[%d;1H\x1b[2K\x1b8", row)); err != nil {
						retErr = errors.Join(retErr, err)
					}
				}
			}
			if err := writeOut("\x1b[?25h\x1b[0m"); err != nil {
				retErr = errors.Join(retErr, err)
			}
			return
		}
		if err := writeOut("\x1bc"); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	if cfg.showDetachInstructions {
		cols, rows, err := terminal.Size()
		if err != nil && !errors.Is(err, ErrTerminalSizeUnavailable) {
			return fmt.Errorf("attach: read terminal size: %w", err)
		}
		if err == nil && cols > 0 && rows > 0 {
			view, err = newAttachView(writeOut, cols, rows)
			if err != nil {
				return err
			}
		}
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
		if view != nil {
			rows, err = view.setSize(cols, rows)
			if err != nil {
				return err
			}
			if rows == 0 {
				return nil
			}
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
				if view != nil {
					err = view.feed(payload)
				} else {
					err = writeBytes(payload)
				}
			case daemon.FrameResync:
				if view != nil {
					err = view.resync(payload)
				} else {
					err = writeBytes(payload)
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
		buf := make([]byte, 64<<10)
		var scanner detachScanner
		for {
			n, err := terminal.ReadContext(attachCtx, buf)
			if n > 0 {
				forward, detach := scanner.feed(buf[:n])
				if len(forward) > 0 {
					if sendErr := send(daemon.FrameInput, forward); sendErr != nil {
						inputErr <- sendErr
						return
					}
				}
				if detach {
					if sendErr := send(daemon.FrameDetach, nil); sendErr != nil {
						inputErr <- sendErr
						return
					}
					detached.Store(true)
					if err != nil && !errors.Is(err, io.EOF) {
						inputErr <- errors.Join(
							errAttachDetached,
							fmt.Errorf("attach: read terminal: %w", err),
						)
					} else {
						inputErr <- errAttachDetached
					}
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					inputErr <- nil
				} else {
					inputErr <- fmt.Errorf("attach: read terminal: %w", err)
				}
				return
			}
		}
	}()

	var viewErr <-chan error
	if view != nil {
		viewErr = view.errs
	}

	var result error
	var resizeErrSelected, frameDone, inputDone bool
	select {
	case <-ctx.Done():
		result = ctx.Err()
	case err := <-resizeErr:
		result = err
		resizeErrSelected = true
	case err := <-frameErr:
		result = err
		frameDone = true
	case err := <-inputErr:
		result = err
		inputDone = true
	case err := <-viewErr:
		result = err
	}

	joinWorkerError := func(err error, shuttingDown bool) {
		err = withoutExpectedAttachShutdownErrors(err, shuttingDown)
		if err != nil {
			result = errors.Join(result, err)
		}
	}
	collectReady := func(worker <-chan error, done *bool) {
		if *done {
			return
		}
		select {
		case err := <-worker:
			joinWorkerError(err, false)
			*done = true
		default:
		}
	}

	collectReady(frameErr, &frameDone)
	collectReady(inputErr, &inputDone)
	remoteStreamTerminated = frameDone

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
		joinWorkerError(<-frameErr, true)
	}
	if !inputDone {
		joinWorkerError(<-inputErr, true)
	}
	return result
}
