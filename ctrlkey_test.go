package main

import "testing"

// TestIsCtrlBackslashMissesSplitSequence documents why attach buffers input
// through a detachScanner: the raw per-chunk check cannot see a ctrl+\ Kitty
// sequence that is split across reads.
func TestIsCtrlBackslashMissesSplitSequence(t *testing.T) {
	first := []byte("\x1b[92")
	second := []byte(";5u")
	if isCtrlBackslash(first) || isCtrlBackslash(second) {
		t.Fatal("expected neither half of a split ctrl+\\ to match on its own")
	}
	if !isCtrlBackslash(append(append([]byte(nil), first...), second...)) {
		t.Fatal("the joined sequence should be a ctrl+\\")
	}
}

// TestDetachScannerDetectsSplitSequence verifies the scanner recognizes a ctrl+\
// detach key whose bytes arrive across two reads, without leaking the partial
// sequence to the session.
func TestDetachScannerDetectsSplitSequence(t *testing.T) {
	var d detachScanner

	forward, detach := d.feed([]byte("\x1b[92"))
	if detach {
		t.Fatal("detach reported before the sequence completed")
	}
	if len(forward) != 0 {
		t.Fatalf("partial detach sequence forwarded to session: %q", forward)
	}

	forward, detach = d.feed([]byte(";5u"))
	if !detach {
		t.Fatal("split ctrl+\\ sequence not detected across reads")
	}
	if len(forward) != 0 {
		t.Fatalf("forwarded bytes on detach: %q", forward)
	}
}

// TestDetachScannerForwardsRegularInput ensures ordinary input passes straight
// through and that a lone trailing ESC (a real Escape keypress) is not withheld.
func TestDetachScannerForwardsRegularInput(t *testing.T) {
	var d detachScanner

	forward, detach := d.feed([]byte("hello"))
	if detach || string(forward) != "hello" {
		t.Fatalf("feed(hello) = %q, detach=%v", forward, detach)
	}

	forward, detach = d.feed([]byte{0x1b})
	if detach || len(forward) != 1 || forward[0] != 0x1b {
		t.Fatalf("lone ESC withheld: %q, detach=%v", forward, detach)
	}
}

// TestDetachScannerDetectsRawByte covers the non-Kitty detach encoding (the raw
// 0x1C control byte), which cannot be split since it is a single byte.
func TestDetachScannerDetectsRawByte(t *testing.T) {
	var d detachScanner
	if _, detach := d.feed([]byte{0x1c}); !detach {
		t.Fatal("raw ctrl+\\ byte not detected")
	}
}
