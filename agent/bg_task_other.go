//go:build windows

package agent

import (
	"fmt"
	"os"
)

// killBgProcess 在没有进程组语义的平台上退化为杀单个进程。
func killBgProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("pid 不合法: %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("杀后台进程 %d 失败: %w", pid, err)
	}
	return nil
}
