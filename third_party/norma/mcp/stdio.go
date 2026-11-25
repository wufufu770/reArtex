package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/Autumn-27/norma/tool/privdrop"
)

// stdioCloser shuts down a spawned MCP server process and its pipes.
type stdioCloser struct {
	cmd   *exec.Cmd
	stdin io.Closer
}

func (s *stdioCloser) Close() error {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	return nil
}

// NewStdioClient spawns an MCP server as a subprocess, performs the handshake,
// and returns a connected Client (FR-10.1). Call Close to terminate it.
//
// env holds extra environment variables (e.g. API tokens an MCP server needs)
// applied on top of the parent process environment; pass nil for none.
func NewStdioClient(ctx context.Context, server, command string, env map[string]string, args ...string) (*Client, error) {
	// A-07B fd-exec: spawn through an opened, identity-verified descriptor so the
	// object that passed validation is the object that runs. A plain path spawn
	// re-resolves the path at start time, so a binary swapped between validate and
	// Start would silently take effect (TOCTOU). Any identity mismatch fails
	// closed — there is deliberately no fallback to spawning by path.
	cmd, closeExec, err := openVerifiedCommand(command, args)
	if err != nil {
		return nil, err
	}
	defer closeExec()
	// Drop to the agent identity: with a root platform process the MCP server
	// would inherit root and could read the platform's 0600 credential files.
	if attr := privdrop.ProcAttr(); attr != nil && privdrop.CanDrop() {
		cmd.SysProcAttr = attr
	}
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		server: server,
		conn:   newConn(stdout, stdin),
		closer: &stdioCloser{cmd: cmd, stdin: stdin},
	}
	// 握手预算（0.15.13）：卡死的 MCP 子进程（半启动/卡在初始化）此前会把装配
	// 路径无限挂死——调用方 ctx 通常无 deadline，而 mcpFailTTL 冷却只在返回错误
	// 后生效。30s 内 initialize 没完成即按失败收掉子进程。
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.initialize(initCtx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// newClientWithConn builds a Client over an arbitrary transport (used by tests).
func newClientWithConn(ctx context.Context, server string, r io.Reader, w io.Writer, closer io.Closer) (*Client, error) {
	c := &Client{server: server, conn: newConn(r, w), closer: closer}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// openVerifiedCommand pins the exec target to an opened, identity-verified file
// handle (A-07B fd-exec). exec.Command(command) resolves the path again at start
// time, so a binary swapped between validation and Start would silently take
// effect — the classic validate-then-exec TOCTOU. Here the command is opened
// with O_NOFOLLOW (a symlink resolves once, here), the opened handle is compared
// against the path by device+inode, and the child executes /proc/self/fd/N —
// the kernel runs the exact object that was inspected. Any mismatch fails
// closed; there is deliberately no fallback to spawning by path.
//
// Linux-only: O_NOFOLLOW and /proc/<pid>/fd are Linux conventions. Other
// platforms keep the plain path spawn — the identity check is a Linux
// hardening layer, not a portable guarantee.
func openVerifiedCommand(command string, args []string) (*exec.Cmd, func(), error) {
	noop := func() {}
	if runtime.GOOS != "linux" {
		return exec.Command(command, args...), noop, nil
	}
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
