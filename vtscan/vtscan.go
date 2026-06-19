// Package vtscan provides a dependency-free, stateful scanner for VT-encoded
// byte streams. It tracks whether a DEC/ANSI parser would be in its "ground"
// state and whether the stream is mid UTF-8 rune, so callers can choose cut
// points that neither split an escape sequence nor split a multi-byte rune.
//
// The escape/CSI/DCS/OSC/string transitions follow Paul Williams' VT500 parser
// model (https://vt100.net/emu/dec_ansi_parser). UTF-8 runes are tracked
// separately because that model predates UTF-8; 8-bit C1 controls are
// intentionally not recognized so their bytes can instead be interpreted as
// UTF-8 lead/continuation bytes.
package vtscan

import "unicode/utf8"

type state uint8

const (
	ground state = iota
	escape
	escapeIntermediate
	csiEntry
	csiParam
	csiIntermediate
	csiIgnore
	dcsEntry
	dcsParam
	dcsIntermediate
	dcsPassthrough
	dcsIgnore
	oscString
	stringIgnore // SOS/PM/APC payloads
)

// Scanner tracks parser ground state and UTF-8 rune progress across an
// arbitrarily chunked byte stream. The zero value is ready to use and starts in
// the ground state.
type Scanner struct {
	st          state
	utf8Pending int
}

// Reset returns the scanner to its initial ground state.
func (s *Scanner) Reset() {
	s.st = ground
	s.utf8Pending = 0
}

// AtGround reports whether the parser is at the ground state and not partway
// through a multi-byte UTF-8 rune. A stream may be cut safely at such a point
// without splitting an escape sequence or a rune.
func (s *Scanner) AtGround() bool {
	return s.st == ground && s.utf8Pending == 0
}

// Advance consumes p, updating the parser state.
func (s *Scanner) Advance(p []byte) {
	for _, b := range p {
		s.step(b)
	}
}

// SafeCut returns the largest offset n, with 0 <= n <= min(target, len(buf)),
// such that consuming buf[:n] from the scanner's current state leaves the parser
// at the ground state and on a UTF-8 rune boundary. It does not modify the
// scanner. It returns -1 when no such offset exists at or before target (for
// example when target falls inside an unterminated escape sequence).
func (s *Scanner) SafeCut(buf []byte, target int) int {
	if target > len(buf) {
		target = len(buf)
	}
	if target < 0 {
		return -1
	}
	cp := *s
	best := -1
	if cp.AtGround() {
		best = 0
	}
	for i := 0; i < len(buf); i++ {
		cp.step(buf[i])
		off := i + 1
		if off > target {
			break
		}
		if off < len(buf) && !utf8.RuneStart(buf[off]) {
			continue
		}
		if cp.AtGround() {
			best = off
		}
	}
	return best
}

func (s *Scanner) step(b byte) {
	if s.st == ground {
		s.stepGround(b)
		return
	}
	switch b {
	case 0x18, 0x1A: // CAN, SUB abort any sequence.
		s.st = ground
		return
	case 0x1B: // ESC restarts a sequence and begins ST in string states.
		s.st = escape
		return
	}
	switch s.st {
	case escape:
		s.stepEscape(b)
	case escapeIntermediate:
		if b >= 0x30 && b <= 0x7E {
			s.st = ground
		}
	case csiEntry, csiParam:
		s.stepCSI(b)
	case csiIntermediate:
		s.stepCSIIntermediate(b)
	case csiIgnore:
		if b >= 0x40 && b <= 0x7E {
			s.st = ground
		}
	case dcsEntry, dcsParam, dcsIntermediate:
		s.stepDCS(b)
	case oscString:
		if b == 0x07 { // BEL terminates OSC.
			s.st = ground
		}
	case dcsPassthrough, dcsIgnore, stringIgnore:
		// Consume payload until ESC/ST or CAN/SUB, handled above.
	}
}

func (s *Scanner) stepGround(b byte) {
	if s.utf8Pending > 0 {
		if b >= 0x80 && b <= 0xBF {
			s.utf8Pending--
			return
		}
		// Malformed continuation: abandon the partial rune and reprocess b.
		s.utf8Pending = 0
	}
	switch {
	case b == 0x1B:
		s.st = escape
	case b < 0x80:
		// C0 control or printable ASCII; stays at ground.
	case b < 0xC0:
		// Stray UTF-8 continuation byte; consumed, stays at ground.
	case b < 0xE0:
		s.utf8Pending = 1
	case b < 0xF0:
		s.utf8Pending = 2
	case b < 0xF8:
		s.utf8Pending = 3
	default:
		// 0xF8-0xFF is not a valid UTF-8 lead; consumed as a single byte.
	}
}

func (s *Scanner) stepEscape(b byte) {
	switch {
	case b == 0x50: // 'P' DCS
		s.st = dcsEntry
	case b == 0x5B: // '[' CSI
		s.st = csiEntry
	case b == 0x5D: // ']' OSC
		s.st = oscString
	case b == 0x58 || b == 0x5E || b == 0x5F: // 'X', '^', '_' SOS/PM/APC
		s.st = stringIgnore
	case b >= 0x20 && b <= 0x2F: // intermediate
		s.st = escapeIntermediate
	case b >= 0x30 && b <= 0x7E: // final byte dispatches.
		s.st = ground
	}
}

func (s *Scanner) stepCSI(b byte) {
	switch {
	case b >= 0x40 && b <= 0x7E: // final byte dispatches.
		s.st = ground
	case b >= 0x20 && b <= 0x2F: // intermediate
		s.st = csiIntermediate
	case b == 0x3A:
		s.st = csiIgnore
	case (b >= 0x30 && b <= 0x39) || b == 0x3B: // parameter digits and separator
		s.st = csiParam
	case b >= 0x3C && b <= 0x3F: // private prefix, valid only before parameters
		if s.st == csiEntry {
			s.st = csiParam
		} else {
			s.st = csiIgnore
		}
	}
}

func (s *Scanner) stepCSIIntermediate(b byte) {
	switch {
	case b >= 0x40 && b <= 0x7E:
		s.st = ground
	case b >= 0x20 && b <= 0x2F:
		// stay
	case b >= 0x30 && b <= 0x3F:
		s.st = csiIgnore
	}
}

func (s *Scanner) stepDCS(b byte) {
	switch s.st {
	case dcsEntry:
		switch {
		case b >= 0x20 && b <= 0x2F:
			s.st = dcsIntermediate
		case b == 0x3A:
			s.st = dcsIgnore
		case (b >= 0x30 && b <= 0x39) || (b >= 0x3B && b <= 0x3F):
			s.st = dcsParam
		case b >= 0x40 && b <= 0x7E:
			s.st = dcsPassthrough
		}
	case dcsParam:
		switch {
		case (b >= 0x30 && b <= 0x39) || b == 0x3B:
			// stay
		case b == 0x3A || (b >= 0x3C && b <= 0x3F):
			s.st = dcsIgnore
		case b >= 0x20 && b <= 0x2F:
			s.st = dcsIntermediate
		case b >= 0x40 && b <= 0x7E:
			s.st = dcsPassthrough
		}
	case dcsIntermediate:
		switch {
		case b >= 0x20 && b <= 0x2F:
			// stay
		case b >= 0x30 && b <= 0x3F:
			s.st = dcsIgnore
		case b >= 0x40 && b <= 0x7E:
			s.st = dcsPassthrough
		}
	}
}
