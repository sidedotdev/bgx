package main

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

func main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newApp() *cli.Command {
	// run stops parsing flags after the session id so the command that follows
	// keeps its own flags (e.g. "sh -c").
	stopAfterID := 1
	return &cli.Command{
		Name:  "bgx",
		Usage: "manage async terminal sessions",
		// A custom JSON version subcommand replaces the built-in plain version flag.
		HideVersion: true,
		Commands: []*cli.Command{
			{
				Name:         "run",
				Usage:        "run a command async in a new session",
				ArgsUsage:    "[--overwrite-id] [--metadata key=value...] <id> <command...>",
				StopOnNthArg: &stopAfterID,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "overwrite-id"},
					&cli.StringSliceFlag{Name: "metadata"},
					&cli.IntFlag{Name: "head-size"},
					&cli.IntFlag{Name: "tail-size"},
					&cli.StringFlag{Name: "storage"},
					&cli.StringFlag{Name: "storage-path"},
					&cli.IntFlag{Name: "retention", Sources: cli.EnvVars("BGX_RETENTION")},
					&cli.IntFlag{Name: "concurrency", Value: defaultConcurrency, Sources: cli.EnvVars("BGX_CONCURRENCY")},
				},
				Action: withDirs(runAction),
			},
			{
				Name:      "wait",
				Usage:     "wait for a session to finish and return its exit code",
				ArgsUsage: "<id>",
				Action:    withDirs(waitAction),
			},
			{
				Name:      "kill",
				Usage:     "kill a running session",
				ArgsUsage: "<id>",
				Action:    withDirs(killAction),
			},
			{
				Name:      "history",
				Usage:     "print the scrollback history of a session",
				ArgsUsage: "<id>",
				Action:    withDirs(historyAction),
			},
			{
				Name:      "attach",
				Usage:     "attach to a running session",
				ArgsUsage: "<id>",
				Action:    withDirs(attachAction),
			},
			{
				Name:      "send",
				Usage:     "send raw input to a session PTY without attaching",
				ArgsUsage: "<id> <text...>",
				Action:    withDirs(sendAction),
			},
			{
				Name:      "info",
				Usage:     "print metadata about a session",
				ArgsUsage: "<id>",
				Action:    withDirs(infoAction),
			},
			{
				Name:    "list",
				Aliases: []string{"ls"},
				Usage:   "list sessions",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "metadata"},
				},
				Action: withDirs(listAction),
			},
			{
				Name:   "version",
				Usage:  "print version and environment info",
				Action: withDirs(versionAction),
			},
			daemonCommand(),
		},
	}
}

// withDirs guards a client action behind base-directory resolution so an
// exhausted fallback chain reports a clear, machine-readable error before the
// command touches the socket or retention directories.
func withDirs(action cli.ActionFunc) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if err := ensureDirs(); err != nil {
			return failJSON("%v", err)
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

// daemonCommand is the hidden entry point bgx re-execs to run a detached
// session daemon. It is not meant for direct use.
func daemonCommand() *cli.Command {
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
