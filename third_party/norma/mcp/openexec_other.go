//go:build !linux

package mcp

import (
	"os/exec"
)

// openVerifiedCommand is the non-Linux counterpart of the fd-exec identity check
// (see openexec_linux.go): O_NOFOLLOW and /proc/<pid>/fd are Linux conventions,
// so other platforms keep the plain path spawn. The identity check is a Linux
// hardening layer, not a portable guarantee.
func openVerifiedCommand(command string, args []string) (*exec.Cmd, func(), error) {
	noop := func() {}
	return exec.Command(command, args...), noop, nil
}
