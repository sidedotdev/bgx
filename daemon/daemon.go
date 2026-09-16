// Package daemon runs a single bgx session: it executes one command in a PTY,
// pumps its output into a bounded scrollback store, and serves a unix domain
// socket speaking a JSON-line request/response protocol (info, wait, kill,
// send). When the command exits the daemon answers pending waiters, persists a
// record plus the retained history to the retention directory, removes the
// socket, and returns.
//
// The design mirrors zmx's daemon-per-session model (see
// https://github.com/neurosnap/zmx/blob/9889a13d62c3ef2d412cab7cf63683a1bb2d7013/src/main.zig)
// but uses Go: writes to the PTY go through a capped queue so a non-reading
// child never blocks the daemon, and DA queries are answered on the daemon side
// when no client is attached so interactive programs don't hang.
//
// Portions of this package are ported from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/sidedotdev/bgx/scrollback"
	"github.com/sidedotdev/bgx/vt"
	"github.com/sidedotdev/bgx/vtscan"
)

const (
	defaultRows = 24
	defaultCols = 80

	// ptyInputCap bounds the bytes queued toward the PTY. When full, newly
	// queued bytes are dropped rather than tearing an already-accepted
	// sequence (matches zmx's queuePtyInput cap).
	ptyInputCap = 256 << 10

	// killGrace is how long kill waits after SIGHUP before escalating to
	// SIGKILL.
	killGrace = 500 * time.Millisecond

	// outputDrainGrace bounds how long Serve waits for the output pump to reach
	// EOF on its own after the child exits before force-closing the PTY master.
	outputDrainGrace = 250 * time.Millisecond

	// connDrainGrace bounds how long Serve waits for in-flight connection
	// handlers to flush their responses (e.g. wait/kill exit codes) before the
	// process exits.
	connDrainGrace = 2 * time.Second
)

var errConnectionDrainTimeout = errors.New("timed out draining connection handlers")

// Config describes the session a daemon serves.
type Config struct {
	ID             string
	Command        []string
	Dir            string
	Env            map[string]string
	Metadata       map[string]string
	SocketPath     string
	RetentionDir   string
	RetentionCount int
	Scrollback     scrollback.Config
}

// Session holds the runtime state of a running command and serves its socket.
type Session struct {
	cfg   Config
	store *scrollback.Store
	term  *vt.Terminal

	listener   net.Listener
	done       chan struct{} // closed when the command has exited and been reaped
	outDone    chan struct{} // closed when all PTY output has been consumed
	inputDone  chan struct{} // closed when the PTY input pump has stopped
	acceptDone chan struct{} // closed when the listener accept loop has stopped

	// outMu serializes terminal writes with output fanout and attach snapshots
	// so a newly attached client neither misses nor duplicates output.
	outMu sync.Mutex

	// outScan tracks the VT ground/rune boundary of the bytes already rendered
	// and fanned out; outPending holds the trailing partial sequence until a
	// later read completes it. Both are owned solely by the output pump.
	outScan    vtscan.Scanner
	outPending []byte

	mu               sync.Mutex
	cond             *sync.Cond
	cmd              *exec.Cmd
	ptmx             *os.File
	startedAt        time.Time
	endedAt          time.Time
	exitCode         int
	ended            bool
	killed           bool
	closing          bool
	clientCount      int
	inputBuf         []byte
	outputErr        error
	inputErr         error
	acceptErr        error
	handlerErr       error
	connDrainTimeout time.Duration
	conns            sync.WaitGroup
	attachers        map[*attacher]struct{}
}

// Serve runs the configured command to completion: it creates the socket,
// starts the command, services client connections, and on exit persists the
// session record and history before removing the socket. It blocks until the
// session has fully ended.
func Serve(cfg Config) error {
	s, err := newSession(cfg)
	if err != nil {
		return err
	}
	return s.run()
}

// run drives the session to completion: it listens, starts the command, serves
// client connections, and on exit drains the output path, persists the record
// and history, and removes the socket. It blocks until the session has ended.
func (s *Session) run() error {
	if err := s.listen(); err != nil {
		closeErr := s.store.Close()
		s.term.Close()
		return errors.Join(err, closeErr)
	}
	if err := s.start(); err != nil {
		persistErr := s.persistSpawnError(err)
		listenerErr := s.listener.Close()
		removeErr := removeSocket(s.cfg.SocketPath)
		storeErr := s.store.Close()
		s.term.Close()
		return errors.Join(err, persistErr, listenerErr, removeErr, storeErr)
	}

	go s.acceptLoop()
	go s.reap()

	<-s.done

	// Stop accepting new connections. In-flight wait/kill handlers were already
	// woken by reap and still need to deliver their responses, so they are not
	// awaited until after the output path is drained below.
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	listenerErr := s.listener.Close()
	<-s.acceptDone

	// Drain and close the PTY output path independently of client handlers so a
	// slow or non-reading client can't delay capturing final output or block
	// shutdown. The grace lets the pump reach EOF on its own before the master
	// is force-closed to unblock a Read that never observes it.
	select {
	case <-s.outDone:
	case <-time.After(outputDrainGrace):
	}
	ptyErr := s.ptmx.Close()
	<-s.outDone
	<-s.inputDone

	// All output has now been fanned out. Tell still-attached clients the
	// session ended so they receive every byte still queued, then a final ended
	// frame, and close on their own instead of racing the drain grace below.
	s.endAttachers()

	persistErr := s.persist()
	removeErr := removeSocket(s.cfg.SocketPath)

	// Give in-flight handlers a bounded window to flush their responses before
	// the process exits, without letting a stuck client hang shutdown.
	connErr := waitConns(&s.conns, s.connDrainTimeout)

	s.mu.Lock()
	outputErr := s.outputErr
	inputErr := s.inputErr
	acceptErr := s.acceptErr
	handlerErr := s.handlerErr
	s.mu.Unlock()
	storeErr := s.store.Close()
	s.term.Close()
	return errors.Join(listenerErr, ptyErr, outputErr, inputErr, acceptErr, handlerErr, connErr, persistErr, removeErr, storeErr)
}

// waitConns waits for all in-flight connection handlers to finish, or reports
// that the bounded drain elapsed while at least one handler remained active.
func waitConns(wg *sync.WaitGroup, d time.Duration) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errConnectionDrainTimeout
	}
}

func newSession(cfg Config) (*Session, error) {
	store, err := scrollback.New(cfg.Scrollback)
	if err != nil {
		return nil, err
	}
	term, err := vt.New(defaultCols, defaultRows)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	s := &Session{
		cfg:              cfg,
		store:            store,
		term:             term,
		done:             make(chan struct{}),
		outDone:          make(chan struct{}),
		inputDone:        make(chan struct{}),
		acceptDone:       make(chan struct{}),
		connDrainTimeout: connDrainGrace,
		attachers:        make(map[*attacher]struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

// listen creates the session's unix domain socket, replacing any stale file.
func (s *Session) listen() error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o700); err != nil {
		return err
	}
	if err := removeSocket(s.cfg.SocketPath); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	// Keep the socket file in place until it is explicitly removed after the
	// ended record is persisted. Otherwise Go unlinks it when the listener
	// closes early in shutdown, leaving a window where a client sees neither a
	// live socket nor the record it would fall back to.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	s.listener = ln
	return nil
}

func environmentWithOverrides(overrides map[string]string) []string {
	env := os.Environ()
	indexes := make(map[string]int, len(env))
	for i, entry := range env {
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			indexes[entry[:separator]] = i
		}
	}
	for name, value := range overrides {
		entry := name + "=" + value
		if i, ok := indexes[name]; ok {
			env[i] = entry
			continue
		}
		indexes[name] = len(env)
		env = append(env, entry)
	}
	return env
}

// start launches the command in a PTY with its own session/process group and
// begins pumping output and input.
func (s *Session) start() error {
	if len(s.cfg.Command) == 0 {
		return errors.New("daemon: empty command")
	}
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Dir = s.cfg.Dir
	cmd.Env = environmentWithOverrides(s.cfg.Env)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: defaultRows, Cols: defaultCols})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cmd = cmd
	s.ptmx = ptmx
	s.startedAt = time.Now()
	s.mu.Unlock()

	go s.pumpOutput()
	go s.pumpInput()
	return nil
}

// reap waits for the command to exit, records its exit state, and wakes any
// waiters.
func (s *Session) reap() {
	err := s.cmd.Wait()
	code := conventionalExitCode(err)

	s.mu.Lock()
	s.ended = true
	s.endedAt = time.Now()
	s.exitCode = code
	s.cond.Broadcast()
	s.mu.Unlock()

	close(s.done)
}

// pumpOutput copies PTY output into the scrollback store and answers DA queries
// when no client is attached. It returns once the PTY reaches EOF, signalling
// that all output has been captured.
func (s *Session) pumpOutput() {
	defer close(s.outDone)
	s.pumpOutputFrom(s.ptmx)
}

func (s *Session) pumpOutputFrom(r io.Reader) {
	recordError := func(err error) {
		if err == nil {
			return
		}
		s.mu.Lock()
		s.outputErr = errors.Join(s.outputErr, err)
		s.mu.Unlock()
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := buf[:n]
			_, storeErr := s.store.Write(data)
			recordError(storeErr)
			recordError(s.feedTerm(data, false))
			if s.noClients() {
				if resp := scanDeviceAttributes(data); len(resp) > 0 {
					s.queueInput(resp)
				}
			}
		}
		if err != nil {
			recordError(s.feedTerm(nil, true))
			if !errors.Is(err, io.EOF) &&
				!errors.Is(err, os.ErrClosed) &&
				!errors.Is(err, syscall.EIO) {
				recordError(err)
			}
			return
		}
	}
}

// feedTerm renders output and fans it out to attachers, advancing only to the
// latest VT ground and UTF-8 rune boundary so the attach snapshot
// (s.term.DumpScreen) and the raw bytes streamed afterwards always tile
// cleanly. The trailing partial sequence is buffered until a later read
// completes it; flush forces any remainder through once the PTY reaches EOF.
func (s *Session) feedTerm(data []byte, flush bool) error {
	s.outPending = append(s.outPending, data...)
	cut := len(s.outPending)
	if !flush {
		cut = s.outScan.SafeCut(s.outPending, len(s.outPending))
	}
	if cut <= 0 {
		return nil
	}
	safe := s.outPending[:cut]
	s.outScan.Advance(safe)
	s.outMu.Lock()
	_, err := s.term.Write(safe)
	if err == nil {
		s.fanout(safe)
	}
	s.outMu.Unlock()
	if err != nil {
		return err
	}
	s.outPending = append(s.outPending[:0], s.outPending[cut:]...)
	return nil
}

// pumpInput flushes queued bytes to the PTY. Running in its own goroutine means
// a blocking write (child not reading) never stalls the daemon; the queue cap
// bounds memory growth.
func (s *Session) pumpInput() {
	defer close(s.inputDone)
	s.pumpInputTo(s.ptmx)
}

func (s *Session) pumpInputTo(w io.Writer) {
	for {
		s.mu.Lock()
		for len(s.inputBuf) == 0 && !s.ended {
			s.cond.Wait()
		}
		if len(s.inputBuf) == 0 && s.ended {
			s.mu.Unlock()
			return
		}
		// Write from a copy but leave the bytes in inputBuf so queueInput's cap
		// check keeps accounting for these still-unwritten, in-flight bytes;
		// they are dropped from the queue only once actually written. This
		// bounds total retained input at the cap even while a write blocks on a
		// non-reading child.
		chunk := append([]byte(nil), s.inputBuf...)
		s.mu.Unlock()

		n, err := w.Write(chunk)

		s.mu.Lock()
		if n > 0 {
			rest := s.inputBuf[n:]
			s.inputBuf = append([]byte(nil), rest...)
		}
		if err != nil {
			if !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
				s.inputErr = errors.Join(s.inputErr, err)
			}
		} else if n != len(chunk) {
			s.inputErr = errors.Join(s.inputErr, io.ErrShortWrite)
		}
		s.mu.Unlock()

		if err != nil || n != len(chunk) {
			return
		}
	}
}

// queueInput appends raw bytes destined for the PTY, dropping them if the queue
// is full so an accepted sequence is never torn mid-write.
func (s *Session) queueInput(p []byte) {
	if len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.closing {
		return
	}
	if len(s.inputBuf)+len(p) > ptyInputCap {
		return
	}
	s.inputBuf = append(s.inputBuf, p...)
	s.cond.Broadcast()
}

func (s *Session) noClients() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientCount == 0
}

// retainedInput reports the number of queued, not-yet-written input bytes.
func (s *Session) retainedInput() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inputBuf)
}

// acceptLoop serves one request per accepted connection until the listener is
// closed.
func (s *Session) acceptLoop() {
	defer close(s.acceptDone)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			if !s.closing {
				s.acceptErr = errors.Join(s.acceptErr, err)
			}
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			s.recordHandlerError(conn.Close())
			continue
		}
		s.conns.Add(1)
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

func (s *Session) serveConn(conn net.Conn) {
	defer s.conns.Done()
	s.recordHandlerError(s.handle(conn))
}

func (s *Session) recordHandlerError(err error) {
	err = withoutNetErrClosed(err)
	if err == nil {
		return
	}
	s.mu.Lock()
	s.handlerErr = errors.Join(s.handlerErr, err)
	s.mu.Unlock()
}

func withoutNetErrClosed(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		remaining := make([]error, 0, len(children))
		for _, child := range children {
			if child = withoutNetErrClosed(child); child != nil {
				remaining = append(remaining, child)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := withoutNetErrClosed(wrapped.Unwrap())
		if child == nil {
			return nil
		}
		if child != wrapped.Unwrap() {
			return child
		}
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// handle serves a single accepted connection. Most ops are one request/response
// over JSON lines; an "attach" request instead upgrades the connection to the
// bidirectional frame protocol for the remainder of its lifetime.
func (s *Session) handle(conn net.Conn) (retErr error) {
	defer func() {
		retErr = errors.Join(retErr, conn.Close())
	}()
	// Read exactly the request line so its trailing newline is consumed and the
	// reader can be reused for binary attach frames without corrupting framing.
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return err
	}
	if req.Op == "attach" {
		return s.serveAttach(conn, br)
	}
	resp := s.dispatch(req)
	return json.NewEncoder(conn).Encode(resp)
}

func (s *Session) dispatch(req Request) Response {
	switch req.Op {
	case "info":
		return Response{OK: true, Info: s.info()}
	case "wait":
		code := s.waitForExit()
		return Response{OK: true, Info: s.info(), ExitCode: &code}
	case "kill":
		if err := s.kill(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		code := s.waitForExit()
		return Response{OK: true, Info: s.info(), ExitCode: &code}
	case "send":
		s.queueInput(req.Input)
		return Response{OK: true}
	case "history":
		history := s.store.Snapshot()
		if err := s.store.Err(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, History: history}
	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}

// waitForExit blocks until the command has exited and returns its exit code.
func (s *Session) waitForExit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.ended {
		s.cond.Wait()
	}
	return s.exitCode
}

// kill terminates the command's process group following zmx's ladder: SIGHUP,
// a grace period, then SIGKILL. It returns once the command has been reaped.
func (s *Session) kill() error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil
	}
	s.killed = true
	pid := s.cmd.Process.Pid
	s.mu.Unlock()

	if err := syscall.Kill(-pid, syscall.SIGHUP); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-s.done:
		return nil
	case <-time.After(killGrace):
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	<-s.done
	return nil
}

// info captures the current metadata snapshot for the session.
func (s *Session) info() *Info {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := &Info{
		ID:          s.cfg.ID,
		Running:     !s.ended,
		Command:     s.cfg.Command,
		Metadata:    s.cfg.Metadata,
		StartedAt:   s.startedAt,
		OutputBytes: s.store.TotalBytes(),
	}
	if s.cmd != nil && s.cmd.Process != nil {
		info.Pid = s.cmd.Process.Pid
	}
	if s.ended {
		endedAt := s.endedAt
		code := s.exitCode
		info.EndedAt = &endedAt
		info.ExitCode = &code
		info.Killed = s.killed
		info.DurationMS = endedAt.Sub(s.startedAt).Milliseconds()
	} else {
		info.DurationMS = time.Since(s.startedAt).Milliseconds()
	}
	return info
}

// snapshot renders the complete terminal state (both screens, active screen,
// cursor and input modes) as VT sequences a client can replay, without
// resetting its terminal, before live output streaming begins or resumes.
func (s *Session) snapshot() ([]byte, error) {
	return s.term.Snapshot()
}

// resize updates both the PTY window size and the emulated terminal so attach
// clients see correctly reflowed output.
func (s *Session) resize(cols, rows uint16) error {
	s.mu.Lock()
	ptmx := s.ptmx
	s.mu.Unlock()
	if ptmx != nil {
		if err := pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
			return err
		}
	}
	return s.term.Resize(cols, rows)
}

// spawnFailureExitCode is reported for a session whose wrapped command never
// execed, mirroring the shell convention for a command that cannot be run.
const spawnFailureExitCode = 127

// persistSpawnError records an ended session whose command never started so a
// client can discover the cause instead of only timing out on the socket.
func (s *Session) persistSpawnError(cause error) error {
	if s.cfg.RetentionDir == "" {
		return nil
	}
	now := time.Now()
	code := spawnFailureExitCode
	info := &Info{
		ID:        s.cfg.ID,
		Running:   false,
		Command:   s.cfg.Command,
		Metadata:  s.cfg.Metadata,
		StartedAt: now,
		EndedAt:   &now,
		ExitCode:  &code,
		Error:     cause.Error(),
	}
	history := s.store.Snapshot()
	if err := s.store.Err(); err != nil {
		return err
	}
	return writeRecord(s.cfg.RetentionDir, info, history)
}

// persist writes the ended session's record and retained history to the
// retention directory.
func (s *Session) persist() error {
	if s.cfg.RetentionDir == "" {
		return s.store.Err()
	}
	history := s.store.Snapshot()
	if err := s.store.Err(); err != nil {
		return err
	}
	if err := writeRecord(s.cfg.RetentionDir, s.info(), history); err != nil {
		return err
	}
	keep := s.cfg.RetentionCount
	if keep <= 0 {
		keep = DefaultRetentionCount
	}
	// Sessions still running in this namespace count toward the retention
	// budget, so reserve a slot for each; the just-ended record is always kept.
	active, err := activeNamespaceSessions(filepath.Dir(s.cfg.SocketPath), s.cfg.ID)
	if err != nil {
		return err
	}
	keep -= active
	if keep < 1 {
		keep = 1
	}
	return pruneRetention(s.cfg.RetentionDir, s.cfg.ID, keep)
}

// conventionalExitCode maps a command's wait error to an exit code, using the
// shell convention of 128+signal for signal-terminated processes.
func conventionalExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

func removeSocket(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
