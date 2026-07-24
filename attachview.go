package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sidedotdev/bgx/vt"
)

// paintInterval coalesces bursts of output frames into at most one repaint, so
// a chatty session doesn't redraw the screen once per PTY chunk.
const paintInterval = 8 * time.Millisecond

const detachHint = " detach: ctrl+\\ "

// detachHintStyle gives the reserved line its own subtle background (dark gray
// with a soft gray foreground) so it reads as client chrome, visually distinct
// from session content.
const detachHintStyle = "\x1b[48;5;236;38;5;250m"

// attachView renders session output through a client-local terminal so the
// bottom line of the physical terminal can be reserved for the detach hint.
// Session bytes are never forwarded to the physical terminal: only the rendered
// screen of the local terminal is painted, so destructive sequences in the
// stream (screen clears, scroll-region changes, absolute cursor addressing, the
// alternate screen, or escapes split across frames) cannot reach — and so
// cannot corrupt — the reserved line.
type attachView struct {
	write   func(string)
	wake    chan struct{}
	done    chan struct{}
	stopped chan struct{}

	mu       sync.Mutex
	term     *vt.Terminal
	cols     uint16
	rows     uint16
	reserved bool
	closed   bool
}

// newAttachView starts a view for a physical terminal of the given size. The
// returned view must be closed to stop painting.
func newAttachView(write func(string), cols, physicalRows uint16) *attachView {
	v := &attachView{
		write:   write,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	v.setSize(cols, physicalRows)
	go v.paintLoop()
	return v
}

// setSize adapts the view to the physical terminal size and reports the row
// count to advertise to the session. The bottom line is reserved for the hint
// whenever the terminal has room for both it and the session.
func (v *attachView) setSize(cols, physicalRows uint16) uint16 {
	if cols == 0 || physicalRows == 0 {
		return 0
	}
	rows := physicalRows
	reserved := physicalRows > 1
	if reserved {
		rows--
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return rows
	}
	changed := v.term == nil || v.cols != cols || v.rows != rows || v.reserved != reserved
	v.cols, v.rows, v.reserved = cols, rows, reserved
	if v.term == nil {
		v.term, _ = vt.New(cols, rows)
	} else {
		_ = v.term.Resize(cols, rows)
	}
	if changed {
		v.markDirty()
	}
	return rows
}

// reservedRow reports the 1-based physical row holding the detach hint, or 0
// when no line is reserved.
func (v *attachView) reservedRow() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.reserved {
		return 0
	}
	return int(v.rows) + 1
}

// feed advances the view's terminal state with streamed session output.
func (v *attachView) feed(payload []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.term == nil {
		return
	}
	_, _ = v.term.Write(payload)
	v.markDirty()
}

// resync replaces the view's state from a full-screen snapshot. The frames it
// supersedes were dropped, so the local terminal is recreated rather than
// written to, discarding any modes those frames may have set.
func (v *attachView) resync(payload []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.term == nil {
		return
	}
	v.term.Close()
	v.term, _ = vt.New(v.cols, v.rows)
	if v.term == nil {
		return
	}
	_, _ = v.term.Write(payload)
	v.markDirty()
}

// close stops painting and releases the local terminal, flushing a repaint that
// was still pending. It is idempotent and waits for any in-flight paint, so
// callers can write their own sequences afterwards without interleaving.
func (v *attachView) close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()

	close(v.done)
	<-v.stopped
	// A repaint queued when the painter stopped carries the session's final
	// output, which would otherwise never be shown.
	select {
	case <-v.wake:
		v.paint()
	default:
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.term != nil {
		v.term.Close()
		v.term = nil
	}
}

// markDirty requests a repaint. The caller must hold v.mu.
func (v *attachView) markDirty() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *attachView) paintLoop() {
	defer close(v.stopped)
	for {
		select {
		case <-v.done:
			return
		case <-v.wake:
		}
		v.paint()
		// Rate-limit repaints; requests arriving during the pause coalesce into
		// the next one.
		select {
		case <-v.done:
			return
		case <-time.After(paintInterval):
		}
	}
}

// paint redraws the whole physical screen from the local terminal state, then
// draws the detach hint on the reserved line.
func (v *attachView) paint() {
	v.mu.Lock()
	if v.term == nil {
		v.mu.Unlock()
		return
	}
	rows, reserved := v.rows, v.reserved
	screen, err := v.term.DumpScreen()
	v.mu.Unlock()
	if err != nil {
		return
	}

	var b strings.Builder
	// Synchronized output makes the redraw atomic on terminals that support it;
	// others ignore the mode and simply see the repaint.
	b.WriteString("\x1b[?2026h")
	if reserved {
		// Confine scrolling to the session area as well, so a session write to
		// the last cell can't scroll content through the reserved line.
		fmt.Fprintf(&b, "\x1b[1;%dr", rows)
	} else {
		b.WriteString("\x1b[r")
	}
	// DumpScreen renders onto a cleared screen; clearing everything each time
	// also removes a hint left on a row that is no longer reserved.
	b.WriteString("\x1b[2J\x1b[H\x1b[m")
	b.Write(screen)
	if reserved {
		// Setting the style before the erase fills the entire line with the
		// hint's background via background-color erase.
		fmt.Fprintf(&b, "\x1b7\x1b[%d;1H%s\x1b[2K%s\x1b[0m\x1b8", rows+1, detachHintStyle, detachHint)
	}
	b.WriteString("\x1b[?2026l")
	v.write(b.String())
}
