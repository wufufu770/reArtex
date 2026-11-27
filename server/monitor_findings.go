package server

// monitor_findings.go：Monitor findings 人工处置 API（B-09G）。
//
// 唯一目标：对 monitor_findings 候选发现提供三种显式人工操作——复现（只调用
// 现有人工建任务入口创建一条待验证任务，绝不自动执行测试或利用）、重判（重新
// 调 B-09E 判定器；失败继续 pending_review）、忽略（理由必填+审计链留痕）。
// 全部操作走审计链；不自动确认发现（monitor_findings 是情报面，绝不写 findings
// 漏洞表，包安全原则逐字：Monitor 结果只是情报或 hint）。
//
// 安全与边界纪律（卡要求逐条）：
//   - 路由注册在 requireAuth 内（与 B-09C /api/monitor/repos 同面）；单租户
//     JWT 无用户身份 claims，审计 Actor 固定 operator（如实申报，同 B-09C）；
//   - 状态转移只允许显式合法路径（transitions 白名单）：复现 new/pending_review
//     →reproduced；忽略 new/pending_review/reproduced→ignored；重判仅对
//     new/pending_review 开放（reproduced/ignored 是人工已处置态，绝不静默
//     覆写人工决定）——绝不把 pending_review/new 直接伪装为 reproduced（唯一的
//     reproduced 写入路径就是复现操作本体，任务创建成功才落状态）；
//   - 复现=经 s.m.CreateTaskWithOptions（POST /api/tasks 同一入口函数）创建
//     未启动验证任务：不调 launchTask、不 seed scope、不 admit——绝不自动扩
//     scope（CompanyIDs 恒空）绝不绕过既有 scope/approval 流程（启动与范围
//     配置完全是操作者在既有任务面的人工动作）；
//   - 重判材料：经 B-09D 客户端按 sha 现拉提交详情（GetCommitDetail 单请求
//     有界摘要）——拉取失败/判定器未装配/判定失败一律 fail-closed 落
//     pending_review（卡要求 2 逐字「失败继续 pending_review」），绝不默认
//     安全或非安全；判定为非安全更新→行原样保留（绝不删除原始提交或判定
//     记录）+审计留痕，处置权留给操作者；
//   - 忽略理由必填（trim 后非空）+有界截断 512；操作者=审计 Actor、时间=
//     审计事件 CreatedAt（审计链原生承载，monitor_findings 表结构零变更）；
//   - 请求体 MaxBytesReader 64KB（cveFeedDispatch 同款包内惯例）+严格 JSON。
//
// 边界如实：db 层无按 id 取单条 finding 的函数（B-09B 允许清单外不可扩）——
// monitorFindingByID 经 ListMonitorFindings("",500) 扫描定位（≤500 行管理面
// 可接受）；列表读取面（GET /api/monitor/findings）落在本文件是 B-09H 前端
// 页面的必要数据源（B-09H 允许清单仅 web 页面单文件，无 server 面）。

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 处置面边界常量。
const (
	monitorFindingMaxBody     = 64 << 10 // 请求体上限（cveFeedDispatch 同款）
	monitorFindingReasonCap   = 512      // 忽略理由有界截断（字节）
	monitorFindingTitleCap    = 200      // 进任务书/DTO 的标题截断
	monitorFindingListDefault = 100      // 列表缺省 limit（显式传参可覆盖）
	monitorFindingListMax     = 500      // 与 db.ListMonitorFindings 上限一致
)

// monitorFindingCommitFetcher 是重判的提交材料面（默认 B-09D 客户端单请求
// GetCommitDetail——客户端自带 30s 超时+SSRF 闸；ctx 贯穿；测试注入 fake
// ——缝隙在本文件内，零清单外文件）。
var monitorFindingCommitFetcher = func(ctx context.Context, repo *db.MonitorRepo, sha string) (*MonitorCommit, error) {
	base := repo.GitHubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	c, err := monitorGHNew(base, monitorDefaultTokenResolver)
	if err != nil {
		return nil, err
	}
	return c.GetCommitDetail(ctx, repo.RepoURL, sha, repo.AuthRef)
}

// monitorFindingStatusFilters 是列表过滤白名单（db 包内白名单不导出——本层
// 快速失败给 400，存储层白名单仍是终闸）。
var monitorFindingStatusFilters = map[string]bool{
	db.MonitorStatusNew: true, db.MonitorStatusPendingReview: true,
	db.MonitorStatusReproduced: true, db.MonitorStatusIgnored: true,
}

// monitorFindingActions 是显式合法状态转移白名单（卡要求 4：状态转移只允许
// 显式合法路径；表外组合一律 409 拒绝）。
var monitorFindingActions = map[string][]string{
	"reproduce": {db.MonitorStatusNew, db.MonitorStatusPendingReview},
	"ignore":    {db.MonitorStatusNew, db.MonitorStatusPendingReview, db.MonitorStatusReproduced},
	"rejudge":   {db.MonitorStatusNew, db.MonitorStatusPendingReview},
}

// monitorFindingByID 按 id 定位发现（db 层无单条读函数——列表扫描，见文件头
// 边界申报）。不存在返回 nil。
func monitorFindingByID(pg *db.DB, id int64) *db.MonitorFinding {
	rows, err := pg.ListMonitorFindings("", monitorFindingListMax)
	if err != nil {
		return nil
	}
	for _, f := range rows {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// monitorFindingToDTO 是发现的管理面投影（无凭据面——表内本就没有）。
func monitorFindingToDTO(f *db.MonitorFinding, repo *db.MonitorRepo) map[string]any {
	dto := map[string]any{
		"id": f.ID, "repo_id": f.RepoID, "sha": f.SHA,
		"title": f.Title, "cve_id": f.CVEID, "ghsa_id": f.GHSAID,
		"status": f.Status, "created_at": f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if repo != nil {
		dto["repo_url"] = repo.RepoURL
		dto["branch"] = repo.Branch
	}
	return dto
}

// monitorFindingAuditAppend 是处置审计的统一入口（best-effort；payload 键恰
// 白名单——reason 只进 ignore 载荷且已截断，凭据面结构性不存在）。
func monitorFindingAuditAppend(pg *db.DB, action string, id int64, reason string, payload map[string]any) {
	payload["id"] = id
	payload["via"] = "rest"
	body, _ := json.Marshal(payload)
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "monitor", Action: action,
		Reason: reason, Payload: body, Source: "monitor-api",
	}); err != nil {
		log.Printf("[monitor] 发现处置审计链写入失败（忽略）: %v", err)
	}
}

// monitorFindingList 是 GET /api/monitor/findings（?status=&limit=；status 走
// 存储层白名单，limit 缺省 100、1-500 显式收窄——零默认夹带语义由本层显式
// 补齐，存储层 limit 必填）。
func (s *Server) monitorFindingList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !monitorFindingStatusFilters[status] {
		writeErr(w, http.StatusBadRequest, "非法 status 过滤值")
		return
	}
	limit := monitorFindingListDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > monitorFindingListMax {
			writeErr(w, http.StatusBadRequest, "limit 必须 1-500")
			return
		}
		limit = n
	}
	findings, err := pg.ListMonitorFindings(status, limit)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
		return
	}
	repos, err := pg.ListMonitorRepos(false)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
		return
	}
	byID := make(map[int64]*db.MonitorRepo, len(repos))
	for _, rp := range repos {
		byID[rp.ID] = rp
	}
	// id DESC（存储层序）→ 输出 id ASC 确定性（最早发现优先，操作面读序）。
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	out := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		out = append(out, monitorFindingToDTO(f, byID[f.RepoID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": out})
}

// monitorFindingReproduce 是 POST /api/monitor/findings/{id}/reproduce：经现有
// 人工建任务入口（s.m.CreateTaskWithOptions——POST /api/tasks 同一函数）创建
// 未启动待验证任务；任务创建成功才落 reproduced（绝不空翻状态）；绝不自动
// 执行/扩 scope（CompanyIDs 恒空、不 launchTask 不 seed 不 admit）。重复操作
// 幂等拒绝（已处置态 409，不产重复任务）。
func (s *Server) monitorFindingReproduce(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法发现 id")
		return
	}
	f := monitorFindingByID(pg, id)
	if f == nil {
		writeErr(w, http.StatusNotFound, "监测发现不存在")
		return
	}
	if !monitorFindingAllowed(monitorFindingActions["reproduce"], f.Status) {
		writeErr(w, http.StatusConflict, "当前状态 "+f.Status+" 不允许复现（仅 new/pending_review；已处置态请勿重复操作）")
		return
	}
	repo, _ := pg.GetMonitorRepo(f.RepoID)
	repoURL, branch := "", ""
	if repo != nil {
		repoURL, branch = repo.RepoURL, repo.Branch
	}
	title := monitorTruncate(f.Title, monitorFindingTitleCap, nil)
	description := "Monitor 候选验证：" + title
	goal := "验证上游仓库安全更新候选（Monitor 发现 #" + strconv.FormatInt(f.ID, 10) + "）。\n" +
		"仓库：" + repoURL + "\n分支：" + branch + "\n提交：" + f.SHA + "\n" +
		"CVE：" + f.CVEID + "\nGHSA：" + f.GHSAID + "\n标题：" + title + "\n" +
		"说明：本任务来自 Monitor 监测候选发现，仅代表上游提交疑似安全更新；" +
		"验证目标与授权范围由操作者在既有任务面显式配置。本操作不自动执行任何测试或利用。"
	// 现有人工建任务入口（POST /api/tasks 同一函数）：零 CompanyIDs（绝不自动
	// 扩 scope）、零 LLM 绑定（沿用默认解析）、零 InterceptRules。
	t, err := s.m.CreateTaskWithOptions(description, goal, db.TaskCreateOptions{})
	if err != nil {
		log.Printf("[monitor] 复现建任务失败 finding=%d: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "创建验证任务失败，请稍后重试")
		return
	}
	// 任务创建成功才落 reproduced——reproduced 唯一写入路径就是本操作本体。
	if err := pg.SetMonitorFindingStatus(f.ID, db.MonitorStatusReproduced); err != nil {
		log.Printf("[monitor] 复现落状态失败 finding=%d task=%s: %v", id, t.ID, err)
		writeErr(w, http.StatusInternalServerError, "验证任务已创建（task="+t.ID+"）但发现状态更新失败，请人工核对")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	monitorFindingAuditAppend(pg, "monitor-finding-reproduce", id,
		"repo="+repoURL+" branch="+branch+" sha="+f.SHA,
		map[string]any{"repo_id": f.RepoID, "sha": f.SHA, "task_id": taskID})
	writeJSON(w, http.StatusOK, map[string]any{
		"finding": monitorFindingToDTO(monitorFindingByID(pg, id), repo), "task_id": t.ID,
	})
}

// monitorFindingIgnore 是 POST /api/monitor/findings/{id}/ignore：理由必填
// （trim 后非空）+有界截断；审计链承载原因/操作者/时间（Actor=operator、
// CreatedAt=审计事件时间——monitor_findings 表结构零变更）；原始提交与判定
// 记录绝不删除（只更新状态列）。
func (s *Server) monitorFindingIgnore(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法发现 id")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	body := http.MaxBytesReader(w, r.Body, monitorFindingMaxBody)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 JSON（含 reason）")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeErr(w, http.StatusBadRequest, "忽略必须填写理由")
		return
	}
	reason = monitorTruncate(reason, monitorFindingReasonCap, nil)
	f := monitorFindingByID(pg, id)
	if f == nil {
		writeErr(w, http.StatusNotFound, "监测发现不存在")
		return
	}
	if f.Status == db.MonitorStatusIgnored {
		writeErr(w, http.StatusConflict, "该发现已忽略，重复操作幂等拒绝")
		return
	}
	if !monitorFindingAllowed(monitorFindingActions["ignore"], f.Status) {
		writeErr(w, http.StatusConflict, "当前状态 "+f.Status+" 不允许忽略")
		return
	}
	if err := pg.SetMonitorFindingStatus(f.ID, db.MonitorStatusIgnored); err != nil {
		writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
		return
	}
	f.Status = db.MonitorStatusIgnored // 响应投影与库内状态一致（f 是更新前加载的）
	monitorFindingAuditAppend(pg, "monitor-finding-ignore", id,
		"sha="+f.SHA+" reason="+reason,
		map[string]any{"sha": f.SHA, "reason": reason})
	writeJSON(w, http.StatusOK, map[string]any{"finding": monitorFindingToDTO(f, nil)})
}

// monitorFindingRejudge 是 POST /api/monitor/findings/{id}/rejudge：现拉提交
// 材料（B-09D 单请求）→ 重新调 B-09E 判定器。材料拉取失败/判定器未装配/
// 判定失败/unsure → pending_review（fail-closed）；判定安全更新 → new（回到
// 候选）；判定非安全更新 → 行原样保留+审计留痕（绝不删除记录，处置权留给
// 操作者）。
func (s *Server) monitorFindingRejudge(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法发现 id")
		return
	}
	f := monitorFindingByID(pg, id)
	if f == nil {
		writeErr(w, http.StatusNotFound, "监测发现不存在")
		return
	}
	if !monitorFindingAllowed(monitorFindingActions["rejudge"], f.Status) {
		writeErr(w, http.StatusConflict, "当前状态 "+f.Status+" 不允许重判（仅 new/pending_review；已处置态不覆写人工决定）")
		return
	}
	repo, err := pg.GetMonitorRepo(f.RepoID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
		return
	}
	if repo == nil {
		writeErr(w, http.StatusNotFound, "关联仓库配置不存在")
		return
	}
	respond := func(status, outcome, reason string) {
		monitorFindingAuditAppend(pg, "monitor-finding-rejudge", id,
			"sha="+f.SHA+" outcome="+outcome+" reason="+reason,
			map[string]any{"sha": f.SHA, "outcome": outcome})
		writeJSON(w, http.StatusOK, map[string]any{
			"finding": monitorFindingToDTO(monitorFindingByID(pg, id), repo),
			"outcome": outcome, "reason": reason,
		})
	}
	pending := func(reason string) {
		if f.Status != db.MonitorStatusPendingReview {
			if err := pg.SetMonitorFindingStatus(f.ID, db.MonitorStatusPendingReview); err != nil {
				log.Printf("[monitor] 重判落 pending_review 失败 finding=%d: %v", id, err)
			}
		}
		respond(db.MonitorStatusPendingReview, "undecided", reason)
	}
	commit, err := monitorFindingCommitFetcher(r.Context(), repo, f.SHA)
	if err != nil {
		log.Printf("[monitor] 重判拉材料失败 finding=%d: %v", id, err)
		pending("提交材料拉取失败→人工待审")
		return
	}
	judge := s.MonitorJudgeFromServer()
	if judge == nil {
		pending("LLM 未配置→人工待审")
		return
	}
	v, jerr := judge.Judge(r.Context(), commit)
	if jerr != nil || !v.Decided {
		pending("判定未决→人工待审")
		return
	}
	if v.SecurityUpdate {
		if err := pg.SetMonitorFindingStatus(f.ID, db.MonitorStatusNew); err != nil {
			writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
			return
		}
		respond(db.MonitorStatusNew, "new", v.Reason)
		return
	}
	// 非安全更新：行原样保留（绝不删除原始提交或判定记录），状态不动。
	respond(f.Status, "not_security", v.Reason)
}

// monitorFindingAllowed 查显式合法转移白名单。
func monitorFindingAllowed(legal []string, status string) bool {
	for _, s := range legal {
		if s == status {
			return true
		}
	}
	return false
}
