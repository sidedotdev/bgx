package main

// Portions of this file are ported from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.

// isCtrlBackslash reports whether buf encodes a ctrl+\ keypress, either as the
// raw control byte 0x1C or as a Kitty keyboard protocol CSI u sequence. It is
// the detach key for attach, ported from zmx's util.isCtrlBackslash.
func isCtrlBackslash(buf []byte) bool {
	if len(buf) == 0 {
		return false
	}
	if buf[0] == 0x1C {
		return true
	}
	// Scan for a CSI u sequence anywhere in the buffer (input may be batched).
	for i := 0; i+2 < len(buf); i++ {
		if buf[i] == 0x1b && buf[i+1] == '[' && keypressWithMod(buf[i+2:], 0x5c, 0b100) {
			return true
		}
	}
	return false
}

// keypressWithMod parses the Kitty CSI u form
//
//	CSI key-code[:alternates] ; modifiers[:event-type] [; text-codepoints] u
//
// reporting whether it encodes expectedKey with exactly expectedMods (ignoring
// ambient lock modifiers) on a press or repeat event (release is rejected).
func keypressWithMod(buf []byte, expectedKey, expectedMods uint32) bool {
	pos := 0

	keyCode, ok := parseDecimal(buf, &pos)
	if !ok || keyCode != expectedKey {
		return false
	}

	// Skip any ':alternate-key' sub-fields (shifted key, base layout key).
	for pos < len(buf) && buf[pos] == ':' {
		pos++
		parseDecimal(buf, &pos)
	}

	if pos >= len(buf) || buf[pos] != ';' {
		return false
	}
	pos++

	modEncoded, ok := parseDecimal(buf, &pos)
	if !ok || modEncoded < 1 {
		return false
	}
	// Kitty encodes modifiers as 1 + bitfield; lock modifiers (caps_lock=64,
	// num_lock=128) are ambient and ignored when matching intentional combos.
	intentionalMods := (modEncoded - 1) & 0b00111111
	if expectedMods > 0 && expectedMods != intentionalMods {
		return false
	}

	if pos < len(buf) && buf[pos] == ':' {
		pos++
		eventType, ok := parseDecimal(buf, &pos)
		if !ok {
			return false
		}
		if eventType == 3 { // release
			return false
		}
	}

	// Skip an optional ';text-codepoints' section.
	if pos < len(buf) && buf[pos] == ';' {
		pos++
		for pos < len(buf) && (isDigit(buf[pos]) || buf[pos] == ':') {
			pos++
		}
	}

	return pos < len(buf) && buf[pos] == 'u'
}

// parseDecimal reads a decimal integer from buf at *pos, advancing past the
// consumed digits and reporting whether any digit was present.
func parseDecimal(buf []byte, pos *int) (uint32, bool) {
	start := *pos
	var value uint32
	for *pos < len(buf) && isDigit(buf[*pos]) {
		value = value*10 + uint32(buf[*pos]-'0')
		*pos++
	}
	return value, *pos != start
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// detachScanner buffers terminal input across reads so a ctrl+\ detach sequence
// split across read boundaries is still recognized rather than forwarded to the
// session. It holds back any trailing fragment that could still grow into a
// ctrl+\ keypress until the following read resolves it.
type detachScanner struct {
	pending []byte
}

// feed appends p to the buffered input and returns the bytes that are safe to
// forward to the session now, along with whether a complete ctrl+\ detach was
// seen. When detach is true no further input should be forwarded.
func (d *detachScanner) feed(p []byte) (forward []byte, detach bool) {
	d.pending = append(d.pending, p...)
	if isCtrlBackslash(d.pending) {
		d.pending = d.pending[:0]
		return nil, true
	}
	hold := ctrlBackslashSuffixLen(d.pending)
	cut := len(d.pending) - hold
	forward = append([]byte(nil), d.pending[:cut]...)
	d.pending = append(d.pending[:0], d.pending[cut:]...)
	return forward, false
}

// ctrlBackslashSuffixLen reports how many trailing bytes of buf could be the
// start of a ctrl+\ Kitty CSI-u sequence whose remaining bytes have not yet
// arrived. Such a fragment is held back so a detach key split across reads is
// still detected once the rest arrives.
func ctrlBackslashSuffixLen(buf []byte) int {
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] != 0x1b {
			continue
		}
		if ctrlBackslashPrefix(buf[i:]) {
			return len(buf) - i
		}
		return 0
	}
	return 0
}

// ctrlBackslashPrefix reports whether s, which begins with ESC, is an incomplete
// prefix that could still grow into a ctrl+\ keypress (CSI key-code 92 with the
// ctrl modifier). A lone ESC is treated as a real Escape key rather than held so
// interactive use is unaffected.
func ctrlBackslashPrefix(s []byte) bool {
	if len(s) < 2 || s[1] != '[' {
		return false
	}
	rest := s[2:]
	i := 0
	for i < len(rest) && isDigit(rest[i]) {
		i++
	}
	keyDigits := rest[:i]
	if i == len(rest) {
		return isPrefixOf(keyDigits, "92")
	}
	if string(keyDigits) != "92" {
		return false
	}
	return rest[i] == ':' || rest[i] == ';'
}

// isPrefixOf reports whether b is a prefix of s.
func isPrefixOf(b []byte, s string) bool {
	return len(b) <= len(s) && string(b) == s[:len(b)]
}
