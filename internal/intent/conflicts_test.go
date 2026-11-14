package intent

import "testing"

func sig(id int64, kind, vuln string, assets ...int64) Signal {
	return Signal{NodeID: id, Kind: kind, VulnClass: vuln, Summary: "测试意图", AssetIDs: assets}
}

// TestScanAssetMutex 钉死同资产互斥：verify×exploit 同资产命中、tie-break
// 判验证先行；同侧类不命中；无共享资产不命中。
func TestScanAssetMutex(t *testing.T) {
	verify := sig(1, "verify", "authz_bypass", 10, 11)
	exploit := sig(2, "exploit", "sqli", 10, 12)
	same := sig(3, "verify", "xss", 20)

	got := Scan([]Signal{verify, exploit})
	if len(got) != 1 || got[0].RuleID != "asset_mutex" {
		t.Fatalf("verify×exploit 同资产应命中 asset_mutex，got %+v", got)
	}
	if got[0].Winner != "a" || got[0].Reason == "" {
		t.Fatalf("verify 在前应 tie-break 判 a（验证先行），got winner=%q reason=%q", got[0].Winner, got[0].Reason)
	}
	if got[0].SideA.NodeID != 1 || got[0].SideB.NodeID != 2 {
		t.Fatalf("SideA/B 应保持输入序，got %d/%d", got[0].SideA.NodeID, got[0].SideB.NodeID)
	}

	if n := len(Scan([]Signal{verify, same})); n != 0 {
		t.Fatalf("无共享资产不应命中，got %d", n)
	}
	if n := len(Scan([]Signal{verify, sig(4, "recon", "info", 10)})); n != 0 {
		t.Fatalf("recon×verify（同类 verifying）不应命中，got %d", n)
	}
}

// TestScanReversePredicate 钉死反向谓词：同资产+同漏洞类+对偶摘要命中且交
// arbiter（winner 空）；类不同不命中。
func TestScanReversePredicate(t *testing.T) {
	pos := sig(1, "verify", "authz_bypass", 10)
	pos.Summary = "存在水平越权，可读取他人订单"
	neg := sig(2, "verify", "authz_bypass", 10)
	neg.Summary = "复测未发现越权，接口无漏洞"

	got := Scan([]Signal{pos, neg})
	if len(got) != 1 || got[0].RuleID != "reverse_predicate" {
		t.Fatalf("对偶摘要应命中 reverse_predicate，got %+v", got)
	}
	if got[0].Winner != "" {
		t.Fatalf("反向谓词无确定性优先序应交 arbiter，got winner=%q", got[0].Winner)
	}

	diff := sig(3, "verify", "sqli", 10)
	diff.Summary = "未发现注入"
	if n := len(Scan([]Signal{pos, diff})); n != 0 {
		t.Fatalf("vulnclass 不同不应命中，got %d", n)
	}
}

// TestScanResourceClash 钉死资源冲突：双 mutating 共工具命中交 arbiter；
// 非 mutating 不命中。
func TestScanResourceClash(t *testing.T) {
	m1 := sig(1, "exploit", "sqli", 10)
	m1.Actions = []string{"burp_replay", "inject"}
	m2 := sig(2, "exploit", "authz_bypass", 10)
	m2.Actions = []string{"burp_replay"}

	got := Scan([]Signal{m1, m2})
	if len(got) != 1 || got[0].RuleID != "resource_clash" {
		t.Fatalf("双 mutating 共工具应命中 resource_clash，got %+v", got)
	}
	if got[0].Winner != "" {
		t.Fatalf("资源冲突应交 arbiter，got winner=%q", got[0].Winner)
	}
}

// TestScanNoAnchorSkipped 钉死宁漏勿误：无资产锚的意图不参与检测。
func TestScanNoAnchorSkipped(t *testing.T) {
	noAnchor := sig(1, "exploit", "sqli")
	withAnchor := sig(2, "verify", "sqli", 10)
	withAnchor.Actions = []string{"inject"}
	if n := len(Scan([]Signal{noAnchor, withAnchor})); n != 0 {
		t.Fatalf("无锚意图不应参与检测，got %d", n)
	}
}

// TestNormalizePair 与 db.conflictPair 同口径。
func TestNormalizePair(t *testing.T) {
	if a, b := NormalizePair(7, 3); a != 3 || b != 7 {
		t.Fatalf("归一对应小 id 在前，got (%d,%d)", a, b)
	}
}
