//go:build linux

package agentcred

import (
	"syscall"

	"github.com/Autumn-27/norma/tool/privdrop"
)

// ProcAttr 返回把子进程降到 agent 身份的 SysProcAttr。
//
// 实现委托给 norma 的 tool/privdrop：框架内置工具与 reArtex 自建工具必须降权到
// 同一个身份，否则框架那条执行路径就是静默的口子。
func ProcAttr() *syscall.SysProcAttr { return privdrop.ProcAttr() }
