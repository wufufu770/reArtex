//go:build !windows

package agent

import (
	"fmt"
	"syscall"
)

// killBgProcess 按**进程组**杀一个后台进程。
//
// 为什么杀进程组而不是单个进程：SDK 起的后台命令是 `bash -c "..."`，真正在干的活
// （编译、扫描、爬虫）在它的子进程里。只杀 bash 会把孙子进程留在原地继续跑 —— 那
// 正是「进程泄漏」的形态。SDK 自己也是按组杀的（tool/task_unix.go 的 treeKill：
// 子进程用 Setpgid 单独成组）。
//
// ESRCH（进程已经没了）不算失败：回收一个已经结束的进程是成功，不是错误。
func killBgProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("pid 不合法: %d", pid)
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil {
		return nil
	}
	if err == syscall.ESRCH {
		return nil
	}
	// 进程组不存在（进程不是组长，或组已经散了）⇒ 退回杀单个进程。
	if perr := syscall.Kill(pid, syscall.SIGKILL); perr != nil && perr != syscall.ESRCH {
		return fmt.Errorf("杀后台进程 %d 失败: %w", pid, err)
	}
	return nil
}
