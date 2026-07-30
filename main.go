package bgx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sidedotdev/bgx/daemon"
	"github.com/sidedotdev/bgx/scrollback"
	cli "github.com/urfave/cli/v3"
)

// version is the bgx release version, overridable at build time via -ldflags.
var version = "0.0.0-dev"

func Main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		emitErrorJSON(errorCode(err), err.Error())
		os.Exit(1)
	}
}

func newApp() *cli.Command {
	root := &cli.Command{
		Name:        "bgx",
		Usage:       "manage async terminal sessions",
		HideVersion: true,
		Commands: []*cli.Command{
			RunCommand(),
			WaitCommand(),
			KillCommand(),
			HistoryCommand(),
			AttachCommand(),
			SendCommand(),
			InfoCommand(),
			ListCommand(),
			VersionCommand(),
			DaemonCommand(),
		},
	}
	applyJSONUsageErrors(root)
	return root
}

// withDirs guards a client action behind base-directory resolution so an
// exhausted fallback chain reports a clear, machine-readable error before the
// command touches the socket or retention directories.
func withDirs(action cli.ActionFunc) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if err := ensureDirs(); err != nil {
			return failJSON(codeFilesystem, "%v", err)
		}
		return action(ctx, cmd)
	}
}

func versionAction(_ context.Context, _ *cli.Command) error {
	out := map[string]any{
		"version":       version,
		"socket_dir":    socketDir(),
		"retention_dir": retentionDir(),
	}
	if notice := fallbackNotice(); notice != "" {
		out["fallback"] = notice
	}
	return printJSON(os.Stdout, out)
}

func notImplemented(_ context.Context, cmd *cli.Command) error {
	return fmt.Errorf("%s: not implemented", cmd.Name)
}

// printJSON writes v as a single line of JSON followed by a newline.
func printJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// DaemonCommand returns the hidden entry point bgx re-execs to run a detached
// session daemon. It is exported so library consumers can assemble the same
// complete command tree as the bgx executable.
func DaemonCommand() *cli.Command {
	return &cli.Command{
		Name:      "__daemon",
		Hidden:    true,
		Usage:     "internal: run a session daemon (not for direct use)",
		ArgsUsage: "<command...>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "id", Required: true},
			&cli.StringFlag{Name: "socket", Required: true},
			&cli.StringFlag{Name: "retention-dir"},
			&cli.IntFlag{Name: "retention"},
			&cli.IntFlag{Name: "head-size"},
			&cli.IntFlag{Name: "tail-size"},
			&cli.StringFlag{Name: "storage"},
			&cli.StringFlag{Name: "storage-path"},
			&cli.StringSliceFlag{Name: "metadata"},
		},
		Action: daemonAction,
	}
}

func daemonAction(_ context.Context, cmd *cli.Command) error {
	command := cmd.Args().Slice()
	if len(command) == 0 {
		return fmt.Errorf("__daemon: no command provided")
	}
	metadata, err := parseMetadata(cmd.StringSlice("metadata"))
	if err != nil {
		return err
	}
	cfg := daemon.Config{
		ID:             cmd.String("id"),
		Command:        command,
		Metadata:       metadata,
		SocketPath:     cmd.String("socket"),
		RetentionDir:   cmd.String("retention-dir"),
		RetentionCount: cmd.Int("retention"),
		Scrollback: scrollback.Config{
			HeadSize:    cmd.Int("head-size"),
			TailSize:    cmd.Int("tail-size"),
			Storage:     scrollback.StorageKind(cmd.String("storage")),
			StoragePath: cmd.String("storage-path"),
		},
	}
	return daemon.Serve(cfg)
}

// parseMetadata converts repeated "key=value" entries into a map.
func parseMetadata(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid metadata %q, expected key=value", e)
		}
		m[k] = v
	}
	return m, nil
}
