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
