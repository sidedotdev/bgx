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
	t.Cleanup(func() {
		if err := mem.Close(); err != nil {
			t.Errorf("close memory store: %v", err)
		}
	})
	if _, err := mem.Write(data); err != nil {
		t.Fatalf("memory write: %v", err)
	}
	memSnap := mem.Snapshot()

	db, err := newDiskBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new disk backend: %v", err)
	}
	disk := newStoreBackend(head, tail, chunk, 1<<20, db)
	t.Cleanup(func() {
		if err := disk.Close(); err != nil {
			t.Errorf("close disk store: %v", err)
		}
	})
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
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close disk store: %v", err)
		}
	})

	if _, err := s.Write(pattern(total)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := s.Snapshot(); len(got) < head+tail || len(got) > head+tail+2*chunk {
		t.Fatalf("snapshot len = %d, want within [%d, %d]", len(got), head+tail, head+tail+2*chunk)
	}

	files, err := filepath.Glob(filepath.Join(db.dir, "chunk-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	s.mu.Lock()
	retained := len(s.headChunks) + len(s.tailChunks)
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
	t.Cleanup(func() {
		if err := db.close(); err != nil {
			t.Errorf("close disk backend: %v", err)
		}
	})

	if filepath.Dir(db.dir) != base {
		t.Fatalf("chunk dir %q not under custom base %q", db.dir, base)
	}

	h, err := db.put([]byte("hello"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	path, ok := h.(string)
	if !ok {
		t.Fatalf("chunk handle has type %T, want string", h)
	}
	if dir := filepath.Dir(path); dir != db.dir {
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
	if snapshot := s.Snapshot(); len(snapshot) == 0 {
		t.Fatal("snapshot is empty after write")
	}

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
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close disk store: %v", err)
		}
	})

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

func FuzzMemoryAndDiskStoresAgree(f *testing.F) {
	f.Add([]byte{}, uint16(1), uint16(1), uint16(1), uint16(1), uint8(1), uint8(1))
	f.Add(pattern(257), uint16(17), uint16(31), uint16(47), uint16(29), uint8(3), uint8(2))
	f.Add(append([]byte("\x1b[31m"), pattern(4096)...), uint16(511), uint16(127), uint16(1023), uint16(257), uint8(7), uint8(3))
	f.Add([]byte("αβγ\x1b[2Jtail"), uint16(2), uint16(5), uint16(7), uint16(3), uint8(2), uint8(4))

	f.Fuzz(func(t *testing.T, data []byte, rawWriteSize, rawHeadSize, rawTailSize, rawChunkSize uint16, rawSnapshotEvery, rawSnapshotRepeats uint8) {
		if len(data) > 16<<10 {
			t.Skip()
		}
		writeSize := int(rawWriteSize%1024) + 1
		headSize := int(rawHeadSize%2048) + 1
		tailSize := int(rawTailSize%4096) + 1
		chunkSize := int(rawChunkSize%1024) + 1
		snapshotEvery := int(rawSnapshotEvery%16) + 1
		snapshotRepeats := int(rawSnapshotRepeats%4) + 1

		diskBase := t.TempDir()
		diskBackend, err := newDiskBackend(diskBase)
		if err != nil {
			t.Fatalf("new disk backend: %v", err)
		}
		diskDir := diskBackend.dir
		memoryStore := newStoreBackend(headSize, tailSize, chunkSize, 1<<20, memoryBackend{})
		diskStore := newStoreBackend(headSize, tailSize, chunkSize, 1<<20, diskBackend)

		for offset, writeIndex := 0, 0; offset < len(data); writeIndex++ {
			end := offset + writeSize
			if end > len(data) {
				end = len(data)
			}
			memoryN, memoryErr := memoryStore.Write(data[offset:end])
			diskN, diskErr := diskStore.Write(data[offset:end])
			if memoryN != diskN || memoryN != end-offset {
				t.Fatalf("write counts differ: memory=%d disk=%d want=%d", memoryN, diskN, end-offset)
			}
			if (memoryErr == nil) != (diskErr == nil) {
				t.Fatalf("write errors differ: memory=%v disk=%v", memoryErr, diskErr)
			}
			if memoryErr != nil && memoryErr.Error() != diskErr.Error() {
				t.Fatalf("write errors differ: memory=%v disk=%v", memoryErr, diskErr)
			}
			offset = end

			if writeIndex%snapshotEvery == 0 {
				if memory, disk := memoryStore.Snapshot(), diskStore.Snapshot(); !bytes.Equal(memory, disk) {
					t.Fatalf("intermediate snapshots differ after %d bytes: memory=%d disk=%d", offset, len(memory), len(disk))
				}
			}
		}

		for i := 0; i < snapshotRepeats; i++ {
			memory, disk := memoryStore.Snapshot(), diskStore.Snapshot()
			if !bytes.Equal(memory, disk) {
				t.Fatalf("snapshot %d differs: memory=%d disk=%d", i, len(memory), len(disk))
			}
			if memoryStore.TotalBytes() != diskStore.TotalBytes() || memoryStore.TotalBytes() != int64(len(data)) {
				t.Fatalf("byte counts differ: memory=%d disk=%d want=%d",
					memoryStore.TotalBytes(), diskStore.TotalBytes(), len(data))
			}
			if (memoryStore.Err() == nil) != (diskStore.Err() == nil) {
				t.Fatalf("store errors differ: memory=%v disk=%v", memoryStore.Err(), diskStore.Err())
			}
		}

		memoryCloseErr := memoryStore.Close()
		diskCloseErr := diskStore.Close()
		if (memoryCloseErr == nil) != (diskCloseErr == nil) {
			t.Fatalf("close errors differ: memory=%v disk=%v", memoryCloseErr, diskCloseErr)
		}
		if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
			t.Fatalf("disk backend directory remains after close: %v", err)
		}
	})
}
