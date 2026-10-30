// Package failpoint provides process-killing fault injection for recovery tests.
// It is inert unless ORBSLEEP_FAILPOINT is set to the exact hook name.
package failpoint

import (
	"fmt"
	"os"
	"syscall"
)

// Hit terminates the process when name matches the configured fail point.
// Modes: kill (default, SIGKILL), exit (exit status 137).
func Hit(name string) {
	want := os.Getenv("ORBSLEEP_FAILPOINT")
	if want == "" || name != want {
		return
	}
	mode := os.Getenv("ORBSLEEP_FAILPOINT_MODE")
	fmt.Fprintf(os.Stderr, "ORBSLEEP failpoint hit: %s mode=%s pid=%d\n", name, mode, os.Getpid())
	if mode == "exit" {
		os.Exit(137)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}
