package main

// The interactive attach bridge (raw mode, ctrl+\ detach, resize forwarding) is
// ported from zmx (https://github.com/neurosnap/zmx); see LICENSE-zmx for its
// license.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/sidedotdev/bgx/daemon"
	cli "github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// attachAction connects to a running session, replays its current screen, and
// bridges the local terminal to the session's PTY until the user detaches with
// ctrl+\ (which keeps the session running).
func attachAction(_ context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "attach: an id is required")
	}
	info, ok := liveInfo(id)
	if !ok || !info.Running {
		return failAttachUnavailable(id, ok && !info.Running)
	}

	conn, err := net.Dial("unix", socketPath(id))
	if err != nil {
		// The session can end between the liveness check and the dial.
		return failAttachUnavailable(id, false)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(daemon.Request{Op: "attach"}); err != nil {
		return failJSON(codeAttachFailed, "attach: %v", err)
	}
	// Read exactly the response line so its trailing newline is consumed before
	// the connection switches to binary frames.
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return failJSON(codeAttachFailed, "attach: %v", err)
	}
	var resp daemon.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return failJSON(codeAttachFailed, "attach: %v", err)
	}
	if !resp.OK {
		return failJSON(codeAttachFailed, "attach: %s", resp.Error)
	}

	return runAttach(conn, br, cmd.Bool("show-detach-instructions"))
}

// failAttachUnavailable reports the distinct reason a session cannot be
// attached to: it either already ended (a daemon answered as not running, or a
// persisted ended record exists) or it never existed.
func failAttachUnavailable(id string, knownEnded bool) error {
	if knownEnded {
		return failJSON(codeSessionEnded, "attach: session %q has already ended", id)
	}
	if _, ended := endedRecord(id); ended {
		return failJSON(codeSessionEnded, "attach: session %q has already ended", id)
	}
	return failJSON(codeSessionNotFound, "attach: session %q does not exist", id)
}

// runAttach drives the interactive bridge over an established attach
// connection: r reads inbound frames while conn is written for outbound ones.
// When showDetachInstructions is set, the bottom line of the local terminal is
// reserved for a detach hint and the session is told a one-row-shorter size.
func runAttach(conn net.Conn, r io.Reader, showDetachInstructions bool) error {
	stdinFd := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFd) {
		// Raw mode disables signal generation, so ctrl+\ arrives as a literal
		// byte we can intercept as the detach key instead of raising SIGQUIT.
		old, err := term.MakeRaw(stdinFd)
		if err == nil {
			defer term.Restore(stdinFd, old)
		}
	}

	// stdout is shared between the frame reader, the view painter and the exit
	// reset, so writes are serialized to keep escape sequences intact.
	var outMu sync.Mutex
	writeOut := func(s string) {
		outMu.Lock()
		defer outMu.Unlock()
		os.Stdout.WriteString(s)
	}

	writeOut("\x1b[2J\x1b[H")

	// Reserving a line means rendering session output locally instead of
	// forwarding it, which requires knowing the terminal size.
	var view *attachView
	if showDetachInstructions && term.IsTerminal(stdinFd) {
		if cols, rows, err := term.GetSize(stdinFd); err == nil && cols > 0 && rows > 0 {
			view = newAttachView(writeOut, uint16(cols), uint16(rows))
		}
	}

	var detached, sessionEnded atomic.Bool
	// The exit reset depends on the cause: a ctrl+\ detach does a full terminal
	// reset to clear the session state the replay left on the local screen,
	// while a session end resets only the cursor so the final rendered output
	// stays visible. A plain connection error falls back to a full reset.
	defer func() {
		// Stop painting before the exit sequences so no repaint lands after them.
		if view != nil {
			view.close()
		}
		if sessionEnded.Load() && !detached.Load() {
			// Release the scroll region and clear the reserved line so the
			// detach hint doesn't linger after the session's final output,
			// restoring the cursor so the final state matches a flagless run.
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
		cols, rows, err := term.GetSize(stdinFd)
		if err != nil || cols <= 0 || rows <= 0 {
			return
		}
		if view != nil {
			rows = int(view.setSize(uint16(cols), uint16(rows)))
			if rows <= 0 {
				return
			}
		}
		_ = send(daemon.FrameResize, daemon.EncodeResize(uint16(rows), uint16(cols)))
	}
	sendSize()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			sendSize()
		}
	}()

	readErr := make(chan struct{})
	go func() {
		defer close(readErr)
		for {
			tag, payload, err := daemon.ReadFrame(r)
			if err != nil {
				return
			}
			switch tag {
			case daemon.FrameOutput:
				if view != nil {
					view.feed(payload)
					break
				}
				outMu.Lock()
				os.Stdout.Write(payload)
				outMu.Unlock()
			case daemon.FrameResync:
				if view != nil {
					view.resync(payload)
					break
				}
				outMu.Lock()
				os.Stdout.Write(payload)
				outMu.Unlock()
			case daemon.FrameResize:
				sendSize()
			case daemon.FrameEnded:
				sessionEnded.Store(true)
				return
			}
		}
	}()

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 64<<10)
		var scanner detachScanner
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				forward, detach := scanner.feed(buf[:n])
				if len(forward) > 0 {
					if werr := send(daemon.FrameInput, forward); werr != nil {
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
	case <-readErr:
	case <-stdinDone:
	}
	return nil
}
