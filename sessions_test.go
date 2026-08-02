package bgx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sidedotdev/bgx/daemon"
)

// findSession returns the entry with the given id, if present.
func findSession(sessions []*Info, id string) (*Info, bool) {
	for _, s := range sessions {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

func TestListSessionsMergesRunningAndEndedWithFilters(t *testing.T) {
	runningID := "listlib/running"
	endedID := "listlib/ended"

	if _, err := Start(context.Background(), runningID, []string{"sleep", "30"}, StartOptions{
		Metadata: map[string]string{"tag": "keep"},
	}); err != nil {
		t.Fatalf("Start running: %v", err)
	}
	t.Cleanup(func() { killSession(t, runningID) })

	if _, err := Start(context.Background(), endedID, []string{"sh", "-c", "exit 0"}, StartOptions{
		Metadata: map[string]string{"tag": "drop"},
	}); err != nil {
		t.Fatalf("Start ended: %v", err)
	}
	waitEnded(t, endedID)

	all := ListSessions(ListOptions{})
	if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].ID < all[j].ID }) {
		t.Fatalf("ListSessions is not sorted by id: %+v", all)
	}
	run, ok := findSession(all, runningID)
	if !ok || !run.Running {
		t.Fatalf("ListSessions running entry = %+v, ok=%v; want running", run, ok)
	}
	end, ok := findSession(all, endedID)
	if !ok || end.Running {
		t.Fatalf("ListSessions ended entry = %+v, ok=%v; want ended", end, ok)
	}

	filtered := ListSessions(ListOptions{Metadata: map[string]string{"tag": "keep"}})
	if _, ok := findSession(filtered, runningID); !ok {
		t.Fatalf("filtered ListSessions missing %q: %+v", runningID, filtered)
	}
	if _, ok := findSession(filtered, endedID); ok {
		t.Fatalf("filtered ListSessions should exclude %q: %+v", endedID, filtered)
	}

	running := ListRunning(ListOptions{})
	if _, ok := findSession(running, runningID); !ok {
		t.Fatalf("ListRunning missing %q: %+v", runningID, running)
	}
	if _, ok := findSession(running, endedID); ok {
		t.Fatalf("ListRunning should exclude ended session %q", endedID)
	}

	ended := ListEnded(ListOptions{Metadata: map[string]string{"tag": "drop"}})
	if _, ok := findSession(ended, endedID); !ok {
		t.Fatalf("ListEnded missing %q: %+v", endedID, ended)
	}
	if _, ok := findSession(ended, runningID); ok {
		t.Fatalf("ListEnded should exclude running session %q", runningID)
	}
}

func TestEndedRecordLookup(t *testing.T) {
	id := "listlib/record"
	if _, err := Start(context.Background(), id, []string{"sh", "-c", "exit 3"}, StartOptions{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEnded(t, id)

	info, ok := EndedRecord(id)
	if !ok {
		t.Fatalf("EndedRecord(%q) not found", id)
	}
	if info.ExitCode == nil || *info.ExitCode != 3 {
		t.Fatalf("EndedRecord exit code = %v, want 3", info.ExitCode)
	}
	if info.Running {
		t.Fatal("EndedRecord reports the session as running")
	}
	if _, ok := EndedRecord("listlib/absent"); ok {
		t.Fatal("EndedRecord found a record for an absent session")
	}
}

// sortedByID reports whether sessions are in ascending id order.
func sortedByID(sessions []*Info) bool {
	return sort.SliceIsSorted(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
}

func TestListSessionsLiveSessionShadowsEndedRecord(t *testing.T) {
	id := "listmerge/shadow"
	if _, err := Start(context.Background(), id, []string{"sleep", "30"}, StartOptions{
		Metadata: map[string]string{"origin": "live"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { killSession(t, id) })

	// Inject a stale retained record under the live session's id, as a crashed
	// overwrite could leave behind, so the merge preference is observable.
	recordPath := daemon.RecordPath(retentionDir(), id)
	stale, err := json.Marshal(&Info{ID: id, Metadata: map[string]string{"origin": "stale"}})
	if err != nil {
		t.Fatalf("marshal stale record: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
		t.Fatalf("mkdir record dir: %v", err)
	}
	if err := os.WriteFile(recordPath, stale, 0o644); err != nil {
		t.Fatalf("write stale record: %v", err)
	}
	t.Cleanup(func() { os.Remove(recordPath) })
	if _, ok := EndedRecord(id); !ok {
		t.Fatalf("injected record for %q is not readable", id)
	}

	var matches []*Info
	for _, s := range ListSessions(ListOptions{}) {
		if s.ID == id {
			matches = append(matches, s)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("ListSessions returned %d entries for %q, want exactly 1: %+v", len(matches), id, matches)
	}
	if !matches[0].Running || matches[0].Metadata["origin"] != "live" {
		t.Fatalf("ListSessions entry = %+v, want the live record to win over the ended one", matches[0])
	}
}

func TestListRunningAndListEndedSortByID(t *testing.T) {
	// Reverse-lexical creation order in each source proves sorting is done by
	// the listing rather than inherited from creation or directory order.
	runningIDs := []string{"listorder/run-b", "listorder/run-a"}
	for _, id := range runningIDs {
		if _, err := Start(context.Background(), id, []string{"sleep", "30"}, StartOptions{}); err != nil {
			t.Fatalf("Start %q: %v", id, err)
		}
		t.Cleanup(func() { killSession(t, id) })
	}
	endedIDs := []string{"listorder/end-b", "listorder/end-a"}
	for _, id := range endedIDs {
		if _, err := Start(context.Background(), id, []string{"sh", "-c", "exit 0"}, StartOptions{}); err != nil {
			t.Fatalf("Start %q: %v", id, err)
		}
		waitEnded(t, id)
	}

	running := ListRunning(ListOptions{})
	for _, id := range runningIDs {
		if _, ok := findSession(running, id); !ok {
			t.Fatalf("ListRunning missing %q: %+v", id, running)
		}
	}
	if !sortedByID(running) {
		t.Fatalf("ListRunning is not sorted by id: %+v", running)
	}

	ended := ListEnded(ListOptions{})
	for _, id := range endedIDs {
		if _, ok := findSession(ended, id); !ok {
			t.Fatalf("ListEnded missing %q: %+v", id, ended)
		}
	}
	if !sortedByID(ended) {
		t.Fatalf("ListEnded is not sorted by id: %+v", ended)
	}
}
