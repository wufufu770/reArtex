package agentcred

import "testing"

// agent 身份必须是「专用的非特权 UID」：不是 0（平台进程自己的身份），
// 也不是 1000（容器里挂载出来的 db 凭据文件属主就是 1000，用它等于没降权）。
func TestAgentUIDIsDedicatedNonPrivileged(t *testing.T) {
	if AgentUID == 0 {
		t.Fatal("AgentUID 不能是 0：那是平台进程自己的身份，降权等于没降")
	}
	if AgentUID == 1000 {
		t.Fatal("AgentUID 不能是 1000：db 凭据文件(/run/secrets/pg_dsn)属主就是 1000，0600 挡不住它")
	}
	if AgentGID == 0 {
		t.Fatal("AgentGID 不能是 0")
	}
}
