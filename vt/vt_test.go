package vt

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	lg "github.com/ehsanul/libghostty-vt-static"
)

// plainText renders the terminal's visible contents as trimmed plain text so
// tests can assert on screen state independent of the VT escape encoding.
func plainText(t *testing.T, term *Terminal) string {
	t.Helper()
	term.mu.Lock()
	defer term.mu.Unlock()
	f, err := lg.NewFormatter(term.term,
		lg.WithFormatterFormat(lg.FormatterFormatPlain),
		lg.WithFormatterTrim(true),
	)
	if err != nil {
		t.Fatalf("NewFormatter: %v", err)
	}
	defer f.Close()
	s, err := f.FormatString()
	if err != nil {
		t.Fatalf("FormatString: %v", err)
	}
	return s
}

func newTerm(t *testing.T) *Terminal {
	t.Helper()
	term, err := New(DefaultCols, DefaultRows)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(term.Close)
	return term
}

func TestDumpScreenReflectsFinalState(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte("\x1b[2J\x1b[Hhello world")); err != nil {
		t.Fatalf("write initial screen: %v", err)
	}
	if _, err := term.Write([]byte("\x1b[HHELLO")); err != nil {
		t.Fatalf("write overwrite: %v", err)
	}

	dump, err := term.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen: %v", err)
	}
	if !bytes.Contains(dump, []byte("HELLO")) {
		t.Errorf("dump missing overwritten text %q: %q", "HELLO", dump)
	}
	if !bytes.Contains(dump, []byte("world")) {
		t.Errorf("dump missing untouched text %q: %q", "world", dump)
	}
	if bytes.Contains(dump, []byte("hello")) {
		t.Errorf("dump still contains overwritten text %q: %q", "hello", dump)
	}
}

func TestDumpScreenPreservesColor(t *testing.T) {
	colored := newTerm(t)
	if _, err := colored.Write([]byte("\x1b[31mX\x1b[0m")); err != nil {
		t.Fatalf("write colored screen: %v", err)
	}
	plainTerm := newTerm(t)
	if _, err := plainTerm.Write([]byte("X")); err != nil {
		t.Fatalf("write plain screen: %v", err)
	}

	cdump, err := colored.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen colored: %v", err)
	}
	pdump, err := plainTerm.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen plain: %v", err)
	}
	if bytes.Equal(cdump, pdump) {
		t.Fatalf("expected colored dump to differ from plain dump, both %q", cdump)
	}
}

func TestDumpScreenReplayReproducesScreen(t *testing.T) {
	src := newTerm(t)
	if _, err := src.Write([]byte("\x1b[2J\x1b[Hline one\r\nline two\x1b[1;1HX")); err != nil {
		t.Fatalf("write source screen: %v", err)
	}

	snap, err := src.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen: %v", err)
	}

	dst := newTerm(t)
	if _, err := dst.Write([]byte("\x1b[2J\x1b[H")); err != nil {
		t.Fatalf("clear destination screen: %v", err)
	}
	if _, err := dst.Write(snap); err != nil {
		t.Fatalf("replay write: %v", err)
	}

	if got, want := plainText(t, dst), plainText(t, src); got != want {
		t.Fatalf("replay mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestResizeReflowsContent(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte(strings.Repeat("a", 100))); err != nil {
		t.Fatalf("write content: %v", err)
	}

	if err := term.Resize(40, DefaultRows); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if got := strings.Count(plainText(t, term), "a"); got != 100 {
		t.Fatalf("after reflow want 100 cells, got %d", got)
	}
}
func TestWrittenRowsTracksCursorHighWaterMark(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte("text\r\n\r\n\x1b[H")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	rows, err := term.WrittenRows()
	if err != nil {
		t.Fatalf("WrittenRows: %v", err)
	}
	if rows != 3 {
		t.Fatalf("WrittenRows = %d, want 3", rows)
	}
}

func TestWrittenRowsClampsWhenTerminalShrinks(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte("\x1b[20Htext")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := term.Resize(80, 10); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	rows, err := term.WrittenRows()
	if err != nil {
		t.Fatalf("WrittenRows: %v", err)
	}
	if rows != 10 {
		t.Fatalf("WrittenRows = %d, want 10", rows)
	}
}

// activeScreen cross-checks the locally tracked screen against libghostty's own
// view of which screen is active.
func activeScreen(t *testing.T, term *Terminal) lg.TerminalScreen {
	t.Helper()
	term.mu.Lock()
	defer term.mu.Unlock()
	screen, err := term.term.ActiveScreen()
	if err != nil {
		t.Fatalf("ActiveScreen: %v", err)
	}
	return screen
}

func cursor(t *testing.T, term *Terminal) (x, y uint16) {
	t.Helper()
	term.mu.Lock()
	defer term.mu.Unlock()
	x, err := term.term.CursorX()
	if err != nil {
		t.Fatalf("CursorX: %v", err)
	}
	y, err = term.term.CursorY()
	if err != nil {
		t.Fatalf("CursorY: %v", err)
	}
	return x, y
}

// primaryText renders the primary screen's plain text even while the alternate
// screen is active, switching screens raw (mode 47 preserves both) so the
// terminal under test is left as it was.
func primaryText(t *testing.T, term *Terminal) string {
	t.Helper()
	if !term.AltScreen() {
		return plainText(t, term)
	}
	term.mu.Lock()
	term.term.VTWrite([]byte("\x1b[?47l"))
	term.mu.Unlock()
	defer func() {
		term.mu.Lock()
		term.term.VTWrite([]byte("\x1b[?47h"))
		term.mu.Unlock()
	}()
	return plainText(t, term)
}

func trackedModeState(term *Terminal) map[int]bool {
	term.mu.Lock()
	defer term.mu.Unlock()
	out := make(map[int]bool, len(term.modes))
	for m, v := range term.modes {
		out[m] = v
	}
	return out
}

func assertAlt(t *testing.T, term *Terminal, want bool, when string) {
	t.Helper()
	if got := term.AltScreen(); got != want {
		t.Fatalf("%s: AltScreen() = %v, want %v", when, got, want)
	}
	wantScreen := lg.ScreenPrimary
	if want {
		wantScreen = lg.ScreenAlternate
	}
	if got := activeScreen(t, term); got != wantScreen {
		t.Fatalf("%s: libghostty active screen = %v, want %v", when, got, wantScreen)
	}
}

func TestAltScreenTracksModeSequencesAcrossWritesAndRIS(t *testing.T) {
	term := newTerm(t)
	assertAlt(t, term, false, "fresh terminal")

	if _, err := term.Write([]byte("\x1b[?1049h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, true, "after ?1049h")

	if _, err := term.Write([]byte("\x1b[?1049l")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, false, "after ?1049l")

	// A sequence split across writes must be recognized when it completes.
	if _, err := term.Write([]byte("text\x1b[?10")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, false, "mid-sequence")
	if _, err := term.Write([]byte("49h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, true, "after split ?1049h")

	if _, err := term.Write([]byte("\x1bc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, false, "after RIS")

	if _, err := term.Write([]byte("\x1b[?47h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, true, "after ?47h")
	if _, err := term.Write([]byte("\x1b[?1047l")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertAlt(t, term, false, "after ?1047l")
}

func TestTrackedModesFollowSetResetAndRIS(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte("\x1b[?1000;1006h\x1b[?2004h\x1b[?25l\x1b[?1h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := trackedModeState(term)
	for _, m := range []int{1, 1000, 1006, 2004} {
		if !got[m] {
			t.Errorf("mode %d not tracked as set: %v", m, got)
		}
	}
	if got[25] {
		t.Errorf("mode 25 still set after ?25l: %v", got)
	}
	if got[1002] || got[1003] || got[1004] {
		t.Errorf("untouched modes changed: %v", got)
	}

	if _, err := term.Write([]byte("\x1b[?1000l")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if trackedModeState(term)[1000] {
		t.Fatalf("mode 1000 still set after ?1000l")
	}

	if _, err := term.Write([]byte("\x1bc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got = trackedModeState(term)
	for _, m := range trackedModes {
		if want := m == 25; got[m] != want {
			t.Errorf("after RIS mode %d = %v, want %v", m, got[m], want)
		}
	}
}

func TestWriteUntilScreenSwitchStopsAfterEachToggle(t *testing.T) {
	term := newTerm(t)
	payload := []byte("abc\x1b[?1049hdef\x1b[?1049h\x1b[?1049lghi")

	before, consumed, switched, err := term.WriteUntilScreenSwitch(payload)
	if err != nil {
		t.Fatalf("WriteUntilScreenSwitch: %v", err)
	}
	if !switched || before != 3 || consumed != 11 {
		t.Fatalf("first call = (%d, %d, %v), want (3, 11, true)", before, consumed, switched)
	}
	assertAlt(t, term, true, "after first switch")
	if !strings.Contains(primaryText(t, term), "abc") || strings.Contains(plainText(t, term), "def") {
		t.Fatalf("bytes past the switch were consumed: primary=%q alt=%q", primaryText(t, term), plainText(t, term))
	}

	// Re-entering the alternate screen while already on it is not a toggle, so
	// the second call must run through to the real exit.
	rest := payload[consumed:]
	before, consumed, switched, err = term.WriteUntilScreenSwitch(rest)
	if err != nil {
		t.Fatalf("WriteUntilScreenSwitch: %v", err)
	}
	if !switched || before != 11 || consumed != 19 {
		t.Fatalf("second call = (%d, %d, %v), want (11, 19, true)", before, consumed, switched)
	}
	assertAlt(t, term, false, "after second switch")

	rest = rest[consumed:]
	before, consumed, switched, err = term.WriteUntilScreenSwitch(rest)
	if err != nil {
		t.Fatalf("WriteUntilScreenSwitch: %v", err)
	}
	if switched || before != len(rest) || consumed != len(rest) {
		t.Fatalf("third call = (%d, %d, %v), want (%d, %d, false)", before, consumed, switched, len(rest), len(rest))
	}
	if got := plainText(t, term); !strings.Contains(got, "ghi") {
		t.Fatalf("trailing bytes not written: %q", got)
	}
}

func TestWriteUntilScreenSwitchReportsSequenceBegunEarlier(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte("\x1b[?10")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before, consumed, switched, err := term.WriteUntilScreenSwitch([]byte("49hxyz"))
	if err != nil {
		t.Fatalf("WriteUntilScreenSwitch: %v", err)
	}
	if !switched || before != 0 || consumed != 3 {
		t.Fatalf("got (%d, %d, %v), want (0, 3, true)", before, consumed, switched)
	}
	assertAlt(t, term, true, "after completing split sequence")
}

func TestWriteUntilScreenSwitchTreatsRISOnAltAsSwitch(t *testing.T) {
	term := newTerm(t)
	if _, err := term.Write([]byte("\x1b[?1049h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before, consumed, switched, err := term.WriteUntilScreenSwitch([]byte("x\x1bcy"))
	if err != nil {
		t.Fatalf("WriteUntilScreenSwitch: %v", err)
	}
	if !switched || before != 1 || consumed != 3 {
		t.Fatalf("got (%d, %d, %v), want (1, 3, true)", before, consumed, switched)
	}
	assertAlt(t, term, false, "after RIS")
}

func replaySnapshot(t *testing.T, snap []byte) *Terminal {
	t.Helper()
	dst := newTerm(t)
	// The replaying terminal carries unrelated prior state to prove the
	// snapshot restores everything it needs without a full reset.
	if _, err := dst.Write([]byte("stale\r\nscreen\x1b[?1000h\x1b[5;10r\x1b[31m")); err != nil {
		t.Fatalf("prime destination: %v", err)
	}
	if _, err := dst.Write(snap); err != nil {
		t.Fatalf("replay snapshot: %v", err)
	}
	return dst
}

func TestSnapshotRoundTripReproducesBothScreensCursorAndModes(t *testing.T) {
	// The alternate cursor is deliberately far from where ?1049h saved the
	// primary cursor (end of "primary two"), so a snapshot that leaked the
	// alternate cursor into the saved primary cursor would be caught below.
	session := [][]byte{
		[]byte("\x1b[2J\x1b[Hprimary one\r\nprimary two\x1b[?2004h\x1b[?1h"),
		[]byte("\x1b[?1049h\x1b[2J\x1b[5;5HALT SCREEN\x1b[?25l\x1b[?1000;1006h\x1b[3;7H"),
	}
	// control receives the identical session stream but is never snapshotted,
	// proving snapshotting does not alter the source's later restoration.
	src, control := newTerm(t), newTerm(t)
	for _, chunk := range session {
		for name, term := range map[string]*Terminal{"source": src, "control": control} {
			if _, err := term.Write(chunk); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}
	altBefore := plainText(t, src)
	primaryBefore := primaryText(t, src)
	cxBefore, cyBefore := cursor(t, src)

	snap, err := src.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.HasPrefix(snap, []byte(SnapshotPrefix)) {
		t.Fatalf("snapshot %q does not begin with SnapshotPrefix", snap)
	}
	for _, forbidden := range []string{"\x1bc", "\x1b[3J"} {
		if bytes.Contains(snap, []byte(forbidden)) {
			t.Fatalf("snapshot contains %q, which would clear client scrollback", forbidden)
		}
	}

	// Snapshotting must leave the source's own state untouched.
	assertAlt(t, src, true, "source after snapshot")
	if got := plainText(t, src); got != altBefore {
		t.Fatalf("source alternate screen changed by snapshot:\n got=%q\nwant=%q", got, altBefore)
	}
	if cx, cy := cursor(t, src); cx != cxBefore || cy != cyBefore {
		t.Fatalf("source cursor moved by snapshot: (%d,%d) -> (%d,%d)", cxBefore, cyBefore, cx, cy)
	}

	dst := replaySnapshot(t, snap)
	assertAlt(t, dst, true, "replayed terminal")
	if got := plainText(t, dst); got != altBefore {
		t.Fatalf("replayed alternate screen mismatch:\n got=%q\nwant=%q", got, altBefore)
	}
	if got := primaryText(t, dst); got != primaryBefore {
		t.Fatalf("replayed primary screen mismatch:\n got=%q\nwant=%q", got, primaryBefore)
	}
	if cx, cy := cursor(t, dst); cx != cxBefore || cy != cyBefore {
		t.Fatalf("replayed cursor = (%d,%d), want (%d,%d)", cx, cy, cxBefore, cyBefore)
	}
	if got, want := trackedModeState(dst), trackedModeState(src); !maps.Equal(got, want) {
		t.Fatalf("replayed modes = %v, want %v", got, want)
	}

	// The session leaving the alternate screen afterwards restores the primary
	// content and the saved cursor identically on the control, the snapshotted
	// source, and the replayed terminal, so ordinary output that follows
	// renders the same everywhere.
	terms := map[string]*Terminal{"control": control, "source": src, "replayed": dst}
	for name, term := range terms {
		if _, err := term.Write([]byte("\x1b[?1049l")); err != nil {
			t.Fatalf("%s leave alt: %v", name, err)
		}
		assertAlt(t, term, false, name+" after ?1049l")
		if got := plainText(t, term); got != primaryBefore {
			t.Fatalf("%s primary screen after leaving alt:\n got=%q\nwant=%q", name, got, primaryBefore)
		}
	}
	wantX, wantY := cursor(t, control)
	for name, term := range terms {
		if cx, cy := cursor(t, term); cx != wantX || cy != wantY {
			t.Fatalf("%s cursor after ?1049l = (%d,%d), want control's (%d,%d)", name, cx, cy, wantX, wantY)
		}
	}
	for name, term := range terms {
		if _, err := term.Write([]byte(" resumed\r\nnext line")); err != nil {
			t.Fatalf("%s write after alt: %v", name, err)
		}
	}
	want := plainText(t, control)
	for name, term := range terms {
		if got := plainText(t, term); got != want {
			t.Fatalf("%s screen after post-alt output:\n got=%q\nwant=%q", name, got, want)
		}
	}
}

func TestSnapshotOnPrimaryScreenRestoresScreenAndModesWithoutAlt(t *testing.T) {
	src := newTerm(t)
	if _, err := src.Write([]byte("\x1b[2J\x1b[Hline one\r\nline two\x1b[?25l\x1b[2;4H")); err != nil {
		t.Fatalf("write source: %v", err)
	}
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if bytes.Contains(snap, []byte("\x1b[?1049h")) {
		t.Fatalf("snapshot of a primary-screen session enters the alternate screen: %q", snap)
	}
	if !bytes.Contains(snap, []byte("\x1b[?25l")) || !bytes.Contains(snap, []byte("\x1b[?1000l")) {
		t.Fatalf("snapshot does not restore tracked modes explicitly: %q", snap)
	}

	dst := replaySnapshot(t, snap)
	assertAlt(t, dst, false, "replayed terminal")
	if got, want := plainText(t, dst), plainText(t, src); got != want {
		t.Fatalf("replayed screen mismatch:\n got=%q\nwant=%q", got, want)
	}
	sx, sy := cursor(t, src)
	if cx, cy := cursor(t, dst); cx != sx || cy != sy {
		t.Fatalf("replayed cursor = (%d,%d), want (%d,%d)", cx, cy, sx, sy)
	}
	if got, want := trackedModeState(dst), trackedModeState(src); !maps.Equal(got, want) {
		t.Fatalf("replayed modes = %v, want %v", got, want)
	}
}
