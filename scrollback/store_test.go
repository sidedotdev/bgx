package scrollback

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/vtscan"
)

// slowBackend widens the window during which Store.flushChunk holds no lock, so
// writes that race with a flush are exercised by the concurrency regression
// test. It otherwise behaves exactly like the in-memory backend.
type slowBackend struct {
	memoryBackend
	delay time.Duration
}

func (b slowBackend) put(data []byte) (chunkHandle, error) {
	time.Sleep(b.delay)
	return b.memoryBackend.put(data)
}

// commonPrefixLen returns the length of the longest shared prefix of a and b.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// atGround reports whether consuming b leaves a VT parser at the ground state
// and on a UTF-8 rune boundary, i.e. b ends at a point where the stream may be
// cut without splitting an escape sequence or a multi-byte rune.
func atGround(b []byte) bool {
	var sc vtscan.Scanner
	sc.Advance(b)
	return sc.AtGround()
}

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

	if int64(len(got)) >= s.TotalBytes() {
		t.Fatalf("snapshot len = %d did not discard the middle of %d bytes", len(got), s.TotalBytes())
	}

	// The discarded middle is replaced by a demarcation block plus a reset
	// preamble, so the tail follows the reset rather than the head directly.
	reset := []byte(resetPreamble)
	resetAt := bytes.Index(got, reset)
	if resetAt < 0 {
		t.Fatal("snapshot is missing the reset preamble before the tail")
	}
	tailGot := got[resetAt+len(reset):]

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	hlen := s.headBytes
	s.mu.Unlock()

	if hlen < head {
		t.Fatalf("retained head len = %d, want >= %d", hlen, head)
	}
	if !bytes.Equal(got[:hlen], data[:hlen]) {
		t.Fatal("retained head is not a verbatim prefix of the stream")
	}
	if !bytes.Equal(tailGot, data[total-len(tailGot):]) {
		t.Fatal("tail is not a contiguous suffix of the stream")
	}
	if len(tailGot) < tail {
		t.Fatalf("retained tail len = %d, want >= %d", len(tailGot), tail)
	}
	if !atGround(got[:hlen]) {
		t.Fatal("retained head does not end on a ground/rune boundary")
	}
	if !atGround(data[:total-len(tailGot)]) {
		t.Fatal("retained tail does not begin on a ground/rune boundary")
	}
	discarded := int64(total - hlen - len(tailGot))
	if !bytes.Equal(got[hlen:resetAt+len(reset)], appendTruncation(nil, discarded)) {
		t.Fatalf("demarcation = %q, want %q", got[hlen:resetAt+len(reset)], appendTruncation(nil, discarded))
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

	reset := []byte(resetPreamble)
	resetAt := bytes.Index(got, reset)
	if resetAt < 0 {
		t.Fatal("snapshot is missing the reset preamble before the tail")
	}
	tailGot := got[resetAt+len(reset):]

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	hlen := s.headBytes
	s.mu.Unlock()

	if !bytes.Equal(got[:hlen], data[:hlen]) {
		t.Fatal("retained head is not a verbatim prefix of the stream")
	}
	if !bytes.Equal(tailGot, data[total-len(tailGot):]) {
		t.Fatal("tail is not a contiguous suffix of the stream")
	}
	if len(tailGot) < tail {
		t.Fatalf("retained tail len = %d, want >= %d (tail size need not be chunk-aligned)", len(tailGot), tail)
	}
	if !atGround(got[:hlen]) {
		t.Fatal("retained head does not end on a ground/rune boundary")
	}
	if !atGround(data[:total-len(tailGot)]) {
		t.Fatal("retained tail does not begin on a ground/rune boundary")
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024 * 1024, "1.0MB"},
		{1536 * 1024, "1.5MB"},
		{1024 * 1024 * 1024, "1.0GB"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.n); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestSnapshotDemarcatesDiscardedMiddle(t *testing.T) {
	const (
		head  = 2000
		tail  = 4000
		chunk = 1000
		total = 40000
	)
	s := newStore(head, tail, chunk, 1<<20)
	defer s.Close()
	if _, err := s.Write(pattern(total)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := s.Snapshot()

	reset := []byte(resetPreamble)
	resetAt := bytes.Index(got, reset)
	if resetAt < 0 {
		t.Fatal("snapshot is missing the reset preamble before the tail")
	}
	if !bytes.Contains(got[:resetAt], []byte("[...] truncated ")) {
		t.Fatal("snapshot is missing the truncation notice")
	}

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	headLen := s.headBytes
	s.mu.Unlock()

	tailGot := got[resetAt+len(reset):]
	discarded := int64(total - headLen - len(tailGot))
	if discarded <= 0 {
		t.Fatalf("expected a discarded middle, got %d", discarded)
	}

	// Assert the exact required shape: a blank line, a marker line, the notice,
	// a marker line, a blank line, then the reset preamble.
	wantBlock := "\r\n\r\n" + truncationRule + "\r\n" +
		"[...] truncated " + humanizeBytes(discarded) + "\r\n" +
		truncationRule + "\r\n\r\n" + resetPreamble
	if gotBlock := string(got[headLen : resetAt+len(reset)]); gotBlock != wantBlock {
		t.Fatalf("demarcation = %q, want %q", gotBlock, wantBlock)
	}
}

func TestSnapshotNoDemarcationWhenNothingDiscarded(t *testing.T) {
	s := newStore(1<<20, 1<<20, 1000, 1<<20)
	defer s.Close()

	data := pattern(50000)
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := s.Snapshot()
	if !bytes.Equal(got, data) {
		t.Fatal("snapshot altered output when nothing was discarded")
	}
	if bytes.Contains(got, []byte("truncated")) || bytes.Contains(got, []byte(resetPreamble)) {
		t.Fatal("emitted demarcation despite retaining the entire stream")
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

func TestBoundariesNeverSplitRunesOrEscapes(t *testing.T) {
	const (
		head  = 2000
		tail  = 4000
		chunk = 1000
	)
	s := newStore(head, tail, chunk, 1<<20)
	defer s.Close()

	// Interleave terminated escape sequences (CSI, OSC) with 2-, 3- and 4-byte
	// UTF-8 runes so that naive byte-count cuts would routinely split a rune or
	// sequence.
	unit := []byte("\x1b[1;31mError:\x1b[0m 世界 café — \xf0\x9f\x98\x80 \x1b]0;title\x07 plain text\n")
	var data []byte
	for len(data) < 60000 {
		data = append(data, unit...)
	}
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := s.Snapshot()
	if int64(len(got)) >= int64(len(data)) {
		t.Fatalf("snapshot len = %d did not discard the middle of %d bytes", len(got), len(data))
	}

	reset := []byte(resetPreamble)
	resetAt := bytes.Index(got, reset)
	if resetAt < 0 {
		t.Fatal("snapshot is missing the reset preamble before the tail")
	}
	tailGot := got[resetAt+len(reset):]

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	hlen := s.headBytes
	s.mu.Unlock()

	if !bytes.Equal(tailGot, data[len(data)-len(tailGot):]) {
		t.Fatal("tail is not a contiguous suffix of the stream")
	}
	if !atGround(got[:hlen]) {
		t.Fatal("retained head ends inside an escape sequence or multi-byte rune")
	}
	if !atGround(data[:len(data)-len(tailGot)]) {
		t.Fatal("retained tail begins inside an escape sequence or multi-byte rune")
	}

	// Every stored chunk must also end on a ground/rune boundary so a chunk is
	// never recompressed across a split sequence.
	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	for i := 1; i <= len(s.headChunks); i++ {
		if !atGround(s.decodeChunks(s.headChunks[:i])) {
			t.Fatalf("head chunk %d does not end on a ground/rune boundary", i)
		}
	}
	for i := 1; i <= len(s.tailChunks); i++ {
		if !atGround(s.decodeChunks(s.tailChunks[:i])) {
			t.Fatalf("tail chunk %d does not end on a ground/rune boundary", i)
		}
	}
	s.mu.Unlock()
}

func TestConcurrentWritesDuringFlushArePreserved(t *testing.T) {
	const (
		writers   = 8
		perWriter = 200
		writeSize = 300
		head      = 1000
		chunk     = 1000
	)
	// A huge tail retains everything so the snapshot must reproduce every
	// written byte; a slow backend forces writes to land while a flush holds no
	// lock, which previously dropped bytes appended during that window.
	s := newStoreBackend(head, 1<<30, chunk, 1<<30, slowBackend{delay: 50 * time.Microsecond})
	defer s.Close()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(val byte) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{val}, writeSize)
			for i := 0; i < perWriter; i++ {
				if _, err := s.Write(payload); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(byte(w + 1))
	}
	wg.Wait()

	got := s.Snapshot()
	wantTotal := writers * perWriter * writeSize
	if len(got) != wantTotal {
		t.Fatalf("snapshot len = %d, want %d (bytes lost or duplicated during flush)", len(got), wantTotal)
	}

	// Each writer used a distinct byte value, so per-value counts must match
	// regardless of how the concurrent writes interleaved.
	counts := make(map[byte]int)
	for _, c := range got {
		counts[c]++
	}
	for w := 0; w < writers; w++ {
		val := byte(w + 1)
		if counts[val] != perWriter*writeSize {
			t.Fatalf("byte %d count = %d, want %d", val, counts[val], perWriter*writeSize)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("store error: %v", err)
	}
}

func TestPartialMultibyteWritePreservedAcrossWrites(t *testing.T) {
	// Each multibyte rune is delivered split across separate Write calls so each
	// write ends mid-rune; tiny head/chunk sizes push cuts toward the runes. No
	// chunk may flush partway through a rune.
	emoji := []byte("\U0001F600") // 4 bytes
	cjk := []byte("世")            // 3 bytes
	s := newStore(8, 1<<30, 4, 1<<20)
	defer s.Close()

	writes := [][]byte{
		[]byte("abc"),
		emoji[:1], emoji[1:3], emoji[3:],
		[]byte("de"),
		cjk[:2], cjk[2:],
		[]byte("fgh"),
	}
	var want []byte
	for _, w := range writes {
		want = append(want, w...)
		if _, err := s.Write(w); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if got := s.Snapshot(); !bytes.Equal(got, want) {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	for i := 1; i <= len(s.headChunks); i++ {
		if !atGround(s.decodeChunks(s.headChunks[:i])) {
			t.Fatalf("head chunk %d ends inside a rune", i)
		}
	}
	for i := 1; i <= len(s.tailChunks); i++ {
		if !atGround(s.decodeChunks(s.tailChunks[:i])) {
			t.Fatalf("tail chunk %d ends inside a rune", i)
		}
	}
	s.mu.Unlock()
}

func TestStatefulEscapeSequenceSpansChunksAndEvictions(t *testing.T) {
	const (
		head  = 2000
		tail  = 4000
		chunk = 1000
	)
	s := newStore(head, tail, chunk, 1<<20)
	defer s.Close()

	// A long OSC payload has no interior ground boundary, so it can only be cut
	// once terminated: it must never be flushed split across chunks even though
	// it dwarfs the chunk size and straddles head/tail and eviction boundaries.
	longOSC := append([]byte("\x1b]0;"), bytes.Repeat([]byte("x"), 5000)...)
	longOSC = append(longOSC, 0x07)
	var data []byte
	for len(data) < 60000 {
		data = append(data, []byte("plain text line\n")...)
		data = append(data, longOSC...)
		data = append(data, []byte(" 世界 café\n")...)
	}
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := s.Snapshot()

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	headLen := s.headBytes
	for i := 1; i <= len(s.headChunks); i++ {
		if !atGround(s.decodeChunks(s.headChunks[:i])) {
			t.Fatalf("head chunk %d ends inside an escape sequence", i)
		}
	}
	for i := 1; i <= len(s.tailChunks); i++ {
		if !atGround(s.decodeChunks(s.tailChunks[:i])) {
			t.Fatalf("tail chunk %d ends inside an escape sequence", i)
		}
	}
	s.mu.Unlock()

	if int64(headLen) >= s.TotalBytes() {
		t.Fatalf("nothing discarded (head %d >= total %d); test would not exercise eviction", headLen, s.TotalBytes())
	}
	if !atGround(got[:headLen]) {
		t.Fatal("retained head ends inside an escape sequence")
	}
	reset := []byte(resetPreamble)
	resetAt := bytes.Index(got, reset)
	if resetAt < 0 {
		t.Fatal("snapshot is missing the reset preamble before the tail")
	}
	tailGot := got[resetAt+len(reset):]
	if !bytes.Equal(tailGot, data[len(data)-len(tailGot):]) {
		t.Fatal("retained tail is not a contiguous suffix of the stream")
	}
	if !atGround(data[:len(data)-len(tailGot)]) {
		t.Fatal("retained tail begins inside an escape sequence")
	}
}

func TestApproximateSizesStayWithinOneBoundaryGap(t *testing.T) {
	const (
		head  = 5000
		tail  = 8000
		chunk = 1000
	)
	// Every unit returns to ground within its own length, so the longest run
	// without a ground/rune boundary is bounded by len(line); head and tail must
	// not overshoot their configured sizes by more than that gap.
	line := []byte("\x1b[32mok\x1b[0m café 世\n")
	maxGap := len(line)
	var data []byte
	for len(data) < 80000 {
		data = append(data, line...)
	}
	s := newStore(head, tail, chunk, 1<<20)
	defer s.Close()
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := s.Snapshot()

	s.mu.Lock()
	for s.busy {
		s.cond.Wait()
	}
	headLen := s.headBytes
	s.mu.Unlock()

	if headLen < head || headLen > head+maxGap {
		t.Fatalf("retained head len = %d, want within [%d, %d]", headLen, head, head+maxGap)
	}
	reset := []byte(resetPreamble)
	resetAt := bytes.Index(got, reset)
	if resetAt < 0 {
		t.Fatal("snapshot is missing the reset preamble before the tail")
	}
	tailGot := got[resetAt+len(reset):]
	if len(tailGot) < tail || len(tailGot) > tail+maxGap {
		t.Fatalf("retained tail len = %d, want within [%d, %d]", len(tailGot), tail, tail+maxGap)
	}
	if !bytes.Equal(got[:headLen], data[:headLen]) {
		t.Fatal("retained head is not a verbatim prefix of the stream")
	}
	if !bytes.Equal(tailGot, data[len(data)-len(tailGot):]) {
		t.Fatal("retained tail is not a contiguous suffix of the stream")
	}
	if !atGround(got[:headLen]) || !atGround(data[:len(data)-len(tailGot)]) {
		t.Fatal("head/tail boundaries are not ground/rune aligned")
	}
}
