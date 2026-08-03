package bgx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/sidedotdev/bgx/daemon"
)

// InfoResult reports whether a session exists and includes its metadata when it
// does.
type InfoResult struct {
	Exists bool `json:"exists"`
	*SessionInfo
}

// SendResult describes a successful send operation.
type SendResult struct {
	ID   string `json:"id"`
	Sent bool   `json:"sent"`
}

// VersionInfo describes the bgx release and its resolved storage locations.
type VersionInfo struct {
	Version      string `json:"version"`
	SocketDir    string `json:"socket_dir"`
	RetentionDir string `json:"retention_dir"`
	Fallback     string `json:"fallback,omitempty"`
}

// SessionNotFoundError reports that an operation could not find a live or
// retained session with the requested id.
type SessionNotFoundError struct {
	Operation string
	ID        string
	Err       error
}

func (e *SessionNotFoundError) Error() string {
	return fmt.Sprintf("%s: session %q not found", e.Operation, e.ID)
}

func (e *SessionNotFoundError) Unwrap() error {
	return e.Err
}

// SessionEndedError reports that an operation requires a running session but
// the requested session has already ended.
type SessionEndedError struct {
	Operation string
	ID        string
	Err       error
}

func (e *SessionEndedError) Error() string {
	return fmt.Sprintf("%s: session %q has already ended", e.Operation, e.ID)
}

func (e *SessionEndedError) Unwrap() error {
	return e.Err
}

// Info returns metadata for a live or retained session. A missing session is
// represented by Exists being false.
func Info(ctx context.Context, id string) (*InfoResult, error) {
	info, err := sessionClient(id).Info(ctx)
	if err == nil {
		return &InfoResult{Exists: true, SessionInfo: info}, nil
	}
	if !isSessionUnavailable(err) {
		return nil, err
	}
	if info, ok := EndedRecord(id); ok {
		return &InfoResult{Exists: true, SessionInfo: info}, nil
	}
	return &InfoResult{}, nil
}

// Wait waits for a live session to exit or returns the exit result retained for
// an already-ended session.
func Wait(ctx context.Context, id string) (*ExitResult, error) {
	result, err := sessionClient(id).Wait(ctx)
	if err == nil {
		return result, nil
	}
	if !isSessionUnavailable(err) {
		return nil, err
	}
	if info, ok := endedExitResult(id); ok {
		return info, nil
	}
	if _, statErr := os.Stat(socketPath(id)); statErr == nil {
		info, waitErr := waitForEndedExitResult(ctx, id, 2*time.Second)
		if waitErr != nil {
			return nil, waitErr
		}
		if info != nil {
			return info, nil
		}
	}
	return nil, sessionNotFound("wait", id, err)
}

// Kill terminates a live session and waits for it to exit. An already-ended
// session is returned unchanged.
func Kill(ctx context.Context, id string) (*InfoResult, error) {
	result, err := sessionClient(id).Kill(ctx)
	if err == nil {
		return &InfoResult{Exists: true, SessionInfo: result.Info}, nil
	}
	if !isSessionUnavailable(err) {
		return nil, err
	}
	if info, ok := EndedRecord(id); ok {
		return &InfoResult{Exists: true, SessionInfo: info}, nil
	}
	return nil, sessionNotFound("kill", id, err)
}

// Send writes input to a live session's PTY.
func Send(ctx context.Context, id string, input []byte) (*SendResult, error) {
	if err := sessionClient(id).Send(ctx, input); err != nil {
		if isSessionUnavailable(err) {
			return nil, sessionNotFound("send", id, err)
		}
		return nil, err
	}
	return &SendResult{ID: id, Sent: true}, nil
}

// History returns the scrollback of a live or retained session.
func History(ctx context.Context, id string) ([]byte, error) {
	history, err := sessionClient(id).History(ctx)
	if err == nil {
		return history, nil
	}
	if !isSessionUnavailable(err) {
		return nil, err
	}
	if _, ok := EndedRecord(id); ok {
		history, readErr := os.ReadFile(daemon.HistoryPath(retentionDir(), id))
		if readErr != nil {
			return nil, fmt.Errorf("history: read retained session %q: %w", id, readErr)
		}
		return history, nil
	}
	return nil, sessionNotFound("history", id, err)
}

// List returns all known sessions matching opts.
func List(opts ListOptions) []*SessionInfo {
	return ListSessions(opts)
}

// Version returns the release version and resolved storage locations.
func Version() VersionInfo {
	return VersionInfo{
		Version:      version,
		SocketDir:    socketDir(),
		RetentionDir: retentionDir(),
		Fallback:     fallbackNotice(),
	}
}

func endedExitResult(id string) (*ExitResult, bool) {
	info, ok := EndedRecord(id)
	if !ok || info.ExitCode == nil {
		return nil, false
	}
	return &ExitResult{Info: info, ExitCode: *info.ExitCode}, true
}

func waitForEndedExitResult(ctx context.Context, id string, timeout time.Duration) (*ExitResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if result, ok := endedExitResult(id); ok {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, nil
		case <-ticker.C:
		}
	}
}

func isSessionUnavailable(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

func sessionNotFound(operation, id string, cause error) error {
	return &SessionNotFoundError{Operation: operation, ID: id, Err: cause}
}
