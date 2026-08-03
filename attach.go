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

// AttachOptions configures an interactive attachment.
type AttachOptions struct {
	ShowDetachInstructions bool
	SSH                    string
	Via                    []string
	Terminal               Terminal
}

// AttachOptionsError reports an invalid combination of attachment options.
type AttachOptionsError struct {
	Err error
}

func (e *AttachOptionsError) Error() string {
	return fmt.Sprintf("attach: %v", e.Err)
}

func (e *AttachOptionsError) Unwrap() error {
	return e.Err
}

// errAttachSocketDial marks a failure to open the attach socket connection, so
// an attachment can distinguish a liveness race from a protocol failure.
var errAttachSocketDial = errors.New("session socket dial failed")

// Attach connects a terminal to a local or remote running session until the
// session ends, the user detaches, the terminal closes, or ctx is canceled.
func Attach(ctx context.Context, id string, opts AttachOptions) error {
	if id == "" {
		return &AttachOptionsError{Err: errors.New("an id is required")}
	}
	if opts.SSH != "" && opts.Via != nil {
		return &AttachOptionsError{Err: errors.New("--ssh and --via are mutually exclusive")}
	}
	if opts.Via != nil && len(opts.Via) == 0 {
		return &AttachOptionsError{Err: errors.New("--via requires a command")}
	}

	terminal := opts.Terminal
	if terminal == nil {
		terminal = NewProcessTerminal()
	}

	via := append([]string(nil), opts.Via...)
	if opts.SSH != "" {
		via = []string{"ssh", opts.SSH}
	}

	var dial Dialer
	var transport *transportConn
	if via != nil {
		argv := append(via, "bgx", "bridge", id)
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
			if ok || func() bool {
				_, ended := endedRecord(id)
				return ended
			}() {
				return &SessionEndedError{Operation: "attach", ID: id}
			}
			return sessionNotFound("attach", id, nil)
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
	if opts.ShowDetachInstructions {
		options = append(options, WithDetachInstructions())
	}
	err := NewClient(dial).Attach(ctx, terminal, options...)
	if err == nil {
		return nil
	}

	if transport != nil {
		if code, message, ok := transport.remoteBGXError(); ok {
			cause := errors.New(message)
			switch code {
			case codeSessionNotFound:
				return sessionNotFound("attach", id, cause)
			case codeSessionEnded:
				return &SessionEndedError{Operation: "attach", ID: id, Err: cause}
			}
		}
	}
	if errors.Is(err, errAttachSocketDial) {
		if _, ended := endedRecord(id); ended {
			return &SessionEndedError{Operation: "attach", ID: id, Err: err}
		}
		return sessionNotFound("attach", id, err)
	}
	if transport != nil {
		if detail := transport.stderrOutput(); detail != "" {
			return fmt.Errorf("attach: %w: %s", err, detail)
		}
	}
	return err
}

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
	opts := AttachOptions{
		ShowDetachInstructions: cmd.Bool("show-detach-instructions"),
		SSH:                    cmd.String("ssh"),
	}
	// Argument parsing stops at the id so --via can consume every following
	// argument as the transport command; the other flags stay usable on either
	// side of the id.
	rest := cmd.Args().Slice()[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--show-detach-instructions":
			opts.ShowDetachInstructions = true
		case "--ssh":
			if i == len(rest)-1 {
				return failJSON(codeInvalidArgument, "attach: --ssh requires a host")
			}
			i++
			opts.SSH = rest[i]
		case "--via":
			if i == len(rest)-1 {
				return failJSON(codeInvalidArgument, "attach: --via requires a command")
			}
			opts.Via = rest[i+1:]
			i = len(rest)
		default:
			return failJSON(codeInvalidArgument, "attach: unexpected argument %q", rest[i])
		}
	}

	err := Attach(ctx, id, opts)
	if err == nil {
		return nil
	}
	var optionsErr *AttachOptionsError
	if errors.As(err, &optionsErr) {
		return failJSON(codeInvalidArgument, "%v", optionsErr)
	}
	var notFound *SessionNotFoundError
	if errors.As(err, &notFound) {
		return failJSON(codeSessionNotFound, "attach: session %q does not exist", id)
	}
	var ended *SessionEndedError
	if errors.As(err, &ended) {
		return failJSON(codeSessionEnded, "attach: session %q has already ended", id)
	}
	var respErr *ResponseError
	if errors.As(err, &respErr) {
		return failJSON(codeAttachFailed, "attach: %s", respErr.Message)
	}
	return failJSON(codeAttachFailed, "attach: %v", err)
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
		return nil, errors.Join(err, stdinR.Close(), stdinW.Close())
	}
	stderr := &bytes.Buffer{}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start transport %q: %w", argv[0], err),
			stdinR.Close(),
			stdinW.Close(),
			stdoutR.Close(),
			stdoutW.Close(),
		)
	}
	if err := errors.Join(stdinR.Close(), stdoutW.Close()); err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		return nil, errors.Join(err, killErr, waitErr, stdinW.Close(), stdoutR.Close())
	}
	return &transportConn{cmd: cmd, stdin: stdinW, stdout: stdoutR, stderr: stderr}, nil
}

func (c *transportConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *transportConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close signals EOF to the transport via its stdin, gives it a grace period to
// exit (letting a remote error flush into the captured stderr), then kills it.
// The stdout side is closed last so a still-blocked reader unblocks.
func (c *transportConn) Close() error {
	c.closeOnce.Do(func() {
		stdinErr := c.stdin.Close()
		waited := make(chan error, 1)
		go func() { waited <- c.cmd.Wait() }()
		select {
		case err := <-waited:
			c.closeErr = errors.Join(stdinErr, err)
		case <-time.After(transportShutdownGrace):
			killErr := c.cmd.Process.Kill()
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			c.closeErr = errors.Join(stdinErr, killErr, <-waited)
		}
		c.closeErr = errors.Join(c.closeErr, c.stdout.Close())
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
