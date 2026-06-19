package e2e

import (
	"encoding/json"
	"testing"
	"time"
)

// decodeJSONArray parses a command's stdout as a JSON array of objects.
func decodeJSONArray(t *testing.T, s string) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		t.Fatalf("invalid JSON array %q: %v", s, err)
	}
	return arr
}

func sessionByID(sessions []map[string]any, id string) (map[string]any, bool) {
	for _, m := range sessions {
		if m["id"] == id {
			return m, true
		}
	}
	return nil, false
}

func TestListShowsRunningAndEndedSessions(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "list-running", "sleep", "30"); res.exitCode != 0 {
		t.Fatalf("run running exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	t.Cleanup(func() { bgxIn(t, dir, "kill", "list-running") })

	if res := bgxIn(t, dir, "run", "list-ended", "echo", "done"); res.exitCode != 0 {
		t.Fatalf("run ended exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "list-ended")

	res := bgxIn(t, dir, "list")
	if res.exitCode != 0 {
		t.Fatalf("list exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	sessions := decodeJSONArray(t, res.stdout)

	running, ok := sessionByID(sessions, "list-running")
	if !ok {
		t.Fatalf("list missing running session; got %v", sessions)
	}
	if running["running"] != true {
		t.Fatalf("running session running=%v, want true", running["running"])
	}
	ended, ok := sessionByID(sessions, "list-ended")
	if !ok {
		t.Fatalf("list missing ended session; got %v", sessions)
	}
	if ended["running"] != false {
		t.Fatalf("ended session running=%v, want false", ended["running"])
	}
}

func TestListMetadataFilterNarrowsResults(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "--metadata", "team=infra", "filt-a", "echo", "a"); res.exitCode != 0 {
		t.Fatalf("run filt-a exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	if res := bgxIn(t, dir, "run", "--metadata", "team=web", "filt-b", "echo", "b"); res.exitCode != 0 {
		t.Fatalf("run filt-b exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "filt-a")
	waitEnded(t, dir, "filt-b")

	res := bgxIn(t, dir, "list", "--metadata", "team=infra")
	if res.exitCode != 0 {
		t.Fatalf("list exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	sessions := decodeJSONArray(t, res.stdout)
	if len(sessions) != 1 {
		t.Fatalf("filtered list len = %d, want 1; got %v", len(sessions), sessions)
	}
	if sessions[0]["id"] != "filt-a" {
		t.Fatalf("filtered list id = %v, want filt-a", sessions[0]["id"])
	}
}

func TestLsAliasListsSessions(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "ls-alias", "echo", "x"); res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "ls-alias")

	res := bgxIn(t, dir, "ls")
	if res.exitCode != 0 {
		t.Fatalf("ls exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	sessions := decodeJSONArray(t, res.stdout)
	if _, ok := sessionByID(sessions, "ls-alias"); !ok {
		t.Fatalf("ls missing session; got %v", sessions)
	}
}

// TestRetentionGlobalBucketSharedAcrossSlashlessIDs proves that ids without a
// "/" share a single retention bucket (so pruning collapses across them) and
// that the limit can be configured via the BGX_RETENTION environment variable.
func TestRetentionGlobalBucketSharedAcrossSlashlessIDs(t *testing.T) {
	dir := runDir(t)
	t.Setenv("BGX_RETENTION", "2")

	for _, id := range []string{"g1", "g2", "g3", "g4"} {
		if res := bgxIn(t, dir, "run", id, "echo", id); res.exitCode != 0 {
			t.Fatalf("run %q exit code = %d, stderr=%q", id, res.exitCode, res.stderr)
		}
		waitEnded(t, dir, id)
		// Space out end times so pruning's newest-first ordering is unambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	// A namespaced id lives in its own bucket and must be unaffected.
	if res := bgxIn(t, dir, "run", "ns/keep", "echo", "k"); res.exitCode != 0 {
		t.Fatalf("run ns/keep exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "ns/keep")

	sessions := decodeJSONArray(t, bgxIn(t, dir, "ls").stdout)
	for _, id := range []string{"g1", "g2"} {
		if _, ok := sessionByID(sessions, id); ok {
			t.Fatalf("global id %q should have been pruned; got %v", id, sessions)
		}
	}
	for _, id := range []string{"g3", "g4", "ns/keep"} {
		if _, ok := sessionByID(sessions, id); !ok {
			t.Fatalf("id %q should be retained; got %v", id, sessions)
		}
	}
}

func TestRetentionPrunesOldestBeyondLimit(t *testing.T) {
	dir := runDir(t)

	ordered := []string{"proj/s1", "proj/s2", "proj/s3", "proj/s4"}
	for _, id := range ordered {
		if res := bgxIn(t, dir, "run", "--retention", "2", id, "echo", id); res.exitCode != 0 {
			t.Fatalf("run %q exit code = %d, stderr=%q", id, res.exitCode, res.stderr)
		}
		waitEnded(t, dir, id)
		// Space out end times so pruning's newest-first ordering is unambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	if res := bgxIn(t, dir, "run", "--retention", "2", "global-keep", "echo", "g"); res.exitCode != 0 {
		t.Fatalf("run global exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "global-keep")

	sessions := decodeJSONArray(t, bgxIn(t, dir, "list").stdout)

	for _, id := range []string{"proj/s1", "proj/s2"} {
		if _, ok := sessionByID(sessions, id); ok {
			t.Fatalf("session %q should have been pruned; got %v", id, sessions)
		}
	}
	for _, id := range []string{"proj/s3", "proj/s4", "global-keep"} {
		if _, ok := sessionByID(sessions, id); !ok {
			t.Fatalf("session %q should be retained; got %v", id, sessions)
		}
	}
}

// TestRetentionCountsActiveSessions proves that a currently-running session
// occupies one of its namespace's retention slots, so fewer ended records are
// kept than the configured limit while it stays alive.
func TestRetentionCountsActiveSessions(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "--retention", "2", "rp/live", "sleep", "30"); res.exitCode != 0 {
		t.Fatalf("run rp/live exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	t.Cleanup(func() { bgxIn(t, dir, "kill", "rp/live") })

	// With one slot held by the live session, a limit of 2 leaves room for only
	// a single ended record, so the older of two finished sessions is pruned.
	for _, id := range []string{"rp/e1", "rp/e2"} {
		if res := bgxIn(t, dir, "run", "--retention", "2", id, "echo", id); res.exitCode != 0 {
			t.Fatalf("run %q exit code = %d, stderr=%q", id, res.exitCode, res.stderr)
		}
		waitEnded(t, dir, id)
		// Space out end times so pruning's newest-first ordering is unambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	if m := decodeJSON(t, bgxIn(t, dir, "info", "rp/e1").stdout); m["exists"] != false {
		t.Fatalf("rp/e1 should have been pruned by the active session; got %v", m)
	}
	if m := decodeJSON(t, bgxIn(t, dir, "info", "rp/e2").stdout); m["exists"] != true {
		t.Fatalf("rp/e2 should be retained; got %v", m)
	}
}
