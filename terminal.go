package bgx

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"
)

// Terminal is the interactive terminal used by Client.Attach.
type Terminal interface {
	io.Reader
	io.Writer
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
	fd := int(t.in.Fd())
	if !term.IsTerminal(fd) {
		return 0, 0, errors.New("stdin is not a terminal")
	}
	width, height, err := term.GetSize(fd)
	if err != nil {
		return 0, 0, err
	}
	if width <= 0 || height <= 0 || width > int(^uint16(0)) || height > int(^uint16(0)) {
		return 0, 0, errors.New("terminal has invalid dimensions")
	}
	return uint16(width), uint16(height), nil
}

// ResizeEvents reports process terminal resize notifications until ctx ends.
func (t *ProcessTerminal) ResizeEvents(ctx context.Context) <-chan struct{} {
	events := make(chan struct{}, 1)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	go func() {
		defer close(events)
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				select {
				case events <- struct{}{}:
				default:
				}
			}
		}
	}()
	return events
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
