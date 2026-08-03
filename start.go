package bgx

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sidedotdev/bgx/daemon"
	"github.com/sidedotdev/bgx/scrollback"
)

// StartOptions configures a typed session start. The zero value matches the
// CLI run defaults.
type StartOptions struct {
	// Metadata tags the session with arbitrary key/value pairs surfaced by
	// info/list and usable as list filters.
	Metadata map[string]string
	// OverwriteID replaces an ended session's retained record and history when
	// the id is already taken; without it such a start fails with
	// ErrSessionExists.
	OverwriteID bool
	// Concurrency caps live sessions in the id's namespace; non-positive uses
	// the default cap.
	Concurrency int
	// RetentionCount bounds retained ended records per namespace; non-positive
	// uses the daemon default.
	RetentionCount int
	// Scrollback configures the session's retained output.
	Scrollback scrollback.Config
}

// ErrSessionRunning reports a start against an id whose session is live.
var ErrSessionRunning = errors.New("session is already running")

// ErrSessionExists reports a start against an id with a retained ended session
// when StartOptions.OverwriteID is not set.
var ErrSessionExists = errors.New("session already exists")

// ConcurrencyLimitError reports that a namespace is already at its
// active-session limit, including every offending session so callers can act
// on the listing.
type ConcurrencyLimitError struct {
	Namespace string
	Limit     int
	Active    []*Info
}

func (e *ConcurrencyLimitError) Error() string {
	label := fmt.Sprintf("namespace %q", e.Namespace)
	if e.Namespace == "" {
		label = "the global namespace"
	}
	return fmt.Sprintf("%s already has %d active session(s); concurrency limit is %d",
		label, len(e.Active), e.Limit)
}

// StartupError reports that a session's daemon or command failed while
// starting, wrapping the underlying cause (which prefers the daemon's own
// stderr when available).
type StartupError struct {
	ID  string
	Err error
}

func (e *StartupError) Error() string { return e.Err.Error() }
func (e *StartupError) Unwrap() error { return e.Err }

// Start launches command in a new detached session identified by id and
// returns the live session's metadata once it is reachable. The daemon is the
// current executable re-exec'd with a private marker, so host binaries must
// call InterceptDaemon first in main(). ctx bounds only the readiness wait;
// the session itself outlives the caller.
func Start(ctx context.Context, id string, command []string, opts StartOptions) (info *Info, retErr error) {
	if id == "" {
		return nil, errors.New("id must not be empty")
	}
	if len(command) == 0 {
		return nil, errors.New("a command is required")
	}
	if err := ensureDirs(); err != nil {
		return nil, err
	}
	if len(socketPath(id)) > maxSocketPathLen {
		return nil, fmt.Errorf("socket path for id %q exceeds %d bytes", id, maxSocketPathLen)
	}

	limit := opts.Concurrency
	if limit <= 0 {
		limit = defaultConcurrency
	}
	ns := daemon.Namespace(id)

	// Serialize the duplicate-id checks, concurrency check, and spawn per
	// namespace so simultaneous starts can't both observe a free id or room
	// under the cap and race past. The lock is held until the new session's
	// socket is live and therefore countable.
	unlock, err := lockNamespace(ns)
	if err != nil {
		return nil, err
	}
	defer func() {
		retErr = errors.Join(retErr, unlock())
	}()

	if _, ok := liveInfo(id); ok {
		return nil, fmt.Errorf("session %q: %w", id, ErrSessionRunning)
	}
	if _, ok := endedRecord(id); ok {
		if !opts.OverwriteID {
			return nil, fmt.Errorf("session %q: %w", id, ErrSessionExists)
		}
		// A half-replaced session must not start: a leftover record or history
		// would misreport the new session's past.
		for _, path := range []string{daemon.RecordPath(retentionDir(), id), daemon.HistoryPath(retentionDir(), id)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("replace session %q: %w", id, err)
			}
		}
	}

	if active := runningInNamespace(ns); len(active) >= limit {
		return nil, &ConcurrencyLimitError{Namespace: ns, Limit: limit, Active: active}
	}

	dc, stderrPath, err := spawnDaemon(daemon.Config{
		ID:             id,
		Command:        command,
		Metadata:       opts.Metadata,
		SocketPath:     socketPath(id),
		RetentionDir:   retentionDir(),
		RetentionCount: opts.RetentionCount,
		Scrollback:     opts.Scrollback,
	})
	if err != nil {
		return nil, &StartupError{ID: id, Err: err}
	}
	defer func() {
		retErr = joinDaemonFileCleanupError(retErr, stderrPath, os.Remove)
	}()

	info, err = waitForSession(ctx, id, dc, stderrPath, socketReadyTimeout)
	if err != nil {
		return nil, &StartupError{ID: id, Err: err}
	}
	if info.Error != "" {
		return nil, &StartupError{ID: id, Err: fmt.Errorf("session %q failed to start: %s", id, info.Error)}
	}
	return info, nil
}
func joinDaemonFileCleanupError(current error, path string, remove func(string) error) error {
	err := remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return current
	}
	return errors.Join(current, fmt.Errorf("remove daemon stderr %q: %w", path, err))
}
