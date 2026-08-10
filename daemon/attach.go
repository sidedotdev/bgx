package daemon

// The multi-client fanout and frame bridging in this file are ported from
// zmx (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// attachQueue bounds the per-client output backlog. A client that can't keep up
// is re-synced with a fresh snapshot of the latest rendered terminal state
// rather than disconnected, so a slow consumer never stalls the PTY output pump
// nor silently drops bytes mid-stream.
const attachQueue = 1024

// outFrame is a frame queued for delivery to an attached client.
type outFrame struct {
	tag     FrameTag
	payload []byte
}

// attacher is a single live attach connection. A dedicated writer goroutine
// drains buf so the output pump never blocks on a slow consumer; when buf
// overflows the client is re-synced with a fresh snapshot instead of being
// disconnected.
type attacher struct {
	conn net.Conn

	// mu guards the outbound frame buffer and snapshot/close state. cond wakes
	// the writer when new frames, a pending snapshot, or a close arrive.
	mu              sync.Mutex
	cond            *sync.Cond
	buf             []outFrame
	pendingSnapshot []byte
	closed          bool

	// rows and cols hold the client's last reported window size (0 = unknown),
	// guarded by s.mu.
	rows uint16
	cols uint16
}

// signalClose tells the client's writer goroutine to stop. The writer returns
// without draining any remaining backlog.
func (a *attacher) signalClose() {
	a.mu.Lock()
	a.closed = true
	a.cond.Signal()
	a.mu.Unlock()
}

// serveAttach upgrades an accepted connection to the bidirectional frame
// protocol: it acks the handshake, replays the current screen as the first
// Output frame, streams subsequent PTY output, and applies the client's Input
// and Resize frames. r reads inbound frames; conn is written for replies.
func (s *Session) serveAttach(conn net.Conn, r io.Reader) error {
	s.mu.Lock()
	ended := s.ended || s.closing
	s.mu.Unlock()
	enc := json.NewEncoder(conn)
	if ended {
		return enc.Encode(Response{OK: false, Error: "session has ended"})
	}
	if err := enc.Encode(Response{OK: true}); err != nil {
		return err
	}

	a := &attacher{conn: conn}
	a.cond = sync.NewCond(&a.mu)

	// Capture the snapshot and join the fanout atomically so no output is lost
	// or duplicated between rendering the screen and subscribing to the stream.
	s.outMu.Lock()
	snap, err := s.snapshot()
	if err == nil {
		s.mu.Lock()
		s.attachers[a] = struct{}{}
		s.clientCount++
		s.mu.Unlock()
	}
	s.outMu.Unlock()
	if err != nil {
		return err
	}

	writerDone := make(chan error, 1)
	go s.attachWriter(a, snap, writerDone)

	// Ask the client for its window size so the PTY tracks the smallest
	// attached client.
	s.enqueueFrame(a, outFrame{tag: FrameResize})

	var resizeErr error
readFrames:
	for {
		tag, payload, err := ReadFrame(r)
		if err != nil {
			break
		}
		if tag == FrameDetach {
			break
		}
		switch tag {
		case FrameInput:
			s.attachInput(a, payload)
		case FrameResize:
			if rp, ok := DecodeResize(payload); ok {
				resizeErr = s.attachResize(a, rp)
				if resizeErr != nil {
					break readFrames
				}
			}
		}
	}

	s.mu.Lock()
	delete(s.attachers, a)
	s.clientCount--
	s.mu.Unlock()
	// The smallest client may have left; grow the PTY back to the new minimum.
	resizeErr = errors.Join(resizeErr, s.applyMinSize())
	a.signalClose()
	writerErr := <-writerDone
	return errors.Join(resizeErr, writerErr)
}

// attachWriter serializes all frames sent to a single client; it is the only
// writer of conn after the handshake ack, so concurrent output and resize
// requests can't interleave on the wire.
func (s *Session) attachWriter(a *attacher, snap []byte, done chan<- error) {
	if err := WriteFrame(a.conn, FrameOutput, snap); err != nil {
		done <- errors.Join(err, a.conn.Close())
		return
	}
	for {
		a.mu.Lock()
		for len(a.buf) == 0 && a.pendingSnapshot == nil && !a.closed {
			a.cond.Wait()
		}
		if a.closed {
			a.mu.Unlock()
			done <- nil
			return
		}
		// deliverOutput already dropped the stale pre-snapshot backlog when it
		// captured the snapshot; the frames still queued here were rendered
		// afterwards and tile from it. Taking one frame at a time and re-checking
		// for a snapshot between writes lets an overflow abandon later stale
		// frames before they are streamed ahead of the fresh state.
		if a.pendingSnapshot != nil {
			snapshot := a.pendingSnapshot
			a.pendingSnapshot = nil
			a.mu.Unlock()
			if err := WriteFrame(a.conn, FrameOutput, snapshot); err != nil {
				done <- errors.Join(err, a.conn.Close())
				return
			}
			continue
		}
		f := a.buf[0]
		a.buf = a.buf[1:]
		a.mu.Unlock()
		if err := WriteFrame(a.conn, f.tag, f.payload); err != nil {
			done <- errors.Join(err, a.conn.Close())
			return
		}
		// A session-ended frame is the last thing a client receives: close the
		// connection so its serveAttach reader unblocks and shuts down.
		if f.tag == FrameEnded {
			done <- a.conn.Close()
			return
		}
	}
}

// enqueueFrame queues a control frame for a client's writer. PTY output is
// delivered via deliverOutput, which bounds the backlog; control frames (window
// size requests) are infrequent and appended directly.
func (s *Session) enqueueFrame(a *attacher, f outFrame) {
	a.mu.Lock()
	a.buf = append(a.buf, f)
	a.cond.Signal()
	a.mu.Unlock()
}

// endAttachers tells every attached client's writer to deliver a final
// session-ended frame after the bytes already queued, then close the
// connection. Appending through each client's ordered buffer keeps the ended
// frame behind all output the client has not yet drained.
func (s *Session) endAttachers() {
	s.mu.Lock()
	attachers := make([]*attacher, 0, len(s.attachers))
	for a := range s.attachers {
		attachers = append(attachers, a)
	}
	s.mu.Unlock()
	for _, a := range attachers {
		a.mu.Lock()
		a.buf = append(a.buf, outFrame{tag: FrameEnded})
		a.cond.Signal()
		a.mu.Unlock()
	}
}

// deliverOutput queues PTY output for a client. A client whose backlog overflows
// is re-synced rather than disconnected: its stale backlog is dropped and its
// writer is handed a fresh snapshot of the latest rendered terminal state,
// after which live streaming resumes. It runs from the output pump while
// holding s.outMu, so the snapshot reflects (and thus tiles cleanly with) the
// bytes being fanned out now and the bytes fanned out afterwards.
func (s *Session) deliverOutput(a *attacher, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.buf) >= attachQueue {
		snap, err := s.snapshot()
		if err != nil {
			s.recordHandlerError(fmt.Errorf("dump screen for attach skip-forward: %w", err))
			return
		}
		a.buf = a.buf[:0]
		a.pendingSnapshot = snap
		a.cond.Signal()
		return
	}
	a.buf = append(a.buf, outFrame{tag: FrameOutput, payload: data})
	a.cond.Signal()
}

// fanout copies PTY output to every attached client. It holds s.mu only while
// snapshotting the client set so per-client delivery (which may trigger a slow
// client's re-sync) happens without the lock held.
func (s *Session) fanout(data []byte) {
	s.mu.Lock()
	if len(s.attachers) == 0 {
		s.mu.Unlock()
		return
	}
	cp := append([]byte(nil), data...)
	targets := make([]*attacher, 0, len(s.attachers))
	for a := range s.attachers {
		targets = append(targets, a)
	}
	s.mu.Unlock()
	for _, a := range targets {
		s.deliverOutput(a, cp)
	}
}

// attachInput forwards a client's input to the PTY. Every attached client's
// input is applied so all concurrently connected clients drive the same
// session.
func (s *Session) attachInput(a *attacher, payload []byte) {
	if len(payload) == 0 {
		return
	}
	s.queueInput(payload)
}

// attachResize records the reporting client's window size and reflows the PTY
// and emulated terminal to the smallest size across all attached clients so
// every client sees output that fits its window.
func (s *Session) attachResize(a *attacher, rp ResizePayload) error {
	if rp.Rows == 0 || rp.Cols == 0 {
		return nil
	}
	s.mu.Lock()
	a.cols, a.rows = rp.Cols, rp.Rows
	s.mu.Unlock()
	return s.applyMinSize()
}

// applyMinSize reflows the PTY and emulated terminal to the smallest cols and
// rows reported across attached clients, taken independently per dimension. It
// is a no-op until at least one client has reported its size.
func (s *Session) applyMinSize() error {
	s.mu.Lock()
	cols, rows, ok := s.minSize()
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return s.resize(cols, rows)
}

// minSize returns the smallest reported cols and rows independently across all
// attached clients with a known window size. ok is false until at least one
// client has reported a size. Callers must hold s.mu.
func (s *Session) minSize() (cols, rows uint16, ok bool) {
	for a := range s.attachers {
		if a.cols == 0 || a.rows == 0 {
			continue
		}
		if !ok {
			cols, rows, ok = a.cols, a.rows, true
			continue
		}
		if a.cols < cols {
			cols = a.cols
		}
		if a.rows < rows {
			rows = a.rows
		}
	}
	return
}
