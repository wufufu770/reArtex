package agent

import (
	"os"
	"syscall"
	"testing"

	"github.com/Autumn-27/artex/internal/agentcred"
)

// agent 的工作目录必须归 agent 身份所有：工具子进程降权到非特权 UID 之后，
// 仍然要在自己的工作目录里写中间产物（规约里明确要求「一律写到本次意图的
// 专属工作目录」）。目录属主还是平台的话，子进程连 mkdir 都做不了。
func TestEnsureRunDirOwnedByAgentUID(t *testing.T) {
	if !agentcred.CanDrop() {
		t.Skip("需要 root（把目录属主改成 agent UID 需要 CAP_CHOWN）")
	}
	base := t.TempDir()
	dir := ensureRunDir(base, 42, 7)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("工作目录不存在: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("拿不到 stat 信息")
	}
	if st.Uid != agentcred.AgentUID {
		t.Fatalf("工作目录属主 uid = %d，期望 %d（agent 专用 UID）", st.Uid, agentcred.AgentUID)
	}
}
