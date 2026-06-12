package scrollback

import (
	"bytes"
	"testing"
)

// pattern returns n bytes with a deterministic, position-dependent value so
// equality checks can detect any reordering or corruption.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + 7)
	}
	return b
}

func TestSmallStreamPassesThrough(t *testing.T) {
	s := NewStore(0, 0)
	defer s.Close()

	data := []byte("hello, scrollback world")
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := s.Snapshot()
	if !bytes.Equal(got, data) {
		t.Fatalf("snapshot = %q, want %q", got, data)
	}
	if total := s.TotalBytes(); total != int64(len(data)) {
		t.Fatalf("total bytes = %d, want %d", total, len(data))
	}
}

func TestLargeStreamKeepsHeadAndTailDiscardsMiddle(t *testing.T) {
	const (
		head  = 2000
		tail  = 4000
		chunk = 1000
		total = 20000
	)
	s := newStore(head, tail, chunk, 1<<20)
	defer s.Close()

	data := pattern(total)
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := s.Snapshot()
	want := append(append([]byte{}, data[:head]...), data[total-tail:]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot len = %d, want %d; head match = %v",
			len(got), len(want), bytes.Equal(got[:head], data[:head]))
	}
	if !bytes.Equal(got[:head], data[:head]) {
		t.Fatal("head was not retained verbatim")
	}
	if !bytes.Equal(got[head:], data[total-tail:]) {
		t.Fatal("tail does not match the last tailSize bytes")
	}
	if total := s.TotalBytes(); total != int64(len(data)) {
		t.Fatalf("total bytes = %d, want %d", total, len(data))
	}
}

func TestTailNotChunkAligned(t *testing.T) {
	const (
		head  = 2000
		tail  = 3500 // deliberately not a multiple of chunk
		chunk = 1000
		total = 20000
	)
	s := newStore(head, tail, chunk, 1<<20)
	defer s.Close()

	data := pattern(total)
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := s.Snapshot()
	want := append(append([]byte{}, data[:head]...), data[total-tail:]...)
	if len(got) != head+tail {
		t.Fatalf("snapshot len = %d, want %d", len(got), head+tail)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot did not retain exact head + last tailSize bytes")
	}
}

func TestTotalBytesAccounting(t *testing.T) {
	s := newStore(1000, 2000, 1000, 1<<20)
	defer s.Close()

	var written int
	for i := 0; i < 50; i++ {
		p := pattern(137)
		n, err := s.Write(p)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		written += n
	}

	if total := s.TotalBytes(); total != int64(written) {
		t.Fatalf("total bytes = %d, want %d", total, written)
	}
}

func TestFallbackStoresUncompressedChunks(t *testing.T) {
	const (
		head     = 10
		tail     = 1 << 20 // large enough to retain everything
		chunk    = 1000
		fallback = 5000
		total    = 30000
	)
	s := newStore(head, tail, chunk, fallback)
	defer s.Close()

	data := pattern(total)
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Sync the drain goroutine and verify retained data is intact regardless of
	// which chunks took the uncompressed fallback path.
	got := s.Snapshot()
	if !bytes.Equal(got, data) {
		t.Fatalf("snapshot did not round-trip: got %d bytes, want %d", len(got), total)
	}

	s.mu.Lock()
	var raw, compressed int
	for _, c := range s.tailChunks {
		if c.compressed {
			compressed++
		} else {
			raw++
		}
	}
	s.mu.Unlock()

	if raw == 0 {
		t.Fatal("expected some chunks to use the uncompressed fallback path")
	}
	if compressed == 0 {
		t.Fatal("expected later chunks to be compressed once the backlog drained")
	}
}
