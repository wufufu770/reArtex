//go:build linux

package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// openVerifiedCommand pins the exec target to an opened, identity-verified file
// handle (A-07B fd-exec). exec.Command(command) resolves the path again at start
// time, so a binary swapped between validation and Start would silently take
// effect — the classic validate-then-exec TOCTOU. Here the command is opened
// with O_NOFOLLOW (a symlink resolves once, here), the opened handle is compared
// against the path by device+inode, and the child executes /proc/self/fd/N —
// the kernel runs the exact object that was inspected. Any mismatch fails
// closed; there is deliberately no fallback to spawning by path.
//
// Linux-only: O_NOFOLLOW and /proc/<pid>/fd are Linux conventions. The non-Linux
// counterpart lives in openexec_other.go (//go:build !linux) and keeps the plain
// path spawn — the identity check is a Linux hardening layer, not a portable
// guarantee. The split into build-tagged files (0.15.95) replaces the previous
// runtime.GOOS check inside a platform-neutral file, which failed to compile on
// platforms lacking syscall.O_NOFOLLOW / syscall.Stat_t (windows, and any target
// without them) even though the guarded branch never ran there.
func openVerifiedCommand(command string, args []string) (*exec.Cmd, func(), error) {
	noop := func() {}
	// O_NOFOLLOW: a symlinked command path resolves here or not at all, so the
	// handle we verify is the final object — not an intermediate link.
	f, err := os.OpenFile(command, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, noop, err
	}
	closer := func() { _ = f.Close() }
	hstat, err := f.Stat()
	if err != nil {
		closer()
		return nil, noop, err
	}
	pstat, err := os.Stat(command)
	if err != nil {
		closer()
		return nil, noop, err
	}
	hs, ok1 := hstat.Sys().(*syscall.Stat_t)
	ps, ok2 := pstat.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 || hs.Dev != ps.Dev || hs.Ino != ps.Ino {
		closer()
		return nil, noop, fmt.Errorf("mcp: %q changed between validation and open (identity mismatch); refusing to exec", command)
	}
	// /proc/self/fd/3: the first ExtraFile is placed at fd 3 in the child, and
	// the kernel resolves the execve target through it at start time. The child
	// still sees the real command name as argv[0] via cmd.Args[0].
	cmd := exec.Command("/proc/self/fd/3", args...)
	cmd.Args[0] = command
	cmd.ExtraFiles = []*os.File{f}
	return cmd, closer, nil
}
