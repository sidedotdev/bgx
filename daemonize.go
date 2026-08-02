package bgx

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sidedotdev/bgx/daemon"
)

// daemonConfigEnv is the private marker session start plants in a re-exec'd
// process's environment; its value is the JSON daemon config InterceptDaemon
// serves. It lives in the environment rather than argv so host binaries'
// own command-line parsing is never involved.
const daemonConfigEnv = "_BGX_DAEMON_CONFIG"

// InterceptDaemon must be called first in a host binary's main() (cmd/bgx does
// the same). It is a no-op normally; when the process was re-exec'd by session
// start it runs the session daemon to completion and exits, so any binary
// embedding the library can serve as its own daemon.
func InterceptDaemon() {
	payload := os.Getenv(daemonConfigEnv)
	if payload == "" {
		return
	}
	// Drop the marker so the session's command doesn't inherit it and get
	// mistaken for a daemon re-exec itself.
	os.Unsetenv(daemonConfigEnv)
	var cfg daemon.Config
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bgx daemon: invalid config: %v\n", err)
		os.Exit(1)
	}
	if err := daemon.Serve(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bgx daemon: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
