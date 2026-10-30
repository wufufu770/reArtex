//go:build !linux

package agentcred

import (
	"syscall"

	"github.com/Autumn-27/norma/tool/privdrop"
)

// ProcAttr 在非 Linux 平台返回 nil：setuid 降权是 Linux 语义。
//
// 桌面版（macOS/Windows）不在本次隔离的威胁模型里 —— 那里没有「容器内 root
// 平台 + 挂载的凭据文件」这组前提，保持平台身份即可。
func ProcAttr() *syscall.SysProcAttr { return privdrop.ProcAttr() }
