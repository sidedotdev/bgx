package daemon

// The multi-client fanout and frame bridging in this file are ported from
// zmx (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.

import (
	"encoding/json"
	"io"
	"net"
)

// attachQueue bounds the per-client output backlog. A client that can't keep up
// is disconnected rather than stalling the PTY output pump or silently dropping
// bytes mid-stream.
const attachQueue = 1024

// outFrame is a frame queued for delivery to an attached client.
type outFrame struct {
	tag     FrameTag
	payload []byte
}

// attacher is a single live attach connection. Frames destined for the client
// flow through ch to a dedicated writer goroutine so the output pump never
// blocks on a slow consumer.
type attacher struct {
	conn net.Conn
	ch   chan outFrame
	done chan struct{}

	// rows and cols hold the client's last reported window size (0 = unknown),
	// guarded by s.mu.
	rows uint16
	cols uint16
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

	a := &attacher{conn: conn, ch: make(chan outFrame, attachQueue), done: make(chan struct{})}

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
	close(a.done)
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
		select {
		case f := <-a.ch:
			if err := WriteFrame(a.conn, f.tag, f.payload); err != nil {
				return
			}
		case <-a.done:
			return
		}
	}
}

// enqueueFrame hands a frame to a client's writer, disconnecting a client whose
// backlog is full so a stuck consumer never stalls the output pump.
func (s *Session) enqueueFrame(a *attacher, f outFrame) {
	select {
	case a.ch <- f:
	default:
		a.conn.Close()
	}
}

// fanout copies PTY output to every attached client. It holds s.mu only while
// snapshotting the client set so enqueueing (which may close a slow client's
// connection) happens without the lock held.
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
		s.enqueueFrame(a, outFrame{tag: FrameOutput, payload: cp})
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
