package bgx

import "testing"

func TestRootCommandTree(t *testing.T) {
	root := rootCommand()
	if root == nil {
		t.Fatal("rootCommand returned nil")
	}
	if root.Name != "bgx" {
		t.Fatalf("Name = %q, want %q", root.Name, "bgx")
	}

	wantAliases := map[string][]string{
		"run":     nil,
		"wait":    nil,
		"kill":    nil,
		"history": nil,
		"attach":  nil,
		"bridge":  nil,
		"send":    nil,
		"info":    nil,
		"list":    {"ls"},
		"version": nil,
	}
	for _, sub := range root.Commands {
		aliases, ok := wantAliases[sub.Name]
		if !ok {
			t.Errorf("unexpected subcommand %q", sub.Name)
			continue
		}
		delete(wantAliases, sub.Name)
		if sub.Hidden {
			t.Errorf("subcommand %q is hidden", sub.Name)
		}
		if sub.Action == nil {
			t.Errorf("subcommand %q has nil Action", sub.Name)
		}
		if len(sub.Aliases) != len(aliases) {
			t.Errorf("subcommand %q Aliases = %v, want %v", sub.Name, sub.Aliases, aliases)
			continue
		}
		for i := range aliases {
			if sub.Aliases[i] != aliases[i] {
				t.Errorf("subcommand %q Aliases[%d] = %q, want %q", sub.Name, i, sub.Aliases[i], aliases[i])
			}
		}
	}
	for name := range wantAliases {
		t.Errorf("missing subcommand %q", name)
	}
}

// The daemon entry point is the InterceptDaemon env marker, not a CLI command,
// so the hidden __daemon subcommand must stay gone from the tree.
func TestRootCommandHasNoDaemonSubcommand(t *testing.T) {
	for _, sub := range rootCommand().Commands {
		if sub.Name == "__daemon" {
			t.Fatal("root command still mounts the hidden __daemon subcommand")
		}
	}
}

func TestRootCommandReturnsIndependentInstances(t *testing.T) {
	if rootCommand() == rootCommand() {
		t.Fatal("rootCommand returned the same mutable command instance twice")
	}
}
