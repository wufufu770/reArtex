//go:build linux

package privdrop

import (
	"os"
	"testing"
)

// The drop has to land on a dedicated unprivileged UID: not 0 (the platform
// process's own identity, so no drop at all) and not 1000 (the container's
// mounted db credential file is owned by uid 1000 with mode 0600, so a child
// running as 1000 still reads it).
func TestAgentIdentityIsDedicatedNonPrivileged(t *testing.T) {
	if AgentUID == 0 {
		t.Fatal("AgentUID 不能是 0：那是平台进程自己的身份，降权等于没降")
	}
	if AgentUID == 1000 {
		t.Fatal("AgentUID 不能是 1000：db 凭据文件(/run/secrets/pg_dsn)属主就是 1000，0600 挡不住它")
	}
	if AgentGID == 0 {
		t.Fatal("AgentGID 不能是 0")
	}
	if AgentGID != AgentUID {
		t.Fatalf("AgentGID(%d) 必须与 AgentUID(%d) 同值", AgentGID, AgentUID)
	}
}

// On Linux the helper must hand back a credential carrying the agent identity,
// otherwise the spawn sites would silently run their children as the platform
// identity.
func TestProcAttrCarriesAgentIdentity(t *testing.T) {
	attr := ProcAttr()
	if attr == nil {
		t.Fatal("Linux 上 ProcAttr 不能是 nil：否则工具子进程仍以平台身份(root)运行")
	}
	if attr.Credential == nil {
		t.Fatal("ProcAttr 必须带上 Credential，否则不会发生降权")
	}
	if attr.Credential.Uid != AgentUID {
		t.Fatalf("Credential.Uid = %d，期望 %d", attr.Credential.Uid, AgentUID)
	}
	if attr.Credential.Gid != AgentGID {
		t.Fatalf("Credential.Gid = %d，期望 %d", attr.Credential.Gid, AgentGID)
	}
}

// The drop needs CAP_SETUID/CAP_SETGID, so it is only available to root.
func TestCanDropTracksRoot(t *testing.T) {
	if got, want := CanDrop(), os.Geteuid() == 0; got != want {
		t.Fatalf("CanDrop() = %v，期望 %v", got, want)
	}
}
