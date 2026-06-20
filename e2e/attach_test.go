package e2e

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

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

	cmd := exec.Command(binPath, "attach", "att")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "TMPDIR="+dir)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer ptmx.Close()

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
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
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	// Detaching restores the local terminal with a full reset (ESC c).
	if !strings.Contains(output(), "\x1bc") {
		t.Fatalf("detach did not reset the terminal; got %q", output())
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
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "TMPDIR="+dir)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 120})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer ptmx.Close()

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
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
	_ = cmd.Wait()

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
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "TMPDIR="+dir)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer ptmx.Close()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 64<<10)
		for {
			if _, rerr := ptmx.Read(buf); rerr != nil {
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
}

func (c *attachE2EClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.got)
}

// startAttachE2EClient launches `bgx attach id` under a pty of the given size
// (nil for the default) and pumps its output into the returned client.
func startAttachE2EClient(t *testing.T, dir, id string, ws *pty.Winsize) *attachE2EClient {
	t.Helper()
	cmd := exec.Command(binPath, "attach", id)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "TMPDIR="+dir)
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
	c := &attachE2EClient{cmd: cmd, ptmx: ptmx, readDone: make(chan struct{})}
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
	_ = c.cmd.Wait()
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
	defer a.ptmx.Close()
	b := startAttachE2EClient(t, dir, "multi", nil)
	defer b.ptmx.Close()

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
	defer a.ptmx.Close()
	b := startAttachE2EClient(t, dir, "minrsz", &pty.Winsize{Rows: 40, Cols: 120})
	defer b.ptmx.Close()

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
// that when the session ends the client exits on its own and resets only the
// cursor (not a full terminal reset), leaving the final rendered output visible.
func TestAttachClosesOnSessionEnd(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "att2", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	if res := bgxIn(t, dir, "send", "att2", "hello"); res.exitCode != 0 {
		t.Fatalf("send exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	historyContains(t, dir, "att2", "hello")

	cmd := exec.Command(binPath, "attach", "att2")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "TMPDIR="+dir)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer ptmx.Close()

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
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
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	// Session end resets only the cursor (show cursor + reset SGR) and must not
	// issue the full terminal reset (ESC c) that a ctrl+\ detach uses.
	out := output()
	if strings.Contains(out, "\x1bc") {
		t.Fatalf("session end performed a full terminal reset; got %q", out)
	}
	if !strings.Contains(out, "\x1b[?25h\x1b[0m") {
		t.Fatalf("session end did not reset the cursor; got %q", out)
	}
}
