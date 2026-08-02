package bgx

// The interactive attach bridge (raw mode, ctrl+\ detach, resize forwarding) is
// ported from zmx (https://github.com/neurosnap/zmx); see LICENSE-zmx for its
// license.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	cli "github.com/urfave/cli/v3"
)

// errAttachSocketDial marks a failure to open the attach socket connection, so
// the CLI can report the session as unavailable rather than as a protocol
// failure.
var errAttachSocketDial = errors.New("session socket dial failed")

// attachAction connects to a running session, replays its current screen, and
// bridges the local terminal to the session's PTY until the user detaches with
// ctrl+\ (which keeps the session running). With --ssh or --via the session is
// remote: a transport subprocess running `bgx bridge <id>` on the far side
// stands in for the local socket connection.
func attachAction(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "attach: an id is required")
	}
	showInstructions := cmd.Bool("show-detach-instructions")
	sshHost := cmd.String("ssh")
	var via []string
	// Argument parsing stops at the id so --via can consume every following
	// argument as the transport command; the other flags stay usable on either
	// side of the id.
	rest := cmd.Args().Slice()[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--show-detach-instructions":
			showInstructions = true
		case "--ssh":
			if i == len(rest)-1 {
				return failJSON(codeInvalidArgument, "attach: --ssh requires a host")
			}
			i++
			sshHost = rest[i]
		case "--via":
			if i == len(rest)-1 {
				return failJSON(codeInvalidArgument, "attach: --via requires a command")
			}
			via = rest[i+1:]
			i = len(rest)
		default:
			return failJSON(codeInvalidArgument, "attach: unexpected argument %q", rest[i])
		}
	}
	if sshHost != "" && len(via) > 0 {
		return failJSON(codeInvalidArgument, "attach: --ssh and --via are mutually exclusive")
	}
	if sshHost != "" {
		via = []string{"ssh", sshHost}
	}

	var dial Dialer
	var transport *transportConn
	if len(via) > 0 {
		// The transport command (e.g. ssh) runs `bgx bridge <id>` on the far
		// side, so its stdio carries the same one-connection attach protocol a
		// local socket would.
		argv := append(append([]string(nil), via...), "bgx", "bridge", id)
		dial = func(ctx context.Context) (io.ReadWriteCloser, error) {
			tc, err := startTransportProcess(ctx, argv)
			if err != nil {
				return nil, err
			}
			transport = tc
			return tc, nil
		}
	} else {
		info, ok := liveInfo(id)
		if !ok || !info.Running {
			return failSessionUnavailable("attach", id, ok && !info.Running)
		}
		dial = func(ctx context.Context) (io.ReadWriteCloser, error) {
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "unix", socketPath(id))
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errAttachSocketDial, err)
			}
			return conn, nil
		}
	}

	var options []AttachOption
	if showInstructions {
		options = append(options, WithDetachInstructions())
	}
	if err := NewClient(dial).Attach(ctx, NewProcessTerminal(), options...); err != nil {
		// A remote bgx bridge reports why the session is unavailable on the
		// transport's stderr; surface it as the single local error so the
		// remote session_not_found/session_ended semantics are preserved.
		if transport != nil {
			if code, msg, ok := transport.remoteBGXError(); ok {
				return failJSON(code, "%s", msg)
			}
		}
		// The session can end between the liveness check and the dial.
		if errors.Is(err, errAttachSocketDial) {
			return failSessionUnavailable("attach", id, false)
		}
		var respErr *ResponseError
		if errors.As(err, &respErr) {
			return failJSON(codeAttachFailed, "attach: %s", respErr.Message)
		}
		if transport != nil {
			if detail := transport.stderrOutput(); detail != "" {
				return failJSON(codeAttachFailed, "attach: %v: %s", err, detail)
			}
		}
		return failJSON(codeAttachFailed, "attach: %v", err)
	}
	return nil
}

// failSessionUnavailable reports the distinct reason a session cannot be
// attached or bridged to: it either already ended (a daemon answered as not
// running, or a persisted ended record exists) or it never existed.
func failSessionUnavailable(op, id string, knownEnded bool) error {
	if knownEnded {
		return failJSON(codeSessionEnded, "%s: session %q has already ended", op, id)
	}
	if _, ended := endedRecord(id); ended {
		return failJSON(codeSessionEnded, "%s: session %q has already ended", op, id)
	}
	return failJSON(codeSessionNotFound, "%s: session %q does not exist", op, id)
}

// transportShutdownGrace bounds how long a transport subprocess may take to
// exit on its own after its stdin closes before it is killed.
const transportShutdownGrace = 2 * time.Second

// transportConn adapts a transport subprocess (e.g. ssh) to the single stream
// an attach connection consumes: writes feed its stdin, reads drain its
// stdout, and its stderr is captured so a remote bgx error can be surfaced as
// the one local error rather than leaking a duplicate JSON error.
type transportConn struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *bytes.Buffer

	closeOnce sync.Once
	closeErr  error
}

// startTransportProcess launches argv with explicit pipes (rather than
// exec's managed ones) so closing and waiting never race the frame reader.
func startTransportProcess(ctx context.Context, argv []string) (*transportConn, error) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	stderr := &bytes.Buffer{}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("start transport %q: %w", argv[0], err)
	}
	stdinR.Close()
	stdoutW.Close()
	return &transportConn{cmd: cmd, stdin: stdinW, stdout: stdoutR, stderr: stderr}, nil
}

func (c *transportConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *transportConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close signals EOF to the transport via its stdin, gives it a grace period to
// exit (letting a remote error flush into the captured stderr), then kills it.
// The stdout side is closed last so a still-blocked reader unblocks.
func (c *transportConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		waited := make(chan error, 1)
		go func() { waited <- c.cmd.Wait() }()
		select {
		case err := <-waited:
			c.closeErr = err
		case <-time.After(transportShutdownGrace):
			_ = c.cmd.Process.Kill()
			c.closeErr = <-waited
		}
		_ = c.stdout.Close()
	})
	return c.closeErr
}

// stderrOutput returns the transport's captured stderr. Only call it after
// Close, whose Wait joins exec's goroutine copying into the buffer.
func (c *transportConn) stderrOutput() string {
	return strings.TrimSpace(c.stderr.String())
}

// remoteBGXError reports the last JSON error a remote bgx wrote to the
// transport's stderr, identified by its "source":"bgx" marker. Only call it
// after Close.
func (c *transportConn) remoteBGXError() (code, message string, ok bool) {
	for _, line := range strings.Split(c.stderr.String(), "\n") {
		var payload struct {
			Code   string `json:"code"`
			Error  string `json:"error"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if payload.Source == "bgx" && payload.Code != "" && payload.Error != "" {
			code, message, ok = payload.Code, payload.Error, true
		}
	}
	return code, message, ok
}
