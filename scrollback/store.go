// Package scrollback retains the output stream of a session while bounding
// memory use: the first headSize bytes are kept verbatim, the last tailSize
// bytes are kept as zstd-compressed chunks, and the middle is discarded.
//
// Writes never block the caller (e.g. a PTY pump). Incoming bytes are appended
// to an in-memory buffer and a background goroutine drains it, performing the
// (CPU-bound) compression off the write path.
package scrollback

import (
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	// DefaultHeadSize is the number of leading bytes kept verbatim.
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

// chunk is a unit of retained tail data, either zstd-compressed or raw.
type chunk struct {
	data       []byte
	rawLen     int
	compressed bool
}

// Store accumulates a byte stream, retaining a verbatim head and a bounded,
// compressed tail. It is safe for concurrent use; Write never blocks on
// compression.
type Store struct {
	headSize      int
	tailSize      int
	chunkSize     int
	fallbackBytes int

	encoder *zstd.Encoder
	decoder *zstd.Decoder

	mu   sync.Mutex
	cond *sync.Cond
	done chan struct{}

	pending    []byte
	head       []byte
	tailChunks []chunk
	tailBuf    []byte
	tailBytes  int
	totalBytes int64
	closed     bool
	busy       bool
}

// NewStore returns a Store retaining headSize leading bytes and tailSize
// trailing bytes. Non-positive sizes fall back to the defaults.
func NewStore(headSize, tailSize int) *Store {
	return newStore(headSize, tailSize, defaultChunkSize, defaultFallbackBytes)
}

// newStore allows tests to use small chunk and fallback thresholds.
func newStore(headSize, tailSize, chunkSize, fallbackBytes int) *Store {
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

// Snapshot returns the retained head followed by the retained tail. It waits
// for any buffered writes to be processed so the result reflects every byte
// written so far.
func (s *Store) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.busy {
		s.cond.Wait()
	}

	// Eviction works at chunk granularity, so the retained chunks may hold more
	// than tailSize bytes. Trim the assembled tail to the exact last tailSize
	// bytes so the result is independent of the compression chunk size.
	tail := make([]byte, 0, s.tailBytes)
	for _, c := range s.tailChunks {
		if c.compressed {
			if d, err := s.decoder.DecodeAll(c.data, nil); err == nil {
				tail = append(tail, d...)
			}
		} else {
			tail = append(tail, c.data...)
		}
	}
	tail = append(tail, s.tailBuf...)
	if len(tail) > s.tailSize {
		tail = tail[len(tail)-s.tailSize:]
	}

	out := make([]byte, 0, len(s.head)+len(tail))
	out = append(out, s.head...)
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
	return nil
}

// drain is the background goroutine that moves pending bytes into the head and
// the compressed tail chunks.
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

// process distributes buf across the head and tail, compressing full chunks.
// It is called with the lock held and may briefly release it while compressing.
func (s *Store) process(buf []byte) {
	if len(s.head) < s.headSize {
		n := s.headSize - len(s.head)
		if n > len(buf) {
			n = len(buf)
		}
		s.head = append(s.head, buf[:n]...)
		buf = buf[n:]
	}

	for len(buf) > 0 {
		n := s.chunkSize - len(s.tailBuf)
		if n > len(buf) {
			n = len(buf)
		}
		s.tailBuf = append(s.tailBuf, buf[:n]...)
		s.tailBytes += n
		buf = buf[n:]
		s.evict()
		if len(s.tailBuf) >= s.chunkSize {
			s.flushChunk(len(buf) + len(s.pending))
		}
	}
}

// flushChunk finalizes the current partial chunk. When the still-unprocessed
// backlog exceeds the fallback threshold the chunk is stored uncompressed so
// the drain goroutine can keep up; otherwise it is zstd-compressed off-lock.
func (s *Store) flushChunk(backlog int) {
	raw := s.tailBuf
	var data []byte
	compressed := false
	if backlog <= s.fallbackBytes {
		s.mu.Unlock()
		data = s.encoder.EncodeAll(raw, nil)
		s.mu.Lock()
		compressed = true
	} else {
		data = raw
	}
	s.tailChunks = append(s.tailChunks, chunk{data: data, rawLen: len(raw), compressed: compressed})
	s.tailBuf = make([]byte, 0, s.chunkSize)
	s.evict()
}

// evict drops the oldest tail chunks while their removal still leaves at least
// tailSize bytes retained, discarding the middle of the stream.
func (s *Store) evict() {
	for len(s.tailChunks) > 1 && s.tailBytes-s.tailChunks[0].rawLen >= s.tailSize {
		s.tailBytes -= s.tailChunks[0].rawLen
		s.tailChunks = s.tailChunks[1:]
	}
}
