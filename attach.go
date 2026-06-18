package main

// The interactive attach bridge (raw mode, ctrl+\ detach, resize forwarding) is
// ported from zmx (https://github.com/neurosnap/zmx); see LICENSE-zmx for its
// license.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
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
		return failJSON("attach: an id is required")
	}
	info, ok := liveInfo(id)
	if !ok || !info.Running {
		return failJSON("attach: session %q is not running", id)
	}

	conn, err := net.Dial("unix", socketPath(id))
	if err != nil {
		return failJSON("attach: session %q is not running", id)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(daemon.Request{Op: "attach"}); err != nil {
		return failJSON("attach: %v", err)
	}
	// Read exactly the response line so its trailing newline is consumed before
	// the connection switches to binary frames.
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return failJSON("attach: %v", err)
	}
	var resp daemon.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return failJSON("attach: %v", err)
	}
	if !resp.OK {
		return failJSON("attach: %s", resp.Error)
	}

	return runAttach(conn, br)
}

// runAttach drives the interactive bridge over an established attach
// connection: r reads inbound frames while conn is written for outbound ones.
func runAttach(conn net.Conn, r io.Reader) error {
	stdinFd := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFd) {
		// Raw mode disables signal generation, so ctrl+\ arrives as a literal
		// byte we can intercept as the detach key instead of raising SIGQUIT.
		old, err := term.MakeRaw(stdinFd)
		if err == nil {
			defer term.Restore(stdinFd, old)
		}
	}

	os.Stdout.WriteString("\x1b[2J\x1b[H")
	// A full terminal reset on detach clears any session state the replay left
	// on the local screen.
	defer os.Stdout.WriteString("\x1bc")

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
				os.Stdout.Write(payload)
			case daemon.FrameResize:
				sendSize()
			}
		}
	}()

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				data := buf[:n]
				if isCtrlBackslash(data) {
					_ = send(daemon.FrameDetach, nil)
					return
				}
				if werr := send(daemon.FrameInput, data); werr != nil {
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
