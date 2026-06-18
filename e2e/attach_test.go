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

// TestAttachResizePropagates verifies the leader's window size reaches the
// session PTY on attach and again after a SIGWINCH-driven resize.
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
