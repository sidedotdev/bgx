package daemon

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// globalNamespaceDir is the retention subdirectory holding records for ids that
// contain no "/" and therefore share a single global namespace.
const globalNamespaceDir = "_global"

// DefaultRetentionCount is the number of most recently ended sessions retained
// per id namespace when a session does not configure its own limit.
const DefaultRetentionCount = 10

// Namespace returns the retention namespace for a session id: the portion
// before the first "/". Ids without a "/" all share a single global namespace,
// reported here as the empty string.
func Namespace(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return ""
}

// encodeName escapes an id or namespace into a single safe filename component,
// reversibly mapping "/" and other special bytes.
func encodeName(name string) string {
	return url.QueryEscape(name)
}

// namespaceDir returns the directory under retentionDir that holds records for
// the given id's namespace.
func namespaceDir(retentionDir, id string) string {
	ns := Namespace(id)
	if ns == "" {
		return filepath.Join(retentionDir, globalNamespaceDir)
	}
	return filepath.Join(retentionDir, encodeName(ns))
}

// RecordPath is the persisted JSON record path for a session id.
func RecordPath(retentionDir, id string) string {
	return filepath.Join(namespaceDir(retentionDir, id), encodeName(id)+".json")
}

// HistoryPath is the persisted raw scrollback history path for a session id.
func HistoryPath(retentionDir, id string) string {
	return filepath.Join(namespaceDir(retentionDir, id), encodeName(id)+".history")
}

// writeRecord persists info as JSON and the raw history bytes for an ended
// session, creating the namespace directory as needed.
func writeRecord(retentionDir string, info *Info, history []byte) error {
	dir := namespaceDir(retentionDir, info.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := os.WriteFile(RecordPath(retentionDir, info.ID), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(HistoryPath(retentionDir, info.ID), history, 0o600)
}

// pruneRetention keeps only the newest keep ended-session records (and their
// histories) in id's namespace directory, deleting the oldest beyond that. A
// non-positive keep falls back to DefaultRetentionCount.
func pruneRetention(retentionDir, id string, keep int) error {
	if keep <= 0 {
		keep = DefaultRetentionCount
	}
	dir := namespaceDir(retentionDir, id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type record struct {
		base  string
		ended time.Time
	}
	var records []record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".json")
		records = append(records, record{base: base, ended: recordEndedAt(filepath.Join(dir, e.Name()))})
	}
	if len(records) <= keep {
		return nil
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].ended.After(records[j].ended)
	})
	for _, r := range records[keep:] {
		os.Remove(filepath.Join(dir, r.base+".json"))
		os.Remove(filepath.Join(dir, r.base+".history"))
	}
	return nil
}

// activeNamespaceSessions counts live session sockets sharing id's namespace,
// excluding id itself. Currently-running sessions count toward the per-namespace
// retention budget alongside ended records, so each occupies a retained slot.
// Stale sockets left by crashed daemons have no listener and are not counted.
func activeNamespaceSessions(socketDir, id string) int {
	entries, err := os.ReadDir(socketDir)
	if err != nil {
		return 0
	}
	ns := Namespace(id)
	count := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		other, err := url.QueryUnescape(strings.TrimSuffix(name, ".sock"))
		if err != nil || other == id || Namespace(other) != ns {
			continue
		}
		if socketAlive(filepath.Join(socketDir, name)) {
			count++
		}
	}
	return count
}

// socketAlive reports whether a unix socket has a live listener, distinguishing
// running sessions from stale socket files left behind by crashed daemons.
func socketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// recordEndedAt reads a persisted record's end time, falling back to the file's
// modification time when the record is unreadable or lacks an end time.
func recordEndedAt(path string) time.Time {
	if data, err := os.ReadFile(path); err == nil {
		var info Info
		if json.Unmarshal(data, &info) == nil && info.EndedAt != nil {
			return *info.EndedAt
		}
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
