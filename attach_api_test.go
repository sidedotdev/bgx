package bgx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type attachAPITerminal struct {
	input  io.Reader
	output bytes.Buffer
	resize chan struct{}

	mu       sync.Mutex
	entered  bool
	restored bool
}

func newAttachAPITerminal(input []byte) *attachAPITerminal {
	return &attachAPITerminal{
		input:  bytes.NewReader(input),
		resize: make(chan struct{}),
	}
}

func (t *attachAPITerminal) Read(p []byte) (int, error) {
	return t.input.Read(p)
}

func (t *attachAPITerminal) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return t.Read(p)
}

func (t *attachAPITerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.output.Write(p)
}

func (t *attachAPITerminal) Size() (uint16, uint16, error) {
	return 80, 24, nil
}

func (t *attachAPITerminal) ResizeEvents(ctx context.Context) <-chan struct{} {
	events := make(chan struct{})
	go func() {
		defer close(events)
		<-ctx.Done()
	}()
	return events
}

func (t *attachAPITerminal) EnterRaw() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entered = true
	return nil
}

func (t *attachAPITerminal) Restore() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restored = true
	return nil
}

func (t *attachAPITerminal) state() (output string, entered, restored bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.output.String(), t.entered, t.restored
}

func TestAttachAPIUsesLocalSessionAndOptions(t *testing.T) {
	ctx := context.Background()
	id := "attachapi/local"
	if _, err := Run(id, []string{"cat"}, RunSpec{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { killSession(t, id) })

	terminal := newAttachAPITerminal([]byte{0x1C})
	err := Attach(ctx, id, AttachOptions{
		ShowDetachInstructions: true,
		Terminal:               terminal,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	output, entered, restored := terminal.state()
	if !strings.Contains(output, "detach: ctrl+\\") {
		t.Fatalf("terminal output = %q, want detach instructions", output)
	}
	if !entered || !restored {
		t.Fatalf("terminal raw state = entered %v, restored %v; want both true", entered, restored)
	}

	info, err := Info(ctx, id)
	if err != nil {
		t.Fatalf("Info after detach: %v", err)
	}
	if !info.Exists || info.SessionInfo == nil || !info.Running {
		t.Fatalf("Info after detach = %#v, want running session", info)
	}
}

func TestAttachAPIRemoteViaAndSSHOptions(t *testing.T) {
	for _, test := range []struct {
		name     string
		options  func(*testing.T, string) AttachOptions
		wantArgs string
	}{
		{
			name: "via",
			options: func(_ *testing.T, _ string) AttachOptions {
				return AttachOptions{
					Via: []string{
						os.Args[0],
						"-test.run=TestAttachAPITransportHelper",
						"--",
					},
				}
			},
			wantArgs: "bgx bridge remote/session",
		},
		{
			name: "ssh",
			options: func(t *testing.T, dir string) AttachOptions {
				ssh := filepath.Join(dir, "ssh")
				script := "#!/bin/sh\nexec \"$BGX_ATTACH_TEST_BINARY\" -test.run=TestAttachAPITransportHelper -- \"$@\"\n"
				if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
					t.Fatalf("write fake ssh: %v", err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("BGX_ATTACH_TEST_BINARY", os.Args[0])
				return AttachOptions{SSH: "example.test"}
			},
			wantArgs: "example.test bgx bridge remote/session",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			argsPath := filepath.Join(dir, "args")
			t.Setenv("BGX_ATTACH_ARGS_PATH", argsPath)

			options := test.options(t, dir)
			options.Terminal = newAttachAPITerminal([]byte{0x1C})
			if err := Attach(context.Background(), "remote/session", options); err != nil {
				t.Fatalf("Attach: %v", err)
			}

			got, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("read transport arguments: %v", err)
			}
			if strings.TrimSpace(string(got)) != test.wantArgs {
				t.Fatalf("transport arguments = %q, want %q", got, test.wantArgs)
			}
		})
	}
}

func TestAttachAPIRemoteErrorsAreTyped(t *testing.T) {
	for _, test := range []struct {
		code string
		want any
	}{
		{code: codeSessionNotFound, want: &SessionNotFoundError{}},
		{code: codeSessionEnded, want: &SessionEndedError{}},
	} {
		t.Run(test.code, func(t *testing.T) {
			t.Setenv("BGX_ATTACH_ARGS_PATH", filepath.Join(t.TempDir(), "args"))
			t.Setenv("BGX_ATTACH_ERROR_CODE", test.code)

			err := Attach(context.Background(), "remote/unavailable", AttachOptions{
				Via: []string{
					os.Args[0],
					"-test.run=TestAttachAPITransportHelper",
					"--",
				},
				Terminal: newAttachAPITerminal([]byte{0x1C}),
			})
			switch want := test.want.(type) {
			case *SessionNotFoundError:
				if !errors.As(err, &want) {
					t.Fatalf("Attach error = %T %v, want SessionNotFoundError", err, err)
				}
			case *SessionEndedError:
				if !errors.As(err, &want) {
					t.Fatalf("Attach error = %T %v, want SessionEndedError", err, err)
				}
			}
			if !strings.Contains(err.Error(), "remote/unavailable") {
				t.Fatalf("Attach error = %q, want session id", err)
			}
		})
	}
}

func TestAttachAPIRejectsInvalidOptions(t *testing.T) {
	terminal := newAttachAPITerminal([]byte{0x1C})

	for _, test := range []struct {
		name    string
		id      string
		options AttachOptions
		want    string
	}{
		{
			name:    "missing id",
			options: AttachOptions{Terminal: terminal},
			want:    "id is required",
		},
		{
			name: "ssh and via",
			id:   "session",
			options: AttachOptions{
				SSH:      "example.test",
				Via:      []string{"transport"},
				Terminal: terminal,
			},
			want: "mutually exclusive",
		},
		{
			name: "empty via",
			id:   "session",
			options: AttachOptions{
				Via:      []string{},
				Terminal: terminal,
			},
			want: "via requires a command",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Attach(context.Background(), test.id, test.options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Attach error = %v, want mention of %q", err, test.want)
			}
		})
	}
}

func TestAttachAPIReportsMissingAndEndedSessions(t *testing.T) {
	err := Attach(context.Background(), "attachapi/missing", AttachOptions{
		Terminal: newAttachAPITerminal([]byte{0x1C}),
	})
	var notFound *SessionNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Attach missing error = %T %v, want SessionNotFoundError", err, err)
	}
	if notFound.Operation != "attach" || notFound.ID != "attachapi/missing" {
		t.Fatalf("SessionNotFoundError = %#v", notFound)
	}

	ctx := context.Background()
	id := "attachapi/ended"
	if _, err := Run(id, []string{"true"}, RunSpec{}); err != nil {
		t.Fatalf("Run ended session: %v", err)
	}
	waitEnded(t, id)

	err = Attach(ctx, id, AttachOptions{
		Terminal: newAttachAPITerminal([]byte{0x1C}),
	})
	var ended *SessionEndedError
	if !errors.As(err, &ended) {
		t.Fatalf("Attach ended error = %T %v, want SessionEndedError", err, err)
	}
	if ended.Operation != "attach" || ended.ID != id {
		t.Fatalf("SessionEndedError = %#v", ended)
	}
}

func TestAttachAPIRemoteMissingIgnoresLocalEndedRecord(t *testing.T) {
	ctx := context.Background()
	id := "attachapi/shared-id"
	if _, err := Run(id, []string{"true"}, RunSpec{}); err != nil {
		t.Fatalf("Run ended session: %v", err)
	}
	waitEnded(t, id)

	t.Setenv("BGX_ATTACH_ARGS_PATH", filepath.Join(t.TempDir(), "args"))
	t.Setenv("BGX_ATTACH_ERROR_CODE", codeSessionNotFound)

	err := Attach(ctx, id, AttachOptions{
		Via: []string{
			os.Args[0],
			"-test.run=TestAttachAPITransportHelper",
			"--",
		},
		Terminal: newAttachAPITerminal([]byte{0x1C}),
	})
	var notFound *SessionNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Attach error = %T %v, want SessionNotFoundError", err, err)
	}
	if notFound.Operation != "attach" || notFound.ID != id {
		t.Fatalf("SessionNotFoundError = %#v", notFound)
	}
}

func TestAttachAPIReportsTransportStartFailure(t *testing.T) {
	err := Attach(context.Background(), "remote/session", AttachOptions{
		Via:      []string{filepath.Join(t.TempDir(), "missing-transport")},
		Terminal: newAttachAPITerminal([]byte{0x1C}),
	})
	if err == nil || !strings.Contains(err.Error(), "start transport") {
		t.Fatalf("Attach error = %v, want transport start failure", err)
	}
}

func TestAttachAPITransportHelper(t *testing.T) {
	argsPath := os.Getenv("BGX_ATTACH_ARGS_PATH")
	if argsPath == "" {
		return
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if err := os.WriteFile(argsPath, []byte(strings.Join(args, " ")), 0o600); err != nil {
		os.Exit(2)
	}
	if code := os.Getenv("BGX_ATTACH_ERROR_CODE"); code != "" {
		message := "remote session unavailable"
		if _, err := os.Stderr.WriteString(`{"source":"bgx","code":"` + code + `","error":"` + message + `"}` + "\n"); err != nil {
			os.Exit(6)
		}
		os.Exit(1)
	}

	var request [256]byte
	if _, err := os.Stdin.Read(request[:]); err != nil {
		os.Exit(3)
	}
	if _, err := os.Stdout.Write([]byte("{\"ok\":true}\n")); err != nil {
		os.Exit(4)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}
