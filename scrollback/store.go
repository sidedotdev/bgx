// Package scrollback retains the output stream of a session while bounding
// memory use: the first headSize bytes are kept verbatim, the last tailSize
// bytes are kept as zstd-compressed chunks, and the middle is discarded.
//
// Writes never block the caller (e.g. a PTY pump). Incoming bytes are appended
// to an in-memory buffer and a background goroutine drains it, performing the
// (CPU-bound) compression off the write path.
package scrollback

import (
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/sidedotdev/bgx/vtscan"
)

const (
	// DefaultHeadSize is the number of leading bytes retained.
	DefaultHeadSize = 1 << 20 // 1MB
	// DefaultTailSize is the number of trailing bytes retained.
	DefaultTailSize = 9 << 20 // 9MB

	// defaultChunkSize is the granularity at which tail bytes are compressed
	// and evicted.
	defaultChunkSize = 1 << 20 // 1MB
	// defaultFallbackBytes is the backlog threshold beyond which chunks are
	// stored uncompressed to keep up with high-throughput writes.
	defaultFallbackBytes = 10 << 20 // 10MB
)

// chunk is a unit of retained tail data, either zstd-compressed or raw. Its
// payload lives in the Store's backend, referenced by handle.
type chunk struct {
	handle     chunkHandle
	rawLen     int
	compressed bool
}

// Store accumulates a byte stream, retaining a compressed head and a bounded,
// compressed tail. It is safe for concurrent use; Write never blocks on
// compression.
type Store struct {
	headSize      int
	tailSize      int
	chunkSize     int
	fallbackBytes int

	backend backend

	encoder *zstd.Encoder
	decoder *zstd.Decoder

	mu   sync.Mutex
	cond *sync.Cond
	done chan struct{}

	pending    []byte
	chunkBuf   []byte
	headChunks []chunk
	headBytes  int
	tailChunks []chunk
	tailBytes  int
	totalBytes int64
	closed     bool
	busy       bool
	err        error
}

// NewStore returns a Store retaining headSize leading bytes and tailSize
// trailing bytes. Non-positive sizes fall back to the defaults.
func NewStore(headSize, tailSize int) *Store {
	return newStore(headSize, tailSize, defaultChunkSize, defaultFallbackBytes)
}

// newStore allows tests to use small chunk and fallback thresholds.
func newStore(headSize, tailSize, chunkSize, fallbackBytes int) *Store {
	return newStoreBackend(headSize, tailSize, chunkSize, fallbackBytes, memoryBackend{})
}

// newStoreBackend builds a Store backed by b, letting tests use small chunk and
// fallback thresholds together with a specific chunk storage backend.
func newStoreBackend(headSize, tailSize, chunkSize, fallbackBytes int, b backend) *Store {
	if headSize <= 0 {
		headSize = DefaultHeadSize
	}
	if tailSize <= 0 {
		tailSize = DefaultTailSize
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	if fallbackBytes <= 0 {
		fallbackBytes = defaultFallbackBytes
	}

	enc, _ := zstd.NewWriter(nil)
	dec, _ := zstd.NewReader(nil)

	s := &Store{
		headSize:      headSize,
		tailSize:      tailSize,
		chunkSize:     chunkSize,
		fallbackBytes: fallbackBytes,
		backend:       b,
		encoder:       enc,
		decoder:       dec,
		done:          make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.drain()
	return s
}

// Write appends p to the stream. It records all bytes for the total accounting
// and returns immediately, leaving compression to the background goroutine.
func (s *Store) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return len(p), nil
	}
	s.pending = append(s.pending, p...)
	s.totalBytes += int64(len(p))
	s.busy = true
	s.cond.Broadcast()
	s.mu.Unlock()
	return len(p), nil
}

// TotalBytes returns the number of bytes ever written, including the discarded
// middle of the stream.
func (s *Store) TotalBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

// Err returns the first error the background goroutine or Snapshot encountered
// while persisting or reading chunks, if any.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Snapshot returns the retained head followed by the retained tail. It waits
// for any buffered writes to be processed so the result reflects every byte
// written so far.
func (s *Store) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.busy {
		s.cond.Wait()
	}

	head := s.decodeChunks(s.headChunks)

	// Eviction works at chunk granularity, so the retained chunks may hold more
	// than tailSize bytes. Trim the assembled tail to roughly the last tailSize
	// bytes, snapping the front cut to a ground/rune boundary so the retained
	// tail never begins with a partial escape sequence or split rune.
	tail := s.decodeChunks(s.tailChunks)
	tail = append(tail, s.chunkBuf...)
	if len(tail) > s.tailSize {
		tail = tail[boundaryTrim(tail, len(tail)-s.tailSize):]
	}

	// Anything written but not retained in either the head or the (trimmed) tail
	// is the discarded middle; surface its size so a history dump reads honestly.
	discarded := s.totalBytes - int64(len(head)) - int64(len(tail))

	out := make([]byte, 0, len(head)+len(tail))
	out = append(out, head...)
	if discarded > 0 {
		out = appendTruncation(out, discarded)
	}
	out = append(out, tail...)
	return out
}

// Close stops the background goroutine and releases the zstd resources. After
// Close, Snapshot must not be called.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()

	<-s.done
	s.encoder.Close()
	s.decoder.Close()
	s.backend.close()
	return nil
}

// drain is the background goroutine that moves pending bytes into the
// compressed head and tail chunks.
func (s *Store) drain() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for len(s.pending) == 0 && !s.closed {
			s.busy = false
			s.cond.Broadcast()
			s.cond.Wait()
		}
		if len(s.pending) == 0 && s.closed {
			s.busy = false
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
		buf := s.pending
		s.pending = nil
		s.process(buf)
		s.mu.Unlock()
	}
}

// process accumulates buf into the current chunk, flushing each full chunk to
// the compressed head or tail. While the head is still filling, the chunk is
// capped at the headSize boundary so the head is sealed (and retained) even
// when it is smaller than a full chunk. It is called with the lock held and may
// briefly release it while compressing.
func (s *Store) process(buf []byte) {
	s.chunkBuf = append(s.chunkBuf, buf...)
	for {
		target := s.chunkSize
		if r := s.headSize - s.headBytes; r > 0 && r < target {
			target = r
		}
		if len(s.chunkBuf) < target {
			return
		}
		cut := s.boundaryCut(target)
		if cut <= 0 {
			// No ground/rune boundary is available yet; keep accumulating
			// rather than splitting an escape sequence or a multi-byte rune.
			return
		}
		backlog := len(s.chunkBuf) - cut + len(s.pending)
		s.flushChunk(cut, backlog)
	}
}

// boundaryCut returns an offset into chunkBuf at which the VT parser is back at
// the ground state and on a UTF-8 rune boundary, chosen near target so chunk
// sizes stay approximate without ever splitting an escape sequence or a rune.
// chunkBuf always begins at such a boundary (every flush ends on one), so a
// fresh scanner reflects its starting state. It prefers the largest boundary
// not exceeding target; when none exists below target (e.g. target lands inside
// a sequence) it takes the next boundary just beyond target so progress is
// still made. It returns -1 when no boundary exists yet, leaving chunkBuf to
// accumulate the unterminated sequence.
func (s *Store) boundaryCut(target int) int {
	var sc vtscan.Scanner
	best := -1
	for off := 1; off <= len(s.chunkBuf); off++ {
		sc.Advance(s.chunkBuf[off-1 : off])
		if off < len(s.chunkBuf) && !utf8.RuneStart(s.chunkBuf[off]) {
			continue
		}
		if !sc.AtGround() {
			continue
		}
		if off <= target {
			best = off
			continue
		}
		if best > 0 {
			return best
		}
		return off
	}
	return best
}

// flushChunk compresses and stores chunkBuf[:cut], then routes it to the head
// until headSize is retained and to the tail thereafter. cut is a ground/rune
// boundary so the stored chunk never splits an escape sequence or rune; the
// trailing bytes are retained for the next chunk. When the still-unprocessed
// backlog exceeds the fallback threshold the chunk is stored uncompressed so
// the drain goroutine can keep up; otherwise it is zstd-compressed. Both the
// compression and the backend write happen with the lock released so neither
// blocks concurrent writers.
func (s *Store) flushChunk(cut, backlog int) {
	consumed := len(s.chunkBuf)
	raw := s.chunkBuf[:cut]
	// rest must be a fresh allocation: the memory backend retains raw's backing
	// array for uncompressed chunks, so it must not be reused for chunkBuf.
	rest := append(make([]byte, 0, s.chunkSize), s.chunkBuf[cut:consumed]...)
	compressed := backlog <= s.fallbackBytes

	s.mu.Unlock()
	data := raw
	if compressed {
		data = s.encoder.EncodeAll(raw, nil)
	}
	h, err := s.backend.put(data)
	s.mu.Lock()

	// Preserve any bytes appended to chunkBuf while the lock was released during
	// compression/backend put rather than dropping them with the stale slice.
	if len(s.chunkBuf) > consumed {
		rest = append(rest, s.chunkBuf[consumed:]...)
	}
	s.chunkBuf = rest
	if err != nil {
		s.err = err
		return
	}
	c := chunk{handle: h, rawLen: cut, compressed: compressed}
	if s.headBytes < s.headSize {
		s.headChunks = append(s.headChunks, c)
		s.headBytes += c.rawLen
		return
	}
	s.tailChunks = append(s.tailChunks, c)
	s.tailBytes += c.rawLen
	s.evict()
}

// evict drops the oldest tail chunks while their removal still leaves at least
// tailSize bytes retained, discarding the middle of the stream.
func (s *Store) evict() {
	for len(s.tailChunks) > 1 && s.tailBytes-s.tailChunks[0].rawLen >= s.tailSize {
		s.tailBytes -= s.tailChunks[0].rawLen
		if err := s.backend.drop(s.tailChunks[0].handle); err != nil {
			s.err = err
		}
		s.tailChunks = s.tailChunks[1:]
	}
}

// decodeChunks returns the concatenated raw bytes of chunks, decompressing any
// stored compressed. Backend read errors are recorded and the chunk skipped.
func (s *Store) decodeChunks(chunks []chunk) []byte {
	var out []byte
	for _, c := range chunks {
		data, err := s.backend.get(c.handle)
		if err != nil {
			s.err = err
			continue
		}
		if c.compressed {
			if d, derr := s.decoder.DecodeAll(data, nil); derr == nil {
				out = append(out, d...)
			}
		} else {
			out = append(out, data...)
		}
	}
	return out
}

// boundaryTrim returns an offset near want at which the VT parser is at the
// ground state and on a UTF-8 rune boundary, so trimming buf[:offset] never
// leaves a partial escape sequence or split rune at the head of the retained
// slice. buf always begins at a ground/rune boundary (every retained chunk is
// cut on one), so a fresh scanner reflects its starting state. It prefers the
// largest boundary not exceeding want, falling back to the next beyond want.
func boundaryTrim(buf []byte, want int) int {
	var sc vtscan.Scanner
	if off := sc.SafeCut(buf, want); off >= 0 {
		return off
	}
	if off := sc.SafeCut(buf, len(buf)); off >= 0 {
		return off
	}
	return 0
}

// resetPreamble is a full terminal reset (RIS). It is emitted immediately
// before the retained tail so the tail renders on a clean state rather than
// inheriting whatever mode the discarded middle left the terminal in.
const resetPreamble = "\x1bc"

// truncationRule is the marker line that brackets the truncation notice in a
// history dump.
const truncationRule = "────────────────────────────────────────"

// appendTruncation writes the demarcation for a discarded middle of n bytes: an
// empty line, a marker line, the truncation notice, a marker line and an empty
// line, followed by a terminal reset so the tail that comes next renders
// cleanly.
func appendTruncation(dst []byte, n int64) []byte {
	// Lead with two breaks so the empty line is genuinely blank even when the
	// retained head does not end on a newline: the first break ends the head's
	// final line, the second leaves a blank line before the marker.
	dst = append(dst, "\r\n\r\n"...)
	dst = append(dst, truncationRule...)
	dst = append(dst, "\r\n[...] truncated "...)
	dst = append(dst, humanizeBytes(n)...)
	dst = append(dst, "\r\n"...)
	dst = append(dst, truncationRule...)
	dst = append(dst, "\r\n\r\n"...)
	dst = append(dst, resetPreamble...)
	return dst
}

// humanizeBytes formats n as a compact, human-readable size such as "1.5MB",
// using binary (1024-based) units.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
