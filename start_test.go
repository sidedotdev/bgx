package bgx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/daemon"
	"github.com/sidedotdev/bgx/scrollback"
)

// waitEnded polls for id's persisted ended record, failing the test if the
// session never ends.
func waitEnded(t *testing.T, id string) *daemon.Info {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, ok := endedRecord(id); ok {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %q never persisted an ended record", id)
	return nil
}

// killSession best-effort kills a live session so tests don't leak daemons.
func killSession(t *testing.T, id string) {
	t.Helper()
	if _, err := dialRequest(id, daemon.Request{Op: "kill"}); err != nil {
		t.Logf("kill %q: %v", id, err)
	}
}

func TestStartRunsSessionToCompletion(t *testing.T) {
	id := "startlib/ok"
	info, err := Start(context.Background(), id, []string{"sh", "-c", "echo hi"}, StartOptions{
		Metadata: map[string]string{"kind": "test"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.ID != id || info.Pid <= 0 {
		t.Fatalf("Start info = %+v, want id %q and a live pid", info, id)
	}

	ended := waitEnded(t, id)
	if ended.ExitCode == nil || *ended.ExitCode != 0 {
		t.Fatalf("ended exit code = %v, want 0", ended.ExitCode)
	}
	if ended.Metadata["kind"] != "test" {
		t.Fatalf("ended metadata = %v, want kind=test", ended.Metadata)
	}
}

func TestStartDuplicateEndedRequiresOverwrite(t *testing.T) {
	ctx := context.Background()
	id := "startlib/dup"
	if _, err := Start(ctx, id, []string{"sh", "-c", "echo first"}, StartOptions{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEnded(t, id)

	if _, err := Start(ctx, id, []string{"sh", "-c", "echo second"}, StartOptions{}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate Start err = %v, want ErrSessionExists", err)
	}
	if _, err := Start(ctx, id, []string{"sh", "-c", "echo third"}, StartOptions{OverwriteID: true}); err != nil {
		t.Fatalf("overwrite Start: %v", err)
	}
	waitEnded(t, id)
}

func TestStartRejectsRunningSession(t *testing.T) {
	ctx := context.Background()
	id := "startlib/running"
	if _, err := Start(ctx, id, []string{"sleep", "30"}, StartOptions{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer killSession(t, id)

	if _, err := Start(ctx, id, []string{"sleep", "30"}, StartOptions{}); !errors.Is(err, ErrSessionRunning) {
		t.Fatalf("Start err = %v, want ErrSessionRunning", err)
	}
}

func TestStartEnforcesConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	if _, err := Start(ctx, "startcc/a", []string{"sleep", "30"}, StartOptions{Concurrency: 1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer killSession(t, "startcc/a")

	_, err := Start(ctx, "startcc/b", []string{"sleep", "30"}, StartOptions{Concurrency: 1})
	var climit *ConcurrencyLimitError
	if !errors.As(err, &climit) {
		t.Fatalf("Start err = %v, want ConcurrencyLimitError", err)
	}
	if climit.Namespace != "startcc" || climit.Limit != 1 || len(climit.Active) != 1 {
		t.Fatalf("ConcurrencyLimitError = %+v, want startcc at limit 1 with one active session", climit)
	}
}

func TestStartSurfacesSpawnFailure(t *testing.T) {
	id := "startlib/badexec"
	_, err := Start(context.Background(), id, []string{"/nonexistent/bgx-not-a-binary"}, StartOptions{})
	var startup *StartupError
	if !errors.As(err, &startup) {
		t.Fatalf("Start err = %v, want StartupError", err)
	}
	if startup.ID != id {
		t.Fatalf("StartupError.ID = %q, want %q", startup.ID, id)
	}
}

// TestStartAppliesScrollbackSizes proves the head/tail scrollback options
// reach the re-exec'd daemon: the retained history keeps the head and tail and
// demarcates the discarded middle exactly as the CLI's --head-size/--tail-size
// flags do.
func TestStartAppliesScrollbackSizes(t *testing.T) {
	id := "startsb/trunc"
	const output = "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	_, err := Start(context.Background(), id, []string{"sh", "-c", "printf '" + output + "'"}, StartOptions{
		Scrollback: scrollback.Config{HeadSize: 8, TailSize: 8},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEnded(t, id)

	history, err := os.ReadFile(daemon.HistoryPath(retentionDir(), id))
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	const rule = "────────────────────────────────────────"
	discarded := len(output) - 16
	want := output[:8] +
		"\r\n\r\n" + rule + "\r\n" +
		fmt.Sprintf("[...] truncated %dB", discarded) +
		"\r\n" + rule + "\r\n\r\n" +
		"\x1bc" + output[len(output)-8:]
	if string(history) != want {
		t.Fatalf("history = %q, want %q", history, want)
	}
}

// TestStartAppliesDiskStorage proves the storage kind and path options reach
// the daemon: a disk-backed session creates its private scrollback directory
// under the configured StoragePath while it is running.
func TestStartAppliesDiskStorage(t *testing.T) {
	id := "startsb/disk"
	storageDir := t.TempDir()
	_, err := Start(context.Background(), id, []string{"sleep", "30"}, StartOptions{
		Scrollback: scrollback.Config{Storage: scrollback.StorageDisk, StoragePath: storageDir},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer killSession(t, id)

	entries, err := os.ReadDir(storageDir)
	if err != nil {
		t.Fatalf("read storage dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bgx-scrollback-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("storage dir %q entries = %v, want a bgx-scrollback-* directory", storageDir, entries)
	}
}

// TestStartRetentionCountPrunes proves RetentionCount reaches the daemon: only
// the newest RetentionCount ended records survive within a namespace.
func TestStartRetentionCountPrunes(t *testing.T) {
	ctx := context.Background()
	ordered := []string{"startret/a", "startret/b", "startret/c"}
	for _, id := range ordered {
		if _, err := Start(ctx, id, []string{"sh", "-c", "echo " + id}, StartOptions{RetentionCount: 2}); err != nil {
			t.Fatalf("Start %q: %v", id, err)
		}
		waitEnded(t, id)
		// Space out end times so pruning's newest-first ordering is unambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	// The prune runs as part of the final session's persist; give the removal
	// of the oldest record a bounded window to land.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := endedRecord("startret/a"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record for startret/a should have been pruned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, id := range []string{"startret/b", "startret/c"} {
		if _, ok := endedRecord(id); !ok {
			t.Fatalf("record for %q should be retained", id)
		}
	}
}

// TestStartRacingSameIDAllowsExactlyOne races two Starts for one id; the
// namespace lock must let exactly one spawn a session and reject the other as
// a duplicate rather than letting both re-exec daemons onto the same socket.
func TestStartRacingSameIDAllowsExactlyOne(t *testing.T) {
	id := "startrace/one"
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := Start(context.Background(), id, []string{"sleep", "30"}, StartOptions{})
			results <- err
		}()
	}
	errs := []error{<-results, <-results}
	defer killSession(t, id)

	var oks, dups int
	for _, err := range errs {
		switch {
		case err == nil:
			oks++
		case errors.Is(err, ErrSessionRunning), errors.Is(err, ErrSessionExists):
			dups++
		default:
			t.Fatalf("unexpected Start error: %v", err)
		}
	}
	if oks != 1 || dups != 1 {
		t.Fatalf("racing Starts: %d succeeded, %d duplicate errors (errors: %v), want exactly 1 and 1", oks, dups, errs)
	}
}
