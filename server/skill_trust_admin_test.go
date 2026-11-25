package server

// A-02 第二批（0.15.91）的验收用例：operator 授信端点、启动 reconcile 存量迁移、
// 批量创建速率告警。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// trustCaseAuditCount 数本用例名字相关的 skill 信任审计事件。
func trustCaseAuditCount(t *testing.T, pg *db.DB, action, name string) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE tool_name IN ('skills','create_skill') AND action=$1 AND reason LIKE '%'||$2||'%'`,
		action, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSkillTrustEndpoint 钉住授信端点的闭环（A-02 验收）：失配降级后，operator
// 经端点显式授信 ⇒ 档位恢复 trusted 且审计链留痕；非法 tier 拒绝；GET 语义之外
// 的写（此处端点是 POST，天然只写不算校验副作用）。
func TestSkillTrustEndpoint(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-endpoint")
	dir := t.TempDir()
	const name = "t-trust-endpoint"
	writeSkillMD(t, dir, name, "operator reviewed this")
	recordSkillTrust(pg, dir, name, skillTierTrusted, skillTrustWriterOperator)

	// agent 直改（攻击复现路径）⇒ 判定降级。
	writeSkillMD(t, dir, name, "agent tampered")
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("直改后应 untrusted，得到 %s", got)
	}

	// operator 复核后经端点授信。
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(testServerCtx(t), m, dir, dir, dir)
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	// 非法 tier 拒绝。
	code, out := sodAPIDo(t, h, tok, "POST", "/api/skills/"+name+"/trust", map[string]string{"tier": "always-trust"})
	if code != 400 {
		t.Fatalf("非法 tier 必须 400，实际 %d %v", code, out)
	}
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("被拒绝的请求不得改变档位，得到 %s", got)
	}

	before := trustCaseAuditCount(t, pg, "skill.trust", name)
	code, out = sodAPIDo(t, h, tok, "POST", "/api/skills/"+name+"/trust", map[string]string{"tier": "trusted", "reason": "复核过 agent 改动，接受为新基线"})
	if code != 200 || out["ok"] != true {
		t.Fatalf("授信应成功: %d %v", code, out)
	}
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("端点授信后应恢复 trusted，得到 %s", got)
	}
	if got := trustCaseAuditCount(t, pg, "skill.trust", name); got != before+1 {
		t.Fatalf("授信操作必须进审计链，期望 %d 条，实际 %d 条", before+1, got)
	}
}

// TestReconcileSkillTrust 钉住启动对账的三分支（A-02 存量迁移）：
// 无记录 → 补钉 trusted（保持升级前展示，无静默翻转）；
// 存量单文件哈希记录 → 静默迁移为树哈希、档位原样保留；
// 已是树哈希的记录 → 不动（reconcile 不重新裁决）。
func TestReconcileSkillTrust(t *testing.T) {
	pg := trustTestDBScoped(t, "rec-none", "rec-legacy", "rec-tree")
	dir := t.TempDir()
	const (
		none   = "rec-none"   // 无记录
		legacy = "rec-legacy" // 存量单文件哈希记录（trusted）
		tree   = "rec-tree"   // 已是树哈希记录（untrusted）
	)
	writeSkillMD(t, dir, none, "distributed with image")
	writeSkillMD(t, dir, legacy, "legacy pinned content")
	writeSkillMD(t, dir, tree, "tree pinned content")

	// legacy：造一条「旧口径」记录（sum = sha256(SKILL.md)，档位 trusted）。
	legacySum, err := skillLegacyContentHash(dir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting(skillTrustKeyPrefix+legacy, legacySum+"|"+skillTierTrusted); err != nil {
		t.Fatal(err)
	}
	// tree：正常 operator 记录（untrusted）。
	recordSkillTrust(pg, dir, tree, skillTierUntrusted, skillTrustWriterOperator)
	treeRec, ok, err := pg.GetSetting(skillTrustKeyPrefix + tree)
	if err != nil || !ok {
		t.Fatalf("tree 记录应存在: ok=%v err=%v", ok, err)
	}

	reconcileSkillTrust(pg, dir)

	// none：补钉为 treeHash|trusted，档位展示与升级前（无记录=trusted）一致。
	rec, ok, err := pg.GetSetting(skillTrustKeyPrefix + none)
	if err != nil || !ok {
		t.Fatalf("无记录 skill 应被补钉: ok=%v err=%v", ok, err)
	}
	sum, tier, found := strings.Cut(rec, "|")
	if !found || sum == "" || tier == "" {
		t.Fatalf("补钉记录格式不对: %q", rec)
	}
	want, err := skillTreeHash(dir, none)
	if err != nil {
		t.Fatal(err)
	}
	if sum != want || tier != skillTierTrusted {
		t.Fatalf("补钉内容不对：期望 %s|%s，实际 %s|%s", want, skillTierTrusted, sum, tier)
	}
	if got := skillTrustTier(pg, dir, skOf(dir, none)); got != skillTierTrusted {
		t.Fatalf("补钉后应为 trusted，得到 %s", got)
	}

	// legacy：记录换成树哈希、档位原样保留（trusted）。
	rec, ok, err = pg.GetSetting(skillTrustKeyPrefix + legacy)
	if err != nil || !ok {
		t.Fatal(err)
	}
	sum, tier, found = strings.Cut(rec, "|")
	if !found {
		t.Fatalf("迁移后记录格式不对: %q", rec)
	}
	want, err = skillTreeHash(dir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if sum != want || tier != skillTierTrusted {
		t.Fatalf("存量迁移应换算树哈希并保留档位：期望 %s|%s，实际 %s|%s", want, skillTierTrusted, sum, tier)
	}
	// 幂等：再跑一遍零变化（tree 记录不再命中 legacy 口径）。
	before := sum
	reconcileSkillTrust(pg, dir)
	rec2, _, _ := pg.GetSetting(skillTrustKeyPrefix + legacy)
	after, _, _ := strings.Cut(rec2, "|")
	if before != after {
		t.Fatalf("reconcile 必须幂等：重复执行改变了记录 %q → %q", before, after)
	}

	// tree：untrusted 记录原样不动（reconcile 不掩盖任何已发生的降级）。
	rec3, ok, err := pg.GetSetting(skillTrustKeyPrefix + tree)
	if err != nil || !ok || rec3 != treeRec {
		t.Fatalf("树哈希记录不应被 reconcile 改动: ok=%v err=%v rec=%q", ok, err, rec3)
	}
	if got := skillTrustTier(pg, dir, skOf(dir, tree)); got != skillTierUntrusted {
		t.Fatalf("untrusted 记录应保持 untrusted，得到 %s", got)
	}
}

// TestSkillCreateRateAlert 钉住批量创建速率告警（A-02）：滑动 1 小时窗口计数，
// 超阈值第一次记审计事件 + 告警日志，同窗口不重复记链；阈值 settings 热改、
// 非法值回落默认。
func TestSkillCreateRateAlert(t *testing.T) {
	pg := trustTestDBScoped(t)
	resetSkillCreateRate()
	t.Cleanup(resetSkillCreateRate)
	// 阈值热改为 2（窗口内第 3 个创建触发告警）。
	if err := pg.SetSetting(skillTrustThresholdSetting, "2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.SetSetting(skillTrustThresholdSetting, "") })

	countAlerts := func() int {
		var n int
		if err := pg.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='skill.create_rate_alert'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	recordSkillCreateAndAlert(pg, "skill-a")
	recordSkillCreateAndAlert(pg, "skill-b")
	if got := countAlerts(); got != 0 {
		t.Fatalf("阈值内不得告警，实际 %d 条", got)
	}
	recordSkillCreateAndAlert(pg, "skill-c") // 第 3 个 > 阈值 2 ⇒ 告警
	if got := countAlerts(); got != 1 {
		t.Fatalf("超阈值应记一条告警，期望 1 条，实际 %d 条", got)
	}
	recordSkillCreateAndAlert(pg, "skill-d") // 同窗口继续超量：不重复记链
	if got := countAlerts(); got != 1 {
		t.Fatalf("同窗口超量不得重复记链，期望仍 1 条，实际 %d 条", got)
	}

	// 非法阈值回落默认（10）：2 个创建不应告警。
	resetSkillCreateRate()
	if err := pg.SetSetting(skillTrustThresholdSetting, "not-a-number"); err != nil {
		t.Fatal(err)
	}
	recordSkillCreateAndAlert(pg, "skill-e")
	recordSkillCreateAndAlert(pg, "skill-f")
	if got := countAlerts(); got != 1 {
		t.Fatalf("非法阈值回落默认后 2 个创建不应告警，期望仍 1 条，实际 %d 条", got)
	}
}

// TestSkillTreeHashIgnoresGitAndTemp 钉住树哈希的忽略面：.git 与临时文件不进
// 摘要——它们的出现/消失/改动不构成 skill 内容变化，不应触发降级。
func TestSkillTreeHashIgnoresGitAndTemp(t *testing.T) {
	dir := t.TempDir()
	const name = "t-tree-ignore"
	writeSkillMD(t, dir, name, "stable content")
	base, err := skillTreeHash(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	// 加 .git 目录 + 临时文件：哈希不变。
	if err := os.MkdirAll(filepath.Join(dir, name, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, ".git", "index"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md~"), []byte("editor junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "upload.tmp"), []byte("partial upload"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := skillTreeHash(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf(".git/临时文件不得进树哈希：%q vs %q", base, got)
	}
}
