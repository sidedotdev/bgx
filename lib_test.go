package bgx_test

import (
	"testing"

	"github.com/sidedotdev/bgx"
	cli "github.com/urfave/cli/v3"
)

func TestCommandIsImportable(t *testing.T) {
	var command *cli.Command = bgx.Command()
	if command == nil {
		t.Fatal("Command returned nil")
	}
	if command.Name != "bgx" {
		t.Fatalf("Command name = %q, want %q", command.Name, "bgx")
	}

	want := map[string]bool{
		"run":     false,
		"wait":    false,
		"kill":    false,
		"history": false,
		"attach":  false,
		"send":    false,
		"info":    false,
		"list":    false,
		"version": false,
	}
	for _, subcommand := range command.Commands {
		if _, ok := want[subcommand.Name]; ok {
			want[subcommand.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Command is missing subcommand %q", name)
		}
	}
}

func TestCommandReturnsIndependentInstances(t *testing.T) {
	first := bgx.Command()
	second := bgx.Command()

	if first == second {
		t.Fatal("Command returned the same mutable command instance twice")
	}
}
