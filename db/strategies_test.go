package db

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// strategyTestSuffix 给用例生成互不撞的标识后缀。
//
// 为什么必须唯一：本包所有用例共用**同一个**派生库（db/testmain_test.go 里的
// TestMain 只派生一次），标识撞名会让后一个用例读到前一个用例留下的行。
func strategyTestSuffix() int64 { return time.Now().UnixNano() }

// 策略库（L4）Task 1 的验收用例。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 幂等 DDL（重复启动不报错、不改结构）；
//   - 删 finding **不得**连带删策略证据（证据链是策略可复现性的唯一依据，
//     它比 finding 活得久）；
//   - utility 的**权威值**在物化视图里，且随 finding 状态变化；
//   - 同名唯一按 company_id 隔离（租户之间可以同名）；
//   - 状态机由 CHECK 约束兜底（不只是应用层校验）；
//   - **正文含真实目标地址一律拒绝写入**（结构性约束，不可配置覆盖）。

// strategyTestCompany 建一个租户。companies.nkey 是 NOT NULL UNIQUE，
// 所以用带序号的 nkey 保证同包多用例互不撞。
func strategyTestCompany(t *testing.T, d *DB, seq int) int64 {
	t.Helper()
	var id int64
	nkey := fmt.Sprintf("strategy-test-%d-%d", strategyTestSuffix(), seq)
	if err := d.QueryRow(`INSERT INTO companies(name, nkey) VALUES($1, $2) RETURNING id`,
		"策略测试租户", nkey).Scan(&id); err != nil {
		t.Fatalf("建租户: %v", err)
	}
	return id
}

// strategyTestStrategy 建一条合规策略（正文只用参数化模板）。
func strategyTestStrategy(t *testing.T, d *DB, name string, companyID *int64) int64 {
	t.Helper()
	id, err := d.CreateStrategy(&Strategy{
		CompanyID: companyID,
		Name:      name,
		Summary:   "对 $TARGET 的 $PATH 做一次探测",
		Steps:     json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	return id
}

// TestEnsureStrategySchemaIsIdempotent 证明迁移幂等：
// 重复应用不会报错，也不会把已有的结构改坏。
func TestEnsureStrategySchemaIsIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	for i := 0; i < 3; i++ {
		if err := d.EnsureStrategySchema(); err != nil {
			t.Fatalf("第 %d 次应用策略库 DDL 失败: %v", i+1, err)
		}
	}

	for _, tbl := range []string{"strategies", "strategy_edges", "strategy_evidence"} {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema='public' AND table_name=$1`, tbl).Scan(&n); err != nil {
			t.Fatalf("查表 %s: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("表 %s 不存在", tbl)
		}
	}
	var mv int
	if err := d.QueryRow(`SELECT count(*) FROM pg_matviews
		WHERE schemaname='public' AND matviewname='strategy_utility'`).Scan(&mv); err != nil {
		t.Fatalf("查物化视图: %v", err)
	}
	if mv != 1 {
		t.Fatalf("物化视图 strategy_utility 不存在")
	}
}

// TestStrategyEvidenceSurvivesFindingDeletion 钉住「删 finding 不连带删策略证据」。
//
// 这条承诺是策略库的地基：finding 可以被撤回、被合并、被判误报，但策略的
// 可复现性来自证据链；证据跟着 finding 一起消失，策略就变成没有出处的断言。
//
// 因此 strategy_evidence.ref_id 是**裸 id + 类型**，没有指回 findings 的外键 ——
// 有了外键，ON DELETE CASCADE 会在删 finding 时把证据一起带走。
func TestStrategyEvidenceSurvivesFindingDeletion(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	task, err := d.CreateTask("证据存活测试", "验证删 finding 不连带删策略证据", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	findingID, err := d.AddFinding(task.ID, 0, "sqli", "测试漏洞", "high", "摘要", "证据文本", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}

	strategyID := strategyTestStrategy(t, d, fmt.Sprintf("证据存活策略-%d", findingID), nil)

	if _, err := d.Exec(`INSERT INTO strategy_evidence(strategy_id, ref_type, ref_id, content_digest, role)
		VALUES($1, 'finding', $2, $3, 'created_by')`,
		strategyID, fmt.Sprint(findingID), "sha256:deadbeef"); err != nil {
		t.Fatalf("挂策略证据: %v", err)
	}

	n, err := d.DeleteFinding(findingID)
	if err != nil {
		t.Fatalf("删 finding: %v", err)
	}
	if n == 0 {
		t.Fatalf("删 finding 没有删掉任何行")
	}

	var cnt int
	if err := d.QueryRow(`SELECT count(*) FROM strategy_evidence
		WHERE strategy_id=$1 AND ref_type='finding' AND ref_id=$2`,
		strategyID, fmt.Sprint(findingID)).Scan(&cnt); err != nil {
		t.Fatalf("查策略证据: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("删 finding 后策略证据被连带删除：期望 1 行，实际 %d 行", cnt)
	}
}

// TestStrategyUtilityViewTracksFindingStatus 证明 utility 的**权威值**在物化视图里，
// 且随 finding 的处置状态变化（confirmed 才计入分子）。
//
// 静态列 strategies.utility 只是缓存 —— 权威值必须能从视图重算出来，
// 否则「证据被撤回 → utility 掉到阈值以下 → 触发淘汰」这条收益就不成立。
func TestStrategyUtilityViewTracksFindingStatus(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	task, err := d.CreateTask("utility 视图测试", "验证 utility 权威值随 finding 状态变化", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	confirmedID, err := d.AddFinding(task.ID, 0, "ssrf", "已确认漏洞", "high", "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	if _, err := d.Exec(`UPDATE findings SET status='confirmed' WHERE id=$1`, confirmedID); err != nil {
		t.Fatalf("置 finding 为 confirmed: %v", err)
	}
	pendingID, err := d.AddFinding(task.ID, 0, "xss", "待处置漏洞", "low", "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}

	strategyID := strategyTestStrategy(t, d, fmt.Sprintf("utility 视图策略-%d", confirmedID), nil)
	for _, fid := range []int64{confirmedID, pendingID} {
		if _, err := d.Exec(`INSERT INTO strategy_evidence(strategy_id, ref_type, ref_id, content_digest, role)
			VALUES($1, 'finding', $2, $3, 'created_by')`,
			strategyID, fmt.Sprint(fid), "sha256:cafe"); err != nil {
			t.Fatalf("挂策略证据: %v", err)
		}
	}

	if _, err := d.Exec(`REFRESH MATERIALIZED VIEW strategy_utility`); err != nil {
		t.Fatalf("刷新物化视图: %v", err)
	}

	var raw float64
	var evidenceCount int
	if err := d.QueryRow(`SELECT utility_raw, evidence_count FROM strategy_utility WHERE strategy_id=$1`,
		strategyID).Scan(&raw, &evidenceCount); err != nil {
		t.Fatalf("读 utility 视图: %v", err)
	}
	if evidenceCount != 2 {
		t.Fatalf("视图证据条数不对：期望 2，实际 %d", evidenceCount)
	}
	// 一条 confirmed 一条 pending ⇒ 均值 0.5
	if raw < 0.49 || raw > 0.51 {
		t.Fatalf("utility 权威值不对：期望 0.5，实际 %v", raw)
	}

	// 把 confirmed 撤回（改成 pending）⇒ 均值掉到 0 ⇒ 「证据被撤回 → utility 反映到
	// utility」这条收益成立。
	if _, err := d.Exec(`UPDATE findings SET status='pending' WHERE id=$1`, confirmedID); err != nil {
		t.Fatalf("撤回 finding: %v", err)
	}
	if _, err := d.Exec(`REFRESH MATERIALIZED VIEW strategy_utility`); err != nil {
		t.Fatalf("刷新物化视图: %v", err)
	}
	if err := d.QueryRow(`SELECT utility_raw FROM strategy_utility WHERE strategy_id=$1`,
		strategyID).Scan(&raw); err != nil {
		t.Fatalf("读 utility 视图: %v", err)
	}
	if raw != 0 {
		t.Fatalf("撤回 confirmed 后 utility 应掉到 0，实际 %v", raw)
	}
}

// TestStrategyNameUniquePerCompany 钉住「同名唯一按 company_id 隔离」。
//
// 唯一索引用 COALESCE(company_id,0) 归一（company_id 可空，NULL 之间互不相等，
// 不归一的话同名平台级策略可以无限重复插入）。
func TestStrategyNameUniquePerCompany(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	companyA := strategyTestCompany(t, d, 1)
	companyB := strategyTestCompany(t, d, 2)
	name := fmt.Sprintf("同名策略-%d", strategyTestSuffix())

	// 同租户同名 ⇒ 第二次必须失败
	strategyTestStrategy(t, d, name, &companyA)
	if _, err := d.CreateStrategy(&Strategy{
		CompanyID: &companyA,
		Name:      name,
		Summary:   "重复",
		Steps:     json.RawMessage(`["curl https://$TARGET/$PATH"]`),
	}); err == nil {
		t.Fatalf("同租户同名策略被重复插入（应当失败）")
	}

	// 跨租户同名 ⇒ 允许（租户之间互不可见）
	strategyTestStrategy(t, d, name, &companyB)

	// 平台级（company_id 为 NULL）同名 ⇒ 也必须失败（COALESCE 归一生效）
	platformName := fmt.Sprintf("平台级同名策略-%d", strategyTestSuffix())
	strategyTestStrategy(t, d, platformName, nil)
	if _, err := d.CreateStrategy(&Strategy{
		Name:    platformName,
		Summary: "重复",
		Steps:   json.RawMessage(`["curl https://$TARGET/$PATH"]`),
	}); err == nil {
		t.Fatalf("平台级同名策略被重复插入（应当失败）")
	}
}

// TestStrategyStateCheckConstraint 证明状态机由数据库 CHECK 兜底，
// 不只是应用层校验 —— 直接绕过应用层写库也进不去非法状态。
func TestStrategyStateCheckConstraint(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	legal := fmt.Sprintf("合法状态策略-%d", strategyTestSuffix())
	if _, err := d.CreateStrategy(&Strategy{
		Name:    legal,
		Summary: "模板 $TARGET",
		Steps:   json.RawMessage(`["curl https://$TARGET/$PATH"]`),
	}); err != nil {
		t.Fatalf("建合法状态策略: %v", err)
	}

	// 应用层先挡：非 quarantined 的新建必须被拒（晋级是裁决写）
	if _, err := d.CreateStrategy(&Strategy{
		Name:    fmt.Sprintf("越权晋级-%d", strategyTestSuffix()),
		Summary: "模板 $TARGET",
		Steps:   json.RawMessage(`["curl https://$TARGET/$PATH"]`),
		State:   StrategyActive,
	}); err == nil {
		t.Fatalf("新建时直接写 active 被放行（应当拒绝：晋级只能由宿主侧裁决写发生）")
	}

	// 数据库层兜底：绕过应用层写非法状态必须被 CHECK 挡住
	illegal := fmt.Sprintf("非法状态策略-%d", strategyTestSuffix())
	if _, err := d.Exec(`INSERT INTO strategies(name, summary, state)
		VALUES($1, $2, 'not-a-state')`, illegal, "模板 $TARGET"); err == nil {
		t.Fatalf("CHECK 约束没挡住非法状态（应当失败）")
	}

	// 四种合法状态都进得去
	for _, st := range []string{StrategyQuarantined, StrategyActive, StrategyRetired, StrategyTombstone} {
		nm := fmt.Sprintf("状态集齐-%s-%d", st, strategyTestSuffix())
		if _, err := d.Exec(`INSERT INTO strategies(name, summary, state) VALUES($1,$2,$3)`,
			nm, "模板 $TARGET", st); err != nil {
			t.Fatalf("合法状态 %s 被拒: %v", st, err)
		}
	}
}

// TestCreateStrategyRejectsRealTargetAddresses 钉住**结构性约束**：
// 正文里出现形似域名 / IP / URL 的字面量 ⇒ 拒绝写入，且库里不落行。
//
// 为什么必须是「拒绝」而不是「提醒」：正文带真实地址 ⇒ 策略退化成「某个目标的
// 攻略」，进了检索注入就可能被套到别的资产上（把 A 的路径打到 B 的资产上）。
// 可复现性由证据链保证，不由正文里的硬编码地址保证。
//
// 这是**不可配置覆盖**的约束：没有开关、没有环境变量、没有 hot-fix。
func TestCreateStrategyRejectsRealTargetAddresses(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	cases := []struct {
		name string
		body string
	}{
		{"URL", `["curl -X POST https://target.example.com/login"]`},
		{"裸域名", `["nslookup target.example.com"]`},
		{"IPv4", `["nmap -p 80 10.0.0.5"]`},
		{"IPv6", `["ping6 2001:db8::1"]`},
	}
	for i, c := range cases {
		nm := fmt.Sprintf("真实地址-%s-%d-%d", c.name, strategyTestSuffix(), i)
		id, err := d.CreateStrategy(&Strategy{
			Name:    nm,
			Summary: "含真实目标地址的正文",
			Steps:   json.RawMessage(c.body),
		})
		if err == nil {
			t.Fatalf("%s 形态的真实目标地址被放行（应当拒绝）", c.name)
		}
		if id != 0 {
			t.Fatalf("%s 形态拒绝后仍返回了 id %d", c.name, id)
		}
		// 库里必须没有这一行（拒绝 = 不落行，不是落行后打标记）
		var cnt int
		if err := d.QueryRow(`SELECT count(*) FROM strategies WHERE name=$1`, nm).Scan(&cnt); err != nil {
			t.Fatalf("查策略: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("%s 形态被拒绝后库里仍留了 %d 行", c.name, cnt)
		}
	}

	// 反面：参数化模板必须进得去（否则策略根本写不进去）
	ok := fmt.Sprintf("模板策略-%d", strategyTestSuffix())
	if _, err := d.CreateStrategy(&Strategy{
		Name:    ok,
		Summary: "对 $TARGET 的 $PATH 做一次探测",
		Steps:   json.RawMessage(`["curl -X POST https://$TARGET/$PATH -H 'X-Trace: $RUN_ID'"]`),
	}); err != nil {
		t.Fatalf("参数化模板被误拒（不该误伤）: %v", err)
	}
}
