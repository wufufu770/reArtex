//go:build windows

package server

// Windows 无 O_NOFOLLOW。Windows 上工作区路径不落在 POSIX 软链语义下，
// wsResolve 的 EvalSymlinks 复核仍提供一层防护；此处的标志为 0。
const openNoFollow = 0
