package bgx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// ErrTerminalSizeUnavailable indicates that a terminal temporarily has no
// usable dimensions. Attach continues without resizing until dimensions become
// available.
var ErrTerminalSizeUnavailable = errors.New("terminal size unavailable")

// Terminal is the interactive terminal used by Client.Attach.
type Terminal interface {
	io.Reader
	io.Writer
	ReadContext(context.Context, []byte) (int, error)
	Size() (cols, rows uint16, err error)
	ResizeEvents(context.Context) <-chan struct{}
	EnterRaw() error
	Restore() error
}

// ProcessTerminal connects an attachment to the process's standard terminal.
type ProcessTerminal struct {
	in  *os.File
	out io.Writer

	mu    sync.Mutex
	state *term.State
}

// NewProcessTerminal returns a terminal backed by stdin and stdout.
func NewProcessTerminal() *ProcessTerminal {
	return &ProcessTerminal{
		in:  os.Stdin,
		out: os.Stdout,
	}
}

func (t *ProcessTerminal) Read(p []byte) (int, error) {
	return t.in.Read(p)
}

func (t *ProcessTerminal) Write(p []byte) (int, error) {
	return t.out.Write(p)
}

// Size returns the process terminal's current dimensions.
func (t *ProcessTerminal) Size() (cols, rows uint16, err error) {
	width, height, err := term.GetSize(int(t.in.Fd()))
	if err != nil {
		return 0, 0, normalizeTerminalSizeError(err)
	}
	if width <= 0 || height <= 0 || width > int(^uint16(0)) || height > int(^uint16(0)) {
		return 0, 0, fmt.Errorf("%w: width=%d height=%d", ErrTerminalSizeUnavailable, width, height)
	}
	return uint16(width), uint16(height), nil
}

// EnterRaw places the process terminal in raw mode. It is a no-op when stdin is
// not a terminal, and best-effort otherwise: attach still functions without raw
// mode, the detach key just isn't intercepted before the line discipline.
func (t *ProcessTerminal) EnterRaw() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != nil || !term.IsTerminal(int(t.in.Fd())) {
		return nil
	}
	if state, err := term.MakeRaw(int(t.in.Fd())); err == nil {
		t.state = state
	}
	return nil
}

// Restore restores the process terminal mode.
func (t *ProcessTerminal) Restore() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		return nil
	}
	state := t.state
	t.state = nil
	return term.Restore(int(t.in.Fd()), state)
}
