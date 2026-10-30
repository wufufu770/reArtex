//go:build linux

package agentcred

import "syscall"

// ProcAttr 返回把子进程降到 agent 身份的 SysProcAttr。
//
// NoSetGroups 保持 false：Go 会先 setgroups(0) 清空附属组，再 setgid/setuid，
// 子进程才不会带着平台进程的组（root 组）残留。
func ProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: AgentUID, Gid: AgentGID},
	}
}
