//go:build !linux

package procguard

// Apply 在非 Linux 平台是空操作：那里没有 /proc 的 dumpable 语义可设。
//
// 桌面版（macOS/Windows）不在本次隔离的威胁模型里 —— 那里没有「容器内 root
// 平台 + 挂载的凭据文件」这组前提。
func Apply() error { return nil }

// Dumpable 在非 Linux 平台恒为 true（没有可观测的降 dumpable 语义）。
func Dumpable() (bool, error) { return true, nil }
