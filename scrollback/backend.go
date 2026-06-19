package scrollback

import (
	"fmt"
	"os"
	"path/filepath"
)

// StorageKind selects where a Store keeps its retained tail chunks.
type StorageKind string

const (
	StorageMemory StorageKind = "memory"
	StorageDisk   StorageKind = "disk"
)

// Config configures a Store's retention sizes and chunk storage. The zero value
// uses the default head/tail sizes and in-memory storage.
type Config struct {
	HeadSize int
	TailSize int

	// CompressionBacklogSize bounds the unprocessed write backlog: once the
	// buffered-but-uncompressed bytes exceed it, chunks are stored uncompressed
	// so the drain goroutine keeps up under high-throughput writes. Non-positive
	// uses the package default.
	CompressionBacklogSize int

	Storage     StorageKind
	StoragePath string // disk only; empty selects an auto-created tmp directory
}

// New returns a Store configured by cfg. Non-positive sizes fall back to the
// package defaults. It errors only when the disk backend cannot be initialized
// or the storage kind is unknown.
func New(cfg Config) (*Store, error) {
	var b backend
	switch cfg.Storage {
	case "", StorageMemory:
		b = memoryBackend{}
	case StorageDisk:
		db, err := newDiskBackend(cfg.StoragePath)
		if err != nil {
			return nil, err
		}
		b = db
	default:
		return nil, fmt.Errorf("scrollback: unknown storage kind %q", cfg.Storage)
	}
	return newStoreBackend(cfg.HeadSize, cfg.TailSize, defaultChunkSize, cfg.CompressionBacklogSize, b), nil
}

// chunkHandle is an opaque reference a backend returns for a stored chunk.
type chunkHandle any

// backend persists chunk payloads (zstd-compressed or raw tail data). The Store
// owns the lifecycle: every handle returned by put is eventually read via get or
// released via drop, and close is called exactly once when the Store closes.
type backend interface {
	put(data []byte) (chunkHandle, error)
	get(h chunkHandle) ([]byte, error)
	drop(h chunkHandle) error
	close() error
}

// memoryBackend keeps chunk payloads in memory; it is the default backend.
type memoryBackend struct{}

func (memoryBackend) put(data []byte) (chunkHandle, error) { return data, nil }

func (memoryBackend) get(h chunkHandle) ([]byte, error) { return h.([]byte), nil }

func (memoryBackend) drop(chunkHandle) error { return nil }

func (memoryBackend) close() error { return nil }

// diskBackend writes each chunk payload to its own file within a private
// subdirectory created under a custom base path or the OS tmp directory.
type diskBackend struct {
	dir string
	seq int
}

func newDiskBackend(path string) (*diskBackend, error) {
	base := path
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, "bgx-scrollback-*")
	if err != nil {
		return nil, err
	}
	return &diskBackend{dir: dir}, nil
}

func (b *diskBackend) put(data []byte) (chunkHandle, error) {
	name := filepath.Join(b.dir, fmt.Sprintf("chunk-%d", b.seq))
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return nil, err
	}
	b.seq++
	return name, nil
}

func (b *diskBackend) get(h chunkHandle) ([]byte, error) {
	return os.ReadFile(h.(string))
}

func (b *diskBackend) drop(h chunkHandle) error {
	return os.Remove(h.(string))
}

func (b *diskBackend) close() error {
	return os.RemoveAll(b.dir)
}
