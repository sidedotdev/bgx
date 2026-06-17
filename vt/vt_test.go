package vt

import (
	"bytes"
	"strings"
	"testing"

	lg "go.mitchellh.com/libghostty"
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
	term.Write([]byte("\x1b[2J\x1b[Hhello world"))
	term.Write([]byte("\x1b[HHELLO"))

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
	colored.Write([]byte("\x1b[31mX\x1b[0m"))
	plainTerm := newTerm(t)
	plainTerm.Write([]byte("X"))

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
	src.Write([]byte("\x1b[2J\x1b[Hline one\r\nline two\x1b[1;1HX"))

	snap, err := src.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen: %v", err)
	}

	dst := newTerm(t)
	dst.Write([]byte("\x1b[2J\x1b[H"))
	if _, err := dst.Write(snap); err != nil {
		t.Fatalf("replay write: %v", err)
	}

	if got, want := plainText(t, dst), plainText(t, src); got != want {
		t.Fatalf("replay mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestResizeReflowsContent(t *testing.T) {
	term := newTerm(t)
	term.Write([]byte(strings.Repeat("a", 100)))

	if err := term.Resize(40, DefaultRows); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if got := strings.Count(plainText(t, term), "a"); got != 100 {
		t.Fatalf("after reflow want 100 cells, got %d", got)
	}
}
