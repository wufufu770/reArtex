package server

import (
	"strconv"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// ---------- 发布任务 API（任务大厅） ----------
//
// 「发布」= 一次调用把「人在资产图上勾选的资产子集」变成「一个能干活的大厅」：
// 建 task → 声明 task_scope(kind='asset') → 建群聊（1 个大厅会话 + N 个成员会话）
// → 可选地开额度；返回 task id 与 group id。
//
// 资产范围是这条链路的要害：agent 的 list_assets 只认 task_scope
// （scopeTargetCTE，db/task_assets_context.go）。少了这些行，人勾的资产对成员
// **不可见** —— 任务开在一张空图上，看着在跑，其实什么也没打。
//
// 路由注册不在本文件：server/server.go 的 mux 注册表是别的单元的文件域（同一
// worktree 内并行开发）。本单元只提供 handler 与这个一次性注册入口 —— 属主在
// 注册区调一次 s.RegisterTaskHallRoutes(mux) 即可。

// 发布任务的错误分类。handler 按它挑 HTTP 状态码，调用方按 errors.Is 判定。
var (
	// ErrTaskHallInvalidRequest：请求不合法（HTTP 400）。
	ErrTaskHallInvalidRequest = errors.New("发布任务参数不合法")
	// ErrTaskHallConflict：该任务已有大厅（HTTP 409）。
	ErrTaskHallConflict = errors.New("该任务已有群聊")
	// ErrTaskHallTaskNotFound：目标任务不存在（HTTP 404）。
	ErrTaskHallTaskNotFound = errors.New("任务不存在")
)

// taskHallDefaultConcurrency 是群组并发上限的缺省值（与 db 层的默认一致）。
const taskHallDefaultConcurrency = 3

// taskHallDefaultTimeoutSeconds 是大厅任务的默认墙钟（方案 v3 缺陷 C3）：
// 2 小时。没有它，发散型 goal 会跑成僵尸（远程 0.4.9 实测 25 轮规划不收敛）。
// 到点走 engine_timeout 的 settle 管线有序收尾，不是硬杀。
const taskHallDefaultTimeoutSeconds = 7200

// PublishTaskRequest is the payload of POST /api/task-hall/tasks.
type PublishTaskRequest struct {
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	Goal                 string   `json:"goal"`
	AssetIDs             []int64  `json:"asset_ids"`
	Members              []string `json:"members"` // 空 = 交给 auto 兜底
	PlanCode             string   `json:"plan_code,omitempty"`
	ConcurrencyLimit     int      `json:"concurrency_limit,omitempty"`
	BudgetTokens         *int64   `json:"budget_tokens,omitempty"`
	EvidencePreservation bool     `json:"evidence_preservation,omitempty"`
	// CategoryID 任务类别（task_categories.id；nil=未分类）。类别是 worker 纪律的
	// 结构性开关（方案 v4 §3-C2）：资产收集类任务 ⇒ 引擎给 worker 追加收集纪律段
	// （server/task_llm.go 的 workerCategoryDiscipline）。不存在 ⇒ 整个发布失败。
	CategoryID *int64 `json:"category_id,omitempty"`
	// TimeoutSeconds 是任务墙钟（秒）（方案 v3 缺陷 C3）：nil=默认 7200；
	// 显式 0=不限时（防呆靠 C4 停滞闸兜底）。指针是刻意的——「没填」与「填了 0」
	// 语义不同，不加指针就会把「不限时」变成事实默认。
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
	// TaskID 指向一个**已有**任务发布（0 = 新建任务）。
	//
	// 为什么需要它：一个 task 至多一个大厅，而"发布"默认新建任务 ⇒ 只靠"发布"
	// 这个动作永远撞不上"一个 task 两个大厅"的冲突。要让这条规则对操作者
	// **可见**（而不只是落在 DB 的唯一约束上），发布必须能指着一个已有任务来：
	// 第一次发布建大厅，第二次发布报冲突。
	TaskID int64 `json:"task_id,omitempty"`
}

// RegisterTaskHallRoutes 挂上「发布任务」的路由。
//
// 为什么要单独一个注册函数而不是把 mux.HandleFunc 写在 server.go 里：路由表是
// 别的单元的文件域（同一 worktree 内并行开发），本单元只能提供 handler 与这个
// 一次性注册入口 —— 属主在注册区调一次即可。
func (s *Server) RegisterTaskHallRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/task-hall/tasks", s.publishTaskHallTask)
	mux.HandleFunc("GET /api/task-hall/tasks", s.listTaskHallTasks)
}

// PublishTask creates the task, declares the selected asset scope, creates the
// group with one conversation per member, and optionally opens a quota
// allocation. It returns the created task id and group id.
//
// 失败语义（先校验、后落库）：参数与资产存在性在**建任务之前**一次性校验完，
// 任何一个不过 ⇒ 整个发布失败且**什么都不创建**（没有任务、没有范围行、没有
// 大厅）。校验通过后才逐条落库，因此不存在"只声明了一半资产"的中间态 ——
// 那种任务看起来有范围，实际范围是残的，比直接报错难查得多。
//
// 落库阶段出错 ⇒ 回滚：先删掉建了一半的大厅（半个大厅比没有大厅更糟：它会占住
// "一个 task 至多一个大厅"的名额，让重试变成冲突），再删掉本次新建的任务。
// 复用已有任务发布时不动那个任务：它的资产范围声明是幂等且追加的。
func (s *Server) PublishTask(req PublishTaskRequest) (taskID int64, groupID int64, err error) {
	pg := s.m.pg
	if pg == nil {
		return 0, 0, errors.New("数据库未连接")
	}

	// ---- 1) 校验：全部在落库之前 ----
	assetIDs, err := taskHallResolveAssetIDs(pg, req.AssetIDs)
	if err != nil {
		return 0, 0, err
	}
	members, err := taskHallResolveMembers(pg, req.Members)
	if err != nil {
		return 0, 0, err
	}
	if req.PlanCode != "" {
		return 0, 0, fmt.Errorf("%w: 套餐额度尚未落地（没有套餐表），请改用 budget_tokens 直接指定额度", ErrTaskHallInvalidRequest)
	}
	if req.EvidencePreservation {
		return 0, 0, fmt.Errorf("%w: 证据保全策略尚未落地，暂不支持该开关", ErrTaskHallInvalidRequest)
	}
	// 额度必填：大厅群挂了 fail-closed 的额度闸（无预算行=拒绝一切模型请求），
	// 「可选额度」会让发布出来的大厅静默饿死——不如在门口就要求填。
	if req.BudgetTokens == nil || *req.BudgetTokens <= 0 {
		return 0, 0, fmt.Errorf("%w: 额度（budget_tokens）必填——大厅群组的模型请求走 fail-closed 额度闸", ErrTaskHallInvalidRequest)
	}
	// 任务墙钟（C3）：默认 7200s，显式 0=不限。负值是参数错误。
	taskHallTimeout := taskHallDefaultTimeoutSeconds
	if req.TimeoutSeconds != nil {
		if *req.TimeoutSeconds < 0 {
			return 0, 0, fmt.Errorf("%w: timeout_seconds 不能为负（0=不限时）", ErrTaskHallInvalidRequest)
		}
		taskHallTimeout = *req.TimeoutSeconds
	}
	// 类别（§3-C2）：给了就必须存在——建到一半才发现类别非法会让「什么都不创建」
	// 的原子性语义靠回滚兜底，不如在校验段就拦下。这里只做存在性判定，落库交给
	// CreateTaskWithOptions（同一事务内解析类别名）。
	if req.CategoryID != nil {
		if *req.CategoryID <= 0 {
			return 0, 0, fmt.Errorf("%w: category_id 必须为正整数", ErrTaskHallInvalidRequest)
		}
		category, err := pg.GetTaskCategory(*req.CategoryID)
		if err != nil {
			return 0, 0, fmt.Errorf("查任务类别失败: %w", err)
		}
		if category == nil {
			return 0, 0, fmt.Errorf("%w: 任务类别 #%d 不存在", ErrTaskHallInvalidRequest, *req.CategoryID)
		}
	}

	// ---- 2) 解析目标任务 ----
	var task *db.Task
	createdTask := false
	if req.TaskID > 0 {
		task, err = pg.GetTask(req.TaskID)
		if err != nil {
			return 0, 0, fmt.Errorf("读任务失败: %w", err)
		}
		if task == nil {
			return 0, 0, fmt.Errorf("%w: %d", ErrTaskHallTaskNotFound, req.TaskID)
		}
	} else {
		// 用 manager 层创建：任务必须进 Manager 内存注册表，否则 launchTask 的
		// s.m.Task 查不到（实测：db 层创建的任务在 admitTaskWhen 被静默拒准入）。
		mt, merr := s.m.CreateTaskWithOptions(taskHallDescription(req), taskHallGoal(req),
			db.TaskCreateOptions{Name: strings.TrimSpace(req.Name), CategoryID: req.CategoryID, TimeoutSeconds: taskHallTimeout})
		if merr != nil {
			return 0, 0, fmt.Errorf("建任务失败: %w", merr)
		}
		createdTask = true
		id64, _ := strconv.ParseInt(mt.ID, 10, 64)
		task = &db.Task{ID: id64}
	}
	taskID = task.ID

	// 回滚用的 id 单独存一份：出错时命名返回值会被置 0，defer 读不到真身。
	rollbackTaskID, rollbackGroupID := taskID, int64(0)
	defer func() {
		if err == nil {
			return
		}
		if rollbackGroupID > 0 {
			_ = pg.DeleteChatGroup(rollbackGroupID)
		}
		if createdTask {
			_ = pg.DeleteTask(rollbackTaskID)
		}
	}()

	// ---- 3) 一个 task 至多一个大厅 ----
	existing, err := pg.GroupByTask(taskID)
	if err != nil {
		return 0, 0, fmt.Errorf("查群组失败: %w", err)
	}
	if existing != nil {
		return 0, 0, fmt.Errorf("%w: %d", ErrTaskHallConflict, taskID)
	}

	// ---- 4) 声明资产范围（agent 可见性的唯一途径）----
	for _, id := range assetIDs {
		if err = pg.AddTaskScopeAsset(taskID, id); err != nil {
			return 0, 0, fmt.Errorf("声明资产范围失败: %w", err)
		}
	}

	// ---- 5) 建群：大厅会话 + 群组 + 成员会话 ----
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(task.Name)
	}
	if name == "" {
		name = "任务大厅"
	}
	hall, err := pg.CreateConversationOf("auto", name, nil, db.OriginHall)
	if err != nil {
		return 0, 0, fmt.Errorf("建大厅会话失败: %w", err)
	}
	limit := req.ConcurrencyLimit
	if limit <= 0 {
		limit = taskHallDefaultConcurrency
	}
	groupID, err = pg.CreateChatGroup(&db.ChatGroup{
		ConversationID:   hall.ID,
		TaskID:           taskID,
		Name:             name,
		Status:           "active",
		ConcurrencyLimit: limit,
		BudgetTokens:     req.BudgetTokens,
	})
	if err != nil {
		groupID = 0
		return 0, 0, taskHallCreateGroupErr(err)
	}
	rollbackGroupID = groupID
	// auto 永远是分发者，绑定大厅会话（它不需要第二个会话）：人只在大厅会话里说话。
	if err = pg.AddChatGroupMember(groupID, "auto", &hall.ID, "dispatcher"); err != nil {
		return 0, 0, fmt.Errorf("加分发者失败: %w", err)
	}
	// 每个成员一个会话：conversations.agent_key 是单值列，一个会话只能跑一个
	// agent ⇒ 成员各拿一个会话（独立 transcript，共享同一个 task 的探索图）。
	for _, key := range members {
		conv, err := pg.CreateConversationOf(key, name+"/"+key, nil, db.OriginHall)
		if err != nil {
			return 0, 0, fmt.Errorf("建成员会话失败(%s): %w", key, err)
		}
		if err = pg.AddChatGroupMember(groupID, key, &conv.ID, "member"); err != nil {
			return 0, 0, fmt.Errorf("加群成员失败(%s): %w", key, err)
		}
	}

	// ---- 6) 额度（可选）：预算行 + 群上的额度列 ----
	if req.BudgetTokens != nil {
		if err = pg.SetGroupQuotaBudget(groupID, *req.BudgetTokens); err != nil {
			return 0, 0, fmt.Errorf("设群组额度失败: %w", err)
		}
	}
	// P5：发布即开工——launchTask 走 goalless 直投路径（goal=seedText），引擎启动、
	// legacy 注入在 launchTask 种子阶段生效。任务失败回滚由上方 defer 处理。
	if t, ok := s.m.Task(strconv.FormatInt(taskID, 10)); ok && t != nil {
		// P5：发布即开工——launchTask 走 goalless 直投路径，legacy 注入随之生效。
		s.launchTask(t, taskHallDescription(req)+" "+taskHallGoal(req), false)
	}
	return taskID, groupID, nil
}

// publishTaskHallTask 发布一个任务大厅。
func (s *Server) publishTaskHallTask(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req PublishTaskRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	taskID, groupID, err := s.PublishTask(req)
	if err != nil {
		writeErr(w, taskHallErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"task_id": taskID, "group_id": groupID})
}

// listTaskHallTasks 列已发布的大厅：一个 task 至多一个大厅 ⇒ 每个条目就是一个
// 已发布的任务。
func (s *Server) listTaskHallTasks(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	groups, err := pg.ListChatGroups(r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		members, err := pg.ChatGroupMembers(g.ID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{
			"task_id": g.TaskID, "group_id": g.ID, "name": g.Name, "status": g.Status,
			"conversation_id": g.ConversationID, "concurrency_limit": g.ConcurrencyLimit,
			"budget_tokens": g.BudgetTokens, "members": members, "created_at": g.CreatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"tasks": items})
}

// taskHallErrorStatus 把错误分类翻译成 HTTP 状态码。
func taskHallErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrTaskHallConflict):
		return 409
	case errors.Is(err, ErrTaskHallTaskNotFound):
		return 404
	case errors.Is(err, ErrTaskHallInvalidRequest):
		return 400
	default:
		return 500
	}
}

// taskHallResolveAssetIDs 校验并去重勾选的资产 id。
//
// 存在性必须在**声明范围之前**查清：声明是逐行写的，中途失败就会留下"只声明了
// 一半资产"的任务。
func taskHallResolveAssetIDs(pg *db.DB, raw []int64) ([]int64, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: 至少选择一个资产", ErrTaskHallInvalidRequest)
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			return nil, fmt.Errorf("%w: 资产 id 非法: %d", ErrTaskHallInvalidRequest, id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	found, err := pg.Assets().GetByIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("查资产失败: %w", err)
	}
	have := map[int64]bool{}
	for _, a := range found {
		have[a.ID] = true
	}
	missing := []int64{}
	for _, id := range ids {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: 资产不存在: %v", ErrTaskHallInvalidRequest, missing)
	}
	return ids, nil
}

// taskHallResolveMembers 归一成员列表：去空白、去重、剔除 auto（auto 恒为分发者），
// 并确认每个成员都是已登记的 agent —— chat_group_members.agent_key 有外键，
// 不存在的 agent 会到落库时才炸，错误信息也难读。
func taskHallResolveMembers(pg *db.DB, raw []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range raw {
		key := strings.TrimSpace(r)
		if key == "" || key == "auto" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	for _, key := range out {
		a, err := pg.GetAgentByKey(key)
		if err != nil {
			return nil, fmt.Errorf("查 agent 失败: %w", err)
		}
		if a == nil {
			return nil, fmt.Errorf("%w: agent 不存在: %s", ErrTaskHallInvalidRequest, key)
		}
	}
	return out, nil
}

// taskHallDescription 兜住空描述：发布的任务得有可读的目标，否则任务列表里是一行
// 没有信息的东西（agent 也拿不到方向）。
func taskHallDescription(req PublishTaskRequest) string {
	for _, s := range []string{req.Description, req.Goal, req.Name} {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return "任务大厅发布"
}

// taskHallGoal 缺目标时用描述顶上（任务必须有一个目标字段非空）。
func taskHallGoal(req PublishTaskRequest) string {
	if t := strings.TrimSpace(req.Goal); t != "" {
		return t
	}
	return taskHallDescription(req)
}

// taskHallCreateGroupErr 把建群失败翻译成可判定的错误：撞上 uq_chat_groups_task
// ⇒ 这个 task 已经有大厅了（并发发布时的兜底；正常路径已在前面查过）。
func taskHallCreateGroupErr(err error) error {
	if strings.Contains(err.Error(), "uq_chat_groups_task") {
		return fmt.Errorf("%w: %v", ErrTaskHallConflict, err)
	}
	return fmt.Errorf("建群失败: %w", err)
}
