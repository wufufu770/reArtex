//go:build linux

package privdrop

import (
	"os"
	"syscall"
)

// ProcAttr returns the SysProcAttr that drops a child to the agent identity.
//
// NoSetGroups stays false: Go clears the supplementary groups with
// setgroups(0) before setgid/setuid, so the child does not keep the platform
// process's groups (the root group) around.
func ProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: AgentUID, Gid: AgentGID},
	}
}

// CanDrop reports whether this process can drop a child to the agent identity.
//
// setuid/setgid need CAP_SETUID/CAP_SETGID, so only root (or a process holding
// both capabilities) can do it. A non-root deployment (desktop build, local
// development) gets false — there the child keeps the platform identity, and
// the tools must not stop working just because they cannot be dropped.
func CanDrop() bool { return os.Geteuid() == 0 }
