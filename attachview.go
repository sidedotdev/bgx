package bgx

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

// attachView paints the isolated presentation: the rendered screen of the
// shared client-local terminal is redrawn onto the physical terminal so its
// bottom line can be reserved for the detach hint. Session bytes are never
// forwarded to the physical terminal in this presentation: only the rendered
// screen is painted, so destructive sequences in the stream (screen clears,
// scroll-region changes, absolute cursor addressing, the alternate screen, or
// escapes split across frames) cannot reach — and so cannot corrupt — the
// reserved line.
type attachView struct {
	write   func(string) error
	term    *vt.Terminal
	wake    chan struct{}
	done    chan struct{}
	stopped chan struct{}
	errs    chan<- error

	mu       sync.Mutex
	rows     uint16
	reserved bool
	closed   bool
}

// newAttachView starts painting term, whose state is owned and advanced by the
// caller, onto a physical terminal whose session area is rows tall. Painting
// failures are reported on errs without blocking. The returned view must be
// closed to stop painting.
func newAttachView(
	write func(string) error,
	term *vt.Terminal,
	errs chan<- error,
	rows uint16,
	reserved bool,
) *attachView {
	v := &attachView{
		write:    write,
		term:     term,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		errs:     errs,
		rows:     rows,
		reserved: reserved,
	}
	v.markDirty()
	go v.paintLoop()
	return v
}

// setSize records the session area height and whether the line below it is
// reserved for the hint, then schedules a repaint. The repaint is unconditional
// because the shared terminal reflows on width changes that leave both of
// these values untouched.
func (v *attachView) setSize(rows uint16, reserved bool) {
	v.mu.Lock()
	v.rows, v.reserved = rows, reserved
	v.mu.Unlock()
	v.markDirty()
}

// close stops painting, flushing a repaint that was still pending. It is
// idempotent and waits for any in-flight paint, so callers can write their own
// sequences afterwards without interleaving.
func (v *attachView) close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	v.mu.Unlock()

	close(v.done)
	<-v.stopped

	// A repaint queued when the painter stopped carries the session's final
	// output, which would otherwise never be shown.
	select {
	case <-v.wake:
		return v.paint()
	default:
		return nil
	}
}

// markDirty requests a repaint; bursts of requests coalesce into one.
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
		if err := v.paint(); err != nil {
			select {
			case v.errs <- err:
			default:
			}
			return
		}
		// Rate-limit repaints; requests arriving during the pause coalesce into
		// the next one.
		select {
		case <-v.done:
			return
		case <-time.After(paintInterval):
		}
	}
}

// paint redraws the whole physical screen from the shared terminal state, then
// draws the detach hint on the reserved line.
func (v *attachView) paint() error {
	v.mu.Lock()
	rows, reserved := v.rows, v.reserved
	v.mu.Unlock()
	screen, err := v.term.DumpScreen()
	if err != nil {
		return fmt.Errorf("render attach view: %w", err)
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
	if err := v.write(b.String()); err != nil {
		return fmt.Errorf("paint attach view: %w", err)
	}
	return nil
}
