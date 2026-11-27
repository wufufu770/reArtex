package db

// monitor_test.go：B-09B 两张表增量迁移与最小 CRUD 验收（live PG）。
//
// 验收逐项（卡验收）：
//   - 空库建表 / 旧库升级：EnsureMonitorSchema 幂等双跑 + information_schema
//     列证据（monitor_repos 绝无 token 形态列——auth_ref 只存引用的表级证据）；
//   - 唯一约束：(repo_url,branch) 幂等收敛、(repo_id,sha) 发现去重；
//   - 状态 CHECK：白名单四态 + 非法态被 CHECK 拒（数据层终闸）；
//   - auth_ref 不存 secret：auth_ref 形态闸拒绝 token 明文（ghp_/github_pat_
//     前缀形态非法）+ 表无 token 列；
//   - 外键删除策略：RESTRICT 拒绝带 findings 的仓库删除（处置历史保留）；
//   - migration 可重复可回滚：DROP 后重 Ensure 建表恢复（新表，无既有数据
//     依赖；放在最后一个测试执行避免影响其他用例）。

import (
	"strings"
	"testing"
)

// monitorTestURL 是本文件测试数据的前缀（t.Cleanup 按前缀精确清理，不按共享
// 维度批量删——仓库既有测试纪律）。
const monitorTestURLPrefix = "https://github.com/b09btest/"

func monitorTestSetup(t *testing.T) *DB {
	t.Helper()
	d, _, _ := testSetup(t)
	if err := d.EnsureMonitorSchema(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM monitor_findings WHERE repo_id IN (SELECT id FROM monitor_repos WHERE repo_url LIKE $1)`, monitorTestURLPrefix+"%")
		_, _ = d.Exec(`DELETE FROM monitor_repos WHERE repo_url LIKE $1`, monitorTestURLPrefix+"%")
	})
	return d
}

const (
	testSHA1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSHA2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSHA3 = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestMonitorSchemaMigration(t *testing.T) {
	d := monitorTestSetup(t)

	// 幂等双跑：重复应用零副作用（Ensure 返回 nil 即成功路径）。
	if err := d.EnsureMonitorSchema(); err != nil {
		t.Fatalf("重复应用 DDL 应幂等成功: %v", err)
	}

	// 表存在性 + monitor_repos 绝无 token 形态列（auth_ref 只存引用的表级证据）。
	rows, err := d.Query(`SELECT table_name, column_name FROM information_schema.columns
WHERE table_name IN ('monitor_repos','monitor_findings') ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, tbl+"."+col)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Fatal("monitor 两张表应已建表")
	}
	for _, c := range cols {
		lc := strings.ToLower(c)
		if strings.Contains(lc, "token") || strings.Contains(lc, "secret_cipher") {
			t.Fatalf("monitor_repos 不得出现 token/secret 形态列: %s", c)
		}
	}
	for _, want := range []string{"monitor_repos.auth_ref", "monitor_repos.last_seen_sha", "monitor_findings.status"} {
		found := false
		for _, c := range cols {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺列 %s（got %v）", want, cols)
		}
	}

	// 状态 CHECK 数据层终闸：直接 INSERT 非法态必须被拒。
	var repoID int64
	if err := d.QueryRow(`INSERT INTO monitor_repos(repo_url, branch) VALUES ($1,'main') RETURNING id`,
		monitorTestURLPrefix+"schema-check-repo").Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO monitor_findings(repo_id, sha, title, status) VALUES ($1,$2,'x','confirmed')`, repoID, testSHA3)
	if err == nil || !strings.Contains(err.Error(), "ck_monitor_findings_status") {
		t.Fatalf("非法状态应被 CHECK 拒绝: %v", err)
	}

	// (repo_id, sha) 唯一闸：直接 INSERT 重复被拒。
	if _, err := d.Exec(`INSERT INTO monitor_findings(repo_id, sha, title) VALUES ($1,$2,'first')`, repoID, testSHA3); err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO monitor_findings(repo_id, sha, title) VALUES ($1,$2,'dup')`, repoID, testSHA3)
	if err == nil || !strings.Contains(err.Error(), "uq_monitor_findings_repo_sha") {
		t.Fatalf("重复 (repo_id,sha) 应被唯一约束拒绝: %v", err)
	}
}

func TestMonitorRepoCRUDLifecycle(t *testing.T) {
	d := monitorTestSetup(t)

	// ① upsert 新建。
	id, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"crud-repo", "main", "", "key:default", "tester", true)
	if err != nil || id <= 0 {
		t.Fatalf("新建应成功: %v id=%d", err, id)
	}
	// ② 同 (url,branch) 再 upsert = 更新（幂等收敛，游标字段不受影响）。
	if err := d.UpdateMonitorRepoCursor(id, testSHA1); err != nil {
		t.Fatal(err)
	}
	id2, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"crud-repo", "main", "https://ghe.corp.example.com/api/v3", "key:alt", "tester2", false)
	if err != nil || id2 != id {
		t.Fatalf("同 (url,branch) upsert 应收敛同一行: err=%v id2=%d id=%d", err, id2, id)
	}
	got, err := d.GetMonitorRepo(id)
	if err != nil || got == nil {
		t.Fatalf("get 应命中: %v", err)
	}
	if got.GitHubBaseURL != "https://ghe.corp.example.com/api/v3" || got.AuthRef != "key:alt" || got.Enabled {
		t.Fatalf("upsert 更新语义不符: %+v", got)
	}
	if got.LastSeenSHA != testSHA1 {
		t.Fatalf("upsert 绝不重置游标: got %q want %q", got.LastSeenSHA, testSHA1)
	}

	// ③ enabledOnly 过滤 + 列表。
	if err := d.SetMonitorRepoEnabled(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"crud-repo2", "dev", "", "", "tester", false); err != nil {
		t.Fatal(err)
	}
	enabled, err := d.ListMonitorRepos(true)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range enabled {
		if r.RepoURL == monitorTestURLPrefix+"crud-repo2" {
			t.Fatal("enabledOnly 列表不应包含停用配置")
		}
		if r.ID == id {
			seen = true
		}
	}
	if !seen {
		t.Fatal("enabledOnly 列表应包含启用配置")
	}

	// ④ RESTRICT：带 findings 的仓库删除被拒（处置历史保留），处置后可删。
	inserted, err := d.RecordMonitorFinding(id, testSHA2, "安全更新：修复 CVE", "CVE-2024-3094", "", "")
	if err != nil || !inserted {
		t.Fatalf("落发现应成功: %v %v", inserted, err)
	}
	if err := d.DeleteMonitorRepo(id); err == nil {
		t.Fatal("带 findings 的仓库删除应被 RESTRICT 拒绝")
	}
	rows, err := d.ListMonitorFindings(MonitorStatusNew, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range rows {
		if f.RepoID == id && f.SHA == testSHA2 {
			found = true
		}
	}
	if !found {
		t.Fatal("删除被拒后 findings 应保留")
	}
	// 显式处置（ignored）后仓库仍不可删——RESTRICT 不看状态（审计历史语义）。
	if err := d.SetMonitorFindingStatus(id, "x"); err == nil {
		t.Fatal("非法状态转移应报错")
	}
	findingRows, err := d.ListMonitorFindings(MonitorStatusNew, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findingRows {
		if f.RepoID == id {
			if err := d.SetMonitorFindingStatus(f.ID, MonitorStatusIgnored); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := d.DeleteMonitorRepo(id); err == nil {
		t.Fatal("findings 仍在（仅改状态）时仓库删除仍应被 RESTRICT 拒绝")
	}
	// 删 findings 后仓库可删。
	if _, err := d.Exec(`DELETE FROM monitor_findings WHERE repo_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteMonitorRepo(id); err != nil {
		t.Fatalf("无 findings 后仓库删除应成功: %v", err)
	}
	if r, err := d.GetMonitorRepo(id); err != nil || r != nil {
		t.Fatalf("删除后 get 应 not-found: %v %v", r, err)
	}
}

func TestMonitorFindingDedupAndStatus(t *testing.T) {
	d := monitorTestSetup(t)

	repoID, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"dedup-repo", "main", "", "", "tester", true)
	if err != nil {
		t.Fatal(err)
	}

	// ① 首见落 new；重复 (repo_id,sha) 幂等收敛 inserted=false。
	inserted, err := d.RecordMonitorFinding(repoID, testSHA1, "安全更新 A", "CVE-2024-0001", "GHSA-xxxx-xxxx-xxxx", "")
	if err != nil || !inserted {
		t.Fatalf("首见应插入: %v %v", inserted, err)
	}
	inserted, err = d.RecordMonitorFinding(repoID, testSHA1, "安全更新 A（重判）", "CVE-2024-0001", "", MonitorStatusPendingReview)
	if err != nil || inserted {
		t.Fatalf("重复应幂等收敛（首见行保持不变）: %v %v", inserted, err)
	}
	// ② 不同 sha 各成行；pending_review 是合法落点（fail-closed）。
	inserted, err = d.RecordMonitorFinding(repoID, testSHA2, "疑似安全更新，模型未判出", "", "", MonitorStatusPendingReview)
	if err != nil || !inserted {
		t.Fatalf("pending_review 落点应合法: %v %v", inserted, err)
	}
	// ③ 判定层写 reproduced/ignored 被拒（人工处置态）。
	for _, bad := range []string{MonitorStatusReproduced, MonitorStatusIgnored} {
		if _, err := d.RecordMonitorFinding(repoID, testSHA3, "x", "", "", bad); err == nil {
			t.Fatalf("判定层落 %s 应被拒", bad)
		}
	}
	// ④ token 明文形态绝不进 auth_ref。
	if _, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"dedup-repo", "main", "", "ghp_"+"REDACTEDTOKEN", "tester", true); err == nil {
		t.Fatal("token 明文形态应被 auth_ref 形态闸拒绝")
	}
	// ⑤ 人工处置状态转移 + 白名单。
	rows, err := d.ListMonitorFindings(MonitorStatusNew, 100)
	if err != nil {
		t.Fatal(err)
	}
	var fid int64
	for _, f := range rows {
		if f.RepoID == repoID && f.SHA == testSHA1 {
			fid = f.ID
		}
	}
	if fid == 0 {
		t.Fatal("应找到首见发现")
	}
	if err := d.SetMonitorFindingStatus(fid, MonitorStatusReproduced); err != nil {
		t.Fatalf("reproduced 处置应成功: %v", err)
	}
	gone, err := d.ListMonitorFindings(MonitorStatusNew, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range gone {
		if f.ID == fid {
			t.Fatal("reproduced 后不应再出现在 new 过滤列表")
		}
	}
	// ⑥ 非法形态闸：sha/limit/status 过滤。
	if _, err := d.RecordMonitorFinding(repoID, "zzz", "x", "", "", ""); err == nil {
		t.Fatal("非法 sha 应被拒")
	}
	if _, err := d.ListMonitorFindings("", 0); err == nil {
		t.Fatal("limit=0 应被拒")
	}
	if _, err := d.ListMonitorFindings("bogus", 10); err == nil {
		t.Fatal("非法 status 过滤应被拒")
	}
	// ⑦ cve_id/ghsa_id 形态闸。
	if _, err := d.RecordMonitorFinding(repoID, testSHA3, "x", "not-a-cve", "", ""); err == nil {
		t.Fatal("非法 cve_id 应被拒")
	}
}

func TestMonitorValidationForms(t *testing.T) {
	d := monitorTestSetup(t)

	// repo_url 形态闸（fail-closed 表驱动）。
	badURLs := []string{
		"",
		"http://github.com/owner/repo",             // 非 https
		"https://user:pass@github.com/o/r",         // userinfo
		"https://github.com/owner/repo?x=1",        // 查询串
		"https://github.com/owner/repo#frag",       // 片段
		"https://github.com/onlyowner",             // 单段
		"https://github.com/",                      // 无段
		"https://github.com/owner/repo/path/extra", // 四段——owner/repo 之外多段按仓库 URL 基本形态拒绝
		"git@github.com:owner/repo",                // scp 形态
	}
	for _, u := range badURLs {
		if _, err := d.UpsertMonitorRepo(u, "main", "", "", "t", true); err == nil {
			t.Fatalf("repo_url %q 应被拒", u)
		}
	}
	goodURLs := []string{
		monitorTestURLPrefix + "forms-ok",
		"https://ghe.corp.example.com/owner/repo",
	}
	for _, u := range goodURLs {
		if _, err := d.UpsertMonitorRepo(u, "main", "", "", "t", true); err != nil {
			t.Fatalf("repo_url %q 应接受: %v", u, err)
		}
	}

	// branch 形态闸。
	badBranches := []string{"", "..evil", "a..b", "head.lock", "-lead", "sp ace", strings.Repeat("x", 200)}
	for _, b := range badBranches {
		if _, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"forms-branch", b, "", "", "t", true); err == nil {
			t.Fatalf("branch %q 应被拒", b)
		}
	}

	// base URL 形态闸。
	badBases := []string{"http://api.github.com", "https://evil.com?q=1", "not-a-url"}
	for _, b := range badBases {
		if _, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"forms-base", "main", b, "", "t", true); err == nil {
			t.Fatalf("base %q 应被拒", b)
		}
	}

	// 游标形态闸：非法 sha 拒绝，合法接受，空串重置合法。
	id, err := d.UpsertMonitorRepo(monitorTestURLPrefix+"forms-cursor", "main", "", "", "t", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateMonitorRepoCursor(id, "NOTASHA"); err == nil {
		t.Fatal("非法游标应被拒")
	}
	if err := d.UpdateMonitorRepoCursor(id, testSHA1); err != nil {
		t.Fatalf("合法游标应接受: %v", err)
	}
	if err := d.UpdateMonitorRepoCursor(id, ""); err != nil {
		t.Fatalf("空串游标重置应合法: %v", err)
	}

	// 不存在的行：enable/cursor/delete 报错不静默。
	if err := d.SetMonitorRepoEnabled(999999999, true); err == nil {
		t.Fatal("不存在 repo 的 enable 应报错")
	}
	if err := d.UpdateMonitorRepoCursor(999999999, testSHA1); err == nil {
		t.Fatal("不存在 repo 的游标推进应报错")
	}
	if err := d.DeleteMonitorRepo(999999999); err == nil {
		t.Fatal("不存在 repo 的删除应报错")
	}
	if r, err := d.GetMonitorRepo(999999999); err != nil || r != nil {
		t.Fatalf("不存在 repo 应 (nil,nil): %v %v", r, err)
	}
}

func TestMonitorRollbackRecreate(t *testing.T) {
	// 放最后一个：DROP+重建验证 migration 可重复可回滚（新表零既有数据依赖）。
	d := monitorTestSetup(t)

	if _, err := d.Exec(`DROP TABLE IF EXISTS monitor_findings CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DROP TABLE IF EXISTS monitor_repos CASCADE`); err != nil {
		t.Fatal(err)
	}
	// 重建后 sync.Map 缓存的是旧连接标记——同连接重复 Ensure 会被缓存跳过，
	// 这里需要真实重放 DDL：直接执行 embedded SQL（EnsureMonitorSchema 对新
	// 连接会自动应用；本测试验证 SQL 本身可重复）。
	if _, err := d.Exec(monitorSchemaSQL); err != nil {
		t.Fatalf("重建（回滚恢复）应成功: %v", err)
	}
	if err := d.EnsureMonitorSchema(); err != nil {
		t.Fatalf("Ensure 幂等: %v", err)
	}
	rows, err := d.ListMonitorRepos(false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("重建后应为空表: %v len=%d", err, len(rows))
	}
}
