package scrollback

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskBackendMatchesMemorySnapshot(t *testing.T) {
	const (
		head  = 2000
		tail  = 4000
		chunk = 1000
		total = 20000
	)
	data := pattern(total)

	mem := newStore(head, tail, chunk, 1<<20)
	defer mem.Close()
	if _, err := mem.Write(data); err != nil {
		t.Fatalf("memory write: %v", err)
	}
	memSnap := mem.Snapshot()

	db, err := newDiskBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new disk backend: %v", err)
	}
	disk := newStoreBackend(head, tail, chunk, 1<<20, db)
	defer disk.Close()
	if _, err := disk.Write(data); err != nil {
		t.Fatalf("disk write: %v", err)
	}
	diskSnap := disk.Snapshot()

	if !bytes.Equal(memSnap, diskSnap) {
		t.Fatalf("disk snapshot differs from memory snapshot (%d vs %d bytes)", len(diskSnap), len(memSnap))
	}
	if err := disk.Err(); err != nil {
		t.Fatalf("disk store error: %v", err)
	}
}

func TestDiskBackendPersistsAndEvictsChunks(t *testing.T) {
	const (
		head  = 1000
		tail  = 3000
		chunk = 1000
		total = 20000
	)
	db, err := newDiskBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new disk backend: %v", err)
	}
	s := newStoreBackend(head, tail, chunk, 1<<20, db)
	defer s.Close()

	if _, err := s.Write(pattern(total)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := s.Snapshot(); len(got) != head+tail {
		t.Fatalf("snapshot len = %d, want %d", len(got), head+tail)
	}

	files, err := filepath.Glob(filepath.Join(db.dir, "chunk-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	s.mu.Lock()
	retained := len(s.tailChunks)
	s.mu.Unlock()

	if len(files) != retained {
		t.Fatalf("on-disk chunk files = %d, want %d retained", len(files), retained)
	}

	written := (total - head) / chunk
	if len(files) >= written {
		t.Fatalf("expected eviction to remove chunk files: %d on disk of %d written", len(files), written)
	}
}

func TestDiskBackendCustomPathHonored(t *testing.T) {
	base := filepath.Join(t.TempDir(), "custom", "scrollback")
	db, err := newDiskBackend(base)
	if err != nil {
		t.Fatalf("new disk backend: %v", err)
	}
	defer db.close()

	if filepath.Dir(db.dir) != base {
		t.Fatalf("chunk dir %q not under custom base %q", db.dir, base)
	}

	h, err := db.put([]byte("hello"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if dir := filepath.Dir(h.(string)); dir != db.dir {
		t.Fatalf("chunk file %q not under %q", h, db.dir)
	}
	got, err := db.get(h)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("get = %q, want %q", got, "hello")
	}
}

func TestDiskBackendCleanupOnClose(t *testing.T) {
	db, err := newDiskBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new disk backend: %v", err)
	}
	dir := db.dir

	s := newStoreBackend(100, 1000, 200, 1<<20, db)
	if _, err := s.Write(pattern(5000)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = s.Snapshot()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("chunk dir should exist before close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("chunk dir should be removed after close, stat err = %v", err)
	}
}

func TestNewDiskStorageRoundTrips(t *testing.T) {
	s, err := New(Config{Storage: StorageDisk, StoragePath: t.TempDir()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer s.Close()

	data := []byte("config-driven disk store")
	if _, err := s.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := s.Snapshot(); !bytes.Equal(got, data) {
		t.Fatalf("snapshot = %q, want %q", got, data)
	}
}

func TestNewUnknownStorageKind(t *testing.T) {
	if _, err := New(Config{Storage: StorageKind("weird")}); err == nil {
		t.Fatal("expected error for unknown storage kind")
	}
}
