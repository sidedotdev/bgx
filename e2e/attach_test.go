package e2e

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	lg "github.com/ehsanul/libghostty-vt-static"
)

func closePTY(t *testing.T, ptmx *os.File) {
	t.Helper()
	if err := ptmx.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close pty: %v", err)
	}
}

func expectedPTYReadError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.EIO)
}

// renderScreen replays a captured attach output stream through an emulated
// terminal of the given size and returns its visible rows as plain text, so
// tests can assert on what a user actually sees rather than on byte sequences.
func renderScreen(t *testing.T, stream string, cols, rows uint16) []string {
	t.Helper()
	term, err := lg.NewTerminal(lg.WithSize(cols, rows))
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	defer term.Close()
	term.VTWrite([]byte(stream))
	f, err := lg.NewFormatter(term, lg.WithFormatterFormat(lg.FormatterFormatPlain))
	if err != nil {
		t.Fatalf("NewFormatter: %v", err)
	}
	defer f.Close()
	s, err := f.FormatString()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return strings.Split(s, "\n")
}

// renderScreenVT is like renderScreen but preserves colors and styles as VT
// sequences, so tests can assert on the rendered styling of each row.
func renderScreenVT(t *testing.T, stream string, cols, rows uint16) []string {
	t.Helper()
	term, err := lg.NewTerminal(lg.WithSize(cols, rows))
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	defer term.Close()
	term.VTWrite([]byte(stream))
	f, err := lg.NewFormatter(term, lg.WithFormatterFormat(lg.FormatterFormatVT))
	if err != nil {
		t.Fatalf("NewFormatter: %v", err)
	}
	defer f.Close()
	s, err := f.FormatString()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return strings.Split(s, "\r\n")
}

// hintRows reports the 1-based rendered rows holding the detach hint.
func hintRows(rendered []string) []int {
	var rows []int
	for i, line := range rendered {
		if strings.Contains(line, "detach: ctrl+\\") {
			rows = append(rows, i+1)
		}
	}
	return rows
}

// preAttachRestoredText is the unfinished prompt line present on the emulated
// terminal when a transcript is replayed over history, so tests can locate
// where the pre-attach content ends.
const preAttachRestoredText = "shell prompt> draft"

// replayAttachTranscriptOverHistory replays an attach transcript onto an
// emulated terminal already holding more history than fits on screen plus an
// unfinished prompt line, and returns everything the terminal retains
// (scrollback and screen) as plain text.
func replayAttachTranscriptOverHistory(t *testing.T, transcript string, cols, physicalRows uint16) string {
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
	emulated.VTWrite([]byte(preAttachRestoredText))
	emulated.VTWrite([]byte(transcript))

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
	return history
}

func assertAttachLifecycleTranscriptPreservesHistory(
	t *testing.T,
	transcript string,
	cols, physicalRows uint16,
	sessionLines []string,
	outcome string,
) {
	t.Helper()
	history := replayAttachTranscriptOverHistory(t, transcript, cols, physicalRows)
	restoredAt := strings.Index(history, preAttachRestoredText)
	if restoredAt < 0 {
		t.Fatalf("pre-attach terminal content was lost; history=%q transcript=%q", history, transcript)
	}
	finalState := strings.Join(sessionLines, "\n") + "\n" + outcome
	finalStateAt := strings.LastIndex(history, finalState)
	if finalStateAt <= restoredAt {
		t.Fatalf("complete final session state and lifecycle outcome were not preserved after restored history; want %q in history=%q transcript=%q", finalState, history, transcript)
	}
}

// TestAttachStreamsAndDetaches drives the attach client under a pty: the
// snapshot replays, live output streams, typed input reaches the session, and
// ctrl+\ detaches without stopping the session.
func TestAttachStreamsAndDetaches(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "att", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	// Pre-seed output the snapshot should reproduce (cat echoes its input).
	if res := bgxIn(t, dir, "send", "att", "hello"); res.exitCode != 0 {
		t.Fatalf("send exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	historyContains(t, dir, "att", "hello")

	cmd := exec.Command(binPath, "attach", "--mode", "isolated", "att")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer closePTY(t, ptmx)

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readDone)
		buf := make([]byte, 64<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if rerr != nil {
				if !expectedPTYReadError(rerr) {
					readErr <- rerr
				}
				return
			}
		}
	}()

	output := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if strings.Contains(output(), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("attach output never contained %q; got %q", want, output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	waitFor("hello")

	if _, err := ptmx.Write([]byte("world")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	waitFor("world")
	historyContains(t, dir, "att", "world")

	if _, err := ptmx.Write([]byte{0x1c}); err != nil {
		t.Fatalf("write ctrl-backslash: %v", err)
	}

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not detach on ctrl+backslash")
	}
	select {
	case err := <-readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	emulated, err := lg.NewTerminal(lg.WithSize(80, 24))
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	defer emulated.Close()
	emulated.VTWrite([]byte(strings.Repeat("older shell output\r\n", 30)))
	emulated.VTWrite([]byte("\x1b[32mshell prompt> draft\x1b[5D"))

	format := func() string {
		formatter, err := lg.NewFormatter(
			emulated,
			lg.WithFormatterFormat(lg.FormatterFormatVT),
			lg.WithFormatterExtraStyle(true),
			lg.WithFormatterExtraCursor(true),
		)
		if err != nil {
			t.Fatalf("NewFormatter: %v", err)
		}
		defer formatter.Close()
		state, err := formatter.Format()
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		return string(state)
	}

	const detachMessage = "\r\nDetached from session\r\n"
	beforeMessage, found := strings.CutSuffix(output(), detachMessage)
	if !found {
		t.Fatalf("attach output = %q, want suffix %q", output(), detachMessage)
	}

	before := format()
	emulated.VTWrite([]byte(beforeMessage))
	after := format()
	if after != before {
		t.Fatalf("visible terminal state and cursor before detach message differ from pre-attach state:\nbefore %q\nafter  %q\nattach output %q", before, after, output())
	}

	info := decodeJSON(t, bgxIn(t, dir, "info", "att").stdout)
	if info["running"] != true {
		t.Fatalf("session not running after detach: %v", info)
	}

	bgxIn(t, dir, "kill", "att")
}

// TestAttachResizePropagates verifies a lone client's window size (the
// effective minimum) reaches the session PTY on attach and again after a
// SIGWINCH-driven resize.
func TestAttachResizePropagates(t *testing.T) {
	dir := runDir(t)

	// The session echoes its controlling terminal size on every input line, so
	// the daemon's applied PTY size is observable in the attach stream.
	if res := bgxIn(t, dir, "run", "rsz", "sh", "-c", "while IFS= read -r _; do stty size; done"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	cmd := exec.Command(binPath, "attach", "rsz")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 120})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer closePTY(t, ptmx)

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readDone)
		buf := make([]byte, 64<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if rerr != nil {
				if !expectedPTYReadError(rerr) {
					readErr <- rerr
				}
				return
			}
		}
	}()

	output := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if strings.Contains(output(), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("attach output never contained %q; got %q", want, output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Give the daemon a moment to apply the initial size request before probing.
	time.Sleep(300 * time.Millisecond)
	if _, err := ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	waitFor("50 120")

	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 100}); err != nil {
		t.Fatalf("setsize: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	waitFor("40 100")

	if _, err := ptmx.Write([]byte{0x1c}); err != nil {
		t.Fatalf("write ctrl-backslash: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not detach on ctrl+backslash")
	}
	select {
	case err := <-readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	bgxIn(t, dir, "kill", "rsz")
}

// TestAttachDetachesOnSplitCtrlBackslash drives the attach client under a pty
// and writes the Kitty-encoded ctrl+\ detach sequence ("\x1b[92;5u") split
// across two writes. Neither half is a complete detach sequence on its own, so a
// per-chunk check would forward both as input and never detach; the buffering
// scanner must still recognize the reassembled sequence and detach.
func TestAttachDetachesOnSplitCtrlBackslash(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "split", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	cmd := exec.Command(binPath, "attach", "split")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer closePTY(t, ptmx)

	readDone := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readDone)
		buf := make([]byte, 64<<10)
		for {
			if _, rerr := ptmx.Read(buf); rerr != nil {
				if !expectedPTYReadError(rerr) {
					readErr <- rerr
				}
				return
			}
		}
	}()

	// Let the client enter raw mode and replay its snapshot before detaching.
	time.Sleep(300 * time.Millisecond)

	if _, err := ptmx.Write([]byte("\x1b[92")); err != nil {
		t.Fatalf("write first half: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := ptmx.Write([]byte(";5u")); err != nil {
		t.Fatalf("write second half: %v", err)
	}

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not detach on split ctrl+backslash")
	}
	select {
	case err := <-readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	info := decodeJSON(t, bgxIn(t, dir, "info", "split").stdout)
	if info["running"] != true {
		t.Fatalf("session not running after detach: %v", info)
	}

	bgxIn(t, dir, "kill", "split")
}

// attachE2EClient is an attach process driven under a pty whose output is
// accumulated for assertions.
type attachE2EClient struct {
	cmd      *exec.Cmd
	ptmx     *os.File
	mu       sync.Mutex
	got      []byte
	readDone chan struct{}
	readErr  chan error
}

func (c *attachE2EClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.got)
}

// startAttachE2EClient launches `bgx attach [extraArgs...] id` under a pty of
// the given size (nil for the default) and pumps its output into the returned
// client.
func startAttachE2EClient(t *testing.T, dir, id string, ws *pty.Winsize, extraArgs ...string) *attachE2EClient {
	t.Helper()
	cmd := exec.Command(binPath, append(append([]string{"attach"}, extraArgs...), id)...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	var (
		ptmx *os.File
		err  error
	)
	if ws != nil {
		ptmx, err = pty.StartWithSize(cmd, ws)
	} else {
		ptmx, err = pty.Start(cmd)
	}
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	c := &attachE2EClient{
		cmd:      cmd,
		ptmx:     ptmx,
		readDone: make(chan struct{}),
		readErr:  make(chan error, 1),
	}
	go func() {
		defer close(c.readDone)
		buf := make([]byte, 64<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.got = append(c.got, buf[:n]...)
				c.mu.Unlock()
			}
			if rerr != nil {
				if !expectedPTYReadError(rerr) {
					c.readErr <- rerr
				}
				return
			}
		}
	}()
	return c
}

func (c *attachE2EClient) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(c.output(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attach output never contained %q; got %q", want, c.output())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *attachE2EClient) detach(t *testing.T) {
	t.Helper()
	if _, err := c.ptmx.Write([]byte{0x1c}); err != nil {
		t.Fatalf("write ctrl-backslash: %v", err)
	}
	select {
	case <-c.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not detach on ctrl+backslash")
	}
	select {
	case err := <-c.readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}
}

// TestAttachMultiClientInputReachesSession verifies that input from every
// concurrently attached client reaches the single session PTY and that all
// clients observe the resulting shared output.
func TestAttachMultiClientInputReachesSession(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "multi", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	a := startAttachE2EClient(t, dir, "multi", nil)
	defer closePTY(t, a.ptmx)
	b := startAttachE2EClient(t, dir, "multi", nil)
	defer closePTY(t, b.ptmx)

	waitBoth := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if strings.Contains(a.output(), want) && strings.Contains(b.output(), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("clients never both saw %q; a=%q b=%q", want, a.output(), b.output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Let both clients finish attaching before driving input.
	time.Sleep(300 * time.Millisecond)

	if _, err := a.ptmx.Write([]byte("alpha\n")); err != nil {
		t.Fatalf("write client A: %v", err)
	}
	waitBoth("alpha")

	if _, err := b.ptmx.Write([]byte("bravo\n")); err != nil {
		t.Fatalf("write client B: %v", err)
	}
	waitBoth("bravo")

	historyContains(t, dir, "multi", "alpha")
	historyContains(t, dir, "multi", "bravo")

	a.detach(t)
	b.detach(t)

	bgxIn(t, dir, "kill", "multi")
}

// TestAttachMultiClientMinSize verifies the daemon applies the smallest cols and
// rows independently across concurrently attached clients, and grows back when
// the client contributing a minimum detaches.
func TestAttachMultiClientMinSize(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "minrsz", "sh", "-c", "while IFS= read -r _; do stty size; done"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	// Min cols (100) comes from A; min rows (40) comes from B, so the applied
	// size combines minima from different clients.
	a := startAttachE2EClient(t, dir, "minrsz", &pty.Winsize{Rows: 50, Cols: 100})
	defer closePTY(t, a.ptmx)
	b := startAttachE2EClient(t, dir, "minrsz", &pty.Winsize{Rows: 40, Cols: 120})
	defer closePTY(t, b.ptmx)

	// Let both size reports land before probing the applied PTY size.
	time.Sleep(500 * time.Millisecond)
	if _, err := a.ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	a.waitFor(t, "40 100")

	// Detach the client contributing the minimum rows; the PTY grows back.
	b.detach(t)

	time.Sleep(500 * time.Millisecond)
	if _, err := a.ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	a.waitFor(t, "50 100")

	a.detach(t)

	bgxIn(t, dir, "kill", "minrsz")
}

// TestAttachClosesOnSessionEnd drives the attach client under a pty and verifies
// that when the session ends the client exits on its own without a full terminal
// reset, preserving prior history before the final session state and outcome.
func TestAttachClosesOnSessionEnd(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "att2", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	if res := bgxIn(t, dir, "send", "att2", "hello"); res.exitCode != 0 {
		t.Fatalf("send exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	historyContains(t, dir, "att2", "hello")

	cmd := exec.Command(binPath, "attach", "--mode", "isolated", "att2")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer closePTY(t, ptmx)

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readDone)
		buf := make([]byte, 64<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if rerr != nil {
				if !expectedPTYReadError(rerr) {
					readErr <- rerr
				}
				return
			}
		}
	}()

	output := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if strings.Contains(output(), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("attach output never contained %q; got %q", want, output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	waitFor("hello")

	// End the session; the attached client must close on its own.
	bgxIn(t, dir, "kill", "att2")

	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not exit when the session ended")
	}
	select {
	case err := <-readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	assertAttachLifecycleTranscriptPreservesHistory(
		t,
		output(),
		80,
		24,
		[]string{"hello"},
		"Session ended",
	)
}

// TestAttachShowDetachInstructionsReservesLine verifies that attaching with
// --show-detach-instructions reserves the bottom terminal line for the detach
// hint: the hint is drawn, scrolling is fenced off above it, the session sees a
// one-row-shorter terminal, and session end clears the reserved line without a
// full terminal reset.
func TestAttachShowDetachInstructionsReservesLine(t *testing.T) {
	dir := runDir(t)

	// The session echoes its controlling terminal size on every input line, so
	// the reduced size reported by the client is observable.
	if res := bgxIn(t, dir, "run", "hint", "sh", "-c", "while IFS= read -r _; do stty size; done"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	c := startAttachE2EClient(t, dir, "hint", &pty.Winsize{Rows: 50, Cols: 120}, "--mode", "isolated", "--show-detach-instructions")
	defer closePTY(t, c.ptmx)

	// The detach hint is drawn on the bottom row and a scroll region keeps
	// session output confined to the rows above it.
	c.waitFor(t, "detach: ctrl+\\")
	c.waitFor(t, "\x1b[1;49r")

	// The reserved line renders with its own subtle background color, distinct
	// from the session rows above it.
	const hintBG = "\x1b[48;5;236m"
	vtRows := renderScreenVT(t, c.output(), 120, 50)
	if last := vtRows[len(vtRows)-1]; !strings.Contains(last, hintBG) || !strings.Contains(last, "detach: ctrl+\\") {
		t.Fatalf("reserved line missing its background styling; got %q", last)
	}
	if sessionRows := strings.Join(vtRows[:len(vtRows)-1], "\n"); strings.Contains(sessionRows, hintBG) {
		t.Fatalf("hint background leaked into session rows; got %q", sessionRows)
	}

	// Give the daemon a moment to apply the reduced size before probing.
	time.Sleep(300 * time.Millisecond)
	if _, err := c.ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	c.waitFor(t, "49 120")

	if rows := hintRows(renderScreen(t, c.output(), 120, 50)); len(rows) != 1 || rows[0] != 50 {
		t.Fatalf("detach hint rendered on rows %v, want only row 50", rows)
	}

	// Growing the terminal moves the hint to the new bottom row, leaving no
	// stale hint on the previously reserved row.
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: 60, Cols: 120}); err != nil {
		t.Fatalf("setsize: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := c.ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	c.waitFor(t, "59 120")
	if rows := hintRows(renderScreen(t, c.output(), 120, 60)); len(rows) != 1 || rows[0] != 60 {
		t.Fatalf("after growing, detach hint rendered on rows %v, want only row 60", rows)
	}

	// Ending the session must clear the reserved line, leaving no trace of the
	// hint on screen, and must not perform the full reset a detach uses.
	bgxIn(t, dir, "kill", "hint")
	select {
	case <-c.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not exit when the session ended")
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	out := c.output()
	if strings.Contains(out, "\x1bc") {
		t.Fatalf("session end performed a full terminal reset; got %q", out)
	}
	if rows := hintRows(renderScreen(t, out, 120, 60)); len(rows) != 0 {
		t.Fatalf("detach hint still rendered on rows %v after session end", rows)
	}
}

// TestAttachShowDetachInstructionsSurvivesDestructiveOutput verifies the
// reserved line is isolated from the session's own control sequences: screen
// clears, scrollback erase, scroll-region changes, alternate-screen
// transitions, absolute cursor addressing and heavy scrolling must all leave the
// hint intact on the bottom row while the session renders above it.
func TestAttachShowDetachInstructionsSurvivesDestructiveOutput(t *testing.T) {
	dir := runDir(t)

	// Each input line triggers a burst of destructive sequences followed by
	// enough lines to scroll the session area several times over.
	const script = `while IFS= read -r _; do
printf '\033[?1049h\033[2J\033[H\033[1;1r\033[5;3HWRECKED\033[?1049l'
printf '\033[2J\033[3J\033[r\033[H\033[1;1HCLEARED'
i=1; while [ $i -le 60 ]; do printf '\nline%d' "$i"; i=$((i+1)); done
printf '\nDONE-MARKER'
done`
	if res := bgxIn(t, dir, "run", "wreck", "sh", "-c", script); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	c := startAttachE2EClient(t, dir, "wreck", &pty.Winsize{Rows: 20, Cols: 80}, "--mode", "isolated", "--show-detach-instructions")
	defer closePTY(t, c.ptmx)

	c.waitFor(t, "detach: ctrl+\\")
	time.Sleep(300 * time.Millisecond)
	if _, err := c.ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	c.waitFor(t, "DONE-MARKER")
	// Let the coalesced repaint of the final state land.
	time.Sleep(300 * time.Millisecond)

	rendered := renderScreen(t, c.output(), 80, 20)
	if rows := hintRows(rendered); len(rows) != 1 || rows[0] != 20 {
		t.Fatalf("detach hint rendered on rows %v, want only row 20; screen=%q", rows, rendered)
	}
	sessionArea := strings.Join(rendered[:len(rendered)-1], "\n")
	if !strings.Contains(sessionArea, "DONE-MARKER") {
		t.Fatalf("session output missing from the reserved-line screen; screen=%q", rendered)
	}
	if strings.Contains(strings.Join(rendered, "\n"), "WRECKED") {
		t.Fatalf("alternate-screen output leaked onto the primary screen; screen=%q", rendered)
	}

	c.detach(t)
	bgxIn(t, dir, "kill", "wreck")
}

// TestAttachShowDetachInstructionsOneRowTerminal verifies that a one-row
// terminal leaves no room for the hint: nothing is reserved and the session
// sees the full (single-row) size.
func TestAttachShowDetachInstructionsOneRowTerminal(t *testing.T) {
	dir := runDir(t)

	// The size is echoed without a trailing newline so it stays visible on a
	// single-row screen instead of immediately scrolling off.
	script := `while IFS= read -r _; do printf '%s' "$(stty size)"; done`
	if res := bgxIn(t, dir, "run", "hint1", "sh", "-c", script); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	c := startAttachE2EClient(t, dir, "hint1", &pty.Winsize{Rows: 20, Cols: 120}, "--mode", "isolated", "--show-detach-instructions")
	defer closePTY(t, c.ptmx)

	c.waitFor(t, "detach: ctrl+\\")

	// Shrinking to a single row leaves no room for both, so the reservation is
	// dropped: the hint is cleared and the session gets the whole terminal.
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: 1, Cols: 120}); err != nil {
		t.Fatalf("setsize: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := c.ptmx.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	c.waitFor(t, "1 120")

	rendered := renderScreen(t, c.output(), 120, 1)
	if rows := hintRows(rendered); len(rows) != 0 {
		t.Fatalf("one-row terminal still rendered the detach hint on rows %v", rows)
	}
	if !strings.Contains(strings.Join(rendered, "\n"), "1 120") {
		t.Fatalf("one-row terminal did not give the session the whole screen; screen=%q", rendered)
	}

	c.detach(t)
	bgxIn(t, dir, "kill", "hint1")
}

// TestAttachReportsEndedAndMissingSessions verifies attach distinguishes a
// session that never existed from one that already ended, emitting a distinct
// JSON error with a machine-readable code on stderr for each case.
func TestAttachReportsEndedAndMissingSessions(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "attach", "no-such-session")
	if res.exitCode == 0 {
		t.Fatalf("attach to missing session succeeded; stdout=%q", res.stdout)
	}
	if res.stdout != "" {
		t.Fatalf("attach error leaked to stdout: %q", res.stdout)
	}
	errObj := decodeJSON(t, res.stderr)
	if errObj["code"] != "session_not_found" {
		t.Fatalf("missing session code = %v, want session_not_found; stderr=%q", errObj["code"], res.stderr)
	}
	msg, ok := errObj["error"].(string)
	if !ok {
		t.Fatalf("missing session error = %T, want string; stderr=%q", errObj["error"], res.stderr)
	}
	if !strings.Contains(msg, "does not exist") {
		t.Fatalf("missing session error = %q, want mention of not existing", msg)
	}

	if res := bgxIn(t, dir, "run", "attend", "true"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "attend")

	res = bgxIn(t, dir, "attach", "attend")
	if res.exitCode == 0 {
		t.Fatalf("attach to ended session succeeded; stdout=%q", res.stdout)
	}
	if res.stdout != "" {
		t.Fatalf("attach error leaked to stdout: %q", res.stdout)
	}
	errObj = decodeJSON(t, res.stderr)
	if errObj["code"] != "session_ended" {
		t.Fatalf("ended session code = %v, want session_ended; stderr=%q", errObj["code"], res.stderr)
	}
	msg, ok = errObj["error"].(string)
	if !ok {
		t.Fatalf("ended session error = %T, want string; stderr=%q", errObj["error"], res.stderr)
	}
	if !strings.Contains(msg, "already ended") {
		t.Fatalf("ended session error = %q, want mention of having ended", msg)
	}
}

// TestAttachInteractiveShellCommands verifies that an attached shell remains
// interactive across commands sent over time, including while a command is
// running without producing output.
func TestAttachInteractiveShellCommands(t *testing.T) {
	dir := runDir(t)

	const id = "interactive-shell"
	if res := bgxIn(t, dir, "run", id, "sh", "-c", "printf 'ATTACH-SHELL-READY\n'; PS1='shell> '; export PS1; exec sh"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	t.Cleanup(func() { bgxIn(t, dir, "kill", id) })

	const (
		cols = 80
		rows = 10
	)
	c := startAttachE2EClient(t, dir, id, &pty.Winsize{Rows: rows, Cols: cols})
	defer closePTY(t, c.ptmx)
	c.waitFor(t, "ATTACH-SHELL-READY")

	if _, err := c.ptmx.Write([]byte("printf '\\033[2J\\033[H'; word=hello; echo \"$word world\"\n")); err != nil {
		t.Fatalf("write first command: %v", err)
	}
	c.waitFor(t, "hello world")

	time.Sleep(time.Second)
	sleepStarted := time.Now()
	if _, err := c.ptmx.Write([]byte("sleep 1; word=\"$word again\"; echo \"$word\"\n")); err != nil {
		t.Fatalf("write second command: %v", err)
	}
	c.waitFor(t, "hello again")
	if elapsed := time.Since(sleepStarted); elapsed < 900*time.Millisecond {
		t.Fatalf("sleeping command completed after %v, want at least 900ms", elapsed)
	}

	time.Sleep(time.Second)
	if _, err := c.ptmx.Write([]byte("word=\"$word from attach\"; printf '<%s>\\n' \"$word\"\n")); err != nil {
		t.Fatalf("write third command: %v", err)
	}
	c.waitFor(t, "<hello again from attach>")
	time.Sleep(300 * time.Millisecond)

	rendered := strings.Join(renderScreen(t, c.output(), cols, rows), "\n")
	want := strings.Join([]string{
		"hello world",
		"shell> sleep 1; word=\"$word again\"; echo \"$word\"",
		"hello again",
		"shell> word=\"$word from attach\"; printf '<%s>\\n' \"$word\"",
		"<hello again from attach>",
		"shell> ",
	}, "\n")
	if rendered != want {
		t.Fatalf("rendered terminal state:\n%q\nwant:\n%q", rendered, want)
	}

	c.detach(t)
}
