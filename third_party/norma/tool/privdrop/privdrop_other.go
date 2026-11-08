//go:build !linux

package privdrop

import "syscall"

// ProcAttr returns nil on non-Linux platforms: dropping via setuid is Linux
// semantics.
//
// The desktop builds (macOS/Windows) are not part of this isolation's threat
// model — there is no "root platform inside a container + mounted credential
// file" premise, so the platform identity is kept.
func ProcAttr() *syscall.SysProcAttr { return nil }

// CanDrop returns false on non-Linux platforms: there is no setuid drop there.
func CanDrop() bool { return false }
