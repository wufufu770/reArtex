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

// trustTestDB 返回一个连到共享测试库的句柄，并在用例结束时清掉本用例写入的
// skill_trust.* 记录——这些用例跑在同一个库上，残留会干扰后续用例。
func trustTestDBScoped(t *testing.T, names ...string) *db.DB {
	t.Helper()
	pg := trustTestDB(t)
	t.Cleanup(func() {
		for _, n := range names {
			_ = pg.SetSetting(skillTrustKeyPrefix+n, "")
		}
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

func skOf(dir, name string) skill.Skill {
	return skill.Skill{Name: name, Dir: filepath.Join(dir, name)}
}

func TestSkillTrustTierDefaultsTrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-default")
	dir := t.TempDir()
	const name = "t-trust-default"
	writeSkillMD(t, dir, name, "original")

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("无记录时应为 trusted，得到 %s", got)
	}
}

func TestSkillTrustTierUntrustedWhenAgentWrote(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-untrusted")
	dir := t.TempDir()
	const name = "t-trust-untrusted"
	writeSkillMD(t, dir, name, "agent wrote this")

	recordSkillTrust(pg, dir, name, skillTierUntrusted)
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("agent 写过的 skill 应为 untrusted，得到 %s", got)
	}
}

// 关键回归：agent 连续写两次时，第二次的哈希与记录不符。若判定逻辑把「哈希不匹配」
// 一律当作 operator 编辑，就会把 agent 刚写的 skill 误判成可信——防线就此失效。
func TestSkillTrustTierAgentWritesTwiceStaysUntrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-twice")
	dir := t.TempDir()
	const name = "t-trust-twice"
	writeSkillMD(t, dir, name, "v1")

	recordSkillTrust(pg, dir, name, skillTierUntrusted)
	writeSkillMD(t, dir, name, "v2")
	// 第二次写入同样走 ARTEX 自己的路径，故必须刷新记录。
	recordSkillTrust(pg, dir, name, skillTierUntrusted)

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierUntrusted {
		t.Fatalf("agent 连写两次后仍应为 untrusted，得到 %s", got)
	}
}

// operator 在宿主机直接改文件（不经 ARTEX 路径）→ 哈希不匹配 → 判为 operator 所为。
func TestSkillTrustTierOperatorHostEditBecomesTrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-hostedit")
	dir := t.TempDir()
	const name = "t-trust-hostedit"
	writeSkillMD(t, dir, name, "v1")

	recordSkillTrust(pg, dir, name, skillTierUntrusted)
	// 模拟 operator 在宿主机直接编辑：内容变了，但没有刷新记录。
	writeSkillMD(t, dir, name, "operator edited on host")

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("operator 宿主机编辑后应判为 trusted，得到 %s", got)
	}
	// 判定应顺带刷新记录，使后续调用稳定。
	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("刷新记录后仍应稳定为 trusted，得到 %s", got)
	}
}

// operator 走管理接口改写 → 记 trusted。
func TestSkillTrustTierOperatorWriteTrusted(t *testing.T) {
	pg := trustTestDBScoped(t, "t-trust-operator")
	dir := t.TempDir()
	const name = "t-trust-operator"
	writeSkillMD(t, dir, name, "v1")

	recordSkillTrust(pg, dir, name, skillTierUntrusted)
	writeSkillMD(t, dir, name, "v2")
	recordSkillTrust(pg, dir, name, skillTierTrusted)

	if got := skillTrustTier(pg, dir, skOf(dir, name)); got != skillTierTrusted {
		t.Fatalf("operator 写过后应为 trusted，得到 %s", got)
	}
}
