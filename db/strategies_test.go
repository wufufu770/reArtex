package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// strategyTestSuffix 给用例生成互不撞的标识后缀。
//
// 为什么必须唯一：本包所有用例共用**同一个**派生库（db/testmain_test.go 里的
// TestMain 只派生一次），标识撞名会让后一个用例读到前一个用例留下的行。
func strategyTestSuffix() int64 { return time.Now().UnixNano() }

// strategyTestBaseDSN 记住进程启动时的 ARTEX_PG_DSN（包初始化时读一次）。
var strategyTestBaseDSN = os.Getenv("ARTEX_PG_DSN")

// strategyTestDerivedDB 是 internal/testdb.Setup("db") 派生的库名。
//
// 命名规则在 internal/testdb/testdb.go（"artex_test_" + 包名）。这里只用它来
// **重建连接串**，不重建库 —— 重建会把本包自己的库删掉。
const strategyTestDerivedDB = "artex_test_db"

// strategyTestDSN 取测试库连接串。
//
// ⚠️ 为什么不能直接用 testDSN：db.DSN() 读入后会把 ARTEX_PG_DSN 从进程环境里
// 抹掉（防 agent 敲一条 env 读出数据库口令，见 db/db.go 的注释），而
// db/pgdsn_env_test.go 的用例会把缓存清空、并把环境变量**还原成空** ——
// 排在它后面的数据库用例于是全都拿不到连接串，被 skip 掉（整包跑时这不是偶发：
// 前面的用例早已把环境变量吃掉了）。
//
// 本文件按文件名排在那个用例之后，所以在自己的文件里把连接串兜回来：按 testdb
// 的派生名重建连接串、确认库还在，再把缓存填上。修复只在本文件内生效（退出时
// 把缓存与环境变量还原成原样）⇒ 其余用例的行为不变。
//
// 真正的修法在 db/pgdsn_env_test.go（把缓存还原成「原值」而不是空值），那个文件
// 不在本单元的文件域内。
func strategyTestDSN(t *testing.T) string {
	t.Helper()
	dsn, _, err := DSN()
	if err == nil && dsn != "" {
		return dsn
	}
	base := strategyTestBaseDSN
	if base == "" {
		t.Skipf("没有数据库配置（%v）—— 跳过", err)
	}
	u, perr := url.Parse(base)
	if perr != nil {
		t.Skipf("没有数据库配置（%v）—— 跳过", err)
	}
	u.Path = "/" + strategyTestDerivedDB
	conn, perr := sql.Open("pgx", u.String())
	if perr == nil {
		perr = conn.Ping()
		_ = conn.Close()
	}
	if perr != nil {
		t.Skipf("派生测试库不可用（%v）—— 跳过", perr)
	}
	_ = os.Setenv("ARTEX_PG_DSN", u.String())
	dsnCached, dsnSource = "", ""
	t.Cleanup(func() {
		_ = os.Unsetenv("ARTEX_PG_DSN")
		dsnCached, dsnSource = "", ""
	})
	dsn, _, err = DSN()
	if err != nil {
		t.Skipf("没有数据库配置（%v）—— 跳过", err)
	}
	return dsn
}

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
	d, err := Open(strategyTestDSN(t))
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
	d, err := Open(strategyTestDSN(t))
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
	d, err := Open(strategyTestDSN(t))
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
	d, err := Open(strategyTestDSN(t))
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
	d, err := Open(strategyTestDSN(t))
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
	d, err := Open(strategyTestDSN(t))
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

// ---------- Task 2：访问层（检索 / 证据链 / 跨层遍历） ----------

// 策略库（L4）Task 2 的验收用例。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 检索只返 active，quarantined/retired 绝不进注入；
//   - 候选集是【结构路径 ∪ 字面路径】的并集，准入是「结构匹配 OR 字面阈值」；
//   - 衰减只影响排序，**永不过滤**候选；
//   - 硬顶 top-k 生效；
//   - activation 是**编译成 SQL 的谓词**，不是运行时解释；
//   - 跨层遍历的 hop 上限真的生效；
//   - 资产签名走 scope 注入版查询（范围外资产进不来）；
//   - 检索热路径上**没有任何模型调用**；
//   - 检索结果不携带策略正文（steps）。

// strategyTestMarker 给每个用例一个互不撞的标记。
//
// 用途：签名 / 激活谓词 / 名字都靠它隔离 —— 本包所有用例共用同一个派生库，
// 不隔离的话一个用例的候选会漏进另一个用例的结果集。
func strategyTestMarker(tag string) string {
	return fmt.Sprintf("%s-%d", tag, strategyTestSuffix())
}

// strategyTestState 建一条指定状态的策略。
//
// 新建只允许 quarantined（晋级是裁决写，只能在宿主侧发生）⇒ 这里直接改状态，
// 与 Task 1 的状态机用例同款做法。
func strategyTestState(t *testing.T, d *DB, name, summary, activation, state string) int64 {
	t.Helper()
	id, err := d.CreateStrategy(&Strategy{
		Name:       name,
		Summary:    summary,
		Activation: json.RawMessage(activation),
		Steps:      json.RawMessage(`["curl -X POST https://$TARGET/$PATH"]`),
	})
	if err != nil {
		t.Fatalf("建策略 %s: %v", name, err)
	}
	if _, err := d.Exec(`UPDATE strategies SET state=$2 WHERE id=$1`, id, state); err != nil {
		t.Fatalf("置策略状态 %s: %v", state, err)
	}
	return id
}

// strategyTestFind 在结果集里找一条策略（找不到返回 nil）。
func strategyTestFind(matches []StrategyMatch, id int64) *StrategyMatch {
	for i := range matches {
		if matches[i].ID == id {
			return &matches[i]
		}
	}
	return nil
}

// strategyTestContains 判断字符串集合里有没有某个值。
func strategyTestContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// TestSearchActiveStrategiesOnlyReturnsActive 证明检索只返 active。
//
// quarantined / retired 绝不进检索注入：隔离态的策略还没被裁决过，退役的策略
// 已经被判死 —— 任何一条进了上下文都是在向执行侧投递未经裁决的指令。
func TestSearchActiveStrategiesOnlyReturnsActive(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("only-active")
	task, err := d.CreateTask("检索状态用例", "把范围里的策略捞出来", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q]}`, marker)
	quarantined := strategyTestState(t, d, "隔离态-"+marker, "隔离态策略", activation, StrategyQuarantined)
	active := strategyTestState(t, d, "生效态-"+marker, "生效态策略", activation, StrategyActive)
	retired := strategyTestState(t, d, "退役态-"+marker, "退役态策略", activation, StrategyRetired)

	matches, err := d.SearchActiveStrategies(task.ID, AssetSignature{AssetTypes: []string{marker}}, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if strategyTestFind(matches, active) == nil {
		t.Fatalf("active 策略没被检索到")
	}
	for _, id := range []int64{quarantined, retired} {
		if strategyTestFind(matches, id) != nil {
			t.Fatalf("非 active 状态（id=%d）被检索注入", id)
		}
	}
}

// TestSearchActiveStrategiesNeverFiltersByDecay 钉住「衰减只影响排序」。
//
// 一条很久没被用过的策略，衰减会掉到地板 0.3 —— 但它必须**仍在结果里**。
// 衰减一旦能过滤，策略库就会「突然失忆」：一条冷门但正确的策略在某天悄悄消失，
// 而且没有任何审计痕迹能解释它去哪了。
func TestSearchActiveStrategiesNeverFiltersByDecay(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("decay")
	task, err := d.CreateTask("衰减用例", "另起一句毫不相干的话", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q],"service_type":[%q]}`, marker, marker)
	id := strategyTestState(t, d, "老策略-"+marker, "很久没被用过的策略", activation, StrategyActive)
	// 让它「很久没被用过」：updated_at 被 BEFORE UPDATE 触发器刷成 now()，
	// 所以先把触发器关掉（退出时恢复）。
	if _, err := d.Exec(`ALTER TABLE strategies DISABLE TRIGGER trg_strategies_upd`); err != nil {
		t.Fatalf("关 updated_at 触发器: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`ALTER TABLE strategies ENABLE TRIGGER trg_strategies_upd`) })
	if _, err := d.Exec(`UPDATE strategies SET updated_at = now() - interval '400 days' WHERE id=$1`, id); err != nil {
		t.Fatalf("把策略改成很久没用: %v", err)
	}

	sig := AssetSignature{AssetTypes: []string{marker}, ServiceTypes: []string{marker}}
	matches, err := d.SearchActiveStrategies(task.ID, sig, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	m := strategyTestFind(matches, id)
	if m == nil {
		t.Fatalf("衰减把策略滤掉了（衰减只该影响排序，不该过滤候选）")
	}
	if m.Decay > StrategyDecayFloor+1e-9 {
		t.Fatalf("老策略的衰减应落到地板 %.2f，实际 %v", StrategyDecayFloor, m.Decay)
	}
}

// TestSearchActiveStrategiesEnforcesTopKAndThreshold 钉住硬顶 top-k。
//
// 传进来的 limit 再大也被压到 StrategySearchTopK —— top-k 是上下文预算的硬约束，
// 不是「调用方给多少就给多少」。
func TestSearchActiveStrategiesEnforcesTopKAndThreshold(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("topk")
	task, err := d.CreateTask("top-k 用例", "批量召回的边界检查", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q]}`, marker)
	ids := make([]int64, 0, 15)
	for i := 0; i < 15; i++ {
		ids = append(ids, strategyTestState(t, d,
			fmt.Sprintf("批量策略-%s-%d", marker, i), "批量策略正文", activation, StrategyActive))
	}
	// 一条 activation 完全不匹配的：它既不该走结构路径，字面相似度也不够。
	other := strategyTestState(t, d, "不匹配-"+marker, "另一类别的策略",
		fmt.Sprintf(`{"asset_type":[%q]}`, marker+"-别的类型"), StrategyActive)

	sig := AssetSignature{AssetTypes: []string{marker}}
	matches, err := d.SearchActiveStrategies(task.ID, sig, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if len(matches) > StrategySearchTopK {
		t.Fatalf("返回条数 %d 超过硬顶 top-k %d", len(matches), StrategySearchTopK)
	}
	// 即使调用方要求更多，也必须被硬顶压住。
	wide, err := d.SearchActiveStrategies(task.ID, sig, 100, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if len(wide) > StrategySearchTopK {
		t.Fatalf("硬顶 top-k 失效：limit=100 时返回了 %d 条", len(wide))
	}
	if strategyTestFind(wide, other) != nil {
		t.Fatalf("activation 完全不匹配的策略被返回")
	}
	hits := 0
	for _, id := range ids {
		if strategyTestFind(wide, id) != nil {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("15 条可命中的策略一条都没返回")
	}
}

// TestSearchActiveStrategiesActivationPredicateIsCompiledToSQL 钉住
// 「activation 是 SQL 谓词，不是运行时解释」。
//
// 签名命中 ⇒ 进结果；签名不命中 ⇒ 不进结果。判定的位置在 SQL 的 WHERE 里
// （可直接命中 activation 上的 GIN 索引），不在 Go 侧的 if 里。
func TestSearchActiveStrategiesActivationPredicateIsCompiledToSQL(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("predicate")
	task, err := d.CreateTask("谓词用例", "另一句毫不相干的话", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q]}`, marker)
	id := strategyTestState(t, d, "谓词策略-"+marker, "谓词策略正文", activation, StrategyActive)

	hit, err := d.SearchActiveStrategies(task.ID, AssetSignature{AssetTypes: []string{marker}}, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if strategyTestFind(hit, id) == nil {
		t.Fatalf("activation 与签名一致却没被检索到")
	}

	miss, err := d.SearchActiveStrategies(task.ID, AssetSignature{AssetTypes: []string{"ip"}}, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if strategyTestFind(miss, id) != nil {
		t.Fatalf("activation 与签名不一致却被检索到（谓词没有编译成 SQL 过滤）")
	}
}

// TestSearchReturnsStructurallyMatchedDespiteNoLexicalOverlap 是本阶段最关键的一条。
//
// 场景：策略的 activation 命中（同一个漏洞类的同一个形状），但 name+summary 与
// 任务文本的字符 bigram 相似度 ≈ 0（措辞完全不同）。
//
// 断言：
//  1. 该策略**出现在结果里** —— 结构路径命中 ⇒ 不得被字面阈值硬过滤；
//  2. 结果里标记 StructuralMatch=true；
//  3. 把 activation 改成不匹配的类型 ⇒ 它不再出现
//     ⇒ 结构路径确实在起作用，而不是「碰巧被字面路径捞回来的」。
//
// 为什么必须这样：接近一半的高度相关命中落在 Jaccard < 0.05 区间，且字面重叠
// 与相关性的相关性只有 r ≈ −0.29 —— 相关性主要由过程相似性驱动。只做字面匹配
// 会漏掉整个「同一个漏洞类、措辞完全不同」的类别。
func TestSearchReturnsStructurallyMatchedDespiteNoLexicalOverlap(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("structural")
	task, err := d.CreateTask("结构路径用例", "审计数据库口令强度与权限配置", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q],"service_type":[%q]}`, marker, marker)
	id := strategyTestState(t, d, "探针族-站点指纹命中法-"+marker, "用服务指纹定位入口", activation, StrategyActive)

	sig := AssetSignature{AssetTypes: []string{marker}, ServiceTypes: []string{marker}}
	matches, err := d.SearchActiveStrategies(task.ID, sig, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	m := strategyTestFind(matches, id)
	if m == nil {
		t.Fatalf("结构命中的策略没被检索到（结构路径被字面阈值误杀了）")
	}
	if !m.StructuralMatch {
		t.Fatalf("结构命中的候选没有标记 StructuralMatch")
	}
	if m.LexicalSimilarity >= StrategyMinRelevance {
		t.Fatalf("用例前提不成立：字面相似度 %v 不该达到阈值 %v", m.LexicalSimilarity, StrategyMinRelevance)
	}
	// 相关性阈值再高也不该把结构命中的候选筛掉。
	strict, err := d.SearchActiveStrategies(task.ID, sig, StrategySearchTopK, 0.99)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if strategyTestFind(strict, id) == nil {
		t.Fatalf("结构命中的候选被字面阈值硬过滤了（准入必须是 结构匹配 OR 字面阈值）")
	}

	// 把 activation 改成一个不匹配的类型 ⇒ 它不再出现。
	if _, err := d.Exec(`UPDATE strategies SET activation = jsonb_build_object('asset_type', jsonb_build_array($2::text)) WHERE id=$1`,
		id, marker+"-别的类型"); err != nil {
		t.Fatalf("改 activation: %v", err)
	}
	after, err := d.SearchActiveStrategies(task.ID, sig, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if strategyTestFind(after, id) != nil {
		t.Fatalf("activation 不匹配后仍被检索到")
	}
}

// TestSearchActiveStrategiesNeverExposesSteps 钉住「检索结果不携带正文」。
//
// 策略的 steps 是参数化模板，但注入路径只需要摘要 —— 正文一旦随检索结果流出去，
// 就可能被直接塞进 planner 上下文，绕过「逐条过既有拦截规则」的那道闸
// （本阶段不执行策略，但闸不能从读路径上被绕过）。
func TestSearchActiveStrategiesNeverExposesSteps(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("steps")
	task, err := d.CreateTask("正文用例", "另一句毫不相干的话", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q]}`, marker)
	id := strategyTestState(t, d, "正文策略-"+marker, "正文策略摘要", activation, StrategyActive)

	matches, err := d.SearchActiveStrategies(task.ID, AssetSignature{AssetTypes: []string{marker}}, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	m := strategyTestFind(matches, id)
	if m == nil {
		t.Fatalf("策略没被检索到")
	}
	if len(m.Steps) != 0 {
		t.Fatalf("检索结果带着策略正文 steps：%s", string(m.Steps))
	}
	// 正文按需单独取 —— 读路径只是「不主动带」，不是「取不到」。
	full, err := d.GetStrategy(id)
	if err != nil || full == nil {
		t.Fatalf("读策略: %v", err)
	}
	if len(full.Steps) == 0 {
		t.Fatalf("策略正文丢了")
	}
}

// TestStrategyEvidenceChainRoundTrip 钉住证据链的读写往返。
//
// 两条链一起验：finding 引用（数字 id 的字符串形态）与 traffic 快照引用
// （**TEXT 主键**）。ref_id 是 TEXT 不是 BIGINT —— traffic_evidence_snapshots.id
// 装不进 BIGINT，用错列类型这条链根本写不进去。
func TestStrategyEvidenceChainRoundTrip(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	task, err := d.CreateTask("证据链用例", "验证证据链的读写往返", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	findingID, err := d.AddFinding(task.ID, 0, "sqli", "证据链漏洞", "high", "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	snapshotID := "snap-" + fmt.Sprint(strategyTestSuffix())
	if _, err := d.Exec(`
INSERT INTO traffic_evidence_snapshots(id, source_traffic_id, captured_at, url, method, status,
                                        req_head, resp_head, req_hash, resp_hash, req_len, resp_len)
VALUES($1,$2,0,$3,'GET',200,'','','','',0,0)`,
		snapshotID, "src-"+snapshotID, "https://example.test/"+snapshotID); err != nil {
		t.Fatalf("建 traffic 快照: %v", err)
	}

	strategyID := strategyTestStrategy(t, d, "证据链策略-"+strategyTestMarker("evidence"), nil)
	if err := d.LinkStrategyEvidence(strategyID, "finding", fmt.Sprint(findingID), "created_by", "sha256:aaa"); err != nil {
		t.Fatalf("挂 finding 证据: %v", err)
	}
	if err := d.LinkStrategyEvidence(strategyID, "traffic_snapshot", snapshotID, "validated_by", "sha256:bbb"); err != nil {
		t.Fatalf("挂 traffic 快照证据: %v", err)
	}

	links, err := d.StrategyEvidence(strategyID)
	if err != nil {
		t.Fatalf("读策略证据: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("证据链条数不对：期望 2，实际 %d", len(links))
	}
	byRef := map[string]StrategyEvidenceLink{}
	for _, l := range links {
		byRef[l.RefType] = l
	}
	findingLink, ok := byRef["finding"]
	if !ok {
		t.Fatalf("证据链里没有 finding 引用：%+v", links)
	}
	if findingLink.RefID != fmt.Sprint(findingID) || findingLink.Role != "created_by" || findingLink.ContentDigest != "sha256:aaa" {
		t.Fatalf("finding 证据对不上：%+v", findingLink)
	}
	trafficLink, ok := byRef["traffic_snapshot"]
	if !ok {
		t.Fatalf("证据链里没有 traffic 快照引用：%+v", links)
	}
	if trafficLink.RefID != snapshotID || trafficLink.Role != "validated_by" || trafficLink.ContentDigest != "sha256:bbb" {
		t.Fatalf("traffic 快照证据对不上：%+v", trafficLink)
	}
}

// TestLinkStrategyEvidenceRejectsMissingRef 钉住「引用必须在写入时校验」。
//
// strategy_evidence.ref_id 是裸 id、没有级联外键（任务归档不得连带删掉策略证据
// 链），所以「引用真实存在」这件事只能由写入路径自己把关 —— 否则一条凭空捏造
// 的引用就是「凭空提权」。
func TestLinkStrategyEvidenceRejectsMissingRef(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	strategyID := strategyTestStrategy(t, d, "证据校验策略-"+strategyTestMarker("refcheck"), nil)
	if err := d.LinkStrategyEvidence(strategyID, "finding", "999999999", "created_by", "sha256:ccc"); err == nil {
		t.Fatalf("引用不存在的 finding 被放行（应当拒绝）")
	}
	if err := d.LinkStrategyEvidence(strategyID, "not-a-type", "1", "created_by", "sha256:ccc"); err == nil {
		t.Fatalf("未知的证据引用类型被放行（应当拒绝）")
	}
	if err := d.LinkStrategyEvidence(strategyID, "finding", "1", "not-a-role", "sha256:ccc"); err == nil {
		t.Fatalf("未知的证据角色被放行（应当拒绝）")
	}
	links, err := d.StrategyEvidence(strategyID)
	if err != nil {
		t.Fatalf("读策略证据: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("被拒绝的写入仍落了行：%+v", links)
	}
}

// TestTraverseFromFindingRespectsHopLimit 钉住「hop 上限真的生效」。
//
// 造一条 finding ← intent ← fact 的链（探索图里 finding 的出处在入边上：
// intent --yields--> finding、fact --derived_from--> intent）。
//
// maxDepth=1 只回得到 intent；maxDepth=4 才回得到 fact。
// ⇒ hop 上限不是摆设。
func TestTraverseFromFindingRespectsHopLimit(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	task, err := d.CreateTask("遍历用例", "从漏洞回溯到意图与事实", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	exp := d.Exploration(task.ExplorationID)
	factID, err := exp.AddNode(KindFact, map[string]any{"summary": "事实节点"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatalf("建事实节点: %v", err)
	}
	intentID, err := exp.AddNode(KindIntent, map[string]any{"summary": "意图节点"}, 10, "open", "planner", nil)
	if err != nil {
		t.Fatalf("建意图节点: %v", err)
	}
	findingNodeID, err := exp.AddNode(KindFinding, map[string]any{"summary": "漏洞节点"}, 9, "confirmed", "worker", nil)
	if err != nil {
		t.Fatalf("建漏洞节点: %v", err)
	}
	if err := exp.Link(intentID, RelYields, findingNodeID); err != nil {
		t.Fatalf("连 intent→finding: %v", err)
	}
	if err := exp.Link(factID, RelDerivedFrom, intentID); err != nil {
		t.Fatalf("连 fact→intent: %v", err)
	}
	findingID, err := d.AddFinding(task.ID, findingNodeID, "sqli", "遍历漏洞", "high", "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}

	one, err := d.TraverseFromFinding(findingID, 1)
	if err != nil {
		t.Fatalf("跨层遍历: %v", err)
	}
	if !strategyTestHopHasNode(one, intentID) {
		t.Fatalf("1 跳内应回得到 intent 节点")
	}
	if strategyTestHopHasNode(one, factID) {
		t.Fatalf("1 跳内不该回得到 fact 节点（hop 上限没生效）")
	}

	deep, err := d.TraverseFromFinding(findingID, 4)
	if err != nil {
		t.Fatalf("跨层遍历: %v", err)
	}
	if !strategyTestHopHasNode(deep, factID) {
		t.Fatalf("4 跳内应回得到 fact 节点")
	}
	for _, hop := range deep {
		if hop.Depth > 4 {
			t.Fatalf("跳的深度 %d 超过上限 4", hop.Depth)
		}
	}
}

// strategyTestHopHasNode 判断遍历结果里有没有某个节点。
func strategyTestHopHasNode(hops []LayerHop, nodeID int64) bool {
	for _, hop := range hops {
		if hop.Kind != LayerHopKindAsset && hop.NodeID == nodeID {
			return true
		}
	}
	return false
}

// TestTraverseFromFindingFindsLinkedStrategy 钉住跨层遍历的 L4 侧。
//
// 引用了该 finding 的策略要能被遍历出来 —— 这正是「从任意 finding 反向遍历到
// 它的策略」这条承诺的落点。
func TestTraverseFromFindingFindsLinkedStrategy(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	task, err := d.CreateTask("跨层策略用例", "验证从漏洞回溯到策略", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	findingID, err := d.AddFinding(task.ID, 0, "ssrf", "跨层漏洞", "high", "摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatalf("建 finding: %v", err)
	}
	strategyID := strategyTestStrategy(t, d, "跨层策略-"+strategyTestMarker("layer"), nil)
	if err := d.LinkStrategyEvidence(strategyID, "finding", fmt.Sprint(findingID), "created_by", "sha256:ddd"); err != nil {
		t.Fatalf("挂证据: %v", err)
	}

	hops, err := d.TraverseFromFinding(findingID, StrategyMaxHop)
	if err != nil {
		t.Fatalf("跨层遍历: %v", err)
	}
	found := false
	for _, hop := range hops {
		if hop.Kind == LayerHopKindStrategy && hop.StrategyID == strategyID {
			found = true
			if hop.StrategyState != StrategyQuarantined {
				t.Fatalf("策略跳的状态不对：%q", hop.StrategyState)
			}
		}
	}
	if !found {
		t.Fatalf("遍历结果里没有引用了该 finding 的策略：%+v", hops)
	}
}

// TestTaskAssetSignatureRespectsScope 钉住「签名走 scope 注入版查询」。
//
// 任务 A 的范围里有一条 subdomain 资产，范围外还有一条 ip 资产 ⇒ 签名里
// **不得**出现 ip。
//
// 这是安全前提而不是性能优化：签名决定哪些策略会被激活，范围外的资产类型
// 进了签名 ⇒ 一条为别的目标写的策略会被套到本任务上（Phase 0 的读面隔离白做）。
func TestTaskAssetSignatureRespectsScope(t *testing.T) {
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("scope")
	task, err := d.CreateTask("签名范围用例", "验证签名只认范围内资产", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	domain := marker + ".example.test"
	if _, err := d.Exec(`INSERT INTO assets(type, domain, root_domain, origin) VALUES('subdomain',$1,$2,'agent')`,
		domain, "example.test"); err != nil {
		t.Fatalf("建范围内资产: %v", err)
	}
	outOfScopeIP := fmt.Sprintf("203.0.113.%d", strategyTestSuffix()%254+1)
	if _, err := d.Exec(`INSERT INTO assets(type, ip, origin) VALUES('ip',$1,'agent')`, outOfScopeIP); err != nil {
		t.Fatalf("建范围外资产: %v", err)
	}
	if err := d.Assets().upsertTaskScope(TaskScope{
		TaskID: task.ID, Kind: "subdomain", Domain: domain, Source: "manual",
	}); err != nil {
		t.Fatalf("写任务范围: %v", err)
	}

	sig, err := d.Assets().TaskAssetSignature(task.ID)
	if err != nil {
		t.Fatalf("算任务资产签名: %v", err)
	}
	if !strategyTestContains(sig.AssetTypes, "subdomain") {
		t.Fatalf("范围内的资产类型没进签名：%v", sig.AssetTypes)
	}
	if strategyTestContains(sig.AssetTypes, "ip") {
		t.Fatalf("范围外的资产类型混进了签名（scope 旁路）：%v", sig.AssetTypes)
	}
}

// TestStructuralRetrievalPathNeverCallsLLM 钉住「检索热路径上没有模型调用」。
//
// 断言分两层：
//  1. 静态：db 包（尤其检索所在的 db/strategies.go）**根本没有模型客户端**可调
//     —— 没有 net/http、没有 openai/anthropic SDK、没有 agent/llmrec/llmpool；
//     ⇒ 检索路径在结构上就不可能发起一次模型调用；
//  2. 运行：整条检索照常跑完并返回结构命中的候选 —— 结构路径是纯 SQL 谓词 +
//     纯 Go 计算。
//
// ⚠️ 结构路径**不是**语义检索：它比的是资产图谓词的形状（类型 / 指纹 / 技术栈
// 标签），不是句子的意思。真正的语义检索留给后续阶段。
func TestStructuralRetrievalPathNeverCallsLLM(t *testing.T) {
	forbidden := []string{"net/http", "openai", "anthropic", "ollama", "gemini",
		"github.com/Autumn-27/artex/agent", "github.com/Autumn-27/artex/llmrec",
		"github.com/Autumn-27/artex/llmpool"}

	packageImports, err := strategyTestPackageImports(t)
	if err != nil {
		t.Fatalf("扫 db 包的 import: %v", err)
	}
	for _, imp := range packageImports {
		for _, bad := range forbidden {
			if imp == bad || strings.Contains(imp, bad) {
				t.Fatalf("db 包引入了模型/网络客户端 %q —— 检索热路径上不许有模型调用", imp)
			}
		}
	}

	// 检索文件自身只许有标准库与 pgx：它连网络客户端的边都没有。
	fileImports, err := strategyTestFileImports(t, "strategies.go")
	if err != nil {
		t.Fatalf("扫 db/strategies.go 的 import: %v", err)
	}
	for _, imp := range fileImports {
		for _, bad := range forbidden {
			if imp == bad || strings.Contains(imp, bad) {
				t.Fatalf("检索文件引入了模型/网络客户端 %q", imp)
			}
		}
	}

	// 运行证明：整条检索跑通，结构命中的候选照常返回。
	d, err := Open(strategyTestDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	marker := strategyTestMarker("nollm")
	task, err := d.CreateTask("无模型调用用例", "审计数据库口令强度与权限配置", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	activation := fmt.Sprintf(`{"asset_type":[%q],"service_type":[%q]}`, marker, marker)
	id := strategyTestState(t, d, "探针族-站点指纹命中法-"+marker, "用服务指纹定位入口", activation, StrategyActive)
	sig := AssetSignature{AssetTypes: []string{marker}, ServiceTypes: []string{marker}}
	matches, err := d.SearchActiveStrategies(task.ID, sig, StrategySearchTopK, 0)
	if err != nil {
		t.Fatalf("检索策略: %v", err)
	}
	if strategyTestFind(matches, id) == nil {
		t.Fatalf("结构路径没有返回结构命中的候选")
	}
}

// strategyTestPackageImports 扫 db 包全部源文件的 import 路径。
func strategyTestPackageImports(t *testing.T) ([]string, error) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		imps, err := strategyTestFileImports(t, name)
		if err != nil {
			return nil, err
		}
		out = append(out, imps...)
	}
	return out, nil
}

// strategyTestFileImports 扫单个源文件的 import 路径。
func strategyTestFileImports(t *testing.T, name string) ([]string, error) {
	t.Helper()
	path := filepath.Join(".", name)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// TestStrategyQualifiesUsesStructuralOrLexical 钉住准入规则。
//
// 结构命中的候选**不受**字面阈值约束；字面路径的候选必须过阈值。
func TestStrategyQualifiesUsesStructuralOrLexical(t *testing.T) {
	if !StrategyQualifies(StrategyMatch{StructuralMatch: true}, 0.99) {
		t.Fatalf("结构命中的候选被字面阈值误杀了")
	}
	if !StrategyQualifies(StrategyMatch{LexicalSimilarity: 0.25}, StrategyMinRelevance) {
		t.Fatalf("字面相似度达标的候选被误杀")
	}
	if StrategyQualifies(StrategyMatch{LexicalSimilarity: 0.10}, StrategyMinRelevance) {
		t.Fatalf("字面相似度不够的候选被放行")
	}
}

// TestStrategyDecayIsBounded 钉住有界衰减。
//
// 地板 0.3 / 天花板 1.5：衰减只影响排序，永不过滤候选。
func TestStrategyDecayIsBounded(t *testing.T) {
	now := time.Now()
	old := now.Add(-400 * 24 * time.Hour)
	if got := StrategyDecay(old, old, now); got != StrategyDecayFloor {
		t.Fatalf("老策略的衰减应落到地板 %.2f，实际 %v", StrategyDecayFloor, got)
	}
	fresh := StrategyDecay(now, now, now)
	if fresh < 0.99 || fresh > 1.01 {
		t.Fatalf("刚用过的策略衰减应约等于 1，实际 %v", fresh)
	}
	// 未来时间不得把衰减顶到天花板之外。
	if got := StrategyDecay(now.Add(time.Hour), now.Add(time.Hour), now); got > StrategyDecayCeiling {
		t.Fatalf("衰减越界：%v", got)
	}
}

// TestStrategyActivationCoverageAndMatches 钉住谓词的 Go 侧语义。
//
// 这套语义必须与 SQL 侧的结构过滤同形（strategyActivationMatchSQL /
// strategyCoverageSQL）—— 两处漂移会让排序与打分对不上。
func TestStrategyActivationCoverageAndMatches(t *testing.T) {
	cases := []struct {
		name         string
		activation   string
		sig          AssetSignature
		wantMatch    bool
		wantCoverage float64
	}{
		{"空谓词不约束", `{}`, AssetSignature{AssetTypes: []string{"site"}}, true, 0},
		{"单键命中", `{"asset_type":["site"]}`, AssetSignature{AssetTypes: []string{"site"}}, true, 0.25},
		{"单键不命中", `{"asset_type":["site"]}`, AssetSignature{AssetTypes: []string{"ip"}}, false, 0},
		{"两键全命中", `{"asset_type":["site"],"service_type":["http"]}`,
			AssetSignature{AssetTypes: []string{"site"}, ServiceTypes: []string{"http"}}, true, 0.5},
		{"两键只命中一个", `{"asset_type":["site"],"service_type":["http"]}`,
			AssetSignature{AssetTypes: []string{"site"}}, false, 0.25},
		{"类别键命中", `{"category":["web"]}`, AssetSignature{Categories: []string{"web"}}, true, 0.25},
		{"记录类型键命中", `{"record_type":["A"]}`, AssetSignature{RecordTypes: []string{"A"}}, true, 0.25},
		{"声明了但签名没有该键", `{"category":["web"]}`, AssetSignature{AssetTypes: []string{"site"}}, false, 0},
	}
	for _, c := range cases {
		got := strategyActivationMatches(json.RawMessage(c.activation), c.sig)
		if got != c.wantMatch {
			t.Fatalf("%s：匹配判定 = %v，期望 %v", c.name, got, c.wantMatch)
		}
		cov := strategyActivationCoverage(json.RawMessage(c.activation), c.sig)
		if cov != c.wantCoverage {
			t.Fatalf("%s：谓词覆盖率 = %v，期望 %v", c.name, cov, c.wantCoverage)
		}
	}
}
