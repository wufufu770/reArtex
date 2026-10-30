//go:build linux

package procguard

import (
	"fmt"
	"syscall"
)

// prctl 的选项编号（见 prctl(2)）。
const (
	prGetDumpable = 3
	prSetDumpable = 4
)

// Apply 把当前进程置为不可 dump。
//
// 幂等：重复调用没有副作用。失败必须让调用方硬失败 —— 这条保护不生效就意味着
// 「agent 读不到平台的 /proc」没有成立，不能悄悄继续跑。
func Apply() error {
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0) 失败: %w", errno)
	}
	return nil
}

// Dumpable 报告当前进程是否可 dump（PR_GET_DUMPABLE）。
//
// 取值：0 = SUID_DUMP_ROOT（不可 dump）、1 = SUID_DUMP_USER（可 dump）、
// 2 = SUID_DUMP_SAFE（只有 root 能 dump）。后两者对「别的 UID 能不能看我的
// /proc」没有区别，所以只把 1 当作可 dump。
func Dumpable() (bool, error) {
	v, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		return false, fmt.Errorf("prctl(PR_GET_DUMPABLE) 失败: %w", errno)
	}
	return v == 1, nil
}
