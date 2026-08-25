package bgx

import (
	"fmt"
	"sync"
)

// attachScreen is the snapshot model of the attached session, used to repaint
// the session's final state onto the normal screen when the session ends.
type attachScreen interface {
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	DumpScreen() ([]byte, error)
}

// attachDisplay is the display model painted onto the physical terminal.
type attachDisplay interface {
	feed(payload []byte) error
	setSize(cols, physicalRows uint16) (uint16, error)
}

type attachSnapshot struct {
	contents     []byte
	cols         uint16
	rows         uint16
	physicalRows uint16
}

// attachModels applies session events to the snapshot screen and the display
// view in lockstep: a payload is never processed by the two models at
// different dimensions, so the final snapshot always matches what the display
// showed.
type attachModels struct {
	mu           sync.Mutex
	screen       attachScreen
	view         attachDisplay
	cols         uint16
	rows         uint16
	physicalRows uint16
}

func (m *attachModels) applyOutput(payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.screen.Write(payload); err != nil {
		return err
	}
	return m.view.feed(payload)
}

// applyResize sizes the display first because it decides the session rows (a
// hint row may be reserved), then matches the snapshot screen. Zero rows means
// the size should not be forwarded to the session.
func (m *attachModels) applyResize(cols, physicalRows uint16) (uint16, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.view.setSize(cols, physicalRows)
	if err != nil || rows == 0 {
		return rows, err
	}
	if err := m.screen.Resize(cols, rows); err != nil {
		return 0, fmt.Errorf("attach: resize terminal state: %w", err)
	}
	m.cols, m.rows, m.physicalRows = cols, rows, physicalRows
	return rows, nil
}

func (m *attachModels) snapshot() (attachSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	contents, err := m.screen.DumpScreen()
	return attachSnapshot{
		contents:     contents,
		cols:         m.cols,
		rows:         m.rows,
		physicalRows: m.physicalRows,
	}, err
}
