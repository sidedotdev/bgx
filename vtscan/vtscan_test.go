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
