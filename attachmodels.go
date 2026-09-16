package bgx

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sidedotdev/bgx/vt"
)

// presentation is how session output reaches the physical terminal.
type presentation int

const (
	// presentationPending is auto mode before the first frame has revealed
	// which screen the session is on.
	presentationPending presentation = iota
	// presentationIsolated paints a rendering of the session onto a protected
	// alternate screen, keeping the outer terminal untouched.
	presentationIsolated
	// presentationNative forwards session bytes to the normal buffer, so the
	// outer terminal's own scrollback and the session's application controls
	// behave as they would over SSH.
	presentationNative
)

// attachOutcome is why an attachment is being torn down.
type attachOutcome int

const (
	// attachOutcomeAborted is a local failure: tear down silently.
	attachOutcomeAborted attachOutcome = iota
	attachOutcomeDetached
	attachOutcomeSessionEnded
	attachOutcomeDisconnected
)

func (o attachOutcome) message() string {
	switch o {
	case attachOutcomeDetached:
		return "Detached from session"
	case attachOutcomeSessionEnded:
		return "Session ended"
	case attachOutcomeDisconnected:
		return "Disconnected from session"
	}
	return ""
}

// isolatedEntry switches the physical terminal to a cleared alternate screen,
// protecting the outer buffer from everything painted afterwards.
const isolatedEntry = "\x1b[?1049h\x1b[2J\x1b[H"

// nativeModeReset disables the input-reporting modes (mouse, focus, bracketed
// paste, application cursor keys) a session may have enabled while its bytes
// were forwarded natively. Those modes belong to the session, so they are
// withdrawn whenever the outer terminal stops receiving session bytes.
const nativeModeReset = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[?1l"

// nativeCleanup returns the outer terminal to a usable state after session
// bytes were forwarded natively. It never resets the terminal or clears
// scrollback: only input-reporting modes the session may have enabled, cursor
// visibility, the pen and the scroll region are restored. DECSTBM homes the
// cursor, so it is bracketed by a cursor save/restore.
const nativeCleanup = nativeModeReset + "\x1b[?25h\x1b[0m\x1b7\x1b[r\x1b8"

// nativeTitleHint is the best-effort terminal title shown alongside the
// one-time native detach hint; XTWINOPS push/pop restore the previous title
// on terminals that support it.
const nativeTitleHint = "\x1b[22;0t\x1b]0;bgx: detach with ctrl+\\\x07"

type attachSnapshot struct {
	contents     []byte
	cols         uint16
	rows         uint16
	physicalRows uint16
	writtenRows  uint16
}

// attachModels owns the client-local terminal that mirrors the session, and
// decides how that state is presented on the physical terminal. Output and
// resize events are serialized so a payload is never modelled at one size and
// presented at another, and the final state always matches what was shown.
type attachModels struct {
	mu                     sync.Mutex
	term                   *vt.Terminal
	mode                   AttachMode
	showDetachInstructions bool
	writeOut               func(string) error
	writeBytes             func([]byte) error
	// viewErrs receives asynchronous paint failures from whichever view is
	// active, so the attach loop can select on one channel for the whole
	// attachment.
	viewErrs chan error

	presentation  presentation
	view          *attachView
	nativeEntered bool
	// nativeHintRow is the 1-based row the one-time native hint was drawn on,
	// or zero while it has not been shown. nativeHintOnPrimary records which
	// screen received it: a hint on the alternate screen is discarded with that
	// screen, while one on the primary screen must be cleaned up at exit.
	nativeHintRow       uint16
	nativeHintOnPrimary bool
	cols                uint16
	rows                uint16
	physicalRows        uint16
}

func newAttachModels(
	term *vt.Terminal,
	cfg attachConfig,
	writeOut func(string) error,
	writeBytes func([]byte) error,
) *attachModels {
	return &attachModels{
		term:                   term,
		mode:                   cfg.mode,
		showDetachInstructions: cfg.showDetachInstructions,
		writeOut:               writeOut,
		writeBytes:             writeBytes,
		viewErrs:               make(chan error, 1),
		cols:                   vt.DefaultCols,
		rows:                   vt.DefaultRows,
		physicalRows:           vt.DefaultRows,
	}
}

// start applies the presentation chosen up front. Isolated mode claims the
// alternate screen immediately; native waits for the first presented bytes and
// auto waits for the first frame to reveal the session's active screen.
func (m *attachModels) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.mode {
	case AttachModeIsolated:
		return m.enterIsolated()
	case AttachModeNative:
		m.presentation = presentationNative
	}
	return nil
}

// enterIsolated switches to the isolated presentation and starts painting.
// The presentation is recorded before the screen switch is written so a failed
// or partial write is still undone by the cleanup. The caller must hold m.mu.
func (m *attachModels) enterIsolated() error {
	m.presentation = presentationIsolated
	if err := m.resizeLocked(m.cols, m.physicalRows); err != nil {
		return err
	}
	if err := m.writeOut(isolatedEntry); err != nil {
		return err
	}
	m.view = newAttachView(m.writeOut, m.term, m.viewErrs, m.rows, m.rows < m.physicalRows)
	return nil
}

// sessionRows is the height advertised to the session for a physical terminal
// of the given height: isolated presentation reserves the bottom line for the
// detach hint whenever there is room for both it and the session.
func (m *attachModels) sessionRows(physicalRows uint16) uint16 {
	if m.presentation == presentationIsolated && m.showDetachInstructions && physicalRows > 1 {
		return physicalRows - 1
	}
	return physicalRows
}

// applyOutput models a session output frame and presents it. resized reports
// that the presentation changed in a way that alters the advertised terminal
// size, which the caller must forward to the session.
func (m *attachModels) applyOutput(payload []byte) (resized bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.presentation == presentationPending || bytes.HasPrefix(payload, []byte(vt.SnapshotPrefix)) {
		// A (re)synchronization point: the first frame is the daemon's
		// snapshot, and later ones replace a backlog the client fell behind
		// on. The model absorbs the whole state, so it is presented from the
		// model rather than by replaying the snapshot's own screen switches,
		// which would flash through screens the session merely passed by.
		if _, err := m.term.Write(payload); err != nil {
			return false, fmt.Errorf("attach: update terminal state: %w", err)
		}
		resized, err = m.followActiveScreen()
		if err != nil {
			return resized, err
		}
		return resized, m.presentFromModel()
	}
	if m.mode != AttachModeAuto {
		if _, err := m.term.Write(payload); err != nil {
			return false, fmt.Errorf("attach: update terminal state: %w", err)
		}
		return false, m.presentRaw(payload)
	}
	// Auto follows the session between screens mid-stream: bytes up to a
	// screen switch are shown under the presentation they were written for,
	// the switch sequence itself is replaced by the client's own transition,
	// and the remainder continues under the new presentation.
	for len(payload) > 0 {
		before, consumed, switched, err := m.term.WriteUntilScreenSwitch(payload)
		if err != nil {
			return resized, fmt.Errorf("attach: update terminal state: %w", err)
		}
		if before > 0 {
			if err := m.presentRaw(payload[:before]); err != nil {
				return resized, err
			}
		}
		payload = payload[consumed:]
		if !switched {
			break
		}
		transitioned, err := m.followActiveScreen()
		if err != nil {
			return resized, err
		}
		resized = resized || transitioned
		if err := m.presentFromModel(); err != nil {
			return resized, err
		}
	}
	return resized, nil
}

// followActiveScreen switches auto mode to the presentation matching the
// session's active screen: primary is presented natively, alternate in
// isolation. It reports whether the advertised terminal height changed. Forced
// modes never transition. The caller must hold m.mu.
func (m *attachModels) followActiveScreen() (resized bool, err error) {
	if m.mode != AttachModeAuto {
		return false, nil
	}
	want := presentationNative
	if m.term.AltScreen() {
		want = presentationIsolated
	}
	if want == m.presentation {
		return false, nil
	}
	previousRows := m.rows
	switch want {
	case presentationIsolated:
		if m.nativeEntered {
			// The isolated view only paints, so modes forwarded natively would
			// otherwise outlive the session's control of the terminal; a
			// later return to native restores them from the model.
			err = m.writeOut(nativeModeReset)
		}
		if err == nil {
			err = m.enterIsolated()
		}
	case presentationNative:
		err = m.leaveIsolated()
	}
	return m.rows != previousRows, err
}

// leaveIsolated returns from the isolated presentation to native: the view is
// stopped, the protected alternate screen is left so the outer terminal's
// primary buffer is back, and the presentation is switched before the model is
// repainted onto it. The caller must hold m.mu.
func (m *attachModels) leaveIsolated() error {
	var err error
	if m.view != nil {
		err = m.view.close()
		m.view = nil
	}
	wasIsolated := m.presentation == presentationIsolated
	m.presentation = presentationNative
	if resizeErr := m.resizeLocked(m.cols, m.physicalRows); resizeErr != nil {
		return errors.Join(err, resizeErr)
	}
	if wasIsolated {
		// The view's scroll region is terminal-wide rather than per screen,
		// so it must be released along with the alternate screen.
		err = errors.Join(err, m.writeOut("\x1b[?1049l\x1b[r"))
	}
	return err
}

// presentFromModel shows the modelled terminal state, used after the model has
// absorbed a snapshot or the presentation changed. The caller must hold m.mu.
func (m *attachModels) presentFromModel() error {
	switch m.presentation {
	case presentationIsolated:
		m.view.markDirty()
		return nil
	case presentationNative:
		if err := m.enterNative(); err != nil {
			return err
		}
		repaint, err := m.nativeRepaint()
		if err != nil {
			return fmt.Errorf("attach: render terminal state: %w", err)
		}
		if err := m.writeBytes(repaint); err != nil {
			return err
		}
		// The repaint covered the whole primary screen, so any one-time hint
		// drawn there is gone and needs no cleanup at exit.
		m.nativeHintOnPrimary = false
		return m.showNativeHintOnce()
	}
	return nil
}

// nativeRepaint renders the model onto the physical terminal without touching
// its scrollback. In forced native mode the physical terminal follows every
// forwarded screen switch, so it may sit on either screen and the snapshot's
// full two-screen replay is needed. In auto mode native presentation implies
// the primary screen on both sides, so only that screen and the tracked modes
// the physical terminal may have missed while isolated are restored. The
// caller must hold m.mu.
func (m *attachModels) nativeRepaint() ([]byte, error) {
	if m.mode == AttachModeNative {
		return m.term.Snapshot()
	}
	screen, err := m.term.DumpScreen()
	if err != nil {
		return nil, err
	}
	repaint := append([]byte("\x1b[2J\x1b[H\x1b[0m"), screen...)
	return append(repaint, m.term.ModeSequences()...), nil
}

// presentRaw shows a streamed payload already applied to the model. The caller
// must hold m.mu.
func (m *attachModels) presentRaw(payload []byte) error {
	switch m.presentation {
	case presentationIsolated:
		m.view.markDirty()
		return nil
	case presentationNative:
		if err := m.enterNative(); err != nil {
			return err
		}
		if err := m.writeBytes(payload); err != nil {
			return err
		}
		return m.showNativeHintOnce()
	}
	return nil
}

// enterNative prepares the normal buffer for forwarded session bytes on the
// first presented output: the pre-attach screen is scrolled into scrollback so
// it survives whatever the session draws, and the cursor is homed. The caller
// must hold m.mu.
func (m *attachModels) enterNative() error {
	if m.nativeEntered {
		return nil
	}
	m.nativeEntered = true
	var b strings.Builder
	if m.physicalRows > 0 {
		fmt.Fprintf(&b, "\x1b[%d;1H", m.physicalRows)
		b.WriteString(strings.Repeat("\r\n", int(m.physicalRows)))
	}
	b.WriteString("\x1b[H")
	if m.showDetachInstructions {
		b.WriteString(nativeTitleHint)
	}
	return m.writeOut(b.String())
}

// showNativeHintOnce draws the detach hint a single time on the first row the
// session has not written to (or the bottom row when it has used them all),
// leaving the cursor where the session put it. Later session output may
// overwrite the hint; nothing is reserved in native presentation. The caller
// must hold m.mu.
func (m *attachModels) showNativeHintOnce() error {
	if !m.showDetachInstructions || m.nativeHintRow != 0 || m.physicalRows == 0 {
		return nil
	}
	writtenRows, err := m.term.WrittenRows()
	if err != nil {
		return fmt.Errorf("attach: locate detach hint: %w", err)
	}
	m.nativeHintRow = min(writtenRows+1, m.physicalRows)
	m.nativeHintOnPrimary = !m.term.AltScreen()
	return m.writeOut(fmt.Sprintf(
		"\x1b7\x1b[%d;1H%s\x1b[2K%s\x1b[0m\x1b8",
		m.nativeHintRow, detachHintStyle, detachHint,
	))
}

// nativeHintCleanup removes whatever remains of the one-time hint from the
// primary screen once the attachment ends. The hint is the only thing on that
// screen the model does not know about, so repainting it from the model erases
// any leftover hint text or styling — including partial overwrites — while
// reproducing the session's content exactly and leaving scrollback untouched.
// The repaint applies after any alternate screen has been left, so it also
// covers a hint drawn before the session entered one.
func (m *attachModels) nativeHintCleanup() (string, error) {
	if m.nativeHintRow == 0 || !m.nativeHintOnPrimary {
		return "", nil
	}
	primary, err := m.term.DumpPrimaryScreen()
	if err != nil {
		return "", fmt.Errorf("attach: render primary screen: %w", err)
	}
	return "\x1b[2J\x1b[H\x1b[0m" + string(primary), nil
}

// applyResize adopts a new physical terminal size and reports the row count to
// advertise to the session. Zero rows means the size should not be forwarded.
func (m *attachModels) applyResize(cols, physicalRows uint16) (uint16, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cols == 0 || physicalRows == 0 {
		return 0, nil
	}
	if err := m.resizeLocked(cols, physicalRows); err != nil {
		return 0, err
	}
	return m.rows, nil
}

// resizeLocked resizes the model for the current presentation. The caller must
// hold m.mu.
func (m *attachModels) resizeLocked(cols, physicalRows uint16) error {
	rows := m.sessionRows(physicalRows)
	if err := m.term.Resize(cols, rows); err != nil {
		return fmt.Errorf("attach: resize terminal state: %w", err)
	}
	m.cols, m.rows, m.physicalRows = cols, rows, physicalRows
	if m.view != nil {
		m.view.setSize(rows, rows < physicalRows)
	}
	return nil
}

// snapshotLocked captures the final rendered state for the isolated
// end/disconnect reprint. The caller must hold m.mu.
func (m *attachModels) snapshotLocked() (attachSnapshot, error) {
	contents, err := m.term.DumpScreen()
	if err != nil {
		return attachSnapshot{}, err
	}
	writtenRows, err := m.term.WrittenRows()
	return attachSnapshot{
		contents:     contents,
		cols:         m.cols,
		rows:         m.rows,
		physicalRows: m.physicalRows,
		writtenRows:  writtenRows,
	}, err
}

// finish stops presenting and writes the lifecycle output for the current
// presentation: isolated restores the pre-attach screen (appending the final
// session screen on end/disconnect), native leaves the session's output in the
// normal buffer and only restores terminal modes. Both print the one-line
// outcome, except on aborts.
func (m *attachModels) finish(outcome attachOutcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	if m.view != nil {
		err = m.view.close()
		m.view = nil
	}
	switch m.presentation {
	case presentationIsolated:
		return errors.Join(err, m.finishIsolated(outcome))
	case presentationNative:
		return errors.Join(err, m.finishNative(outcome))
	}
	if message := outcome.message(); message != "" {
		err = errors.Join(err, m.writeOut("\r\n"+message+"\r\n"))
	}
	return err
}

// finishIsolated restores the pre-attach screen. When auto mode presented
// natively before isolating, that screen holds forwarded session output and
// possibly the one-time hint, which is erased by repainting the primary screen
// from the model right after leaving the alternate screen.
func (m *attachModels) finishIsolated(outcome attachOutcome) error {
	hintCleanup, hintErr := m.nativeHintCleanup()
	if hintCleanup != "" {
		if err := m.writeOut("\x1b[?1049l" + hintCleanup); err != nil {
			return errors.Join(hintErr, err)
		}
	}
	switch outcome {
	case attachOutcomeSessionEnded, attachOutcomeDisconnected:
		retErr := hintErr
		snapshot, err := m.snapshotLocked()
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("attach: render final terminal state: %w", err))
		}
		if err := m.writeOut(finalAttachScreenPrefix(snapshot)); err != nil {
			return errors.Join(retErr, err)
		}
		if len(snapshot.contents) > 0 {
			if err := m.writeBytes(snapshot.contents); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
		if err := m.writeOut(finalAttachOutcomePrefix(snapshot)); err != nil {
			retErr = errors.Join(retErr, err)
		}
		if err := m.writeOut(outcome.message() + "\r\n"); err != nil {
			retErr = errors.Join(retErr, err)
		}
		return retErr
	case attachOutcomeDetached:
		var b strings.Builder
		b.WriteString("\x1b[?1049l")
		switch {
		case m.nativeEntered:
			// Session bytes forwarded during an earlier native phase may have
			// hidden the cursor or changed the pen; restore the outer terminal
			// as a native detach would.
			b.WriteString(nativeCleanup)
		case m.rows < m.physicalRows:
			// The view confined scrolling to the session area, and DECSTBM is
			// terminal-wide rather than per screen. Releasing it homes the
			// cursor, so the restored position is saved around it.
			b.WriteString("\x1b7\x1b[r\x1b8")
		}
		b.WriteString("\r\nDetached from session\r\n")
		return errors.Join(hintErr, m.writeOut(b.String()))
	}
	return errors.Join(hintErr, m.writeOut("\x1b[?1049l"))
}

// finishNative leaves the session's final screen in place and prints the
// outcome below the rows it wrote, so the message never lands in the middle of
// a primary-buffer TUI's output. Nothing was written before native entry, so
// the outer terminal is left exactly as found apart from the outcome line.
func (m *attachModels) finishNative(outcome attachOutcome) error {
	message := outcome.message()
	if !m.nativeEntered {
		if message == "" {
			return nil
		}
		return m.writeOut("\r\n" + message + "\r\n")
	}
	var retErr error
	var b strings.Builder
	leftAlt := m.term.AltScreen()
	if leftAlt {
		b.WriteString("\x1b[?1049l")
	}
	b.WriteString(nativeCleanup)
	if m.showDetachInstructions {
		b.WriteString("\x1b[23;0t")
	}
	snapshot, err := m.snapshotLocked()
	if err != nil {
		retErr = fmt.Errorf("attach: locate final terminal state: %w", err)
	}
	hintCleanup, err := m.nativeHintCleanup()
	if err != nil {
		retErr = errors.Join(retErr, err)
	}
	b.WriteString(hintCleanup)
	if leftAlt {
		// The written rows describe the alternate screen just left; the
		// restored primary screen's cursor is the only safe anchor.
		snapshot = attachSnapshot{}
	}
	if message != "" {
		b.WriteString(finalAttachOutcomePosition(snapshot))
		b.WriteString(message + "\r\n")
	}
	return errors.Join(retErr, m.writeOut(b.String()))
}
