package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/norma/skill"

	"github.com/Autumn-27/artex/db"
)

func trustTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// trustTestDBScoped 返回测试库句柄，并在用例结束时清掉本用例写入的 skill_trust.*
// 记录与本用例产生的审计事件——用例跑在同一个库上，残留会干扰后续用例。
func trustTestDBScoped(t *testing.T, names ...string) *db.DB {
	t.Helper()
	pg := trustTestDB(t)
	t.Cleanup(func() {
		for _, n := range names {
			_ = pg.SetSetting(skillTrustKeyPrefix+n, "")
		}
		_, _ = pg.Exec(`DELETE FROM audit_events WHERE actor='platform' AND tool_name='skill_trust' AND reason LIKE '%trust-case-%'`)
	})
	return pg
}

func writeSkillMD(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSkillFile(t *testing.T, dir, name, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, name, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skOf(dir, name string) skill.Skill {
	return skill.Skill{Name: name, Dir: filepath.Join(dir, name)}
}

// TestSkillTrustTierDefaultsUntrusted 钉住 fail-closed 默认（A-02 翻转）：
// 无记录 = 无法证明是谁的内容 = 不可信。随镜像分发的 skill 由启动 reconcile /
// 信任端点首次标记，标记前按 untrusted 注入，不静默放行。
func TestSkillTrustTierDefaultsUntrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-default")
	dir := t.TempDir()
	const name = "t-trust-default"
	writeSkillMD(t, dir, name, "original")

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("无记录时应为 untrusted（fail-closed），得到 %s", got)
	}
}

func TestSkillTrustTierUntrustedWhenAgentWrote(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-untrusted")
	dir := t.TempDir()
	const name = "t-trust-untrusted"
	writeSkillMD(t, dir, name, "agent wrote this")

	recordSkillTrust(pg, dir, name, skillTierUntrusted, skillTrustWriterAgent)
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("agent 写过的 skill 应为 untrusted，得到 %s", got)
	}
}

// 关键回归：agent 连续写两次时，第二次的树哈希与记录不符。若判定逻辑把「哈希
// 不匹配」当作可信信号，就会把 agent 刚写的内容误判成可信——防线就此失效。
func TestSkillTrustTierAgentWritesTwiceStaysUntrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-twice")
	dir := t.TempDir()
	const name = "t-trust-twice"
	writeSkillMD(t, dir, name, "v1")

	recordSkillTrust(pg, dir, name, skillTierUntrusted, skillTrustWriterAgent)
	writeSkillMD(t, dir, name, "v2")
	// 第二次写入同样走 ARTEX 自己的路径，故必须刷新记录。
	recordSkillTrust(pg, dir, name, skillTierUntrusted, skillTrustWriterAgent)

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("agent 连写两次后仍应为 untrusted，得到 %s", got)
	}
}

// TestSkillTrustTierAgentWriterNeverTrusted 钉住写入方身份的强制语义（A-02 核心）：
// agent 身份调用 recordSkillTrust 哪怕传 trusted，落库也必须是 untrusted——
// 「信任记录只能由 operator 侧写入」写在函数里而不是靠调用方自觉。
func TestSkillTrustTierAgentWriterNeverTrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-agent-force")
	dir := t.TempDir()
	const name = "t-trust-agent-force"
	writeSkillMD(t, dir, name, "agent authored")

	recordSkillTrust(pg, dir, name, skillTierTrusted, skillTrustWriterAgent)
	rec, ok, err := pg.GetSetting(skillTrustKeyPrefix + name)
	if err != nil || !ok {
		t.Fatalf("信任记录应已落库: ok=%v err=%v", ok, err)
	}
	if rec == "" {
		t.Fatal("信任记录不应为空")
	}
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("agent 请求 trusted 也必须落 untrusted，判定得到 %s（记录=%q）", got, rec)
	}
}

// TestSkillTrustTreeHashSensitivity 钉住树哈希的覆盖面（A-02 验收）：改 SKILL.md、
// 改 scripts/*.py、新增文件、删文件四种情况都使树哈希变化且档位落到 untrusted。
func TestSkillTrustTreeHashSensitivity(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-tree")
	dir := t.TempDir()
	const name = "t-trust-tree"
	writeSkillMD(t, dir, name, "v1")
	writeSkillFile(t, dir, name, "scripts/run.py", "print('v1')")

	base, err := skillTreeHash(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	recordSkillTrust(pg, dir, name, skillTierTrusted, skillTrustWriterOperator)
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("operator 记录后应为 trusted，得到 %s", got)
	}

	cases := []struct {
		what string
		mut  func()
	}{
		{"改 SKILL.md", func() { writeSkillMD(t, dir, name, "v2") }},
		{"改 scripts/*.py", func() { writeSkillFile(t, dir, name, "scripts/run.py", "print('v2')") }},
		{"新增文件", func() { writeSkillFile(t, dir, name, "references/extra.md", "x") }},
		{"删文件", func() {
			if err := os.Remove(filepath.Join(dir, name, "scripts", "run.py")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		// 每轮从「记录=当前树、trusted」出发，模拟一次记录之外的改动。
		recordSkillTrust(pg, dir, name, skillTierTrusted, skillTrustWriterOperator)
		if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
			t.Fatalf("[%s] 基线应为 trusted，得到 %s", tc.what, got)
		}
		tc.mut()
		cur, err := skillTreeHash(dir, name)
		if err != nil {
			t.Fatalf("[%s] 树哈希计算: %v", tc.what, err)
		}
		if cur == base {
			t.Fatalf("[%s] 树哈希不应与基线相同（哈希没覆盖到这次改动）", tc.what)
		}
		if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
			t.Fatalf("[%s] 记录之外的改动必须降级 untrusted，得到 %s", tc.what, got)
		}
		// 失配已被重钉（cur|untrusted）：同一次改动的再次判定稳定为 untrusted，
		// 且不再追加审计（重钉后 sum 命中，不走失配分支）。
		if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
			t.Fatalf("[%s] 重钉后应稳定为 untrusted，得到 %s", tc.what, got)
		}
		// 恢复基线树，供下一轮使用（重新写全 base 树的内容）。
		writeSkillMD(t, dir, name, "v1")
		writeSkillFile(t, dir, name, "scripts/run.py", "print('v1')")
		os.RemoveAll(filepath.Join(dir, name, "references"))
	}
}

// TestSkillTrustTierMismatchDemotesAndAudits 钉住失配的完整行为（A-02 验收——
// 攻击复现）：agent 经 Write 直接改 SKILL.md（绕开 ARTEX 写入路径）后，下一次
// 判定必须落到 untrusted（skill_demote.go 的降权注入由此触发），且审计链上留
// 一条降级事件；同一次改动的第二次判定不再重复写审计。
func TestSkillTrustTierMismatchDemotesAndAudits(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-mismatch")
	dir := t.TempDir()
	const name = "trust-case-mismatch"
	writeSkillMD(t, dir, name, "operator content")

	recordSkillTrust(pg, dir, name, skillTierTrusted, skillTrustWriterOperator)

	countAudits := func() int {
		var n int
		if err := pg.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE actor='platform' AND tool_name='skill_trust' AND action='skill.demoted' AND reason LIKE '%` + name + `%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countAudits()

	// 攻击复现：绕开 recordSkillTrust，直接改文件（等同 agent 用 Write 工具）。
	writeSkillMD(t, dir, name, "agent tampered content")
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("记录之外的直改必须判 untrusted，得到 %s", got)
	}
	if got := countAudits(); got != before+1 {
		t.Fatalf("失配降级必须记一条审计事件，期望 %d 条，实际 %d 条", before+1, got)
	}
	// 同一次改动的再次判定：稳定 untrusted，不重复审计。
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("重钉后应稳定 untrusted，得到 %s", got)
	}
	if got := countAudits(); got != before+1 {
		t.Fatalf("同一次改动不应重复记审计，期望仍 %d 条，实际 %d 条", before+1, got)
	}
	// 内容再次被改：新的一次失配 ⇒ 再降级 + 再记一条。
	writeSkillMD(t, dir, name, "agent tampered again")
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("再次改动仍应 untrusted，得到 %s", got)
	}
	if got := countAudits(); got != before+2 {
		t.Fatalf("第二次改动应再记一条审计，期望 %d 条，实际 %d 条", before+2, got)
	}
}

// operator 走管理接口写（重钉 trusted）→ 判 trusted。
func TestSkillTrustTierOperatorWriteTrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-operator")
	dir := t.TempDir()
	const name = "t-trust-operator"
	writeSkillMD(t, dir, name, "v1")

	recordSkillTrust(pg, dir, name, skillTierUntrusted, skillTrustWriterAgent)
	writeSkillMD(t, dir, name, "v2")
	recordSkillTrust(pg, dir, name, skillTierTrusted, skillTrustWriterOperator)

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("operator 重钉后应为 trusted，得到 %s", got)
	}
}

// TestSkillTreeHashEmptyDirErrors 钉住空目录/缺目录返回错误：没有可复核的内容
// 就不能产出「可信」的哈希记录。
func TestSkillTreeHashEmptyDirErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "empty-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := skillTreeHash(dir, "empty-skill"); err == nil {
		t.Fatal("空目录应返回错误")
	}
	if _, err := skillTreeHash(dir, "no-such-skill"); err == nil {
		t.Fatal("目录不存在应返回错误")
	}
	// 只有 .git / 临时文件的目录等于没有有效内容，同样拒绝。
	full := filepath.Join(dir, "junk-skill")
	if err := os.MkdirAll(filepath.Join(full, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "SKILL.md.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "SKILL.md~"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := skillTreeHash(dir, "junk-skill"); err == nil {
		t.Fatal("仅 .git/临时文件的目录应视为空并返回错误")
	}
}
