// Package vt maintains a libghostty-vt terminal emulator fed the same byte
// stream as a session's PTY output. Unlike the raw scrollback store, it tracks
// the live visible screen so a newly attaching client can be handed a faithful
// snapshot of the current terminal state (contents, styles, and cursor) to
// replay, then continue streaming raw output.
//
// It wraps the cgo-backed libghostty-vt-static bindings, which bundle native
// static libraries for the supported platforms.
//
// DumpScreen ports the terminal-state serialization approach from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.
package vt

import (
	"sync"

	lg "github.com/ehsanul/libghostty-vt-static"
	"github.com/sidedotdev/bgx/vtscan"
)

const (
	// DefaultCols and DefaultRows mirror the daemon's default PTY size.
	DefaultCols = 80
	DefaultRows = 24
)

// Terminal is a concurrency-safe wrapper around a libghostty terminal. The
// daemon feeds PTY output via Write from its output pump while attach handlers
// concurrently call DumpScreen, so every libghostty call is serialized.
type Terminal struct {
	mu          sync.Mutex
	term        *lg.Terminal
	scan        vtscan.Scanner
	writtenRows uint16
}

// New returns a Terminal sized to cols x rows.
func New(cols, rows uint16) (*Terminal, error) {
	term, err := lg.NewTerminal(lg.WithSize(cols, rows))
	if err != nil {
		return nil, err
	}
	return &Terminal{term: term}, nil
}

// Write feeds raw VT-encoded bytes through the terminal's parser, updating the
// visible screen state. Cursor position is sampled between complete terminal
// operations so later cursor movement cannot hide rows touched earlier in the
// same write. Extent sampling is supplemental and does not alter io.Writer's
// all-bytes-consumed contract.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	start := 0
	flush := func(end int) {
		if start == end {
			return
		}
		t.term.VTWrite(p[start:end])
		start = end
		row, err := t.term.CursorY()
		if err != nil {
			return
		}
		if touched := row + 1; touched > t.writtenRows {
			t.writtenRows = touched
		}
	}

	for i, b := range p {
		wasGround := t.scan.AtGround()
		control := b < 0x20 || b == 0x7f
		if wasGround && (b == 0x1b || control) {
			flush(i)
		}

		t.scan.Advance(p[i : i+1])
		sequenceEnded := !wasGround && t.scan.AtGround()
		groundControl := wasGround && control && b != 0x1b
		if sequenceEnded || groundControl {
			flush(i + 1)
		}
	}
	flush(len(p))
	return len(p), nil
}

// Resize changes the terminal dimensions, reflowing existing content. Cell
// pixel dimensions are irrelevant to text rendering and are left at zero.
func (t *Terminal) Resize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.term.Resize(cols, rows, 0, 0); err != nil {
		return err
	}
	if t.writtenRows > rows {
		t.writtenRows = rows
	}
	return nil
}

// WrittenRows returns the 1-based greatest row reached while processing output.
// It is zero until output first affects the terminal parser.
func (t *Terminal) WrittenRows() (uint16, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writtenRows, nil
}

// DumpScreen renders the current visible terminal state as VT sequences that
// reproduce the screen (contents, styles, and cursor position) when replayed
// onto a client terminal. It reflects the final state of the stream rather than
// the raw byte history. The caller is expected to clear its screen first.
func (t *Terminal) DumpScreen() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := lg.NewFormatter(t.term,
		lg.WithFormatterFormat(lg.FormatterFormatVT),
		lg.WithFormatterExtraStyle(true),
		lg.WithFormatterExtraCursor(true),
	)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Format()
}

// Close releases the underlying libghostty terminal.
func (t *Terminal) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.term != nil {
		t.term.Close()
		t.term = nil
	}
}
