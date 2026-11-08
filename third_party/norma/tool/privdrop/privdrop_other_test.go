//go:build !linux

package privdrop

import "testing"

// Off Linux there is no setuid drop: the helper must report "no drop" instead
// of failing hard, so the spawn sites keep the platform identity.
func TestProcAttrIsNilOffLinux(t *testing.T) {
	if attr := ProcAttr(); attr != nil {
		t.Fatalf("非 Linux 上 ProcAttr 必须是 nil，实际返回 %+v", attr)
	}
}

func TestCanDropIsFalseOffLinux(t *testing.T) {
	if CanDrop() {
		t.Fatal("非 Linux 上 CanDrop 必须是 false")
	}
}
