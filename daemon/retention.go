package daemon

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// globalNamespaceDir is the retention subdirectory holding records for ids that
// contain no "/" and therefore share a single global namespace.
const globalNamespaceDir = "_global"

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
