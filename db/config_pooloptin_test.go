package db

// config_pooloptin_test.go：LLM 轮询池「显式加入」语义（缺陷②③修复）验收——
// 一次性迁移把非激活配置退出故障转移链 + 水位防重跑（迁移后用户手动勾选的
// 「参与轮询」绝不被改写）+ 会话 pin 列举（删除配置时收口会话的前置读取面）。
//
// 数据库副作用如实申报：本测试会在共享 dev 库上落下水位键
// llm_pool_optin_migrated（与启动期 MigrateLLMPoolOptIn 的效果一致，绝不删除
// ——删掉会让下一次启动重新改写用户显式勾选过的配置）。测试自身的 profile/
// conversation 夹具全部清理。

import (
	"testing"
)

func poolOptinWipeWatermark(t *testing.T, d *DB) {
	t.Helper()
	if _, err := d.Exec(`DELETE FROM settings WHERE key=$1`, llmPoolOptInWatermark); err != nil {
		t.Fatalf("清水位: %v", err)
	}
}

// 迁移主链路：非激活配置 pool_exclude=false → 迁移翻成 true（退出链）→ 激活
// 配置不动 → 水位落定；再手动放一个「参与轮询」的非激活配置后重跑迁移 → 不
// 被改写（水位防重跑）。
func TestMigrateLLMPoolOptIn(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	poolOptinWipeWatermark(t, d)

	headID, err := d.SaveProfile(&LLMProfile{Name: "t-optin-head", Format: "openai", Model: "m", APIKey: "k-head"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteProfile(headID) }()
	otherID, err := d.SaveProfile(&LLMProfile{Name: "t-optin-other", Format: "openai", Model: "m", APIKey: "k-other"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteProfile(otherID) }()
	if err := d.SetActiveProfile(headID); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, headID) }()
	// 迁移前：other 未激活但未排除（旧语义=自动进链）。
	if _, err := d.Exec(`UPDATE llm_profiles SET pool_exclude=false WHERE id=$1`, otherID); err != nil {
		t.Fatal(err)
	}

	if err := d.MigrateLLMPoolOptIn(); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	other, err := d.ProfileByID(otherID)
	if err != nil || other == nil {
		t.Fatalf("查 other: %+v err=%v", other, err)
	}
	if !other.PoolExclude {
		t.Fatal("非激活配置迁移后应退出轮询链（pool_exclude=true）")
	}
	head, err := d.ProfileByID(headID)
	if err != nil || head == nil || head.PoolExclude {
		t.Fatalf("激活配置不得被改: %+v err=%v", head, err)
	}
	var done bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1)`, llmPoolOptInWatermark).Scan(&done); err != nil || !done {
		t.Fatalf("水位应落定: done=%v err=%v", done, err)
	}

	// 水位在位：用户显式勾选「参与轮询」后重跑迁移绝不被改写。
	if _, err := d.Exec(`UPDATE llm_profiles SET pool_exclude=false WHERE id=$1`, otherID); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateLLMPoolOptIn(); err != nil {
		t.Fatalf("重跑迁移: %v", err)
	}
	other2, _ := d.ProfileByID(otherID)
	if other2 == nil || other2.PoolExclude {
		t.Fatalf("水位后用户显式加入不得被改写: %+v", other2)
	}
}

// 幂等：已带水位时迁移是 no-op（不碰任何行）。
func TestMigrateLLMPoolOptInIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if _, err := d.Exec(`INSERT INTO settings(key, value) VALUES($1, 'seed') ON CONFLICT (key) DO UPDATE SET value='seed'`, llmPoolOptInWatermark); err != nil {
		t.Fatal(err)
	}
	pid, err := d.SaveProfile(&LLMProfile{Name: "t-optin-idem", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteProfile(pid) }()
	if _, err := d.Exec(`UPDATE llm_profiles SET pool_exclude=false WHERE id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateLLMPoolOptIn(); err != nil {
		t.Fatalf("带水位重跑: %v", err)
	}
	p, _ := d.ProfileByID(pid)
	if p == nil || p.PoolExclude {
		t.Fatalf("带水位重跑必须是 no-op: %+v", p)
	}
}

// ConversationIDsByProfile：pin 的会话被列出、未 pin 的不列、不存在的配置=空。
func TestConversationIDsByProfile(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	pid, err := d.SaveProfile(&LLMProfile{Name: "t-convid", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteProfile(pid) }()
	pinned, err := d.CreateConversation("planner", "pin 的会话", &pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteConversation(pinned.ID) }()
	free, err := d.CreateConversation("planner", "没 pin 的会话", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteConversation(free.ID) }()

	ids, err := d.ConversationIDsByProfile(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != pinned.ID {
		t.Fatalf("应恰列出 pin 的会话: %v", ids)
	}
	ghost, err := d.ConversationIDsByProfile(999999999)
	if err != nil || len(ghost) != 0 {
		t.Fatalf("不存在的配置应空: %v err=%v", ghost, err)
	}
	// 删除 profile 后 pin 被 FK 置空：再列举=空（这正是删除前必须先读的原因）。
	if err := d.DeleteProfile(pid); err != nil {
		t.Fatalf("删除 profile: %v", err)
	}
	after, err := d.ConversationIDsByProfile(pid)
	if err != nil || len(after) != 0 {
		t.Fatalf("删除后列举应空: %v err=%v", after, err)
	}
	if c, err := d.GetConversation(pinned.ID); err != nil || c == nil || c.LLMProfileID != nil {
		t.Fatalf("删除后会话 pin 应被 FK 置空: %+v err=%v", c, err)
	}
}
