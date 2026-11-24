package db

import (
	"fmt"
	"testing"
	"time"
)

// TestScopeSiblingBase 钉住「同根域 DNS 发现」切面的范围侧判定（0.15.56）：
// 被查名字与范围中某条域名行同 eTLD+1 ⇒ 返回基域；跨域 / IP / 空任务 ⇒ ("", nil)；
// 多段后缀（com.cn 形）按 publicsuffix 算，不会把 com.cn 当基域。
func TestScopeSiblingBase(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	taskID, err := d.CreateTask("sibling fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskID.ID) })

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	// 精确子域行（task 9 的现实形态：agent 只沉淀了 www 一行）。
	if _, err := d.Assets().AddManualScope(taskID.ID, "subdomain", "www.sib-"+stamp+".example.test", "测试范围"); err != nil {
		t.Fatal(err)
	}
	// 多段后缀行：mcdonalds.com.cn 形——基域必须是 mcdonalds.com.cn 而不是 com.cn。
	if _, err := d.Assets().AddManualScope(taskID.ID, "root_domain", "multi-"+stamp+".example.com.cn", "测试范围"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		host string
		want string
	}{
		// 兄弟子域（范围行 www.<stamp>.example.test ⇒ 被查 sibling.…）⇒ 基域
		// example.test（.test 单标签 TLD，eTLD+1 = 末两段；机制与生产域
		// lingguang.com ⇒ lingguang.com 同构）。
		{"sibling.sib-" + stamp + ".example.test", "example.test"},
		{"deep.nested.sib-" + stamp + ".example.test", "example.test"},
		// 范围行自身/其子域本来就 in scope（闸门在更早分支放行）——本函数仍如实返回基域。
		{"www.sib-" + stamp + ".example.test", "example.test"},
		// 多段后缀：com.cn 形——eTLD=com.cn，+1 取两段 ⇒ example.com.cn；
		// 生产行的 mcdonalds.com.cn（eTLD+1 = 它自己）同构命中。
		{"api.multi-" + stamp + ".example.com.cn", "example.com.cn"},
		// 跨域 ⇒ 无。
		{"elsewhere.example.net", ""},
		{"sib-" + stamp + ".example.test.evil.test", ""},
		// IP 字面量与域名行无同根关系 ⇒ 无。
		{"8.8.8.8", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, err := d.Assets().ScopeSiblingBase(taskID.ID, c.host)
		if err != nil {
			t.Fatalf("host %q: %v", c.host, err)
		}
		if got != c.want {
			t.Fatalf("host %q 基域=%q want %q", c.host, got, c.want)
		}
	}

	// 没有范围行的任务 ⇒ ("", nil)：不是错误，也不放行。
	empty, err := d.CreateTask("sibling empty", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(empty.ID) })
	if got, err := d.Assets().ScopeSiblingBase(empty.ID, "sibling.sib-"+stamp+".example.test"); err != nil || got != "" {
		t.Fatalf("空范围任务应得 (\"\", nil)，实际 (%q, %v)", got, err)
	}
}

// TestScopeSiblingBaseCrossTaskIsolation 钉住口径边界：判定只看本任务（及直接
// 来源任务）的范围行——别的任务授权的域不能借来当自己的同根依据。
func TestScopeSiblingBaseCrossTaskIsolation(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	taskA, err := d.CreateTask("sibling A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := d.CreateTask("sibling B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(taskA.ID); _ = d.DeleteTask(taskB.ID) })

	if _, err := d.Assets().AddManualScope(taskA.ID, "root_domain", "only-a-"+stamp+".example.test", "测试范围"); err != nil {
		t.Fatal(err)
	}
	host := "sibling.only-a-" + stamp + ".example.test"
	if got, err := d.Assets().ScopeSiblingBase(taskA.ID, host); err != nil || got == "" {
		t.Fatalf("任务 A 自己的范围内应命中基域: got=%q err=%v", got, err)
	}
	if got, err := d.Assets().ScopeSiblingBase(taskB.ID, host); err != nil || got != "" {
		t.Fatalf("任务 B 不得借用任务 A 的范围行: got=%q err=%v", got, err)
	}
}
