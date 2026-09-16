package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// assertNativeTranscriptNeverIsolatesOrResets checks that a natively presented
// attachment never claimed the alternate screen on its own initiative, reset
// the terminal, or cleared the outer scrollback.
func assertNativeTranscriptNeverIsolatesOrResets(t *testing.T, transcript string) {
	t.Helper()
	for _, forbidden := range []string{"\x1b[?1049h", "\x1b[?47h", "\x1b[?1047h", "\x1bc", "\x1b[3J"} {
		if strings.Contains(transcript, forbidden) {
			t.Fatalf("native attach wrote %q; transcript %q", forbidden, transcript)
		}
	}
}

// assertHistoryOrder checks that the emulated history, after replaying an
// attach transcript over pre-attach content, still holds the pre-attach text
// followed by each wanted string in order.
func assertHistoryOrder(t *testing.T, transcript string, cols, rows uint16, wants ...string) {
	t.Helper()
	history := replayAttachTranscriptOverHistory(t, transcript, cols, rows)
	last := strings.Index(history, preAttachRestoredText)
	if last < 0 {
		t.Fatalf("pre-attach terminal content was lost; history=%q transcript=%q", history, transcript)
	}
	for _, want := range wants {
		at := strings.Index(history[last:], want)
		if at < 0 {
			t.Fatalf("history lost %q after %q; history=%q transcript=%q", want, history[last:min(last+40, len(history))], history, transcript)
		}
		last += at
	}
}

// waitForSessionSize gives the daemon a moment to apply the client's advertised
// size, then asks the session (which answers each input line with its tagged
// `stty size`) to report it.
func waitForSessionSize(t *testing.T, c *attachE2EClient, want string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if _, err := c.ptmx.Write([]byte("size\n")); err != nil {
		t.Fatalf("write size probe: %v", err)
	}
	c.waitFor(t, want)
}

// TestAttachNativeStreamsInputAndDetachKeepsHistory drives the default (auto)
// and explicit native presentations against a primary-buffer session: output
// streams into the normal buffer, typed input reaches the session, detach
// prints its outcome, and neither the pre-attach history nor the session's
// lines are lost or hidden behind an alternate screen.
func TestAttachNativeStreamsInputAndDetachKeepsHistory(t *testing.T) {
	for name, args := range map[string][]string{
		"auto":   nil,
		"native": {"--mode", "native"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := runDir(t)
			id := "nat-" + name
			if res := bgxIn(t, dir, "run", id, "cat"); res.exitCode != 0 {
				t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
			}
			t.Cleanup(func() { bgxIn(t, dir, "kill", id) })
			if res := bgxIn(t, dir, "send", id, "hello"); res.exitCode != 0 {
				t.Fatalf("send exit = %d, stderr=%q", res.exitCode, res.stderr)
			}
			historyContains(t, dir, id, "hello")

			c := startAttachE2EClient(t, dir, id, &pty.Winsize{Rows: 24, Cols: 80}, args...)
			defer closePTY(t, c.ptmx)
			c.waitFor(t, "hello")

			if _, err := c.ptmx.Write([]byte("world")); err != nil {
				t.Fatalf("write input: %v", err)
			}
			c.waitFor(t, "world")
			historyContains(t, dir, id, "world")

			c.detach(t)
			out := c.output()
			if !strings.HasSuffix(out, "Detached from session\r\n") {
				t.Fatalf("attach output = %q, want detach outcome suffix", out)
			}
			assertNativeTranscriptNeverIsolatesOrResets(t, out)
			assertAttachLifecycleTranscriptPreservesHistory(t, out, 80, 24, []string{"helloworld"}, "Detached from session")

			info := decodeJSON(t, bgxIn(t, dir, "info", id).stdout)
			if info["running"] != true {
				t.Fatalf("session not running after detach: %v", info)
			}
		})
	}
}

// TestAttachAutoFollowsAlternateScreenTransitions verifies that auto
// presentation starts native on the primary buffer, isolates when the session
// enters the alternate screen (reserving the hint row and shrinking the
// advertised height), returns to native with the primary content restored when
// the session leaves it, and detaches without losing prior history.
func TestAttachAutoFollowsAlternateScreenTransitions(t *testing.T) {
	dir := runDir(t)
	const id = "auto-alt"
	const script = `printf 'PRIMARY-READY\n'
n=0
while IFS= read -r line; do
  case "$line" in
    size) n=$((n+1)); printf 'SIZE%d:%s\n' "$n" "$(stty size)";;
    enter) printf '\033[?1049h\033[2J\033[H\033[1;1HTUI-READY\n';;
    exit) printf '\033[?1049l'; printf 'BACK-ON-PRIMARY\n';;
  esac
done`
	if res := bgxIn(t, dir, "run", id, "sh", "-c", script); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	t.Cleanup(func() { bgxIn(t, dir, "kill", id) })

	c := startAttachE2EClient(t, dir, id, &pty.Winsize{Rows: 24, Cols: 80}, "--show-detach-instructions")
	defer closePTY(t, c.ptmx)
	c.waitFor(t, "PRIMARY-READY")
	waitForSessionSize(t, c, "SIZE1:24 80")
	assertNativeTranscriptNeverIsolatesOrResets(t, c.output())

	if _, err := c.ptmx.Write([]byte("enter\n")); err != nil {
		t.Fatalf("write enter: %v", err)
	}
	c.waitFor(t, "TUI-READY")
	c.waitFor(t, "\x1b[1;23r")
	waitForSessionSize(t, c, "SIZE2:23 80")
	// Let the coalesced repaint of the isolated view land.
	time.Sleep(300 * time.Millisecond)
	rendered := renderScreen(t, c.output(), 80, 24)
	if rows := hintRows(rendered); len(rows) != 1 || rows[0] != 24 {
		t.Fatalf("while isolated, detach hint rendered on rows %v, want only row 24; screen=%q", rows, rendered)
	}
	screen := strings.Join(rendered, "\n")
	if !strings.Contains(screen, "TUI-READY") || strings.Contains(screen, "PRIMARY-READY") {
		t.Fatalf("isolated screen should show only the alternate screen; screen=%q", rendered)
	}

	if _, err := c.ptmx.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	c.waitFor(t, "BACK-ON-PRIMARY")
	waitForSessionSize(t, c, "SIZE3:24 80")
	rendered = renderScreen(t, c.output(), 80, 24)
	screen = strings.Join(rendered, "\n")
	if !strings.Contains(screen, "PRIMARY-READY") || !strings.Contains(screen, "BACK-ON-PRIMARY") {
		t.Fatalf("primary content not restored after the session left the alternate screen; screen=%q", rendered)
	}
	if strings.Contains(screen, "TUI-READY") {
		t.Fatalf("alternate-screen content leaked onto the primary screen; screen=%q", rendered)
	}
	if rows := hintRows(rendered); len(rows) != 0 {
		t.Fatalf("reserved hint still rendered on rows %v after returning to native presentation", rows)
	}

	c.detach(t)
	out := c.output()
	if !strings.HasSuffix(out, "Detached from session\r\n") {
		t.Fatalf("attach output = %q, want detach outcome suffix", out)
	}
	assertHistoryOrder(t, out, 80, 24, "PRIMARY-READY", "BACK-ON-PRIMARY", "SIZE3:24 80", "Detached from session")
}

// TestAttachNativeSessionEndResetsModesAndKeepsHistory verifies that when a
// natively presented session ends, the client prints the outcome, withdraws
// the mouse and bracketed-paste reporting the session had enabled, and leaves
// the pre-attach history and the session's output in the normal buffer.
func TestAttachNativeSessionEndResetsModesAndKeepsHistory(t *testing.T) {
	dir := runDir(t)
	const id = "nat-end"
	const script = `printf '\033[?1000h\033[?1006h\033[?2004h'; printf 'NATIVE-END-READY\n'; cat`
	if res := bgxIn(t, dir, "run", id, "sh", "-c", script); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	c := startAttachE2EClient(t, dir, id, &pty.Winsize{Rows: 24, Cols: 80}, "--mode", "native")
	defer closePTY(t, c.ptmx)
	c.waitFor(t, "NATIVE-END-READY")

	bgxIn(t, dir, "kill", id)
	select {
	case <-c.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("attach client did not exit when the session ended")
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("attach client wait: %v", err)
	}

	out := c.output()
	if !strings.HasSuffix(out, "Session ended\r\n") {
		t.Fatalf("attach output = %q, want session-ended outcome suffix", out)
	}
	assertNativeTranscriptNeverIsolatesOrResets(t, out)
	for _, mode := range []string{"1000", "1006", "2004"} {
		set, reset := "\x1b[?"+mode+"h", "\x1b[?"+mode+"l"
		if strings.LastIndex(out, reset) < strings.LastIndex(out, set) {
			t.Fatalf("mode %s left enabled after session end; transcript %q", mode, out)
		}
	}
	assertHistoryOrder(t, out, 80, 24, "NATIVE-END-READY", "Session ended")
}

// TestAttachPrimaryBufferTUIRendersNatively verifies that a TUI redrawing the
// primary buffer with cursor addressing and line erasure (never entering the
// alternate screen) renders exactly under auto and native presentation.
func TestAttachPrimaryBufferTUIRendersNatively(t *testing.T) {
	for name, args := range map[string][]string{
		"auto":   nil,
		"native": {"--mode", "native"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := runDir(t)
			id := "tui-" + name
			const script = `stty -echo
printf '\033[2J\033[H\033[1;1HTITLE-BAR\033[3;1HCOUNT 0'
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '\033[3;1H\033[2KCOUNT %d\033[5;1H\033[2KLAST %s\033[7;1H\033[2KEND-%d' "$n" "$line" "$n"
done`
			if res := bgxIn(t, dir, "run", id, "sh", "-c", script); res.exitCode != 0 {
				t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
			}
			t.Cleanup(func() { bgxIn(t, dir, "kill", id) })

			c := startAttachE2EClient(t, dir, id, &pty.Winsize{Rows: 24, Cols: 80}, args...)
			defer closePTY(t, c.ptmx)
			c.waitFor(t, "TITLE-BAR")
			for i, line := range []string{"alpha", "beta"} {
				if _, err := c.ptmx.Write([]byte(line + "\n")); err != nil {
					t.Fatalf("write %q: %v", line, err)
				}
				c.waitFor(t, "END-"+string(rune('1'+i)))
			}

			assertNativeTranscriptNeverIsolatesOrResets(t, c.output())
			rendered := strings.Join(renderScreen(t, c.output(), 80, 24), "\n")
			want := strings.Join([]string{
				"TITLE-BAR",
				"",
				"COUNT 2",
				"",
				"LAST beta",
				"",
				"END-2",
			}, "\n")
			if rendered != want {
				t.Fatalf("rendered terminal state:\n%q\nwant:\n%q", rendered, want)
			}

			c.detach(t)
			assertHistoryOrder(t, c.output(), 80, 24, "TITLE-BAR", "END-2", "Detached from session")
		})
	}
}

// TestAttachAutoJoinsSessionAlreadyInAlternateScreen verifies that auto
// presentation attaching to a session already on its alternate screen starts
// isolated (reserving the hint row and advertising one row less), then follows
// the session back to native presentation with the primary content it never
// displayed restored, and detaches with prior history intact.
func TestAttachAutoJoinsSessionAlreadyInAlternateScreen(t *testing.T) {
	dir := runDir(t)
	const id = "auto-in-alt"
	const script = `printf 'PRIMARY-BEFORE\n'
printf '\033[?1049h\033[2J\033[H\033[1;1HALT-READY\n'
while IFS= read -r line; do
  case "$line" in
    size) printf 'SIZE:%s\n' "$(stty size)";;
    exit) printf '\033[?1049l'; printf 'PRIMARY-AFTER\n';;
  esac
done`
	if res := bgxIn(t, dir, "run", id, "sh", "-c", script); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	t.Cleanup(func() { bgxIn(t, dir, "kill", id) })
	historyContains(t, dir, id, "ALT-READY")

	c := startAttachE2EClient(t, dir, id, &pty.Winsize{Rows: 24, Cols: 80}, "--show-detach-instructions")
	defer closePTY(t, c.ptmx)
	c.waitFor(t, "ALT-READY")
	c.waitFor(t, "detach: ctrl+\\")
	if !strings.Contains(c.output(), "\x1b[?1049h") {
		t.Fatalf("attach to a session on its alternate screen did not isolate; transcript %q", c.output())
	}
	waitForSessionSize(t, c, "SIZE:23 80")
	time.Sleep(300 * time.Millisecond)
	rendered := renderScreen(t, c.output(), 80, 24)
	if rows := hintRows(rendered); len(rows) != 1 || rows[0] != 24 {
		t.Fatalf("detach hint rendered on rows %v, want only row 24; screen=%q", rows, rendered)
	}
	if screen := strings.Join(rendered, "\n"); strings.Contains(screen, "PRIMARY-BEFORE") {
		t.Fatalf("primary content shown while the session is on its alternate screen; screen=%q", rendered)
	}

	if _, err := c.ptmx.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	c.waitFor(t, "PRIMARY-AFTER")
	waitForSessionSize(t, c, "SIZE:24 80")
	rendered = renderScreen(t, c.output(), 80, 24)
	screen := strings.Join(rendered, "\n")
	if !strings.Contains(screen, "PRIMARY-BEFORE") || !strings.Contains(screen, "PRIMARY-AFTER") {
		t.Fatalf("primary content not restored after leaving the alternate screen; screen=%q", rendered)
	}
	if strings.Contains(screen, "ALT-READY") {
		t.Fatalf("alternate-screen content leaked onto the primary screen; screen=%q", rendered)
	}

	c.detach(t)
	assertHistoryOrder(t, c.output(), 80, 24, "PRIMARY-BEFORE", "PRIMARY-AFTER", "Detached from session")
}
