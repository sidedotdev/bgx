package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
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
	return &cli.Command{
		Name:  "bgx",
		Usage: "manage async terminal sessions",
		// A custom JSON version subcommand replaces the built-in plain version flag.
		HideVersion: true,
		Commands: []*cli.Command{
			{
				Name:      "run",
				Usage:     "run a command async in a new session",
				ArgsUsage: "<id> [--overwrite-id] [--metadata key=value...] <command...>",
				Action:    notImplemented,
			},
			{
				Name:      "wait",
				Usage:     "wait for a session to finish and return its exit code",
				ArgsUsage: "<id>",
				Action:    notImplemented,
			},
			{
				Name:      "kill",
				Usage:     "kill a running session",
				ArgsUsage: "<id>",
				Action:    notImplemented,
			},
			{
				Name:      "history",
				Usage:     "print the scrollback history of a session",
				ArgsUsage: "<id>",
				Action:    notImplemented,
			},
			{
				Name:      "attach",
				Usage:     "attach to a running session",
				ArgsUsage: "<id>",
				Action:    notImplemented,
			},
			{
				Name:      "send",
				Usage:     "send raw input to a session PTY without attaching",
				ArgsUsage: "<id> <text...>",
				Action:    notImplemented,
			},
			{
				Name:      "info",
				Usage:     "print metadata about a session",
				ArgsUsage: "<id>",
				Action:    notImplemented,
			},
			{
				Name:    "list",
				Aliases: []string{"ls"},
				Usage:   "list sessions",
				Action:  notImplemented,
			},
			{
				Name:   "version",
				Usage:  "print version and environment info",
				Action: versionAction,
			},
		},
	}
}

func versionAction(_ context.Context, _ *cli.Command) error {
	return printJSON(os.Stdout, map[string]any{
		"version":       version,
		"socket_dir":    socketDir(),
		"retention_dir": retentionDir(),
	})
}

func notImplemented(_ context.Context, cmd *cli.Command) error {
	return fmt.Errorf("%s: not implemented", cmd.Name)
}

// printJSON writes v as a single line of JSON followed by a newline.
func printJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// socketDir is where per-session unix domain sockets live, under the XDG
// runtime dir when available, otherwise a tmp fallback.
func socketDir() string {
	if xdg.RuntimeDir != "" {
		return filepath.Join(xdg.RuntimeDir, "bgx")
	}
	return filepath.Join(os.TempDir(), "bgx", "run")
}

// retentionDir holds persisted records and histories for ended sessions,
// grouped by id namespace beneath it.
func retentionDir() string {
	return filepath.Join(os.TempDir(), "bgx", "ended")
}
