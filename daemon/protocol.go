package daemon

import "time"

// Request is a single JSON-line command sent by a client over the session
// socket. One request is served per connection.
type Request struct {
	Op string `json:"op"`
	// Input carries raw PTY bytes for the "send" op, base64-encoded on the wire
	// so arbitrary (non-UTF-8) byte sequences survive JSON transport.
	Input []byte `json:"input,omitempty"`
}

// Response is the JSON-line reply to a Request.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Info  *Info  `json:"info,omitempty"`
	// History carries the raw head+tail scrollback bytes for the "history" op,
	// base64-encoded on the wire so arbitrary bytes survive JSON transport.
	History  []byte `json:"history,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Info is the metadata snapshot for a session. The same shape is persisted to
// the retention directory when a session ends so ended sessions report
// identical fields.
type Info struct {
	ID          string            `json:"id"`
	Running     bool              `json:"running"`
	Pid         int               `json:"pid"`
	Command     []string          `json:"command"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"`
	DurationMS  int64             `json:"duration_ms"`
	OutputBytes int64             `json:"output_bytes"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	Killed      bool              `json:"killed,omitempty"`
}
