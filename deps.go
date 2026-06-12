//go:build bgx_deps

// This file is never compiled in normal builds. It exists solely to pin the
// module requirements for packages that are wired up in later steps so that
// `go mod tidy` keeps them in go.mod. go.mitchellh.com/libghostty in
// particular requires the native libghostty-vt-static library (cgo + pkg-config)
// which is not available in every build environment, so it must stay out of the
// default build graph.
package main

import (
	_ "github.com/creack/pty"
	_ "github.com/klauspost/compress/zstd"
	_ "go.mitchellh.com/libghostty"
)
