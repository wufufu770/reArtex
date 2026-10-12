//go:build !windows

package server

import "syscall"

// openNoFollow 使 O_NOFOLLOW 可用：os 包未导出该标志，Go 标准做法是取
// syscall 的值。不带此标志时 os.Create/ OpenFile 会跟随符号链接，
// 工作目录内的软链可把上传写到根外任意位置。
const openNoFollow = syscall.O_NOFOLLOW
