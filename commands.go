package bgx

import cli "github.com/urfave/cli/v3"

func RunCommand() *cli.Command {
	stopAfterID := 1
	return &cli.Command{
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
	}
}

func WaitCommand() *cli.Command {
	return &cli.Command{
		Name:      "wait",
		Usage:     "wait for a session to finish and return its exit code",
		ArgsUsage: "<id>",
		Action:    withDirs(waitAction),
	}
}

func KillCommand() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "kill a running session",
		ArgsUsage: "<id>",
		Action:    withDirs(killAction),
	}
}

func HistoryCommand() *cli.Command {
	return &cli.Command{
		Name:      "history",
		Usage:     "print the scrollback history of a session",
		ArgsUsage: "<id>",
		Action:    withDirs(historyAction),
	}
}

func AttachCommand() *cli.Command {
	return &cli.Command{
		Name:      "attach",
		Usage:     "attach to a running session",
		ArgsUsage: "[--show-detach-instructions] <id>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "show-detach-instructions"},
		},
		Action: withDirs(attachAction),
	}
}

func SendCommand() *cli.Command {
	return &cli.Command{
		Name:      "send",
		Usage:     "send raw input to a session PTY without attaching",
		ArgsUsage: "<id> <text...>",
		Action:    withDirs(sendAction),
	}
}

func InfoCommand() *cli.Command {
	return &cli.Command{
		Name:      "info",
		Usage:     "print metadata about a session",
		ArgsUsage: "<id>",
		Action:    withDirs(infoAction),
	}
}

func ListCommand() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "list sessions",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "metadata"},
		},
		Action: withDirs(listAction),
	}
}

func VersionCommand() *cli.Command {
	return &cli.Command{
		Name:   "version",
		Usage:  "print version and environment info",
		Action: withDirs(versionAction),
	}
}
