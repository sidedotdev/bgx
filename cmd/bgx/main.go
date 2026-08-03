package main

import (
	"context"
	"errors"
	"os"

	"github.com/sidedotdev/bgx"
	"github.com/sidedotdev/bgx/internal/cli"
)

func main() {
	bgx.InterceptDaemon()
	err := cli.Run(context.Background(), os.Args, os.Stdout, os.Stderr)
	if code := exitCode(err); code != 0 {
		os.Exit(code)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return 1
}
