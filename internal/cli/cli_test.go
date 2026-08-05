package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/sidedotdev/bgx"
)

func stubOperations() operations {
	return operations{
		ensureDirs: func() error {
			return nil
		},
		run: func(string, []string, bgx.RunSpec) (*bgx.SessionInfo, error) {
			return &bgx.SessionInfo{}, nil
		},
		info: func(context.Context, string) (*bgx.InfoResult, error) {
			return &bgx.InfoResult{}, nil
		},
		wait: func(context.Context, string) (*bgx.ExitResult, error) {
			return &bgx.ExitResult{}, nil
		},
		kill: func(context.Context, string) (*bgx.InfoResult, error) {
			return &bgx.InfoResult{}, nil
		},
		send: func(_ context.Context, id string, _ []byte) (*bgx.SendResult, error) {
			return &bgx.SendResult{ID: id, Sent: true}, nil
		},
		history: func(context.Context, string) ([]byte, error) {
			return nil, nil
		},
		attach: func(context.Context, string, bgx.AttachOptions) error {
			return nil
		},
		bridge: func(context.Context, string) error {
			return nil
		},
		list: func(bgx.ListOptions) []*bgx.SessionInfo {
			return nil
		},
		version: func() bgx.VersionInfo {
			return bgx.VersionInfo{
				Version:      "test",
				SocketDir:    "/tmp/bgx/run",
				RetentionDir: "/tmp/bgx/ended",
			}
		},
	}
}

func TestRootCommandsHaveLibraryOperations(t *testing.T) {
	r := newRunner(&bytes.Buffer{}, &bytes.Buffer{}, defaultOperations())
	root := r.rootCommand()
	if root == nil {
		t.Fatal("rootCommand returned nil")
	}
	if root.Name != "bgx" {
		t.Fatalf("root command name = %q, want %q", root.Name, "bgx")
	}

	commandNames := make([]string, 0, len(root.Commands))
	for _, command := range root.Commands {
		commandNames = append(commandNames, command.Name)
	}
	wantCommands := []string{"run", "wait", "kill", "history", "attach", "bridge", "send", "info", "list", "version"}
	if !reflect.DeepEqual(commandNames, wantCommands) {
		t.Fatalf("CLI commands = %v, want %v", commandNames, wantCommands)
	}

	wantAliases := map[string][]string{"list": {"ls"}}
	for _, command := range root.Commands {
		if command.Hidden {
			t.Errorf("CLI command %q is hidden", command.Name)
		}
		if command.Action == nil {
			t.Errorf("CLI command %q has a nil action", command.Name)
		}
		if !reflect.DeepEqual(command.Aliases, wantAliases[command.Name]) {
			t.Errorf("CLI command %q aliases = %v, want %v", command.Name, command.Aliases, wantAliases[command.Name])
		}
	}

	libraryOperations := map[string]any{
		"run":     (func(string, []string, bgx.RunSpec) (*bgx.SessionInfo, error))(bgx.Run),
		"wait":    (func(context.Context, string) (*bgx.ExitResult, error))(bgx.Wait),
		"kill":    (func(context.Context, string) (*bgx.InfoResult, error))(bgx.Kill),
		"history": (func(context.Context, string) ([]byte, error))(bgx.History),
		"attach":  (func(context.Context, string, bgx.AttachOptions) error)(bgx.Attach),
		"bridge":  (func(context.Context, string, io.ReadWriteCloser) error)(bgx.Bridge),
		"send":    (func(context.Context, string, []byte) (*bgx.SendResult, error))(bgx.Send),
		"info":    (func(context.Context, string) (*bgx.InfoResult, error))(bgx.Info),
		"list":    (func(bgx.ListOptions) []*bgx.SessionInfo)(bgx.List),
		"version": (func() bgx.VersionInfo)(bgx.Version),
	}

	operationType := reflect.TypeOf(r.ops)
	operationValue := reflect.ValueOf(r.ops)
	for _, command := range root.Commands {
		if libraryOperations[command.Name] == nil {
			t.Errorf("CLI command %q has no typed root-package library operation", command.Name)
		}

		field, ok := operationType.FieldByName(command.Name)
		if !ok || command.Name == "ensureDirs" {
			t.Errorf("CLI command %q has no same-named typed operation adapter", command.Name)
			continue
		}
		if operationValue.FieldByIndex(field.Index).IsNil() {
			t.Errorf("CLI command %q has a nil typed operation adapter", command.Name)
		}
	}
}

func TestRootCommandFlagsHaveMirroredOptionFields(t *testing.T) {
	root := newRunner(&bytes.Buffer{}, &bytes.Buffer{}, defaultOperations()).rootCommand()
	optionTypes := map[string]reflect.Type{
		"run":    reflect.TypeOf(bgx.RunSpec{}),
		"attach": reflect.TypeOf(bgx.AttachOptions{}),
		"list":   reflect.TypeOf(bgx.ListOptions{}),
	}

	for _, command := range root.Commands {
		if len(command.Flags) == 0 {
			continue
		}
		optionType, ok := optionTypes[command.Name]
		if !ok {
			t.Errorf("flagged CLI command %q has no associated library option type", command.Name)
			continue
		}

		fields := make(map[string]bool, optionType.NumField())
		for i := 0; i < optionType.NumField(); i++ {
			field := optionType.Field(i)
			if field.IsExported() {
				fields[cliOptionName(field.Name)] = true
			}
		}
		for _, flag := range command.Flags {
			for _, name := range flag.Names() {
				if !fields[name] {
					t.Errorf("CLI flag --%s on %q has no field on %s", name, command.Name, optionType)
				}
			}
		}
	}
}

func TestRunAdapterMirrorsFlagsAndOutput(t *testing.T) {
	var gotID string
	var gotCommand []string
	var gotSpec bgx.RunSpec
	ops := stubOperations()
	ops.run = func(id string, command []string, spec bgx.RunSpec) (*bgx.SessionInfo, error) {
		gotID = id
		gotCommand = append([]string(nil), command...)
		gotSpec = spec
		return &bgx.SessionInfo{ID: id, Pid: 42, StartedAt: time.Unix(123, 0)}, nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(context.Background(), []string{
		"bgx", "run",
		"--overwrite-id",
		"--metadata", "env=test",
		"--head-size", "10",
		"--tail-size", "20",
		"--storage", "memory",
		"--storage-path", "/tmp/history",
		"--retention", "7",
		"--concurrency", "5",
		"session",
		"printf", "%s", "hello",
	})
	if err != nil {
		t.Fatalf("run returned error: %v; stderr=%q", err, stderr.String())
	}

	if gotID != "session" {
		t.Fatalf("id = %q, want session", gotID)
	}
	if want := []string{"printf", "%s", "hello"}; !reflect.DeepEqual(gotCommand, want) {
		t.Fatalf("command = %v, want %v", gotCommand, want)
	}
	wantSpec := bgx.RunSpec{
		OverwriteID: true,
		Metadata:    map[string]string{"env": "test"},
		HeadSize:    10,
		TailSize:    20,
		Storage:     "memory",
		StoragePath: "/tmp/history",
		Retention:   7,
		Concurrency: 5,
	}
	if !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Fatalf("spec = %#v, want %#v", gotSpec, wantSpec)
	}

	output := decodeObject(t, stdout.Bytes())
	if output["id"] != "session" || output["pid"] != float64(42) {
		t.Fatalf("run output = %v", output)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestAdaptersPreserveJSONAndRawOutput(t *testing.T) {
	ops := stubOperations()
	ops.info = func(_ context.Context, id string) (*bgx.InfoResult, error) {
		if id == "missing" {
			return &bgx.InfoResult{}, nil
		}
		return &bgx.InfoResult{
			Exists:      true,
			SessionInfo: &bgx.SessionInfo{ID: id, Pid: 11},
		}, nil
	}
	ops.history = func(_ context.Context, id string) ([]byte, error) {
		return []byte("raw\x00history:" + id), nil
	}
	ops.list = func(opts bgx.ListOptions) []*bgx.SessionInfo {
		if opts.Metadata["env"] != "test" {
			t.Fatalf("list metadata = %v, want env=test", opts.Metadata)
		}
		return []*bgx.SessionInfo{{ID: "listed"}}
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing info includes id", args: []string{"bgx", "info", "missing"}, want: `{"exists":false,"id":"missing"}` + "\n"},
		{name: "history remains raw", args: []string{"bgx", "history", "live"}, want: "raw\x00history:live"},
		{name: "list alias", args: []string{"bgx", "ls", "--metadata", "env=test"}, want: `[{"id":"listed","running":false,"pid":0,"command":null,"started_at":"0001-01-01T00:00:00Z","duration_ms":0,"output_bytes":0}]` + "\n"},
		{name: "version", args: []string{"bgx", "version"}, want: `{"version":"test","socket_dir":"/tmp/bgx/run","retention_dir":"/tmp/bgx/ended"}` + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := newRunner(&stdout, &stderr, ops).run(context.Background(), test.args)
			if err != nil {
				t.Fatalf("run returned error: %v; stderr=%q", err, stderr.String())
			}
			if stdout.String() != test.want {
				t.Fatalf("stdout = %q, want %q", stdout.String(), test.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestSendJoinsArgumentsWithoutNewline(t *testing.T) {
	ops := stubOperations()
	var got []byte
	ops.send = func(_ context.Context, id string, input []byte) (*bgx.SendResult, error) {
		if id != "session" {
			t.Fatalf("id = %q, want session", id)
		}
		got = append([]byte(nil), input...)
		return &bgx.SendResult{ID: id, Sent: true}, nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(
		context.Background(),
		[]string{"bgx", "send", "session", "hello", "world"},
	)
	if err != nil {
		t.Fatalf("run returned error: %v; stderr=%q", err, stderr.String())
	}
	if string(got) != "hello world" {
		t.Fatalf("input = %q, want %q", got, "hello world")
	}
	if stdout.String() != `{"id":"session","sent":true}`+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestWaitEmitsJSONAndReturnsRequestedExitStatus(t *testing.T) {
	ops := stubOperations()
	ops.wait = func(context.Context, string) (*bgx.ExitResult, error) {
		return &bgx.ExitResult{ExitCode: 3}, nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(
		context.Background(),
		[]string{"bgx", "wait", "session"},
	)
	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("error = %v, want ExitError", err)
	}
	if exit.Code != 3 {
		t.Fatalf("exit code = %d, want 3", exit.Code)
	}
	if stdout.String() != `{"exit_code":3,"id":"session"}`+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestAdapterErrorsAreSingleJSONObjectsOnStderr(t *testing.T) {
	ops := stubOperations()
	ops.wait = func(context.Context, string) (*bgx.ExitResult, error) {
		return nil, &bgx.SessionNotFoundError{Operation: "wait", ID: "ghost"}
	}

	tests := []struct {
		name     string
		args     []string
		wantCode string
	}{
		{name: "missing argument", args: []string{"bgx", "info"}, wantCode: codeInvalidArgument},
		{name: "invalid metadata", args: []string{"bgx", "list", "--metadata", "invalid"}, wantCode: codeInvalidArgument},
		{name: "missing session", args: []string{"bgx", "wait", "ghost"}, wantCode: codeNotFound},
		{name: "usage error", args: []string{"bgx", "kill", "--bogus", "id"}, wantCode: codeInvalidArgument},
		{name: "unknown command", args: []string{"bgx", "unknown"}, wantCode: codeInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := newRunner(&stdout, &stderr, ops).run(context.Background(), test.args)
			if err == nil {
				t.Fatal("run returned nil error")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			payload := decodeObject(t, stderr.Bytes())
			if payload["code"] != test.wantCode {
				t.Fatalf("code = %v, want %q", payload["code"], test.wantCode)
			}
			if payload["source"] != errorSource {
				t.Fatalf("source = %v, want %q", payload["source"], errorSource)
			}
			if _, ok := payload["error"].(string); !ok {
				t.Fatalf("error field = %T, want string", payload["error"])
			}
		})
	}
}

func TestDirectoryResolutionFailureEmitsFilesystemError(t *testing.T) {
	ops := stubOperations()
	resolveErr := errors.New("all directory candidates failed")
	ops.ensureDirs = func() error {
		return resolveErr
	}
	called := false
	ops.info = func(context.Context, string) (*bgx.InfoResult, error) {
		called = true
		return &bgx.InfoResult{}, nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(
		context.Background(),
		[]string{"bgx", "info", "session"},
	)
	if err == nil {
		t.Fatal("run returned nil error")
	}
	if called {
		t.Fatal("info operation ran after directory resolution failed")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	payload := decodeObject(t, stderr.Bytes())
	if payload["code"] != codeFilesystem {
		t.Fatalf("code = %v, want %q", payload["code"], codeFilesystem)
	}
	if payload["source"] != errorSource {
		t.Fatalf("source = %v, want %q", payload["source"], errorSource)
	}
	if payload["error"] != resolveErr.Error() {
		t.Fatalf("error = %v, want %q", payload["error"], resolveErr)
	}
}

func decodeObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return value
}
func TestAttachAdapterMirrorsOptions(t *testing.T) {
	ops := stubOperations()
	var gotID string
	var gotOpts bgx.AttachOptions
	ops.attach = func(_ context.Context, id string, opts bgx.AttachOptions) error {
		gotID = id
		gotOpts = opts
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(context.Background(), []string{
		"bgx",
		"attach",
		"--ssh",
		"host",
		"session",
		"--show-detach-instructions",
	})
	if err != nil {
		t.Fatalf("run attach: %v, stderr=%q", err, stderr.String())
	}
	if gotID != "session" {
		t.Fatalf("attach id = %q, want session", gotID)
	}
	if !gotOpts.ShowDetachInstructions {
		t.Fatal("ShowDetachInstructions = false, want true")
	}
	if gotOpts.SSH != "host" {
		t.Fatalf("SSH = %q, want host", gotOpts.SSH)
	}
	if gotOpts.Via != nil {
		t.Fatalf("Via = %v, want nil", gotOpts.Via)
	}
	if gotOpts.Terminal != nil {
		t.Fatalf("Terminal = %T, want nil", gotOpts.Terminal)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestAttachAdapterPassesTrailingViaCommand(t *testing.T) {
	ops := stubOperations()
	var gotOpts bgx.AttachOptions
	ops.attach = func(_ context.Context, _ string, opts bgx.AttachOptions) error {
		gotOpts = opts
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(context.Background(), []string{
		"bgx",
		"attach",
		"session",
		"--via",
		"transport",
		"--transport-option",
	})
	if err != nil {
		t.Fatalf("run attach: %v, stderr=%q", err, stderr.String())
	}
	want := []string{"transport", "--transport-option"}
	if !reflect.DeepEqual(gotOpts.Via, want) {
		t.Fatalf("Via = %v, want %v", gotOpts.Via, want)
	}
}

func TestAttachAdapterMapsTypedErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "invalid options",
			err:      &bgx.AttachOptionsError{Err: errors.New("bad options")},
			wantCode: codeInvalidArgument,
		},
		{
			name:     "missing session",
			err:      &bgx.SessionNotFoundError{Operation: "attach", ID: "session"},
			wantCode: codeSessionNotFound,
		},
		{
			name:     "ended session",
			err:      &bgx.SessionEndedError{Operation: "attach", ID: "session"},
			wantCode: codeSessionEnded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := stubOperations()
			ops.attach = func(context.Context, string, bgx.AttachOptions) error {
				return test.err
			}

			var stdout, stderr bytes.Buffer
			err := newRunner(&stdout, &stderr, ops).run(
				context.Background(),
				[]string{"bgx", "attach", "session"},
			)
			if err == nil {
				t.Fatal("run attach returned nil error")
			}
			payload := decodeObject(t, stderr.Bytes())
			if payload["code"] != test.wantCode {
				t.Fatalf("code = %v, want %q", payload["code"], test.wantCode)
			}
		})
	}
}

func TestBridgeAdapterChecksSessionAndDelegates(t *testing.T) {
	ops := stubOperations()
	ops.info = func(_ context.Context, id string) (*bgx.InfoResult, error) {
		return &bgx.InfoResult{
			Exists: true,
			SessionInfo: &bgx.SessionInfo{
				ID:      id,
				Running: true,
			},
		}, nil
	}
	var gotID string
	ops.bridge = func(_ context.Context, id string) error {
		gotID = id
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := newRunner(&stdout, &stderr, ops).run(
		context.Background(),
		[]string{"bgx", "bridge", "session"},
	)
	if err != nil {
		t.Fatalf("run bridge: %v, stderr=%q", err, stderr.String())
	}
	if gotID != "session" {
		t.Fatalf("bridge id = %q, want session", gotID)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestBridgeAdapterReportsUnavailableSessions(t *testing.T) {
	tests := []struct {
		name     string
		result   *bgx.InfoResult
		wantCode string
	}{
		{
			name:     "missing",
			result:   &bgx.InfoResult{Exists: false},
			wantCode: codeSessionNotFound,
		},
		{
			name: "ended",
			result: &bgx.InfoResult{
				Exists: true,
				SessionInfo: &bgx.SessionInfo{
					ID:      "session",
					Running: false,
				},
			},
			wantCode: codeSessionEnded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := stubOperations()
			ops.info = func(context.Context, string) (*bgx.InfoResult, error) {
				return test.result, nil
			}
			ops.bridge = func(context.Context, string) error {
				t.Fatal("bridge called for unavailable session")
				return nil
			}

			var stdout, stderr bytes.Buffer
			err := newRunner(&stdout, &stderr, ops).run(
				context.Background(),
				[]string{"bgx", "bridge", "session"},
			)
			if err == nil {
				t.Fatal("run bridge returned nil error")
			}
			payload := decodeObject(t, stderr.Bytes())
			if payload["code"] != test.wantCode {
				t.Fatalf("code = %v, want %q", payload["code"], test.wantCode)
			}
		})
	}
}
func cliOptionName(fieldName string) string {
	var name strings.Builder
	runes := []rune(fieldName)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 &&
			(unicode.IsLower(runes[i-1]) ||
				(i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]))) {
			name.WriteByte('-')
		}
		name.WriteRune(unicode.ToLower(r))
	}
	return name.String()
}

type failingJSONWriter struct {
	err error
}

func (w failingJSONWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestRootCommandReturnsIndependentInstances(t *testing.T) {
	r := newRunner(&bytes.Buffer{}, &bytes.Buffer{}, stubOperations())
	if r.rootCommand() == r.rootCommand() {
		t.Fatal("rootCommand returned the same mutable command instance twice")
	}
}

func TestEmitErrorJSONReturnsWriteError(t *testing.T) {
	writeErr := errors.New("write stderr")
	err := emitErrorJSON(failingJSONWriter{err: writeErr}, codeInternal, "failed", nil)
	if !errors.Is(err, writeErr) {
		t.Fatalf("emitErrorJSON error = %v, want write error", err)
	}
}

func TestRunPropagatesErrorJSONWriteFailure(t *testing.T) {
	writeErr := errors.New("write stderr")
	err := newRunner(io.Discard, failingJSONWriter{err: writeErr}, stubOperations()).run(
		context.Background(),
		[]string{"bgx", "--definitely-not-a-flag"},
	)
	if !errors.Is(err, writeErr) {
		t.Fatalf("run error = %v, want JSON write error", err)
	}
}

func TestUnknownCommandReturnsCodedError(t *testing.T) {
	var stderr bytes.Buffer
	err := newRunner(io.Discard, &stderr, stubOperations()).run(
		context.Background(),
		[]string{"bgx", "bogus-command"},
	)
	if err == nil {
		t.Fatal("run returned nil for unknown command")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run error = %q, want unknown-command message", err)
	}

	var coded *codedError
	if !errors.As(err, &coded) {
		t.Fatalf("run error = %T %v, want codedError", err, err)
	}
	if coded.code != codeInvalidArgument {
		t.Fatalf("error code = %q, want %q", coded.code, codeInvalidArgument)
	}

	payload := decodeObject(t, stderr.Bytes())
	if payload["code"] != codeInvalidArgument {
		t.Fatalf("JSON error code = %v, want %q", payload["code"], codeInvalidArgument)
	}
}
