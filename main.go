package bgx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	cli "github.com/urfave/cli/v3"
)

// version is the bgx release version, overridable at build time via -ldflags.
var version = "0.0.0-dev"

// Run executes the bgx command and emits machine-readable errors.
func Run(ctx context.Context, args []string) error {
	if err := rootCommand().Run(ctx, args); err != nil {
		emitErrorJSON(errorCode(err), err.Error())
		return err
	}
	return nil
}

// rootCommand returns a new urfave/cli command configured with all bgx
// subcommands. The command tree is intentionally unexported: the public
// library surface is typed-only and embedding binaries integrate via Run and
// InterceptDaemon instead of mounting bgx commands.
func rootCommand() *cli.Command {
	root := &cli.Command{
		Name:        "bgx",
		Usage:       "manage async terminal sessions",
		HideVersion: true,
		Commands: []*cli.Command{
			runCommand(),
			waitCommand(),
			killCommand(),
			historyCommand(),
			attachCommand(),
			bridgeCommand(),
			sendCommand(),
			infoCommand(),
			listCommand(),
			versionCommand(),
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
