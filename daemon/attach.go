package daemon

// The multi-client fanout and frame bridging in this file are ported from
// zmx (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.

import (
	"encoding/json"
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

	// mu guards the outbound frame buffer and resync/close state. cond wakes the
	// writer goroutine when new frames, a pending resync, or a close arrive.
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []outFrame
	resync []byte // fresh snapshot pending after a backlog overflow (nil = none)
	closed bool

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
func (s *Session) serveAttach(conn net.Conn, r io.Reader) {
	s.mu.Lock()
	ended := s.ended || s.closing
	s.mu.Unlock()
	enc := json.NewEncoder(conn)
	if ended {
		_ = enc.Encode(Response{OK: false, Error: "session has ended"})
		return
	}
	if err := enc.Encode(Response{OK: true}); err != nil {
		return
	}

	a := &attacher{conn: conn}
	a.cond = sync.NewCond(&a.mu)

	// Capture the snapshot and join the fanout atomically so no output is lost
	// or duplicated between rendering the screen and subscribing to the stream.
	s.outMu.Lock()
	snap, _ := s.term.DumpScreen()
	s.mu.Lock()
	s.attachers[a] = struct{}{}
	s.clientCount++
	s.mu.Unlock()
	s.outMu.Unlock()

	writerDone := make(chan struct{})
	go s.attachWriter(a, snap, writerDone)

	// Ask the client for its window size so the PTY tracks the smallest
	// attached client.
	s.enqueueFrame(a, outFrame{tag: FrameResize})

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
				s.attachResize(a, rp)
			}
		}
	}

	s.mu.Lock()
	delete(s.attachers, a)
	s.clientCount--
	s.mu.Unlock()
	// The smallest client may have left; grow the PTY back to the new minimum.
	s.applyMinSize()
	a.signalClose()
	<-writerDone
}

// attachWriter serializes all frames sent to a single client; it is the only
// writer of conn after the handshake ack, so concurrent output and resize
// requests can't interleave on the wire.
func (s *Session) attachWriter(a *attacher, snap []byte, done chan struct{}) {
	defer close(done)
	if err := WriteFrame(a.conn, FrameOutput, snap); err != nil {
		return
	}
	for {
		a.mu.Lock()
		for len(a.buf) == 0 && a.resync == nil && !a.closed {
			a.cond.Wait()
		}
		if a.closed {
			a.mu.Unlock()
			return
		}
		// deliverOutput already dropped the stale pre-snapshot backlog when it
		// captured the resync; the frames still queued here were rendered after
		// the snapshot and so tile from it, so emit the snapshot ahead of them
		// rather than dropping them. Taking one frame at a time and re-checking
		// for a resync between writes means an overflow landing mid-write
		// abandons any later stale frames on the next iteration instead of
		// streaming the whole backlog ahead of the fresh snapshot.
		if a.resync != nil {
			resync := a.resync
			a.resync = nil
			a.mu.Unlock()
			if err := WriteFrame(a.conn, FrameResync, resync); err != nil {
				return
			}
			continue
		}
		f := a.buf[0]
		a.buf = a.buf[1:]
		a.mu.Unlock()
		if err := WriteFrame(a.conn, f.tag, f.payload); err != nil {
			return
		}
		// A session-ended frame is the last thing a client receives: close the
		// connection so its serveAttach reader unblocks and shuts down.
		if f.tag == FrameEnded {
			a.conn.Close()
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
		a.buf = a.buf[:0]
		snap, _ := s.term.DumpScreen()
		// DumpScreen assumes a pre-cleared screen, so prepend a clear+home.
		a.resync = append([]byte("\x1b[2J\x1b[H"), snap...)
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
func (s *Session) attachResize(a *attacher, rp ResizePayload) {
	if rp.Rows == 0 || rp.Cols == 0 {
		return
	}
	s.mu.Lock()
	a.cols, a.rows = rp.Cols, rp.Rows
	s.mu.Unlock()
	s.applyMinSize()
}

// applyMinSize reflows the PTY and emulated terminal to the smallest cols and
// rows reported across attached clients, taken independently per dimension. It
// is a no-op until at least one client has reported its size.
func (s *Session) applyMinSize() {
	s.mu.Lock()
	cols, rows, ok := s.minSize()
	s.mu.Unlock()
	if ok {
		_ = s.resize(cols, rows)
	}
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
