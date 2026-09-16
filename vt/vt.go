// Package vt maintains a libghostty-vt terminal emulator fed the same byte
// stream as a session's PTY output. Unlike the raw scrollback store, it tracks
// the live visible screen so a newly attaching client can be handed a faithful
// snapshot of the current terminal state (contents, styles, cursor, active
// screen, and input modes) to replay, then continue streaming raw output.
//
// It wraps the cgo-backed libghostty-vt-static bindings, which bundle native
// static libraries for the supported platforms.
//
// DumpScreen ports the terminal-state serialization approach from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.
package vt

import (
	"bytes"
	"fmt"
	"sync"

	lg "github.com/ehsanul/libghostty-vt-static"
	"github.com/sidedotdev/bgx/vtscan"
)

const (
	// DefaultCols and DefaultRows mirror the daemon's default PTY size.
	DefaultCols = 80
	DefaultRows = 24
)

// SnapshotPrefix begins every Snapshot. It returns the replaying terminal to
// its primary screen and clears that screen without touching scrollback, so a
// snapshot both replays correctly from any prior state and is recognizable in
// the output stream as a (re)synchronization point.
const SnapshotPrefix = "\x1b[?1049l\x1b[2J\x1b[H"

// trackedModes are the DEC private modes a Snapshot restores explicitly: the
// ones a replaying terminal must agree with the session on for cursor
// visibility (25), keyboard (1), mouse (1000/1002/1003/1006), focus (1004) and
// bracketed paste (2004) reporting to work after the replay.
var trackedModes = []int{1, 25, 1000, 1002, 1003, 1004, 1006, 2004}

// maxTrackedSequence bounds the buffered in-progress escape sequence. Mode
// sequences are short; longer strings (OSC, DCS) are never inspected.
const maxTrackedSequence = 64

// Terminal is a concurrency-safe wrapper around a libghostty terminal. The
// daemon feeds PTY output via Write from its output pump while attach handlers
// concurrently call Snapshot, so every libghostty call is serialized.
//
// The active screen and tracked modes are followed locally by inspecting each
// completed escape sequence rather than queried from libghostty, so screen
// switches can be located to the byte within a write.
type Terminal struct {
	mu          sync.Mutex
	term        *lg.Terminal
	scan        vtscan.Scanner
	writtenRows uint16

	// seq accumulates the in-progress escape sequence so a mode change split
	// across writes is still recognized once it completes.
	seq   []byte
	alt   bool
	modes map[int]bool
	// primarySaved is the primary-screen cursor saved by ?1049h. libghostty
	// keeps it internally but copies the alternate cursor over the primary one
	// on every screen switch, so it must be sampled here for Snapshot to
	// reproduce the position ?1049l restores.
	primarySaved savedCursor
}

type savedCursor struct {
	x, y uint16
	ok   bool
}

// New returns a Terminal sized to cols x rows.
func New(cols, rows uint16) (*Terminal, error) {
	term, err := lg.NewTerminal(lg.WithSize(cols, rows))
	if err != nil {
		return nil, err
	}
	t := &Terminal{term: term}
	t.resetModes()
	return t, nil
}

// resetModes returns the tracked screen and mode state to power-on defaults.
func (t *Terminal) resetModes() {
	t.alt = false
	t.primarySaved = savedCursor{}
	t.modes = make(map[int]bool, len(trackedModes))
	for _, m := range trackedModes {
		t.modes[m] = m == 25
	}
}

// Write feeds raw VT-encoded bytes through the terminal's parser, updating the
// visible screen state. Cursor position is sampled between complete terminal
// operations so later cursor movement cannot hide rows touched earlier in the
// same write. Extent sampling is supplemental and does not alter io.Writer's
// all-bytes-consumed contract.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.write(p, false)
	return len(p), nil
}

// WriteUntilScreenSwitch feeds p like Write but stops right after the first
// complete sequence that toggles the active screen (alternate screen modes
// 47/1047/1049, or RIS while on the alternate screen). It returns the offset at
// which that sequence begins (0 when it began in an earlier write) and the
// offset just past it, so a caller can present p[:before] under the previous
// screen's presentation, switch, and continue from p[consumed:]. When no switch
// completes, all of p is consumed and switched is false.
func (t *Terminal) WriteUntilScreenSwitch(p []byte) (before, consumed int, switched bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	before, consumed, switched = t.write(p, true)
	return before, consumed, switched, nil
}

// write is the shared body of Write and WriteUntilScreenSwitch. The caller
// must hold t.mu.
func (t *Terminal) write(p []byte, stopOnSwitch bool) (before, consumed int, switched bool) {
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

	// seqStart is the offset in p of the in-progress sequence, or -1 when it
	// began in an earlier write.
	seqStart := -1
	for i, b := range p {
		wasGround := t.scan.AtGround()
		control := b < 0x20 || b == 0x7f
		if wasGround && (b == 0x1b || control) {
			flush(i)
		}

		// ESC restarts a sequence even inside string states, matching vtscan.
		if wasGround || b == 0x1b {
			t.seq = t.seq[:0]
			seqStart = i
		}
		if len(t.seq) < maxTrackedSequence {
			t.seq = append(t.seq, b)
		}

		t.scan.Advance(p[i : i+1])
		sequenceEnded := !wasGround && t.scan.AtGround()
		groundControl := wasGround && control && b != 0x1b
		if sequenceEnded && t.trackSequence(t.seq) && stopOnSwitch {
			flush(i + 1)
			return max(seqStart, 0), i + 1, true
		}
		if sequenceEnded || groundControl {
			flush(i + 1)
		}
	}
	flush(len(p))
	return len(p), len(p), false
}

// trackSequence applies a just-completed sequence to the locally tracked screen
// and mode state and reports whether it toggled the active screen. Only RIS and
// DEC private mode set/reset (CSI ? Pm h/l) are inspected. It runs before the
// sequence reaches libghostty, so the cursor it samples on ?1049h is exactly the
// primary cursor that sequence saves.
func (t *Terminal) trackSequence(seq []byte) (switched bool) {
	if len(seq) < 2 || seq[0] != 0x1b {
		return false
	}
	wasAlt := t.alt
	if len(seq) == 2 && seq[1] == 'c' {
		t.resetModes()
		return wasAlt
	}
	if len(seq) < 5 || seq[1] != '[' || seq[2] != '?' {
		return false
	}
	final := seq[len(seq)-1]
	if final != 'h' && final != 'l' {
		return false
	}
	set := final == 'h'
	for _, param := range bytes.Split(seq[3:len(seq)-1], []byte{';'}) {
		n, ok := parseModeParam(param)
		if !ok {
			continue
		}
		switch n {
		case 1049:
			if set && !t.alt {
				t.primarySaved = t.sampleCursor()
			} else if !set {
				t.primarySaved = savedCursor{}
			}
			t.alt = set
		case 47, 1047:
			t.alt = set
		default:
			if _, tracked := t.modes[n]; tracked {
				t.modes[n] = set
			}
		}
	}
	return t.alt != wasAlt
}

// sampleCursor reads the active cursor position. The caller must hold t.mu.
func (t *Terminal) sampleCursor() savedCursor {
	x, err := t.term.CursorX()
	if err != nil {
		return savedCursor{}
	}
	y, err := t.term.CursorY()
	if err != nil {
		return savedCursor{}
	}
	return savedCursor{x: x, y: y, ok: true}
}

// parseModeParam parses a plain decimal CSI parameter; anything else (empty,
// sub-parameters, intermediates) is not a mode number.
func parseModeParam(param []byte) (int, bool) {
	if len(param) == 0 || len(param) > 5 {
		return 0, false
	}
	n := 0
	for _, c := range param {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// AltScreen reports whether the session is currently on the alternate screen.
func (t *Terminal) AltScreen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alt
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
	return t.dumpScreen()
}

// Snapshot renders the complete terminal state as VT sequences that, replayed
// onto a terminal in any prior state, reproduce both screens, the active
// screen, cursor, and tracked modes, after which raw session output can
// continue. It begins with SnapshotPrefix and never resets the terminal or
// clears scrollback, so a replaying client's pre-existing history survives.
//
// The daemon feeds the terminal only up to VT ground boundaries, so the raw
// screen switches injected here never land inside a session sequence.
func (t *Terminal) Snapshot() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var b bytes.Buffer
	b.WriteString(SnapshotPrefix)
	b.WriteString("\x1b[0m")
	if t.alt {
		// Mode 47 switches screens without clearing either one or saving and
		// restoring the cursor, so the inactive primary screen can be dumped and
		// the alternate screen resumed with the session's own state intact.
		// These writes bypass mode tracking on purpose.
		t.term.VTWrite([]byte("\x1b[?47l"))
	}
	primary, err := t.dumpScreen()
	if t.alt {
		t.term.VTWrite([]byte("\x1b[?47h"))
	}
	if err != nil {
		return nil, err
	}
	b.Write(primary)
	if t.alt {
		// The primary dump carries the alternate cursor copied over by the
		// screen switch; place the cursor where the session's ?1049h saved it so
		// the replaying terminal's own ?1049h saves, and its later ?1049l
		// restores, the same position as the session.
		if t.primarySaved.ok {
			fmt.Fprintf(&b, "\x1b[%d;%dH", t.primarySaved.y+1, t.primarySaved.x+1)
		}
		b.WriteString("\x1b[?1049h\x1b[2J\x1b[H\x1b[0m")
		alt, err := t.dumpScreen()
		if err != nil {
			return nil, err
		}
		b.Write(alt)
	}
	b.Write(t.modeSequences())
	return b.Bytes(), nil
}

// ModeSequences returns explicit set/reset sequences for every tracked DEC
// private mode, bringing a terminal that missed the session's mode changes into
// agreement with it.
func (t *Terminal) ModeSequences() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.modeSequences()
}

// modeSequences is ModeSequences without locking. The caller must hold t.mu.
func (t *Terminal) modeSequences() []byte {
	var b bytes.Buffer
	for _, m := range trackedModes {
		final := 'l'
		if t.modes[m] {
			final = 'h'
		}
		fmt.Fprintf(&b, "\x1b[?%d%c", m, final)
	}
	return b.Bytes()
}

// DumpPrimaryScreen renders the primary screen like DumpScreen, whichever
// screen is active. While the alternate screen is active the rendering ends by
// placing the cursor where the session's ?1049h saved it, which is where a
// ?1049l on the replaying terminal leaves it.
func (t *Terminal) DumpPrimaryScreen() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.alt {
		return t.dumpScreen()
	}
	// Mode 47 switches screens without clearing either one or touching the
	// saved cursor; these writes bypass mode tracking on purpose.
	t.term.VTWrite([]byte("\x1b[?47l"))
	primary, err := t.dumpScreen()
	t.term.VTWrite([]byte("\x1b[?47h"))
	if err != nil {
		return nil, err
	}
	if t.primarySaved.ok {
		primary = fmt.Appendf(primary, "\x1b[%d;%dH", t.primarySaved.y+1, t.primarySaved.x+1)
	}
	return primary, nil
}

// dumpScreen renders the active screen. The caller must hold t.mu.
func (t *Terminal) dumpScreen() ([]byte, error) {
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
