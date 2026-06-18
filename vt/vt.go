// Package vt maintains a libghostty-vt terminal emulator fed the same byte
// stream as a session's PTY output. Unlike the raw scrollback store, it tracks
// the live visible screen so a newly attaching client can be handed a faithful
// snapshot of the current terminal state (contents, styles, and cursor) to
// replay, then continue streaming raw output.
//
// It wraps the cgo-backed go.mitchellh.com/libghostty bindings, which link the
// native libghostty-vt-static library via pkg-config.
//
// DumpScreen ports the terminal-state serialization approach from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.
package vt

import (
	"sync"

	lg "go.mitchellh.com/libghostty"
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
	mu   sync.Mutex
	term *lg.Terminal
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
// visible screen state. Malformed input is handled gracefully by libghostty and
// never reported as an error, so Write always reports the full length consumed,
// satisfying io.Writer for use as a tee target alongside the scrollback store.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.term.VTWrite(p)
	return len(p), nil
}

// Resize changes the terminal dimensions, reflowing existing content. Cell
// pixel dimensions are irrelevant to text rendering and are left at zero.
func (t *Terminal) Resize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.term.Resize(cols, rows, 0, 0)
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
