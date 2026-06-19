package daemon

// The tagged, length-prefixed frame protocol in this file is ported from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameTag identifies the kind of payload carried by an attach frame. After the
// JSON "attach" handshake the connection switches to these tagged,
// length-prefixed frames, modeled on zmx's tagged message protocol.
type FrameTag uint8

const (
	FrameInput  FrameTag = 0 // client -> daemon: raw PTY input bytes
	FrameOutput FrameTag = 1 // daemon -> client: raw PTY output bytes
	FrameResize FrameTag = 2 // window size; an empty body requests the peer's size
	FrameDetach FrameTag = 3 // client -> daemon: detach without stopping the session
	// FrameResync carries a fresh screen snapshot for a client that fell behind:
	// its payload begins with a clear so the client repaints from the latest
	// rendered terminal state, after which live streaming resumes.
	FrameResync FrameTag = 4 // daemon -> client: clear and repaint with a fresh snapshot
	// FrameEnded signals the session has ended; it carries no payload and the
	// daemon closes the connection after sending it.
	FrameEnded FrameTag = 5 // daemon -> client: session has ended
)

// maxFrameLen caps an inbound frame payload so a corrupt or hostile length
// can't make the reader allocate unbounded memory.
const maxFrameLen = 16 << 20

// frameHeaderLen is the fixed size of a frame header: a one-byte tag followed by
// a big-endian uint32 payload length.
const frameHeaderLen = 5

// ResizePayload is the body of a FrameResize frame: rows then cols.
type ResizePayload struct {
	Rows uint16
	Cols uint16
}

// WriteFrame writes a single tagged, length-prefixed frame to w.
func WriteFrame(w io.Writer, tag FrameTag, payload []byte) error {
	var hdr [frameHeaderLen]byte
	hdr[0] = byte(tag)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single frame from r, returning its tag and payload.
func ReadFrame(r io.Reader) (FrameTag, []byte, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrameLen {
		return 0, nil, fmt.Errorf("daemon: frame payload too large: %d", n)
	}
	if n == 0 {
		return FrameTag(hdr[0]), nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return FrameTag(hdr[0]), buf, nil
}

// EncodeResize encodes a window size as a FrameResize payload.
func EncodeResize(rows, cols uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:], rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return b
}

// DecodeResize decodes a FrameResize payload, reporting whether it was well
// formed. An empty payload is the peer's request for the current size.
func DecodeResize(b []byte) (ResizePayload, bool) {
	if len(b) != 4 {
		return ResizePayload{}, false
	}
	return ResizePayload{
		Rows: binary.BigEndian.Uint16(b[0:]),
		Cols: binary.BigEndian.Uint16(b[2:]),
	}, true
}
