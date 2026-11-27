package mcp

import (
	"context"
	"io"
	"os"
	"os/exec"
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
