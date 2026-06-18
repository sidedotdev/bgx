package daemon

// The multi-client leader model and frame bridging in this file are ported from
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
	requestSize := s.leader == nil
	if requestSize {
		s.leader = a
	}
	s.mu.Unlock()
	s.outMu.Unlock()

	writerDone := make(chan struct{})
	go s.attachWriter(a, snap, writerDone)

	// Ask the new leader for its window size so the PTY and emulated terminal
	// match the controlling client.
	if requestSize {
		s.enqueueFrame(a, outFrame{tag: FrameResize})
	}

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
	var promoted *attacher
	if s.leader == a {
		s.leader = nil
		// Promote a surviving client so the PTY keeps a size controller; its
		// window size is requested below so the terminal reflows to match.
		for other := range s.attachers {
			s.leader = other
			promoted = other
			break
		}
	}
	s.mu.Unlock()
	if promoted != nil {
		s.enqueueFrame(promoted, outFrame{tag: FrameResize})
	}
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

// attachInput forwards a client's input to the PTY. Only the leader's input is
// applied; a non-leader claims leadership the moment it sends real keystrokes,
// at which point its window size is requested so the PTY tracks the new
// controlling client.
func (s *Session) attachInput(a *attacher, payload []byte) {
	if len(payload) == 0 {
		return
	}
	s.mu.Lock()
	leader := s.leader == a
	claimed := false
	if !leader && (s.leader == nil || isUserInput(payload)) {
		s.leader = a
		leader = true
		claimed = true
	}
	s.mu.Unlock()
	if claimed {
		s.enqueueFrame(a, outFrame{tag: FrameResize})
	}
	if leader {
		s.queueInput(payload)
	}
}

// attachResize applies the leader's reported window size to both the PTY and
// the emulated terminal so output reflows correctly for attached clients.
func (s *Session) attachResize(a *attacher, rp ResizePayload) {
	s.mu.Lock()
	if s.leader == nil {
		s.leader = a
	}
	leader := s.leader == a
	s.mu.Unlock()
	if !leader || rp.Rows == 0 || rp.Cols == 0 {
		return
	}
	_ = s.resize(rp.Cols, rp.Rows)
}

// isUserInput reports whether payload looks like a deliberate keystroke (text or
// an editing/navigation key) rather than an automatic terminal report such as a
// mouse or focus event, mirroring zmx's leader-claim heuristic.
func isUserInput(payload []byte) bool {
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if c == 0x1b {
			if i+2 < len(payload) && payload[i+1] == '[' {
				switch payload[i+2] {
				case 'M', '<', 'I', 'O':
					return false
				}
			}
			return true
		}
		return true
	}
	return false
}
