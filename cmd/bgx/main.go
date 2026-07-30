package main

import (
	"context"
	"os"

	"github.com/sidedotdev/bgx"
)

func main() {
	if err := bgx.Run(context.Background(), os.Args); err != nil {
		os.Exit(1)
	}
}
