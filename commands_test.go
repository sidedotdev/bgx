package bgx_test

import (
	"testing"

	bgx "github.com/sidedotdev/bgx"
	cli "github.com/urfave/cli/v3"
)

func TestExportedSubcommands(t *testing.T) {
	tests := []struct {
		name    string
		aliases []string
		hidden  bool
		new     func() *cli.Command
	}{
		{name: "run", new: bgx.RunCommand},
		{name: "wait", new: bgx.WaitCommand},
		{name: "kill", new: bgx.KillCommand},
		{name: "history", new: bgx.HistoryCommand},
		{name: "attach", new: bgx.AttachCommand},
		{name: "send", new: bgx.SendCommand},
		{name: "info", new: bgx.InfoCommand},
		{name: "list", aliases: []string{"ls"}, new: bgx.ListCommand},
		{name: "version", new: bgx.VersionCommand},
		{name: "__daemon", hidden: true, new: bgx.DaemonCommand},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := tt.new()
			second := tt.new()
			if first == nil {
				t.Fatal("constructor returned nil")
			}
			if first == second {
				t.Fatal("constructor reused mutable command state")
			}
			if first.Name != tt.name {
				t.Errorf("Name = %q, want %q", first.Name, tt.name)
			}
			if first.Action == nil {
				t.Error("Action is nil")
			}
			if first.Hidden != tt.hidden {
				t.Errorf("Hidden = %t, want %t", first.Hidden, tt.hidden)
			}
			if len(first.Aliases) != len(tt.aliases) {
				t.Fatalf("Aliases = %v, want %v", first.Aliases, tt.aliases)
			}
			for i := range tt.aliases {
				if first.Aliases[i] != tt.aliases[i] {
					t.Errorf("Aliases[%d] = %q, want %q", i, first.Aliases[i], tt.aliases[i])
				}
			}
		})
	}
}
