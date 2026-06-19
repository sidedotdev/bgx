package vtscan

import "testing"

func TestPlainTextStaysGround(t *testing.T) {
	var s Scanner
	s.Advance([]byte("hello world"))
	if !s.AtGround() {
		t.Fatal("plain text should leave scanner at ground")
	}
}

func TestC0ControlsStayGround(t *testing.T) {
	var s Scanner
	s.Advance([]byte("a\r\n\tb\x07"))
	if !s.AtGround() {
		t.Fatal("C0 controls should not leave ground")
	}
}

func TestLoneEscapeNotGround(t *testing.T) {
	var s Scanner
	s.Advance([]byte{0x1B})
	if s.AtGround() {
		t.Fatal("lone ESC should not be at ground")
	}
}

func TestCSISequence(t *testing.T) {
	var s Scanner
	s.Advance([]byte("\x1b[31"))
	if s.AtGround() {
		t.Fatal("mid CSI should not be at ground")
	}
	s.Advance([]byte("m"))
	if !s.AtGround() {
		t.Fatal("completed CSI should return to ground")
	}
}

func TestOSCBELTerminator(t *testing.T) {
	var s Scanner
	s.Advance([]byte("\x1b]0;title"))
	if s.AtGround() {
		t.Fatal("mid OSC should not be at ground")
	}
	s.Advance([]byte{0x07})
	if !s.AtGround() {
		t.Fatal("BEL should terminate OSC")
	}
}

func TestOSCStringTerminator(t *testing.T) {
	var s Scanner
	s.Advance([]byte("\x1b]0;title"))
	if s.AtGround() {
		t.Fatal("mid OSC should not be at ground")
	}
	s.Advance([]byte("\x1b\\"))
	if !s.AtGround() {
		t.Fatal("ST should terminate OSC")
	}
}

func TestSplitTwoByteRune(t *testing.T) {
	r := []byte("é") // 0xC3 0xA9
	var s Scanner
	s.Advance(r[:1])
	if s.AtGround() {
		t.Fatal("mid multi-byte rune should not be a safe boundary")
	}
	s.Advance(r[1:])
	if !s.AtGround() {
		t.Fatal("completed rune should be at ground")
	}
}

func TestSplitThreeAndFourByteRunes(t *testing.T) {
	for _, in := range []string{"€", "𝄞"} {
		b := []byte(in)
		var s Scanner
		for i := 1; i < len(b); i++ {
			s.Reset()
			s.Advance(b[:i])
			if s.AtGround() {
				t.Fatalf("%q: prefix of %d bytes should not be at ground", in, i)
			}
		}
		s.Reset()
		s.Advance(b)
		if !s.AtGround() {
			t.Fatalf("%q: full rune should be at ground", in)
		}
	}
}

func TestSafeCutAvoidsEscapeSequence(t *testing.T) {
	buf := []byte("abc\x1b[31mdef")
	var s Scanner
	if got := s.SafeCut(buf, 6); got != 3 {
		t.Fatalf("SafeCut inside CSI = %d, want 3", got)
	}
	if got := s.SafeCut(buf, len(buf)); got != len(buf) {
		t.Fatalf("SafeCut full = %d, want %d", got, len(buf))
	}
}

func TestSafeCutAvoidsSplitRune(t *testing.T) {
	buf := []byte("ab" + "é" + "c") // a b C3 A9 c
	var s Scanner
	if got := s.SafeCut(buf, 3); got != 2 {
		t.Fatalf("SafeCut inside rune = %d, want 2", got)
	}
	if got := s.SafeCut(buf, 4); got != 4 {
		t.Fatalf("SafeCut after rune = %d, want 4", got)
	}
}

func TestSafeCutNoGroundReturnsNegative(t *testing.T) {
	var s Scanner
	s.Advance([]byte("\x1b[")) // enter an unterminated CSI sequence
	if got := s.SafeCut([]byte("12"), 2); got != -1 {
		t.Fatalf("SafeCut with no ground point = %d, want -1", got)
	}
}

func TestDCSWithSTTerminator(t *testing.T) {
	var s Scanner
	s.Advance([]byte("\x1bP1;2|payload"))
	if s.AtGround() {
		t.Fatal("mid DCS should not be at ground")
	}
	s.Advance([]byte("\x1b\\"))
	if !s.AtGround() {
		t.Fatal("ST should terminate DCS")
	}
}

func TestSOSPMAPCStringsTerminatedByST(t *testing.T) {
	for _, intro := range []string{"\x1bX", "\x1b^", "\x1b_"} {
		var s Scanner
		s.Advance([]byte(intro + "payload"))
		if s.AtGround() {
			t.Fatalf("%q: mid string should not be at ground", intro)
		}
		s.Advance([]byte("\x1b\\"))
		if !s.AtGround() {
			t.Fatalf("%q: ST should terminate string", intro)
		}
	}
}

func TestStringStateEscapeStartsNewSequence(t *testing.T) {
	// ESC ends the OSC string; the following bytes form a fresh CSI, so the
	// scanner only returns to ground once that CSI's final byte arrives.
	var s Scanner
	s.Advance([]byte("\x1b]0;title\x1b[0"))
	if s.AtGround() {
		t.Fatal("CSI begun mid-OSC should not be at ground before its final byte")
	}
	s.Advance([]byte("m"))
	if !s.AtGround() {
		t.Fatal("CSI final byte should return to ground")
	}
}

func TestStringStateEscapeNonBackslashFinalExits(t *testing.T) {
	// Any ESC <final> exits a string state per the VT500 model, not just ST.
	var s Scanner
	s.Advance([]byte("\x1b]0;t"))
	if s.AtGround() {
		t.Fatal("mid OSC should not be at ground")
	}
	s.Advance([]byte("\x1bA"))
	if !s.AtGround() {
		t.Fatal("ESC <final> should exit the string and return to ground")
	}
}

func TestCANSUBAbortSequence(t *testing.T) {
	for _, abort := range []byte{0x18, 0x1A} {
		var s Scanner
		s.Advance([]byte("\x1b[31"))
		if s.AtGround() {
			t.Fatal("mid CSI should not be at ground")
		}
		s.Advance([]byte{abort})
		if !s.AtGround() {
			t.Fatalf("%#x should abort the sequence back to ground", abort)
		}
	}
}

func TestSingleC1BytesStayGround(t *testing.T) {
	// In a UTF-8 stream the single bytes 0x80-0x9F are not C1 controls; they are
	// stray continuation bytes that are consumed without leaving ground (so they
	// remain valid cut points), rather than 8-bit ST/CSI/OSC/NEL controls.
	for _, b := range []byte{0x85, 0x90, 0x9B, 0x9C, 0x9D} {
		var s Scanner
		s.Advance([]byte{b})
		if !s.AtGround() {
			t.Fatalf("single C1 byte %#x should leave scanner at ground", b)
		}
	}
}

func TestNELViaEscapeReturnsToGround(t *testing.T) {
	// NEL in a UTF-8 stream is the 7-bit ESC E, not the 8-bit 0x85.
	var s Scanner
	s.Advance([]byte("\x1bE"))
	if !s.AtGround() {
		t.Fatal("ESC E (NEL) should return to ground")
	}
}

func TestC1BytesAsUTF8Continuation(t *testing.T) {
	// U+0085 and U+009C encode as 0xC2 0x85 / 0xC2 0x9C: the C1 byte is a
	// legitimate continuation byte and must be consumed as part of the rune.
	for _, r := range []string{"\u0085", "\u009c"} {
		b := []byte(r)
		var s Scanner
		s.Advance(b[:1])
		if s.AtGround() {
			t.Fatalf("%q: lead byte alone should not be at ground", r)
		}
		s.Reset()
		s.Advance(b)
		if !s.AtGround() {
			t.Fatalf("%q: complete rune should be at ground", r)
		}
	}
}

func TestSafeCutAvoidsDCS(t *testing.T) {
	buf := []byte("ab\x1bP1|x\x1b\\cd")
	var s Scanner
	if got := s.SafeCut(buf, 5); got != 2 {
		t.Fatalf("SafeCut inside DCS = %d, want 2", got)
	}
	if got := s.SafeCut(buf, len(buf)); got != len(buf) {
		t.Fatalf("SafeCut full = %d, want %d", got, len(buf))
	}
}
