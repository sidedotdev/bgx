package bgx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/sidedotdev/bgx/daemon"
)

type attachConfig struct {
	showDetachInstructions bool
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
func (c *Client) Attach(ctx context.Context, terminal Terminal, options ...AttachOption) error {
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
	defer conn.Close()

	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stopCancellation()

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

	return runTerminalAttach(ctx, conn, br, terminal, cfg)
}

func runTerminalAttach(ctx context.Context, conn io.Writer, frames io.Reader, terminal Terminal, cfg attachConfig) error {
	if err := terminal.EnterRaw(); err != nil {
		return fmt.Errorf("attach: enter raw mode: %w", err)
	}
	defer terminal.Restore()

	var outMu sync.Mutex
	writeOut := func(s string) {
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = io.WriteString(terminal, s)
	}
	writeBytes := func(p []byte) {
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = terminal.Write(p)
	}

	writeOut("\x1b[2J\x1b[H")

	var view *attachView
	if cfg.showDetachInstructions {
		if cols, rows, err := terminal.Size(); err == nil && cols > 0 && rows > 0 {
			view = newAttachView(writeOut, cols, rows)
		}
	}

	var detached, sessionEnded atomic.Bool
	defer func() {
		if view != nil {
			view.close()
		}
		if sessionEnded.Load() && !detached.Load() {
			if view != nil {
				if row := view.reservedRow(); row > 0 {
					writeOut(fmt.Sprintf("\x1b7\x1b[r\x1b[%d;1H\x1b[2K\x1b8", row))
				}
			}
			writeOut("\x1b[?25h\x1b[0m")
			return
		}
		writeOut("\x1bc")
	}()

	var writeMu sync.Mutex
	send := func(tag daemon.FrameTag, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return daemon.WriteFrame(conn, tag, payload)
	}
	sendSize := func() {
		cols, rows, err := terminal.Size()
		if err != nil || cols == 0 || rows == 0 {
			return
		}
		if view != nil {
			rows = view.setSize(cols, rows)
			if rows == 0 {
				return
			}
		}
		_ = send(daemon.FrameResize, daemon.EncodeResize(rows, cols))
	}
	sendSize()

	attachCtx, stopAttach := context.WithCancel(ctx)
	defer stopAttach()

	resizes := terminal.ResizeEvents(attachCtx)
	go func() {
		for range resizes {
			sendSize()
		}
	}()

	frameDone := make(chan struct{})
	go func() {
		defer close(frameDone)
		for {
			tag, payload, err := daemon.ReadFrame(frames)
			if err != nil {
				return
			}
			switch tag {
			case daemon.FrameOutput:
				if view != nil {
					view.feed(payload)
				} else {
					writeBytes(payload)
				}
			case daemon.FrameResync:
				if view != nil {
					view.resync(payload)
				} else {
					writeBytes(payload)
				}
			case daemon.FrameResize:
				sendSize()
			case daemon.FrameEnded:
				sessionEnded.Store(true)
				return
			}
		}
	}()

	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, 64<<10)
		var scanner detachScanner
		for {
			n, err := terminal.Read(buf)
			if n > 0 {
				forward, detach := scanner.feed(buf[:n])
				if len(forward) > 0 {
					if send(daemon.FrameInput, forward) != nil {
						return
					}
				}
				if detach {
					detached.Store(true)
					_ = send(daemon.FrameDetach, nil)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-frameDone:
	case <-inputDone:
	}
	return nil
}
