// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.

import type { ChatMention } from "@/lib/chat-mentions";
import { MOCK } from "@/lib/mock/enabled";
import { mockHandle } from "@/lib/mock/handler";
import type {
  Session,
  SessionCreateInput,
  SessionMember,
  SessionMessage,
  SessionMessageInput,
  SessionPatchInput,
  SessionRole,
} from "@/lib/session-types";
import type {
  ActiveFindingRetest,
  Activity,
  Agent,
  AgentDetail,
  AgentTrigger,
  ArchiveBatchItem,
  Asset,
  AssetInterceptRule,
  AssetInterceptRuleInput,
  Audit,
  AuditChainVerification,
  AuditEventsPage,
  BatchCategoryItem,
  BatchControlItem,
  BizGraphData,
  BizLayerDiff,
  ChatAttachment,
  CommandRecord,
  Company,
  CompanyScopeMutation,
  CompanyScopeRule,
  Conversation,
  ConvTokenSummary,
  CoverageAssetRefs,
  CoverageGraphData,
  DailyTokenBucket,
  DeleteTaskOptions,
  DeleteTaskResult,
  Edge,
  EdgeCoverage,
  EdgeMethodology,
  EdgeTaskOverview,
  EgressGovernanceState,
  EvidenceBodyPreview,
  ExplorationNodePage,
  ExplorationNodeQuery,
  Finding,
  FindingAssetTree,
  FindingDeepenResponse,
  FindingGroupsPage,
  FindingQuery,
  FindingRetest,
  FindingStats,
  FindingStatus,
  FindingsPage,
  FindingTraffic,
  FindingTrafficDetail,
  IntentAsset,
  InterceptApprovalFilter,
  InterceptApprovalRow,
  InterceptDetail,
  InterceptPending,
  InterceptRule,
  JudgeConfig,
  JudgeUsage,
  LegacyNode,
  LLMPoolStatus,
  LLMProfile,
  LLMRecordDetail,
  LLMRecordItem,
  LLMRetryOverride,
  LLMRetryPolicy,
  LLMTask,
  MCPServer,
  MCPTool,
  MiniprogramArtifact,
  MiniprogramToken,
  MissingSkill,
  ModelTokenStat,
  NotificationChannel,
  NotificationDelivery,
  NotificationFilter,
  NotificationMeta,
  PromptVar,
  PromptVersion,
  ReportCandidate,
  ReportFull,
  ReportMeta,
  SessionTokenUsage,
  Settings,
  Severity,
  ShadowDiff,
  SkillCall,
  SkillItem,
  SodCategory,
  SodCategoryAction,
  SodCategoryFallback,
  SodCategorySuggestion,
  SodCircuit,
  SodDecision,
  SodFeed,
  SodHookChain,
  SodManualOverrideRow,
  SodMergedGateMode,
  SodReviewItem,
  SSProject,
  SSTask,
  Stats,
  Strategy,
  StrategyDecision,
  StrategyDetail,
  StrategyPinDecision,
  StrategyPinQueue,
  StrategyProposal,
  StrategyProposalDecision,
  StrategyState,
  Task,
  TaskArchive,
  TaskArchivePage,
  TaskAssetMutation,
  TaskAssetScopeMutation,
  TaskCategory,
  TaskConstraint,
  TaskGoal,
  TaskGovernance,
  TaskHallGroupDetail,
  TaskHallMessage,
  TaskHallQuota,
  TaskHallRow,
  TaskLLMResolutions,
  TaskNode,
  TaskScopeRow,
  TaskTemplate,
  ThinkingCapability,
  TokenTotal,
  TokenUsage,
  Tool,
  ToolStat,
  TrafficDetail,
  TrafficEvidenceRef,
  TrafficEvidenceRole,
  TrafficHost,
  TrafficResp,
  UpdateCheck,
  UsageStats,
  WorkspaceFile,
  WorkspaceListing,
} from "@/lib/types";

function getToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem("artex_token");
}

// ApiError 带 HTTP 状态码。
//
// 为什么需要它：有的后端用 404 表达「这个东西存在但没配置」（例：群组没配额度
// 时 GET /task-hall/groups/{id}/quota 回 404），而不是「读不到」。没有状态码时
// 只能靠错误文案猜，猜错就把「没配」报成「挂了」。
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

export async function http<T>(path: string, init?: RequestInit): Promise<T> {
  if (MOCK) return mockHandle<T>(init?.method ?? "GET", path, init?.body ?? null);
  const token = getToken();
  const r = await fetch(`/api${path}`, {
    ...init,
    headers: {
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init?.headers as Record<string, string> | undefined),
    },
  });
  if (r.status === 401) {
    if (typeof window !== "undefined") {
      localStorage.removeItem("artex_token");
      // biome-ignore lint/suspicious/noDocumentCookie: 登出时清理 artex_token cookie
      document.cookie = "artex_token=; path=/; max-age=0";
      window.location.href = "/login";
    }
    throw new Error("未授权");
  }
  if (!r.ok) {
    const fallback = `${init?.method ?? "GET"} ${path}: ${r.status}`;
    let message = fallback;
    try {
      const payload = (await r.json()) as { error?: unknown };
      if (typeof payload.error === "string" && payload.error.trim()) {
        message = payload.error.trim();
      }
    } catch {
      // Keep the status-based fallback for empty or non-JSON error responses.
    }
    throw new ApiError(r.status, message);
  }
  if (r.status === 204) return undefined as T;
  return r.json();
}

// sseUrl builds a URL for Server-Sent Events streams.
//
// Production (static export / Docker): the Go backend serves the web UI *and* the
// SSE streams on the same port, so we default to a same-origin (relative) URL.
// This is what makes reverse-proxy deploys work: a page loaded from
// https://<domain>/ connects to https://<domain>/api/..., which the proxy forwards
// to the backend — no need to expose :8787 publicly. Hardcoding :8787 here used to
// break exactly that (https://<domain>:8787/... is unreachable when only 443 is open).
//
// Dev (next dev): SSE must NOT go through the Next.js `/api` rewrite — that proxy
// buffers the streamed response, so event frames never reach the browser (the
// EventSource opens but receives 0 messages). So in dev only we connect straight
// to the Go backend on :8787, whose CORS is open.
//
// Override either default with NEXT_PUBLIC_SSE_BASE (set it to "" to force same-origin).
// Token is appended as ?token= because SSE can't carry cookies cross-origin.
// mockReport returns a canned Markdown report for the demo.
function mockReport(_task?: string): string {
  return `# ARTEX 渗透测试报告 — Acme Corp

## 概览
- 范围：acme.com（含 www / admin / api / shop / vpn 子域）
- 已确认发现：6 项（高危 3 · 中危 3 · 低危 2）
- 引擎模式：exploring

## 关键发现
1. **[高] 后台默认口令** admin.acme.com admin/admin123 → 可完全接管后台。
2. **[高] SQL 注入** www.acme.com/search?q= → 可读取 acme_prod 库。
3. **[高] IDOR** api.acme.com/v1/orders?id= → 可越权读取他人订单（含手机号/地址）。
4. **[中] 反射型 XSS**、**暴露 .git 源码**、**登录无速率限制**。

## 建议
- 后台强制改密 + 启用 MFA、封禁默认口令。
- search 接口参数化查询、输出编码。
- API 增加对象级授权校验（IDOR）、更换强 JWT 密钥。

> （demo）本报告由 mock 数据生成，仅用于界面演示。`;
}

export function sseUrl(path: string): string {
  const base =
    process.env.NEXT_PUBLIC_SSE_BASE ??
    (process.env.NODE_ENV !== "production" && typeof window !== "undefined"
      ? `${window.location.protocol}//${window.location.hostname}:8787`
      : "");
  const token = getToken();
  const sep = path.includes("?") ? "&" : "?";
  return token ? `${base}${path}${sep}token=${encodeURIComponent(token)}` : `${base}${path}`;
}

const get = <T>(p: string) => http<T>(p);
const post = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "POST", body: body ? JSON.stringify(body) : undefined });
const put = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "PUT", body: body ? JSON.stringify(body) : undefined });
const patch = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "PATCH", body: body ? JSON.stringify(body) : undefined });
const del = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "DELETE", body: body ? JSON.stringify(body) : undefined });

// Go serializes nil slices as JSON null — coerce to [].
const arr = <T>(x: T[] | null | undefined): T[] => x ?? [];
const tq = (task?: string, sep: "?" | "&" = "?") => (task ? `${sep}task=${encodeURIComponent(task)}` : "");

// findingFilterParams 把发现页的筛选条件序列化成 query string。列表 / 分组 /
// 资产树 / 导出共用同一份,新增筛选项只改这里(后端也只解析这一份)。
function findingFilterParams(q: Omit<FindingQuery, "page" | "pageSize">): URLSearchParams {
  const p = new URLSearchParams();
  if (q.severity && q.severity !== "all") p.set("severity", q.severity);
  if (q.status && q.status !== "all") p.set("status", q.status);
  if (q.vulnclass && q.vulnclass !== "all") p.set("vulnclass", q.vulnclass);
  if (q.task && q.task !== "all") p.set("task_id", q.task);
  if (q.query?.trim()) p.set("q", q.query.trim());
  if (q.sort) p.set("sort", q.sort);
  if (q.assetScope) p.set("asset_scope", q.assetScope);
  return p;
}

function interceptPageQuery(page: number, size: number, filter: InterceptApprovalFilter) {
  const query = new URLSearchParams({ page: String(page), size: String(size) });
  if (filter.status) query.set("status", filter.status);
  if (filter.decision_source) query.set("decision_source", filter.decision_source);
  return query.toString();
}

export const api = {
  // 后端应用版本号（release 时由 ldflags 注入，默认 "dev"）。
  health: () => get<{ ok: boolean; service: string; version: string }>("/health"),

  // ---- auth ----
  authStatus: () => get<{ initialized: boolean }>("/auth/status"),
  login: (username: string, password: string) => post<{ token: string }>("/auth/login", { username, password }),
  initPassword: (password: string) => post<{ token: string }>("/auth/init", { password }),
  changePassword: (oldPassword: string, newPassword: string) =>
    post<{ ok: boolean }>("/auth/change-password", { old_password: oldPassword, new_password: newPassword }),

  // ---- tasks ----
  tasks: () =>
    get<{ tasks: Task[]; active: string }>("/tasks").then((r) => ({ tasks: arr(r.tasks), active: r.active ?? "" })),
  task: (id: string) => get<Task>(`/tasks/${encodeURIComponent(id)}`),
  createTask: (input: {
    name?: string;
    categoryId?: number;
    description: string;
    goal: string;
    llmProfileIds?: number[];
    sourceTaskIds?: string[];
    companyIds?: number[];
    timeoutSeconds?: number;
    seedFirstIntent?: boolean;
    planHeartbeatSeconds?: number;
    coverageEnabled?: boolean;
    interceptRules?: AssetInterceptRuleInput[];
  }) =>
    post<Task>("/tasks", {
      name: input.name ?? "",
      category_id: input.categoryId ?? null,
      description: input.description,
      goal: input.goal,
      llm_profile_ids: input.llmProfileIds ?? [],
      source_task_ids: input.sourceTaskIds ?? [],
      company_ids: input.companyIds ?? [],
      timeout_seconds: input.timeoutSeconds ?? 0,
      seed_first_intent: input.seedFirstIntent ?? false,
      plan_heartbeat_seconds: input.planHeartbeatSeconds ?? 0, // 0 = 后端归一到默认 600(10min)
      coverage_enabled: input.coverageEnabled ?? true, // 默认开;false=关闭资产覆盖度功能
      intercept_rules: input.interceptRules ?? [], // 任务级资产拦截规则
    }),
  taskCategories: () => get<{ categories: TaskCategory[] }>("/task-categories").then((r) => arr(r.categories)),
  updateTask: (id: string, input: { name?: string; pinned?: boolean }) => patch<Task>(`/tasks/${id}`, input),
  renameTask: (id: string, name: string) => patch<Task>(`/tasks/${id}`, { name }),
  pinTask: (id: string, pinned: boolean) => patch<Task>(`/tasks/${id}`, { pinned }),
  createTaskCategory: (name: string) => post<TaskCategory>("/task-categories", { name }),
  renameTaskCategory: (id: number, name: string) => patch<TaskCategory>(`/task-categories/${id}`, { name }),
  deleteTaskCategory: (id: number) => del<{ deleted: number }>(`/task-categories/${id}`),
  updateTaskCategory: (taskId: string, categoryId?: number) =>
    patch<Task>(`/tasks/${taskId}/category`, { category_id: categoryId ?? null }),
  // categoryId 省略/undefined = 移出分类（后端收到 null）
  updateTasksCategory: (taskIds: string[], categoryId?: number) =>
    post<{ items: BatchCategoryItem[]; category: TaskCategory | null }>("/tasks/category/batch", {
      task_ids: taskIds,
      category_id: categoryId ?? null,
    }),
  taskTemplates: () => get<{ templates: TaskTemplate[] }>("/task-templates").then((r) => arr(r.templates)),
  createTaskTemplate: (
    input: Pick<TaskTemplate, "name" | "description" | "goal" | "category_id" | "intercept_rules">,
  ) => post<TaskTemplate>("/task-templates", input),
  updateTaskTemplate: (
    id: number,
    input: Partial<Pick<TaskTemplate, "name" | "description" | "goal" | "category_id" | "intercept_rules">>,
  ) => patch<TaskTemplate>(`/task-templates/${id}`, input),
  deleteTaskTemplate: (id: number) => del<{ deleted: number }>(`/task-templates/${id}`),
  updateTaskLLMProfiles: (id: string, llmProfileIds: number[], activeLLMProfileId?: number) =>
    put<{
      id: string;
      llm_profile_ids: number[];
      active_llm_profile_id?: number;
      llm_failover_state: string;
      reopened_intents: number;
      switch_event?: Activity;
    }>(`/tasks/${id}/llm`, {
      llm_profile_ids: llmProfileIds,
      active_llm_profile_id: activeLLMProfileId ?? null,
    }),
  deleteTask: (id: string, options: DeleteTaskOptions) => del<DeleteTaskResult>(`/tasks/${id}`, options),
  // C7 人工有序收尾：走完整 settle 时序落终态（区别于硬 cancel）。
  settleTask: (id: string) => post<{ ok: boolean }>(`/tasks/${id}/settle`, {}),
  controlTask: (id: string, action: "pause" | "resume") =>
    post<{ id: string; paused: boolean; queued: boolean; status: string; blocked_intents?: number }>(
      `/tasks/${id}/control`,
      { action },
    ),
  controlTasksBatch: (taskIds: string[], action: "pause" | "resume") =>
    post<{ items: BatchControlItem[] }>("/tasks/control/batch", { task_ids: taskIds, action }),
  taskArchives: (input?: { page?: number; size?: number; q?: string; state?: string }) => {
    const query = new URLSearchParams();
    query.set("page", String(input?.page ?? 1));
    query.set("size", String(input?.size ?? 20));
    if (input?.q) query.set("q", input.q);
    if (input?.state) query.set("state", input.state);
    return get<TaskArchivePage>(`/task-archives?${query.toString()}`).then((response) => ({
      ...response,
      items: arr(response.items),
    }));
  },
  taskArchive: (id: number) => get<TaskArchive>(`/task-archives/${id}`),
  archiveTask: (id: string) => post<TaskArchive>(`/tasks/${id}/archive`),
  archiveTasks: (taskIds: string[]) =>
    post<{ items: ArchiveBatchItem[] }>("/tasks/archive/batch", { task_ids: taskIds }),
  restoreTaskArchive: (id: number) => post<TaskArchive>(`/task-archives/${id}/restore`),
  restoreTaskArchives: (archiveIds: number[]) =>
    post<{ items: ArchiveBatchItem[] }>("/task-archives/restore/batch", { archive_ids: archiveIds }),
  deleteTaskArchive: (id: number) => del<TaskArchive>(`/task-archives/${id}`),
  deleteTaskArchives: (archiveIds: number[]) =>
    post<{ items: ArchiveBatchItem[] }>("/task-archives/delete/batch", { archive_ids: archiveIds }),
  controlIntent: (
    taskId: string,
    intentId: string,
    action: "pause" | "resume" | "cancel",
    reason?: string,
    // cancel 专用:soft(默认,假删除,意图置 deleted + 记删除原因)| hard(真删除,级联移除独占子孙)。
    mode?: "soft" | "hard",
  ) =>
    post<{
      id: number;
      state: "paused" | "open" | "deleted" | "";
      deleted?: { intents: number; facts: number; findings: number; activities: number };
    }>(`/tasks/${taskId}/intents/${intentId}/control`, { action, reason: reason ?? "", mode: mode ?? "soft" }),
  sendWorkerMessage: (taskId: string, intentId: string, message: string, requestId: string) =>
    post<{
      id: number;
      state: "running";
      accepted: true;
      request_id: string;
    }>(`/tasks/${taskId}/intents/${intentId}/messages`, { message, request_id: requestId }),
  taskLLMResolution: (id: string) => get<TaskLLMResolutions>(`/tasks/${id}/llm/resolution`),
  // 重跑一条没跑成功的意图(blocked/exhausted/stopped)：置回 open，worker 会重新认领、从头再跑。
  rerunIntent: (taskId: string, intentId: string) =>
    post<{ id: string; reopened: number }>(`/tasks/${taskId}/intents/${intentId}/rerun`),
  // 批量重跑本任务全部 blocked 意图（一次网络/LLM 断连导致多条 blocked 时一键全部重试）。
  rerunBlocked: (taskId: string) => post<{ id: string; reopened: number }>(`/tasks/${taskId}/intents/rerun-blocked`),
  setActive: (id: string) => post<{ active: string }>("/active", { id }),
  // ---- stats ----
  stats: (task?: string) => get<Stats>(`/stats${tq(task)}`),
  // 资产测试覆盖度(粗估，供参考)：范围内资产被 fact 碰过的占比 + 按类型的 总数/已测。
  taskCoverage: (id: string) =>
    get<{
      enabled: boolean; // 资产覆盖度功能是否开启；false 时其余字段为零值
      scope_rows: number;
      denominator: number;
      tested: number;
      pct: number | null;
      by_type: { type: string; total: number; tested: number }[];
    }>(`/tasks/${id}/coverage`),
  // 资产覆盖图：范围内全部资产 + 连接用的根域名/公司节点，含 tested/in_scope。
  taskCoverageGraph: (id: string) => get<CoverageGraphData>(`/tasks/${id}/coverage-graph`),
  // ---- 业务建模（第三张图）× 偏锋探索（合并施工）----
  // 业务图（按公司；task_id 可选=按任务范围读）。
  bizGraph: (companyID: number, taskID?: number, layer?: string) =>
    get<BizGraphData>(
      `/business-graph?company_id=${companyID}${taskID ? `&task_id=${taskID}` : ""}${layer ? `&layer=${layer}` : ""}`,
    ),
  // 双层差异清单。
  bizDiffs: (params: { company_id?: number; task_id?: number; status?: string; severity?: string; limit?: number }) => {
    const q = new URLSearchParams();
    if (params.company_id) q.set("company_id", String(params.company_id));
    if (params.task_id) q.set("task_id", String(params.task_id));
    if (params.status) q.set("status", params.status);
    if (params.severity) q.set("severity", params.severity);
    if (params.limit) q.set("limit", String(params.limit));
    return get<{ diffs: BizLayerDiff[] }>(`/business-graph/diffs?${q.toString()}`);
  },
  // 人工裁决一条差异（confirmed/false_positive/open），写审计链。
  bizDiffDecision: (diffID: number, status: string, reason: string) =>
    post<{ ok: boolean }>(`/business-graph/diffs/${diffID}/decision`, { status, reason }),
  // 任务维度的业务图 + 摘要（任务详情 tab 用）。
  taskBizGraph: (id: string) =>
    get<BizGraphData & { summary: { design_nodes: number; behavior_nodes: number; open_diffs: number } }>(
      `/tasks/${id}/biz-graph`,
    ),
  // 偏锋格点覆盖与判据进度 + stop_reason 统计（M8 评测口径数据源）。
  taskEdgeCoverage: (id: string) =>
    get<EdgeCoverage & { stop_stats: Record<string, number> }>(`/tasks/${id}/edge-coverage`),
  // 小程序 token 池（清单面：标签+指纹，无凭据内容）。
  mpTokens: (taskId: number, appId?: string) =>
    get<{ tokens: MiniprogramToken[] }>(`/miniprogram/tokens?task_id=${taskId}${appId ? `&app_id=${appId}` : ""}`),
  // 保存/轮换 token（落库即加密；此处只回指纹）。
  mpTokenSave: (p: { task_id: number; app_id: string; label: string; value: string; scope?: string }) =>
    post<{ ok: boolean; fingerprint: string }>("/miniprogram/tokens", p),
  // 删除 token。
  mpTokenDelete: (id: number) => del<{ ok: boolean }>(`/miniprogram/tokens/${id}`),
  // 解包产物目录（artifacts/miniprogram/）。
  mpArtifacts: () => get<{ apps: MiniprogramArtifact[] }>("/miniprogram/artifacts"),
  // 运行时地址（内嵌浏览器面板的数据面；部署配置，agent 不可改）。
  mpRuntime: () => get<{ url: string }>("/miniprogram/runtime"),
  mpRuntimeSet: (url: string) => put<{ ok: boolean }>("/miniprogram/runtime", { url }),
  // 任务级宿主授权集合（桌面控制闸门第五分支的判定依据）。
  taskHostTargets: (id: string) => get<{ targets: string[] }>(`/tasks/${id}/host-targets`),
  taskSetHostTargets: (id: string, targets: string[]) =>
    put<{ ok: boolean }>(`/tasks/${id}/host-targets`, { targets }),
  // 方法论注册表（偏锋总览页）。
  edgeMethodologies: () => get<{ methodologies: EdgeMethodology[] }>("/edge/methodologies"),
  // 偏锋任务总览（判据进度 + stop 统计）。
  edgeTasks: () => get<{ tasks: EdgeTaskOverview[] }>("/edge/tasks"),
  // 全局 llm_usage 聚合（仪表盘新版 token 视图）：按 profile 总量 + 按天分桶。
  usageStats: (days = 365) => get<UsageStats>(`/tokens/usage?days=${days}`),
  // ---- 目标管理（总览）----
  // 本任务全部目标（text/vulnclass/state）。
  taskGoals: (id: string) =>
    get<{ goals: TaskGoal[] | null }>(`/tasks/${id}/goals`).then((response) => ({ goals: arr(response.goals) })),
  // 人工新增目标：写入图谱并通知 planner、复活任务。
  addGoal: (id: string, text: string, vulnclass?: string) =>
    post<TaskGoal>(`/tasks/${id}/goals`, { text, vulnclass: vulnclass ?? "" }),
  // 人工修改目标文本（及 vulnclass）：通知 planner「由 old 变为 new」、复活任务。
  updateGoal: (id: string, goalId: string, text: string, vulnclass?: string) =>
    patch<TaskGoal>(`/tasks/${id}/goals/${goalId}`, { text, vulnclass: vulnclass ?? "" }),
  // 人工删除目标（硬删除）：通知 planner，删除不复活任务。
  deleteGoal: (id: string, goalId: string) => del<{ ok: boolean }>(`/tasks/${id}/goals/${goalId}`),

  // ---- 操作约束管理（总览）----
  // 本任务全部操作约束（allow/deny）。
  taskConstraints: (id: string) =>
    get<{ constraints: TaskConstraint[] | null }>(`/tasks/${id}/constraints`).then((response) => ({
      constraints: arr(response.constraints),
    })),
  // 新增约束（不通知 planner，下一轮规划自然读到）。
  addConstraint: (id: string, text: string, kind: TaskConstraint["kind"]) =>
    post<TaskConstraint>(`/tasks/${id}/constraints`, { text, kind }),
  // 修改约束（文本 + allow/deny）。
  updateConstraint: (id: string, constraintId: string, text: string, kind: TaskConstraint["kind"]) =>
    patch<TaskConstraint>(`/tasks/${id}/constraints/${constraintId}`, { text, kind }),
  // 删除约束。
  deleteConstraint: (id: string, constraintId: string) =>
    del<{ ok: boolean }>(`/tasks/${id}/constraints/${constraintId}`),

  // ---- 任务级资产拦截/允许规则（总览）----
  taskInterceptRules: (id: string) =>
    get<{ rules: AssetInterceptRule[] | null }>(`/tasks/${id}/intercept-rules`).then((r) => arr(r.rules)),
  createTaskInterceptRule: (id: string, rule: AssetInterceptRuleInput) =>
    post<AssetInterceptRule>(`/tasks/${id}/intercept-rules`, rule),
  updateTaskInterceptRule: (id: string, ruleId: number, rule: AssetInterceptRuleInput) =>
    put<AssetInterceptRule>(`/tasks/${id}/intercept-rules/${ruleId}`, rule),
  deleteTaskInterceptRule: (id: string, ruleId: number) =>
    del<{ deleted: boolean }>(`/tasks/${id}/intercept-rules/${ruleId}`),
  toggleTaskInterceptRule: (id: string, ruleId: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/tasks/${id}/intercept-rules/${ruleId}/toggle`, { enabled }),

  // 本任务测试范围列表（含继承自来源任务的范围）。
  taskScope: (id: string) => get<{ scope: TaskScopeRow[] }>(`/tasks/${id}/scope`),
  // 手动新增一条测试范围（kind=company/root_domain/subdomain/ip/cidr）。
  addTaskScope: (id: string, kind: string, value: string, reason?: string) =>
    post<TaskScopeRow>(`/tasks/${id}/scope`, { kind, value, reason }),
  // 删除本任务的一条测试范围。
  deleteTaskScope: (id: string, scopeId: number) => del<{ ok: boolean }>(`/tasks/${id}/scope/${scopeId}`),
  // 某资产在本任务里关联的意图 / 事实 / 发现（覆盖图节点抽屉用）。
  taskAssetRefs: (id: string, assetId: number) => get<CoverageAssetRefs>(`/tasks/${id}/asset-refs?asset_id=${assetId}`),

  // ---- workspace file manager (workDir) ----
  workspaceList: (path = "") => get<WorkspaceListing>(`/workspace/list?path=${encodeURIComponent(path)}`),
  workspaceRead: (path: string) => get<WorkspaceFile>(`/workspace/read?path=${encodeURIComponent(path)}`),
  workspaceWrite: (path: string, content: string) =>
    post<{ ok: boolean; path: string }>(`/workspace/write`, { path, content }),
  workspaceMkdir: (path: string) => post<{ ok: boolean; path: string }>(`/workspace/mkdir`, { path }),
  workspaceDelete: (path: string) => del<{ ok: boolean }>(`/workspace/delete?path=${encodeURIComponent(path)}`),
  workspaceUpload: async (dir: string, files: File[]) => {
    if (MOCK) return { uploaded: files.length };
    const fd = new FormData();
    for (const f of files) fd.append("file", f);
    const token = getToken();
    const r = await fetch(`/api/workspace/upload?path=${encodeURIComponent(dir)}`, {
      method: "POST",
      headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: fd,
    });
    if (!r.ok) throw new Error(`上传失败: ${r.status}`);
    return r.json() as Promise<{ uploaded: number }>;
  },
  workspaceDownload: async (path: string) => {
    let blob: Blob;
    if (MOCK) {
      blob = new Blob([`（demo）${path} 的下载内容示例。`], { type: "text/plain" });
    } else {
      const token = getToken();
      const r = await fetch(`/api/workspace/download?path=${encodeURIComponent(path)}`, {
        headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      });
      if (!r.ok) throw new Error(`下载失败: ${r.status}`);
      blob = await r.blob();
    }
    const objUrl = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = objUrl;
    a.download = path.split("/").pop() || "download";
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(objUrl);
  },

  // ---- 报告中心（/system/reports）----
  reports: () => get<{ reports: ReportMeta[]; count: number }>("/reports").then((r) => r.reports ?? []),
  reportGet: (id: number) => get<ReportFull>(`/reports/${id}`),
  createReport: (title: string, content = "") => post<{ id: number }>("/reports", { title, content }),
  updateReport: (id: number, patch: { title?: string; content?: string }) =>
    put<{ ok: boolean }>(`/reports/${id}`, patch),
  deleteReport: (id: number) => del<{ ok: boolean }>(`/reports/${id}`),
  reportsDiscover: () =>
    get<{ items: ReportCandidate[]; count: number }>("/reports/discover").then((r) => r.items ?? []),
  importReport: (path: string) => post<{ id: number; title: string }>("/reports/import", { path }),
  reportDownload: async (id: number, title: string) => {
    const token = getToken();
    const r = await fetch(`/api/reports/${id}/download`, {
      headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
    });
    if (!r.ok) throw new Error(`下载失败: ${r.status}`);
    const blob = await r.blob();
    const objUrl = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = objUrl;
    a.download = `${title || "report"}.md`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(objUrl);
  },

  // ---- assets ----
  // Server-side paginated: pass limit/offset, get back the page + full match total.
  assets: (type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(`/assets?type=${type}&limit=${limit}&offset=${offset}`).then(
      (r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 }),
    ),
  searchAssets: (dsl: string, type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(
      `/assets?dsl=${encodeURIComponent(dsl)}${type ? `&type=${encodeURIComponent(type)}` : ""}&limit=${limit}&offset=${offset}`,
    ).then((r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 })),
  assetCounts: (taskId = "") =>
    get<Record<string, number>>(`/assets/counts${taskId ? `?task_id=${encodeURIComponent(taskId)}` : ""}`),
  deleteAssets: (ids: number[]) =>
    http<{ deleted: number }>("/assets", { method: "DELETE", body: JSON.stringify({ ids }) }),
  // task-scoped view of the same endpoint — server-side paginated like `assets`
  taskAssets: (taskId: string, type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(
      `/assets?task_id=${encodeURIComponent(taskId)}&type=${encodeURIComponent(type)}&limit=${limit}&offset=${offset}`,
    ).then((r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 })),
  // DSL search scoped to a task — the backend forces the task_id filter, so it
  // always stays within that task's assets (same DSL grammar as `searchAssets`).
  searchTaskAssets: (taskId: string, dsl: string, type = "", limit = 50, offset = 0) =>
    get<{ count: number; total: number; assets: Asset[] }>(
      `/assets?task_id=${encodeURIComponent(taskId)}&dsl=${encodeURIComponent(dsl)}&type=${encodeURIComponent(type)}&limit=${limit}&offset=${offset}`,
    ).then((r) => ({ assets: r?.assets ?? [], total: r?.total ?? r?.count ?? 0 })),
  attachTaskAssets: (taskId: string, assetIds: number[], sourceSummary: string) =>
    post<TaskAssetMutation>(`/tasks/${taskId}/assets`, {
      asset_ids: assetIds,
      source_summary: sourceSummary,
    }),
  registerTaskAssetScopes: (taskId: string, scope: CompanyScopeRule[]) =>
    post<TaskAssetScopeMutation>(`/tasks/${taskId}/assets`, { scope }),
  detachTaskAsset: (taskId: string, assetId: number) => del<{ detached: number }>(`/tasks/${taskId}/assets/${assetId}`),
  taskIntentAssets: (taskId: string) =>
    get<{ assets: IntentAsset[] }>(`/tasks/${taskId}/intent-assets`).then((r) => arr(r.assets)),
  // 文件导入资产：解析平台工作区内的上传件（uploads/…，表格主机清单），逐条过域名校验
  // 后 upsert。skipped = 非法域名/写入失败的条数，notes 只留前 5 条原因。
  assetImportFile: (task_id: string, path: string) =>
    post<{ ok: boolean; imported: number; skipped: number; notes: string[] }>("/assets/import-file", {
      task_id,
      path,
    }),

  // ---- companies (企业 + 资产范围；归属唯一来源) ----
  companies: () => get<Company[]>("/companies").then(arr),
  createCompany: (name: string, scope: CompanyScopeRule[]) =>
    post<{ id: number; created: boolean; scope_added?: number; scope_invalid?: number; scope_errors?: string[] }>(
      "/companies",
      {
        name,
        scope,
      },
    ),
  addCompanyScope: (id: number, scope: CompanyScopeRule[], reason = "") =>
    post<CompanyScopeMutation>(`/companies/${id}/scope`, {
      scope,
      reason,
    }),
  updateCompanyScope: (id: number, scope: CompanyScopeRule[], reason = "") =>
    post<CompanyScopeMutation>(`/companies/${id}/scope`, {
      scope,
      reason,
      reset: true,
    }),
  deleteCompany: (id: number, deleteAssets = false) =>
    http<{ deleted: number; assets_deleted: number }>(`/companies/${id}`, {
      method: "DELETE",
      body: JSON.stringify({ delete_assets: deleteAssets }),
    }),

  // ---- exploration (per task) ----
  frontier: (task?: string) => get<TaskNode[]>(`/exploration/frontier${tq(task)}`).then(arr),
  findings: (task?: string) => get<Finding[]>(`/exploration/findings${tq(task)}`).then(arr),
  findingsPage: (q: FindingQuery) => {
    const p = findingFilterParams(q);
    p.set("page", String(q.page));
    p.set("limit", String(q.pageSize));
    return get<FindingsPage>(`/exploration/findings?${p.toString()}`);
  },
  findingGroups: (q: FindingQuery) => {
    const p = findingFilterParams(q);
    p.set("page", String(q.page));
    p.set("limit", String(q.pageSize));
    return get<FindingGroupsPage>(`/exploration/findings/groups?${p.toString()}`);
  },
  // findingAssetTree 取「按资产」视图的左侧树:只含有发现的资产及其祖先,
  // 节点带子树聚合计数。不分页——树是导航结构,一次取完。
  findingAssetTree: (q: Omit<FindingQuery, "page" | "pageSize">) =>
    get<FindingAssetTree>(`/exploration/findings/asset-tree?${findingFilterParams(q).toString()}`),
  findingStats: () => get<FindingStats>("/exploration/findings/stats"),
  // exportFindings 触发发现页导出并下载文件。scope=selected 时传 ids(finding_id 列表);
  // scope=filtered 时传当前筛选(沿用 FindingQuery 的筛选字段);scope=all 忽略筛选。
  exportFindings: async (opts: {
    format: "md-single" | "md-zip" | "csv" | "json";
    scope: "filtered" | "all" | "selected";
    filters?: Omit<FindingQuery, "page" | "pageSize">;
    ids?: string[];
  }) => {
    const p = new URLSearchParams({ format: opts.format, scope: opts.scope });
    if (opts.scope === "selected") {
      p.set("ids", (opts.ids ?? []).join(","));
    } else if (opts.scope === "filtered" && opts.filters) {
      for (const [k, v] of findingFilterParams(opts.filters)) p.set(k, v);
    }
    const token = getToken();
    const r = await fetch(`/api/exploration/findings/export?${p.toString()}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(`export: ${r.status}`);
    const blob = await r.blob();
    // 文件名优先取后端 Content-Disposition,取不到则用默认名。
    const disp = r.headers.get("Content-Disposition") ?? "";
    const m = disp.match(/filename="?([^"]+)"?/);
    const filename = m?.[1] ?? `findings-export`;
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  },
  getFinding: (id: string, contextTaskId?: string) =>
    get<Finding>(
      `/exploration/findings/${id}${contextTaskId ? `?context_task=${encodeURIComponent(contextTaskId)}` : ""}`,
    ),
  findingTraffic: (id: string, contextTask?: string) =>
    get<FindingTraffic>(
      `/exploration/findings/${id}/traffic${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  bindFindingTraffic: (id: string, traffic_refs: TrafficEvidenceRef[], contextTask?: string) =>
    post<FindingTraffic>(
      `/exploration/findings/${id}/traffic${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { traffic_refs },
    ),
  editFindingTraffic: (
    id: string,
    bindingId: string,
    version: number,
    fields: { role: TrafficEvidenceRole; note: string },
    contextTask?: string,
  ) =>
    patch<FindingTraffic>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { version, ...fields },
    ),
  removeFindingTraffic: (id: string, bindingId: string, version: number, contextTask?: string) =>
    del<FindingTraffic>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { version },
    ),
  orderFindingTraffic: (id: string, binding_ids: string[], version: number, contextTask?: string) =>
    http<FindingTraffic>(
      `/exploration/findings/${id}/traffic/order${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { method: "PUT", body: JSON.stringify({ binding_ids, version }) },
    ),
  findingTrafficDetail: (id: string, bindingId: string, contextTask?: string) =>
    get<FindingTrafficDetail>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  findingTrafficBody: (
    id: string,
    bindingId: string,
    side: "request" | "response",
    offset: number,
    contextTask?: string,
  ) =>
    get<EvidenceBodyPreview>(
      `/exploration/findings/${id}/traffic/${bindingId}/body?side=${side}&offset=${offset}${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),
  downloadFindingTrafficBody: async (
    id: string,
    bindingId: string,
    side: "request" | "response",
    contextTask?: string,
  ) => {
    const token = getToken();
    const response = await fetch(
      `/api/exploration/findings/${id}/traffic/${bindingId}/body?side=${side}&download=1${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { headers: token ? { Authorization: `Bearer ${token}` } : {} },
    );
    if (!response.ok) {
      const error = await response.json().catch(() => ({ error: "下载失败" }));
      throw new Error(error.error ?? "下载失败");
    }
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `evidence-${bindingId}-${side}.bin`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
  // 漏洞链路:该漏洞节点回溯到任务初始节点的子图(节点 + 关系)。
  findingLineage: (id: string) => get<{ nodes: TaskNode[]; edges: Edge[] }>(`/exploration/findings/${id}/lineage`),
  setFindingStatus: (id: string, status: FindingStatus) => patch<Finding>(`/exploration/findings/${id}`, { status }),
  setFindingSeverity: (id: string, severity: Severity) => patch<Finding>(`/exploration/findings/${id}`, { severity }),
  // 一次保存漏洞的名称/类别/严重等级(发现列表行内编辑用),只传出现的字段。
  updateFinding: (
    id: string,
    fields: { name?: string; vulnclass?: string; severity?: Severity; status?: FindingStatus },
  ) => patch<Finding>(`/exploration/findings/${id}`, fields),
  // 删除漏洞:移除 findings 记录 + 来源探索节点(从发现列表/任务发现 Tab/探索图一并消失)。
  deleteFinding: (id: string) => del<{ deleted: boolean; id: number }>(`/exploration/findings/${id}`),
  findingRetests: (id: string) =>
    get<{ retests: FindingRetest[] }>(`/exploration/findings/${encodeURIComponent(id)}/retests`).then((r) =>
      arr(r.retests),
    ),
  activeFindingRetests: () =>
    get<{ retests: ActiveFindingRetest[] }>("/exploration/findings/retests/active").then((r) => arr(r.retests)),
  startFindingRetest: (id: string, notes: string) =>
    post<{ retest: FindingRetest; created: boolean }>(`/exploration/findings/${encodeURIComponent(id)}/retests`, {
      notes,
    }),
  deepenFinding: (id: string, description: string) =>
    post<FindingDeepenResponse>(`/exploration/findings/${id}/deepen`, { description }),
  intents: (task?: string) => get<TaskNode[]>(`/exploration/intents${tq(task)}`).then(arr),
  tokenStats: (task?: string) =>
    get<{ workers: TokenUsage[]; sessions: SessionTokenUsage[]; total: TokenTotal }>(
      `/exploration/tokens${tq(task)}`,
    ).then((r) => ({
      workers: arr(r.workers),
      sessions: arr(r.sessions),
      total: r.total,
    })),
  tokenDaily: (days = 30) => get<DailyTokenBucket[]>(`/tokens/daily?days=${days}`).then(arr),
  conversationTokens: () =>
    get<ConvTokenSummary[]>("/tokens/conversations")
      .then(arr)
      .catch(() => [] as ConvTokenSummary[]),
  explorationGraph: (task?: string) => get<{ nodes: TaskNode[]; edges: Edge[] }>(`/exploration/graph${tq(task)}`),
  // 播报板:服务端按创建顺序分页的探索节点(默认最新在前)。
  explorationNodes: (task: string, query: ExplorationNodeQuery = {}) => {
    const q = new URLSearchParams();
    if (task) q.set("task", task);
    q.set("page", String(query.page ?? 1));
    q.set("size", String(query.size ?? 20));
    if (query.kinds?.length) q.set("kind", query.kinds.join(","));
    if (query.states?.length) q.set("state", query.states.join(","));
    if (query.q?.trim()) q.set("q", query.q.trim());
    if (query.order === "asc") q.set("order", "asc");
    return get<ExplorationNodePage>(`/exploration/nodes?${q.toString()}`).then((r) => ({
      items: arr(r.items),
      total: r.total ?? 0,
      page: r.page ?? 1,
      size: r.size ?? query.size ?? 20,
      edges: arr(r.edges),
      refs: r.refs ?? {},
      assets: r.assets ?? {},
    }));
  },
  activity: (task?: string, opts?: { intent?: string; since?: number; limit?: number }) => {
    const q = new URLSearchParams();
    if (task) q.set("task", task);
    if (opts?.intent) q.set("intent", opts.intent);
    if (opts?.since) q.set("since", String(opts.since));
    if (opts?.limit) q.set("limit", String(opts.limit));
    return get<{ items: Activity[]; cursor: number }>(`/exploration/activity?${q.toString()}`).then((r) => ({
      items: arr(r.items),
      cursor: r.cursor ?? 0,
    }));
  },
  activityDetail: (id: number, task?: string) => get<{ detail: string }>(`/exploration/activity/${id}${tq(task)}`),
  // Reverse-paginated session history. session = "main" | "plan" | "intent:<id>".
  // before=0 → latest page; before=<id> → the older page ending before that id.
  // snapshotCursor is the TASK-level max id at query time — open the task SSE at
  // since=snapshotCursor so history (id≤cursor) and the live tail (id>cursor) meet
  // gap-free. hasMore = still-older steps exist (drives scroll-up loading).
  activityHistory: (task: string, session: string, before = 0, limit = 200) => {
    const q = new URLSearchParams({ session, limit: String(limit) });
    if (task) q.set("task", task);
    if (before > 0) q.set("before", String(before));
    return get<{ items: Activity[]; snapshot_cursor: number; earliest_cursor: number; has_more: boolean }>(
      `/exploration/activity/history?${q.toString()}`,
    ).then((r) => ({
      items: arr(r.items),
      snapshotCursor: r.snapshot_cursor ?? 0,
      earliestCursor: r.earliest_cursor ?? 0,
      hasMore: !!r.has_more,
    }));
  },
  // Paged worker (intent) session list — reaches past the legacy fixed 300 cap.
  // before=0 → newest page; before=<id> → older page. has_more = older intents exist.
  intentsPage: (task: string, before = 0, limit = 300) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (task) q.set("task", task);
    q.set("before", String(before > 0 ? before : 0));
    q.set("page", "1"); // marker so the backend returns the paged {items,has_more} shape
    return get<{ items: TaskNode[]; has_more: boolean }>(`/exploration/intents?${q.toString()}`).then((r) => ({
      items: arr(r.items),
      hasMore: !!r.has_more,
    }));
  },

  // Main-agent conversation segments of a task. Each segment is a resettable session
  // (clean transcript/context) over the same task; `current` is the writable one.
  mainSessions: (task: string) =>
    get<{ sessions: { seq: number; created_at: string }[]; current: number }>(
      `/exploration/main-sessions${tq(task)}`,
    ).then((r) => ({ sessions: arr(r.sessions), current: r.current ?? 0 })),
  // Start a fresh main-agent session segment (does not touch the task's graph/assets/goal).
  newMainSession: (task: string) =>
    post<{ seq: number; created_at: string; current: number }>(`/exploration/main-session/new${tq(task)}`),

  // ---- traffic / audit / report / chat ----
  audit: (task?: string) => get<Audit>(`/audit${tq(task)}`),
  traffic: (
    page = 0,
    size = 100,
    host = "",
    method = "",
    q = "",
    opts: {
      body?: string;
      path?: string;
      status?: string;
      respMin?: string;
      respMax?: string;
      sort?: string;
      order?: string;
    } = {},
  ) =>
    get<TrafficResp>(
      `/traffic?page=${page}&size=${size}` +
        (host ? `&host=${encodeURIComponent(host)}` : "") +
        (method && method !== "all" ? `&method=${encodeURIComponent(method)}` : "") +
        (q ? `&q=${encodeURIComponent(q)}` : "") +
        (opts.body ? `&body=${encodeURIComponent(opts.body)}` : "") +
        (opts.path ? `&path=${encodeURIComponent(opts.path)}` : "") +
        (opts.status && opts.status !== "all" ? `&status=${encodeURIComponent(opts.status)}` : "") +
        (opts.respMin ? `&resp_min=${encodeURIComponent(opts.respMin)}` : "") +
        (opts.respMax ? `&resp_max=${encodeURIComponent(opts.respMax)}` : "") +
        (opts.sort ? `&sort=${encodeURIComponent(opts.sort)}` : "") +
        (opts.order ? `&order=${encodeURIComponent(opts.order)}` : ""),
    ),
  trafficExchange: (id: string) => get<TrafficDetail>(`/traffic/exchange?id=${encodeURIComponent(id)}`),
  trafficHosts: () => get<{ hosts: TrafficHost[] }>(`/traffic/hosts`),
  trafficDeleteHost: (host: string) => del<{ deleted: number }>(`/traffic?host=${encodeURIComponent(host)}`),
  trafficDeleteHosts: (hosts: string[]) => del<{ deleted: number }>(`/traffic/hosts`, { hosts }),
  // Purges every exchange and compacts the index; `reclaimed` is the bytes of
  // index handed back to the filesystem. Evidence bound to findings is kept.
  trafficDeleteAll: () => del<{ deleted: number; reclaimed: number }>(`/traffic/all`),

  // ---- app settings (runtime toggles) ----
  settings: () => get<Settings>(`/settings`),
  setSettings: (patch: Partial<Settings>) => put<Settings>(`/settings`, patch),
  // 出网治理状态：未覆盖通道清单由后端给定，前端不复制也不兜底成空数组
  // （空数组 = 没有未覆盖通道，兜底会把「读不到」说成「都管住了」）。
  egressGovernance: () => get<EgressGovernanceState>(`/settings/egress-governance`),
  // Run a real "test" search with the given (or saved) config to verify it works.
  testWebSearch: (patch: {
    web_search_backend?: string;
    web_search_proxy?: string;
    brave_search_api_key?: string;
    tavily_search_api_key?: string;
  }) => post<{ ok: boolean; error?: string; count?: number; backend?: string }>(`/settings/web-search/test`, patch),

  // ---- 漏洞 IM 推送 ----
  // 渠道是多实例资源（同一类型可配多个机器人、各有过滤规则），因此独立成组，
  // 不塞进扁平的 settings 键值里。
  notifyMeta: () => get<NotificationMeta>(`/notify/meta`),
  notifyChannels: () => get<{ channels: NotificationChannel[] }>(`/notify/channels`).then((r) => arr(r.channels)),
  notifyCreateChannel: (payload: {
    name: string;
    kind: string;
    enabled?: boolean;
    mode?: string;
    config: Record<string, unknown>;
    filter?: NotificationFilter;
    rate_per_min?: number;
  }) => post<{ id: number }>(`/notify/channels`, payload),
  // PATCH 语义：只提交要改的字段。config 里的掩码值原样回传即表示「保持原值」。
  notifyUpdateChannel: (
    id: number,
    payload: {
      name?: string;
      kind?: string;
      enabled?: boolean;
      mode?: string;
      config?: Record<string, unknown>;
      filter?: NotificationFilter;
      rate_per_min?: number;
    },
  ) => patch<{ id: number }>(`/notify/channels/${id}`, payload),
  notifyDeleteChannel: (id: number) => del<{ ok: boolean }>(`/notify/channels/${id}`),
  // 同步发一条测试消息；失败时后端会把渠道的原始错误回传，供排查配置。
  notifyTestChannel: (id: number) => post<{ ok: boolean; latency_ms: number }>(`/notify/channels/${id}/test`),
  notifyDeliveries: (q: { channelId?: number; state?: string; page?: number; pageSize?: number } = {}) => {
    const p = new URLSearchParams();
    if (q.channelId) p.set("channel_id", String(q.channelId));
    if (q.state) p.set("state", q.state);
    p.set("page", String(q.page ?? 1));
    p.set("page_size", String(q.pageSize ?? 50));
    return get<{ deliveries: NotificationDelivery[]; total: number; page: number; page_size: number }>(
      `/notify/deliveries?${p.toString()}`,
    ).then((r) => ({ ...r, deliveries: arr(r.deliveries) }));
  },
  notifyRetryDelivery: (id: number) => post<{ ok: boolean }>(`/notify/deliveries/${id}/retry`),
  report: async (task?: string) => {
    if (MOCK) return mockReport(task);
    const token = getToken();
    const r = await fetch(`/api/report${tq(task)}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(`report: ${r.status}`);
    return r.text();
  },
  chatMentions: (kind: string, query: string, signal?: AbortSignal, cursor = "") =>
    http<{ items: ChatMention[]; next_cursor?: string }>(
      `/chat/mentions?${new URLSearchParams({ kind, q: query, cursor })}`,
      { signal },
    ),
  chat: (message: string, task?: string, attachments?: ChatAttachment[], seg?: number) =>
    post<{ reply: string; mode: string }>(`/chat${tq(task)}`, { message, attachments, seg }),
  chatStatus: (taskId: string) => get<{ running: boolean }>(`/tasks/${taskId}/chat/status`),
  // 方式1 文件上传:落到会话/任务工作目录 uploads/，返回可供 agent Read 的相对路径。
  chatUpload: async (scope: "task" | "session" | "staging", id: string, files: File[]) => {
    if (MOCK)
      return {
        attachments: files.map((f) => ({
          name: f.name,
          path: `uploads/${f.name}`,
          size: f.size,
          abs: `/mock/${f.name}`,
        })),
      };
    const fd = new FormData();
    for (const f of files) fd.append("file", f);
    const token = getToken();
    const r = await fetch(`/api/chat/upload?scope=${scope}&id=${encodeURIComponent(id)}`, {
      method: "POST",
      headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: fd,
    });
    if (!r.ok) throw new Error(`上传失败: ${r.status} ${await r.text()}`);
    return r.json() as Promise<{ attachments: ChatAttachment[] }>;
  },
  stopChat: (taskId: string) => post<{ status: string }>(`/tasks/${taskId}/chat/stop`, {}),
  gc: (ttl = 86400) => post<{ removed: number }>(`/gc?ttl=${ttl}`, {}),

  // ---- P2 分层经济 / P5 传承图 / P0 度量 ----
  rolePolicy: () => get<{ llm_role_policy: string }>("/settings").then((r) => r.llm_role_policy ?? ""),
  setRolePolicy: (llm_role_policy: string) => put<{ ok: boolean }>("/settings", { llm_role_policy }),
  legacyNodes: (state = "") =>
    get<{ nodes: LegacyNode[] | null }>(`/legacy/nodes${state ? `?state=${state}` : ""}`).then((r) => arr(r.nodes)),
  // F3 人工复核改判：对一条 deny/needs_review 裁决发放 call-precision 放行凭据。
  // Tier3 高危工具后端 409 拒绝；同工具改判 ≥3 次响应带 suggest_trust_rule。
  sodDecisionReview: (id: number, body: { scope: "once" | "task"; reason?: string; operator?: string }) =>
    post<{
      ok: boolean;
      override_id: number;
      scope: string;
      override_count_same_tool: number;
      suggest_trust_rule: boolean;
    }>(`/sod/decisions/${id}/review`, body),
  // 人工改判历史（职责分离页复核历史过滤 + 决策账本页「最近改判」卡）。
  sodOverrides: (taskId?: number) =>
    get<{ overrides: SodManualOverrideRow[]; count: number }>(`/sod/overrides${taskId ? `?task_id=${taskId}` : ""}`),
  // 评审经验：accepted 可带 override+reason 越级（三门未过时后端 409 返回
  // missing_gates + override_required——手工 fetch 是为了把 409 body 里的
  // 结构化缺门明细透传给评审弹窗，通用 http 助手只保留 error 字符串）。
  reviewLegacyNode: async (
    id: number,
    state: "accepted" | "retired" | "draft",
    opts?: { override?: boolean; reason?: string },
  ) => {
    const token = getToken();
    const r = await fetch(`/api/legacy/nodes/${id}/review`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({
        state,
        reviewer: "sir",
        override: opts?.override ?? false,
        reason: opts?.reason ?? "",
      }),
    });
    const body = (await r.json().catch(() => ({}))) as {
      ok?: boolean;
      error?: string;
      missing_gates?: string[];
      override_required?: boolean;
    };
    if (!r.ok) {
      const err = new Error(body?.error || `评审失败(${r.status})`) as Error & {
        missing_gates?: string[];
        override_required?: boolean;
      };
      err.missing_gates = body?.missing_gates;
      err.override_required = body?.override_required;
      throw err;
    }
    return body;
  },
  // v0.10.1 三门自动评估明细（结构完整/历史复盘/实战证据）：评审页在 draft 卡
  // 上直接画三门徽章——「自动走流程，显示在这里」，不等点了接受才弹含糊的错。
  legacyGates: (id: number) =>
    get<{ pass: boolean; missing_gates: string[]; note: string }>(`/legacy/nodes/${id}/gates`),
  // 删除一条经验（draft/retired；accepted 后端 409）。操作者身份走 query（后端审计留人）。
  deleteLegacyNode: (id: number) => del<{ ok: boolean }>(`/legacy/nodes/${id}?operator=${encodeURIComponent("sir")}`),
  runMetrics: (taskId: number) => get<Record<string, unknown>>(`/metrics/run?task_id=${taskId}`),

  // ---- 任务大厅（群聊）----
  // 发布任务（POST /api/task-hall/tasks）：一次调用建任务 + 声明范围 + 建大厅 +
  // 可选开额度，返回 task_id 与 group_id。一个任务至多一个大厅（重复发布 409）。
  publishTaskHallTask: (input: {
    name?: string;
    description?: string;
    goal: string;
    asset_ids?: number[];
    members?: string[];
    concurrency_limit?: number;
    budget_tokens?: number;
    category_id?: number; // 任务类别(task_categories.id)；缺省=未分类。资产收集类 ⇒ worker 追加收集纪律
    timeout_seconds?: number; // 任务墙钟(秒)；0=不限时，缺省由后端取 7200
  }) => post<{ task_id: number; group_id: number }>("/task-hall/tasks", input),
  // 大厅列表。/task-hall/tasks 带出成员与额度上限，/task-hall/groups 只有群本体，
  // 两个接口列的是同一批群（都读 chat_groups），这里用带成员的那个。
  taskHallTasks: () =>
    get<{ tasks: TaskHallRow[] | null }>("/task-hall/tasks").then((r) =>
      arr(r.tasks).map((row) => ({ ...row, members: arr(row.members) })),
    ),
  taskHallGroup: (id: number) => get<TaskHallGroupDetail>(`/task-hall/groups/${id}`),
  // 建群：一个 task 至多一个大厅，重复建会被后端判 409。
  createTaskHallGroup: (input: { task_id: number; name: string; members: string[]; concurrency_limit: number }) =>
    post<TaskHallGroupDetail>("/task-hall/groups", input),
  taskHallMessages: (id: number) =>
    get<{ messages: TaskHallMessage[] | null }>(`/task-hall/groups/${id}/messages`).then((r) => arr(r.messages)),
  // 人往群里插话。from_agent 由后端写死成 "human"；被准入规则拦下的消息照样返回，
  // 状态与拦截原因在响应里（status / blocked_reason）。
  postTaskHallMessage: (id: number, input: { to_agent?: string; body: string; kind?: string; intent_id?: number }) =>
    post<TaskHallMessage>(`/task-hall/groups/${id}/messages`, input),
  // 群内工具禁列表：出现的 key 就是被禁用的（表里没有 enabled 列 ⇒ 只能收紧）。
  taskHallDisabledTools: (id: number) =>
    get<{ disabled_tools: Record<string, boolean> | null }>(`/task-hall/groups/${id}/disabled-tools`).then(
      (r) => r.disabled_tools ?? {},
    ),
  disableTaskHallTool: (id: number, toolKey: string) =>
    post<{ ok: boolean; disabled_tools: Record<string, boolean> }>(`/task-hall/groups/${id}/disabled-tools`, {
      tool_key: toolKey,
    }),
  enableTaskHallTool: (id: number, toolKey: string) =>
    del<{ ok: boolean; disabled_tools: Record<string, boolean> }>(
      `/task-hall/groups/${id}/disabled-tools/${encodeURIComponent(toolKey)}`,
    ),
  // 额度读数。群组没配预算时后端回 404 —— 那是「未配置」这个状态，不是故障，
  // 所以这里兜成 null，不把「没配」报成「读失败」。
  taskHallQuota: async (id: number): Promise<TaskHallQuota | null> => {
    try {
      return await get<TaskHallQuota>(`/task-hall/groups/${id}/quota`);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) return null;
      throw e;
    }
  },
  // 改额度/状态。理由必填：额度与状态都是授权决定，没有理由事后追不回来。
  setTaskHallQuota: (id: number, input: { budget_tokens?: number; state?: string; reason: string }) =>
    put<{ quota: TaskHallQuota; audited: boolean }>(`/task-hall/groups/${id}/quota`, input),

  // ---- 策略库（L4）----
  // 策略列表。state 白名单："" | quarantined | active | retired | tombstone；
  // 越界后端回 400（"state 只能是 …"），所以这里只透传页面选出来的值。
  strategies: (state?: StrategyState) =>
    get<{ strategies: Strategy[] | null }>(`/strategies${state ? `?state=${encodeURIComponent(state)}` : ""}`).then(
      (r) => arr(r.strategies),
    ),
  // 策略详情：本体 + 证据链。404 = 策略不存在。
  strategy: (id: number) => get<StrategyDetail>(`/strategies/${id}`),
  // 提议列表。status 不传 = 只看待裁决的（后端默认就是 pending）。
  strategyProposals: (status?: string) =>
    get<{ proposals: StrategyProposal[] | null }>(
      `/strategy-proposals${status ? `?status=${encodeURIComponent(status)}` : ""}`,
    ).then((r) => arr(r.proposals)),
  // 裁决一条提议。⚠️ approved ≠ 生效：生效还要过 curator 的三条件。
  // reason / actor 都必填（后端缺一个就回 400），裁决进 audit_events 哈希链。
  decideStrategyProposal: (id: number, input: StrategyProposalDecision) =>
    post<{ ok: boolean; id: number; status: string }>(`/strategy-proposals/${id}/decide`, input),
  // 人工置位 / 解除 pin。pinned 必填（缺字段会被后端当成「解除唯一证人」），
  // reason / actor 也必填 —— 解 pin 是能断掉漏洞报告可复现性的动作。
  setStrategyPinned: (id: number, input: StrategyPinDecision) =>
    post<{ ok: boolean; id: number; pinned: boolean; changed: boolean; note?: string }>(`/strategies/${id}/pin`, input),
  // 人工退役。退役 ≠ 删除：后端写 state=retired + retired_reason，行与证据链都留着。
  retireStrategy: (id: number, input: StrategyDecision) =>
    post<{ ok: boolean; id: number; state: string }>(`/strategies/${id}/retire`, input),
  // pin 复审队列。⚠️ advisory_only：这个队列**只读**，绝不自动解 pin。
  strategyPinQueue: () =>
    get<{
      queue: Strategy[] | null;
      pinned_count: number;
      threshold: number;
      review_required: boolean;
      advisory_only: boolean;
    }>("/strategy-pin-queue").then((r): StrategyPinQueue => ({ ...r, queue: arr(r.queue) })),
  // 刷新 utility 物化视图（权威 utility 在视图里，策略表上的那一列只是缓存）。
  refreshStrategyUtility: () => post<{ ok: boolean }>("/strategies/refresh-utility", {}),
  // 摄入学习：报告/网页 URL 或粘贴的文本 → LLM 总结提炼 → 候选策略提议（进待裁决队列，
  // 不直接入库）。proposals=0 时 note 说明原因（如「素材中没有可沉淀的内容」）。
  strategyIngest: (body: { url?: string; text?: string }) =>
    post<{ ok: boolean; proposals: number; ids?: number[]; note?: string }>("/strategies/ingest", body),
  // 版本化编辑策略（name/summary/六元组 jsonb 列；jsonb 字段后端校验 JSON 合法性），
  // version = version+1。空 patch 后端回 400「没有可更新字段」。
  strategyEdit: (id: string | number, body: Record<string, unknown>) =>
    patch<{ ok: boolean; version: number }>(`/strategies/${id}`, body),
  // v0.9.17 注入拼装预览：把真正会进 agent 上下文的两块注入文本（策略摘要块 +
  // 经验块）原样取回。task_id 可选——给了 = 该任务的真实快照；不给 = active 池全量。
  strategyAssembled: (opts?: { taskId?: number; goal?: string }) => {
    const q = new URLSearchParams();
    if (opts?.taskId !== undefined && Number.isFinite(opts.taskId)) q.set("task_id", String(opts.taskId));
    if (opts?.goal) q.set("goal", opts.goal);
    const suffix = q.toString();
    return get<{
      strategies_block: string;
      legacy_block: string;
      entries: { strategy_id: number; name: string; summary: string; version: number; utility: number }[] | null;
      note: string;
      advisory_only: boolean;
    }>(`/strategies/assembled${suffix ? `?${suffix}` : ""}`).then((r) => ({
      ...r,
      entries: arr(r.entries),
    }));
  },

  // ---- SoD 链（职责分离）----
  // 决策账本。task_id 是任务的**数字 id**（= Task.id 转数字，后端就是数字主键）。
  // ⚠️ 读不出来时后端回 500（fail closed）—— 前端不把失败读成「没有决策」。
  sodDecisions: (opts?: { taskId?: number; limit?: number }) => {
    const q = new URLSearchParams();
    if (opts?.taskId !== undefined && Number.isFinite(opts.taskId)) q.set("task_id", String(opts.taskId));
    if (opts?.limit) q.set("limit", String(opts.limit));
    const suffix = q.toString();
    return get<{
      decisions: SodDecision[] | null;
      count: number;
      limit: number;
      tier_note: string;
    }>(`/sod/decisions${suffix ? `?${suffix}` : ""}`).then((r) => ({
      decisions: arr(r.decisions),
      count: r.count ?? 0,
      limit: r.limit ?? 0,
      tier_note: r.tier_note ?? "",
    }));
  },
  // 对抗复核队列。⚠️ advisory_only：这张队列交给 redteam 证伪，**不是**人工
  // 审批队列（人工审批走 intercept_pending），本页不提供任何裁决动作。
  sodReviewQueue: (opts?: { taskId?: number; limit?: number }) => {
    const q = new URLSearchParams();
    if (opts?.taskId !== undefined && Number.isFinite(opts.taskId)) q.set("task_id", String(opts.taskId));
    if (opts?.limit) q.set("limit", String(opts.limit));
    const suffix = q.toString();
    return get<{
      queue: SodReviewItem[] | null;
      count: number;
      limit: number;
      status: string;
      advisory_only: boolean;
      human_approval_queue: boolean;
      consumer: string;
      purpose: string;
      note: string;
    }>(`/sod/review-queue${suffix ? `?${suffix}` : ""}`).then((r) => ({
      queue: arr(r.queue),
      count: r.count ?? 0,
      limit: r.limit ?? 0,
      status: r.status ?? "",
      advisory_only: r.advisory_only !== false,
      human_approval_queue: r.human_approval_queue === true,
      consumer: r.consumer ?? "",
      purpose: r.purpose ?? "",
      note: r.note ?? "",
    }));
  },
  // 全部工具的熔断状态。列表只含「出过事」的工具（没出事的没有行）。
  sodCircuits: () =>
    get<{ circuits: SodCircuit[] | null; count: number; states: Record<string, number>; note: string }>(
      "/sod/circuit",
    ).then((r) => ({
      circuits: arr(r.circuits),
      count: r.count ?? 0,
      states: r.states ?? {},
      note: r.note ?? "",
    })),
  // 单个工具的熔断状态。没有行 ⇒ closed（还没出过事），不是「查不到」。
  sodCircuit: (toolKey: string) =>
    get<{ circuit: SodCircuit }>(`/sod/circuit/${encodeURIComponent(toolKey)}`).then((r) => r.circuit),

  // ---- 拦截种类 / auditor 建议队列 / 影子对照（方案 v4.1 §2/§3/§11-G1）----
  // 种类表（含停用与锁定行）。version 是表版本号：任何 CRUD 都会 bump，链侧缓存
  // 靠它失效；UI 只透传展示。
  sodCategories: () =>
    get<{ categories: SodCategory[] | null; version: string }>("/sod/categories").then((r) => ({
      categories: arr(r.categories),
      version: r.version ?? "",
    })),
  // 新建种类（custom 正则）。400=参数不合法；409=pattern 含锁定基线特征却想配成 pass——绕道不认。
  createSodCategory: (body: {
    name: string;
    kind?: string;
    pattern: string;
    action: SodCategoryAction;
    cap_per_task: number;
    fallback: SodCategoryFallback;
    priority: number;
    reason: string;
    created_by: string;
  }) => post<{ ok: boolean; category: SodCategory }>("/sod/categories", body),
  // 部分更新（动作/上限/兜底/启停/优先级/理由任选）。锁定行改动作后端 409，原文透传给 toast。
  updateSodCategory: (
    id: number,
    body: {
      action?: SodCategoryAction;
      cap_per_task?: number;
      fallback?: SodCategoryFallback;
      enabled?: boolean;
      priority?: number;
      reason?: string;
    },
  ) => post<{ ok: boolean; category: SodCategory }>(`/sod/categories/${id}`, body),
  // 删除种类。409 = 锁定基线行，不可删。
  deleteSodCategory: (id: number) => del<{ ok: boolean }>(`/sod/categories/${id}`),
  // auditor 的种类建议。status=pending 只看待裁决的；不传 = 全部。
  sodCategorySuggestions: (status?: string) =>
    get<{ suggestions: SodCategorySuggestion[] | null }>(
      `/sod/category-suggestions${status ? `?status=${encodeURIComponent(status)}` : ""}`,
    ).then((r) => arr(r.suggestions)),
  // 裁决一条建议：adopted=落成种类行（裁量经验沉淀为确定性规则）/ dismissed=忽略。
  decideSodCategorySuggestion: (id: number, decision: "adopted" | "dismissed", operator: string) =>
    post<{ ok: boolean; adopted: boolean }>(`/sod/category-suggestions/${id}/decide`, { decision, operator }),
  // 撤销采纳：删除采纳时落成的回链种类，把建议放回 pending。removed_category =
  // 是否真的删了一个种类（false = 种类已不在——可能被手动删过——只把建议放回）。
  revokeSodCategorySuggestion: (id: number, operator: string) =>
    post<{ ok: boolean; removed_category: boolean }>(`/sod/category-suggestions/${id}/revoke`, { operator }),
  // 影子对照：近期新旧链裁决差异（倒序，limit 后端截到 500）+ 当前合并闸 mode。
  sodShadowDiffs: (limit = 100) =>
    get<{ diffs: ShadowDiff[] | null; mode: SodMergedGateMode }>(`/sod/shadow?limit=${limit}`).then((r) => ({
      diffs: arr(r.diffs),
      mode: r.mode ?? "off",
    })),
  // 合并闸三态开关：off（旧链）/ shadow（灰度对照）/ enforce（新链接管）。推进与回退都走它。
  sodMergedGate: () => get<{ mode: SodMergedGateMode }>("/sod/merged-gate").then((r) => r.mode ?? "off"),
  setSodMergedGate: (mode: SodMergedGateMode, operator: string) =>
    post<{ ok: boolean; mode: SodMergedGateMode }>("/sod/merged-gate", { mode, operator }),

  // 裁决大屏聚合 feed（方案 v6 主题 A）：recent + pending + stats + mode 一次拉全，
  // 2s 轮询。决策动作不在这里——复用下面的 interceptDecide，不新开权限面。
  sodFeed: () => get<SodFeed>("/sod/feed"),

  // 任务治理聚合视图（方案 v10 §9 E1-6，只读）：本任务裁决流 + 人工改判凭据 +
  // 系统事件一次拉全，供任务详情「治理」tab。taskId 是任务的数字 id。
  sodGovernance: (taskId: string | number) =>
    get<TaskGovernance>(`/tasks/${taskId}/governance`).then((r) => ({
      ...r,
      decisions: arr(r.decisions),
      overrides: arr(r.overrides),
      system_events: arr(r.system_events),
    })),
  // 生效 hook 链的只读描述（E1-8）：可热改面 + 代码常量只读面。
  hookChain: () =>
    get<SodHookChain>("/sod/hookchain").then((r) => ({
      editable: arr(r.editable),
      readonly: arr(r.readonly),
    })),

  // 审计事件明细分页（方案 v10 §9 E1-8）：audit_events 哈希链逐条明细（倒序），
  // 与 /audit/chain 的链校验同源。append-only，页面上没有任何写入口。
  auditEvents: (page = 1, size = 50) =>
    get<AuditEventsPage>(`/audit/events?page=${page}&size=${size}`).then((r) => ({
      ...r,
      items: arr(r.items),
    })),

  // 审计链校验。⚠️ 断链是**结论**（200 + intact=false），不是接口坏了 ——
  // 所以这里不把 intact=false 当异常抛出去。
  auditChain: () => get<AuditChainVerification>("/audit/chain"),

  // ---- LLM ----
  getLLM: () =>
    get<{
      configured: boolean;
      provider: string;
      model: string;
      base_url: string;
      proxy?: string;
      key_set: boolean;
      rate_per_second?: number;
      rate_per_minute?: number;
      context_window_k?: number;
      thinking_type?: string;
      reasoning_effort?: string;
    }>("/llm"),
  setLLM: (
    provider: string,
    model: string,
    base_url: string,
    api_key: string,
    rate_per_second = 0,
    rate_per_minute = 0,
    proxy = "",
    context_window_k = 0,
  ) => post("/llm", { provider, model, base_url, proxy, api_key, rate_per_second, rate_per_minute, context_window_k }),
  testLLM: (
    provider: string,
    model: string,
    base_url: string,
    api_key: string,
    proxy = "",
    thinking_type = "",
    reasoning_effort = "",
    profile_id?: number,
    streaming = true, // 用该配置真实的收发模式来测，别让"流式能通、非流式不通"漏到会话里
    session_header_key = "", // 非空=测试请求也带该自定义会话头（值为一次性 session id）
  ) =>
    // reply = 模型实际回复(已截断);一个字都不回的配置后端直接判失败
    post<{ ok: boolean; error?: string; latency_ms?: number; model?: string; reply?: string }>("/llm/test", {
      provider,
      model,
      base_url,
      proxy,
      api_key,
      thinking_type,
      reasoning_effort,
      profile_id,
      streaming,
      session_header_key,
    }),
  llmProfiles: () => get<{ profiles: LLMProfile[] }>("/llm/profiles").then((r) => arr(r.profiles)),
  // 思考档位能力发现（方案 v9.2 批次 A）：注册表查询 + 逐档探测验证。
  thinkingLevels: (format: string, model: string) =>
    get<ThinkingCapability>(
      `/llm/thinking-levels?format=${encodeURIComponent(format)}&model=${encodeURIComponent(model)}`,
    ),
  thinkingProbe: (p: {
    provider: string;
    model: string;
    base_url: string;
    proxy?: string;
    api_key?: string;
    thinking_type?: string;
    reasoning_effort?: string;
    profile_id?: number;
    streaming?: boolean;
    session_header_key?: string;
    shape?: string; // effort|switch（省略=按注册表）
    levels: string[]; // 待探测档位（≤6，后端截断）
  }) =>
    post<{
      ok: boolean;
      error?: string;
      shape?: string;
      results: { level: string; ok: boolean; latency_ms?: number; reply?: string; error?: string }[];
    }>("/llm/thinking-probe", p),
  saveLLMProfile: (p: {
    id?: number; // omit/0 = create; set = update that profile
    name: string;
    format: string;
    model: string;
    models?: string[]; // E1 多模型：该 profile 可用模型列表（含默认模型）
    base_url?: string;
    base_urls?: string[]; // 网关候选地址：完整候选列表（第一项 = 主地址）；空数组 = 只用 base_url
    proxy?: string;
    api_key?: string; // blank on update keeps the existing key
    rate_per_second?: number;
    rate_per_minute?: number;
    context_window_k?: number;
    thinking_type?: string; // ""(不发送)|"disabled"|"enabled"
    reasoning_effort?: string; // ""(不发送)|"low"|"medium"|"high"|"xhigh"|"max"
    priority?: number; // 轮询顺位，越大越先用
    pool_exclude?: boolean; // true=不作为故障转移目标
    streaming?: boolean; // true(默认)=流式 | false=非流式
    max_tokens?: number; // 单次回复输出上限；0=不发送，由服务端默认值决定
    max_tokens_field?: string; // ""=max_tokens(默认) | "max_completion_tokens"（仅 openai 格式）
    session_header_key?: string; // 非空=每次请求带该 HTTP 头，头值=当前会话 session id；""=不发送
    retry?: LLMRetryOverride; // 本配置的重试覆盖；各项留 0 = 跟随全局重试策略
  }) => post<{ id: number }>("/llm/profiles", p),
  deleteLLMProfile: (id: string) => del<{ deleted: number }>(`/llm/profiles/${id}`),
  activateLLMProfile: (id: string) => post<{ ok: boolean }>("/llm/profiles/active", { id: Number(id) }),
  // 轮询链的实际顺序 + 各配置的熔断状态。
  llmPool: () => get<LLMPoolStatus>("/llm/pool"),
  // 清除熔断，让下一次调用立刻重试该配置；不传 id = 全部清除。
  resetLLMPool: (id?: string) => post<LLMPoolStatus>("/llm/pool/reset", { id: id ? Number(id) : 0 }),
  // 全局重试策略（五层各自的次数+间隔）。全 0 = 全部走内置默认。
  llmRetryPolicy: () => get<LLMRetryPolicy>("/llm/retry-policy"),
  saveLLMRetryPolicy: (p: LLMRetryPolicy) => post<LLMRetryPolicy>("/llm/retry-policy", p),
  fetchLLMModels: (provider: string, base_url: string, api_key: string, proxy = "", profile_id?: number) =>
    post<{ ok: boolean; error?: string; models?: string[] }>("/llm/models", {
      provider,
      base_url,
      api_key,
      proxy,
      profile_id,
    }),

  // ---- agents ----
  agents: () => get<{ agents: Agent[] }>("/agents").then((r) => arr(r.agents)),
  getAgent: (key: string) => get<AgentDetail>(`/agents/${key}`),
  createAgent: (key: string, name: string, description = "") => post<Agent>("/agents", { key, name, description }),
  updateAgent: (key: string, name: string, description = "") =>
    patch<{ ok: boolean }>(`/agents/${key}`, { name, description }),
  deleteAgent: (key: string) => del<{ deleted: string }>(`/agents/${key}`),

  // ---- conversations (chat page) ----
  conversations: () => get<{ conversations: Conversation[] }>("/conversations").then((r) => arr(r.conversations)),
  createConversation: (agent_key: string, title = "", llm_profile_id?: number | null) =>
    post<Conversation>("/conversations", { agent_key, title, llm_profile_id: llm_profile_id ?? null }),
  updateConversation: (id: number, input: { title?: string; pinned?: boolean }) =>
    patch<Conversation>(`/conversations/${id}`, input),
  renameConversation: (id: number, title: string) => patch<Conversation>(`/conversations/${id}`, { title }),
  pinConversation: (id: number, pinned: boolean) => patch<Conversation>(`/conversations/${id}`, { pinned }),
  updateConversationProfile: (id: number, llm_profile_id: number | null) =>
    patch<{ ok: boolean }>(`/conversations/${id}/profile`, { llm_profile_id }),
  deleteConversation: (id: number) => del<{ deleted: number }>(`/conversations/${id}`),
  deleteConversations: (ids: number[]) =>
    post<{ items: { id: number; ok: boolean; error?: string }[] }>("/conversations/delete/batch", { ids }),
  // Incremental tail: steps after `since` (id ASC) — live poll + post-send fetch.
  conversationMessages: (id: number, since = 0) =>
    get<{ items: Activity[]; cursor: number; running: boolean }>(`/conversations/${id}/messages?since=${since}`).then(
      (r) => ({ items: arr(r.items), cursor: r.cursor ?? 0, running: !!r.running }),
    ),
  // Reverse pagination: the latest `limit` steps (before=0) or the page of older
  // steps ending before id `before`. hasMore = still-older steps exist.
  conversationHistory: (id: number, before = 0, limit = 200) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (before > 0) q.set("before", String(before));
    return get<{ items: Activity[]; cursor: number; running: boolean; hasMore: boolean }>(
      `/conversations/${id}/messages?${q.toString()}`,
    ).then((r) => ({ items: arr(r.items), cursor: r.cursor ?? 0, running: !!r.running, hasMore: !!r.hasMore }));
  },
  conversationMsgDetail: (id: number, seq: number) =>
    get<{ detail: string }>(`/conversations/${id}/messages/${seq}`).then((r) => r.detail ?? ""),
  sendConversationMessage: (id: number, message: string, attachments?: ChatAttachment[]) =>
    post<{ status: string }>(`/conversations/${id}/messages`, { message, attachments }),
  stopConversation: (id: number) => post<{ status: string }>(`/conversations/${id}/stop`, {}),

  // ---- sessions (chat page, session-based) ----
  //
  // 会话是第一类对象：一个 session 下可以有多个成员（agent），人在同一条消息流里
  // 与 agent 往来。旧的 /api/conversations 仍可用（兼容期），但新界面优先走这里。
  // 端点与形状见方案书 §4.5；类型定义在 lib/session-types.ts。
  sessions: () => get<{ sessions: Session[] }>("/sessions").then((r) => arr(r.sessions)),
  createSession: (input: SessionCreateInput) => post<Session>("/sessions", input),
  session: (id: number) => get<Session>(`/sessions/${id}`),
  updateSession: (id: number, input: SessionPatchInput) => patch<Session>(`/sessions/${id}`, input),
  deleteSession: (id: number) => del<{ deleted: number }>(`/sessions/${id}`),
  // 会话消息（含 agent 消息、收据条、线程）。since=0 取全部，之后按 id 增量拉。
  sessionMessages: (id: number, since = 0) =>
    get<{ items: SessionMessage[]; cursor: number; running: boolean }>(`/sessions/${id}/messages?since=${since}`).then(
      (r) => ({ items: arr(r.items), cursor: r.cursor ?? 0, running: !!r.running }),
    ),
  // 发言：@Agent名 路由由 to_agent 表达（空串 = 广播给全会话）。
  postSessionMessage: (id: number, input: SessionMessageInput) =>
    post<{ status: string }>(`/sessions/${id}/messages`, input),
  // 热加入：会话中随时加 agent（@ 未在席的 agent 时前端先调用它）。
  addSessionMember: (id: number, agent_key: string, role: SessionRole = "member") =>
    post<{ ok: boolean }>(`/sessions/${id}/members`, { agent_key, role }),
  // 退出：成员离席，历史消息归属不变。
  removeSessionMember: (id: number, agent_key: string) =>
    del<{ ok: boolean }>(`/sessions/${id}/members/${encodeURIComponent(agent_key)}`),
  // 更新成员在场状态（presence）：working / blocked(+eta,blocked_on) / listening / idle。
  updateSessionMemberPresence: (
    id: number,
    agent_key: string,
    input: { presence: string; eta_minutes?: number; blocked_on?: string },
  ) => patch<{ ok: boolean; member: SessionMember }>(`/sessions/${id}/members/${encodeURIComponent(agent_key)}`, input),
  // 成员状态（presence 三态）。
  sessionPresence: (id: number) =>
    get<{ presence: SessionMember[] }>(`/sessions/${id}/presence`).then((r) => arr(r.presence)),
  // 反查：某个工件关联了哪些会话（工件 → 会话方向）。
  sessionsForAttachment: (attachment_type: string, identity_key: string) =>
    get<{ sessions: Session[] }>(
      `/sessions/attachments/${encodeURIComponent(attachment_type)}/${encodeURIComponent(identity_key)}/sessions`,
    ).then((r) => arr(r.sessions)),

  saveAgentPrompt: (key: string, template: string, note = "") =>
    put<{ version: number }>(`/agents/${key}/prompt`, { template, note }),
  resetAgentPrompt: (key: string) => post<{ version: number }>(`/agents/${key}/prompt/reset`, {}),
  // 收尾提示词(超时/步数耗尽的 settlement 提示);prompt 空串=清除覆盖、用内置默认;
  // max_turns 省略则不动、传 0=用内置默认轮数
  saveAgentWrapup: (key: string, prompt: string, maxTurns?: number) =>
    put<{ ok: boolean }>(`/agents/${key}/wrapup`, { prompt, max_turns: maxTurns }),
  resetAgentWrapup: (key: string) =>
    post<{ ok: boolean; wrapup_default: string; wrapup_max_turns_default: number }>(`/agents/${key}/wrapup/reset`, {}),
  // 任务级超时收尾词(仅 worker/planner);prompt 空=清除、用内置默认
  saveAgentTaskTimeoutWrapup: (key: string, prompt: string, maxTurns?: number) =>
    put<{ ok: boolean }>(`/agents/${key}/wrapup/task-timeout`, { prompt, max_turns: maxTurns }),
  resetAgentTaskTimeoutWrapup: (key: string) =>
    post<{ ok: boolean; task_timeout_wrapup_default: string; task_timeout_wrapup_max_turns_default: number }>(
      `/agents/${key}/wrapup/task-timeout/reset`,
      {},
    ),
  // P3 triggers (仅自定义 agent)
  agentTriggers: (key: string) =>
    get<{ triggers: AgentTrigger[] }>(`/agents/${key}/triggers`).then((r) => arr(r.triggers)),
  createTrigger: (key: string, t: Omit<AgentTrigger, "id" | "agent_key" | "last_fire">) =>
    post<AgentTrigger>(`/agents/${key}/triggers`, t),
  updateTrigger: (id: number, t: Omit<AgentTrigger, "id" | "agent_key" | "last_fire">) =>
    patch<{ ok: boolean }>(`/triggers/${id}`, t),
  deleteTrigger: (id: number) => del<{ deleted: number }>(`/triggers/${id}`),
  saveAgentConfig: (
    key: string,
    patch: {
      llm_profile_id?: number | null; // number=绑定；null=解绑(跟随任务/全局)；缺省=不动
      max_turns?: number;
      run_seconds?: number;
      web_search?: boolean;
      interactive_shell?: boolean;
      trigger_run_mode?: "serial" | "parallel";
      trigger_merge_mode?: "by_task" | "all" | "none";
      trigger_max_parallel?: number;
      thinking_effort?: string; // ""=跟随绑定 profile 默认；"enabled"/"disabled"=开关；其余=effort 档位
    },
  ) => put<{ ok: boolean }>(`/agents/${key}/config`, patch),
  agentPromptVersions: (key: string) =>
    get<{ versions: PromptVersion[] }>(`/agents/${key}/prompts`).then((r) => arr(r.versions)),
  agentVariables: (key: string) =>
    get<{ variables: PromptVar[] }>(`/agents/${key}/variables`).then((r) => arr(r.variables)),
  previewAgentPrompt: (key: string, template: string, sample?: Record<string, string>) =>
    post<{ rendered: string; error?: string }>(`/agents/${key}/prompt/preview`, { template, sample }),
  getAgentVisibility: (key: string) => get<{ mcp: number[]; skill: string[] }>(`/agents/${key}/visibility`),
  setAgentVisibility: (key: string, mcp: number[], skill: string[]) =>
    put<{ ok: boolean }>(`/agents/${key}/visibility`, { mcp, skill }),

  // ---- tools (内置工具目录) ----
  tools: () => get<{ tools: Tool[] }>("/tools").then((r) => arr(r.tools)),
  saveTool: (key: string, patch: Pick<Tool, "description" | "schema" | "agents" | "enabled">) =>
    put<{ ok: boolean }>(`/tools/${key}`, patch),
  resetTool: (key: string) => post<{ ok: boolean }>(`/tools/${key}/reset`, {}),
  // custom tools (自定义工具)
  createCustomTool: (
    t: Pick<Tool, "key" | "description" | "schema" | "agents" | "enabled" | "kind" | "exec" | "deferred">,
  ) => post<{ key: string }>("/tools/custom", t),
  updateCustomTool: (
    key: string,
    t: Pick<Tool, "description" | "schema" | "agents" | "enabled" | "kind" | "exec" | "deferred">,
  ) => put<{ ok: boolean }>(`/tools/custom/${key}`, t),
  deleteCustomTool: (key: string) => del<{ deleted: string }>(`/tools/custom/${key}`),
  testCustomTool: (body: { kind: string; exec: Record<string, unknown>; params: Record<string, unknown> }) =>
    post<{ output: string; is_error: boolean }>("/tools/custom/test", body),
  detectPython: () => post<{ python_interpreter: string }>("/settings/python/detect", {}),

  // ---- mcp ----
  mcpServers: () => get<{ servers: MCPServer[] }>("/mcp").then((r) => arr(r.servers)),
  saveMcpServer: (m: Partial<MCPServer>) => post<{ id: number }>("/mcp", m),
  deleteMcpServer: (id: number) => del<{ deleted: number }>(`/mcp/${id}`),
  mcpTools: (id: number) => get<{ tools: MCPTool[] }>(`/mcp/${id}/tools`).then((r) => arr(r.tools)),
  refreshMcpServer: (id: number) => post<{ tools: MCPTool[] }>(`/mcp/${id}/refresh`, {}).then((r) => arr(r.tools)),

  // ---- 资产同步 (ScopeSentry 数据源) ----
  ssStatus: () =>
    get<{ exists: boolean; configured: boolean; enabled: boolean; reachable: boolean; url?: string; tools: string[] }>(
      "/sync/scopesentry/status",
    ),
  ssDatasource: (body: { url?: string; api_key?: string }) =>
    post<{ id: number; enabled: boolean }>("/sync/scopesentry/datasource", body),
  ssProjects: (page = 1, size = 50, search = "") =>
    get<{ projects: SSProject[]; tag: Record<string, number> }>(
      `/sync/scopesentry/projects?page=${page}&size=${size}${search ? `&search=${encodeURIComponent(search)}` : ""}`,
    ).then((r) => ({ projects: arr(r.projects), tag: r.tag ?? {} })),
  ssTasks: (page = 1, size = 50, search = "") =>
    get<{ tasks: SSTask[] }>(
      `/sync/scopesentry/tasks?page=${page}&size=${size}${search ? `&search=${encodeURIComponent(search)}` : ""}`,
    ).then((r) => arr(r.tasks)),
  ssSync: (body: {
    dimension: "project" | "task";
    targets: string[];
    asset_types: string[];
    create_company?: boolean;
    page_size?: number;
  }) =>
    post<{
      synced: Record<string, number>;
      companies: string[] | null;
      warnings: string[] | null;
      errors: string[] | null;
    }>("/sync/scopesentry/sync", body),

  // ---- skills (文件系统) ----
  skills: () => get<{ skills: SkillItem[] }>("/skills").then((r) => arr(r.skills)),
  createSkill: (s: {
    name: string;
    description: string;
    license?: string;
    compatibility?: string;
    mcps?: string[];
    instructions?: string;
  }) => post<{ name: string }>("/skills", s),
  // uploadSkill installs a skill from a .zip (multipart). Surfaces the backend
  // error text (e.g. 已存在 / 缺少 SKILL.md) so the UI can show a precise message.
  uploadSkill: async (file: File, overwrite = false): Promise<{ name: string; files: number }> => {
    if (MOCK) return { name: file.name.replace(/\.zip$/i, ""), files: 1 };
    const fd = new FormData();
    fd.append("file", file);
    const token = getToken();
    const r = await fetch(`/api/skills/upload${overwrite ? "?overwrite=true" : ""}`, {
      method: "POST",
      body: fd,
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body?.error || `上传失败(${r.status})`);
    return body;
  },
  deleteSkill: (name: string) => del<{ deleted: string }>(`/skills/${name}`),
  updateSkillMeta: (
    name: string,
    meta: { mcps?: string[]; description?: string; license?: string; compatibility?: string },
  ) => put<{ ok: boolean }>(`/skills/${name}/meta`, meta),
  createSkillDir: (skill: string, path: string) => post<{ dir: string }>(`/skills/${skill}/dirs`, { path }),
  skillFiles: (name: string) => get<{ files: string[] }>(`/skills/${name}/files`).then((r) => r.files),
  readSkillFile: (name: string, file: string) =>
    get<{ content: string; file: string }>(`/skills/${name}/files/${file}`).then((r) => r.content),
  writeSkillFile: (name: string, file: string, content: string) =>
    put<{ ok: boolean }>(`/skills/${name}/files/${file}`, { content }),
  deleteSkillPath: (skill: string, path: string) => del<{ deleted: string }>(`/skills/${skill}/files/${path}`),
  skillUsage: (name: string, limit = 50) =>
    get<{ calls: SkillCall[] }>(`/skills/${name}/usage?limit=${limit}`).then((r) => arr(r.calls)),
  missingSkills: (limit = 20) =>
    get<{ missing: MissingSkill[] }>(`/skills/missing?limit=${limit}`).then((r) => arr(r.missing)),

  // ---- visibility (MCP resource side) ---- (agent ids are strings per spec)
  resourceVisibility: (kind: string, id: number) =>
    get<{ agents: string[] }>(`/visibility/${kind}/${id}`).then((r) => arr(r.agents)),
  toggleVisibility: (agentId: string, kind: string, resourceId: number, visible: boolean) =>
    post<{ ok: boolean }>("/visibility/toggle", { agent_id: agentId, kind, resource_id: resourceId, visible }),

  // ---- visibility (Skill，按名称) ----
  skillVisibility: (name: string) => get<{ agents: string[] }>(`/visibility/skill/${name}`).then((r) => arr(r.agents)),
  toggleSkillVisibility: (agentId: string, skillName: string, visible: boolean) =>
    post<{ ok: boolean }>("/visibility/skill/toggle", { agent_id: agentId, skill_name: skillName, visible }),

  // ---- intercept rules ----
  interceptRules: () => get<{ rules: InterceptRule[] }>("/intercept/rules").then((r) => arr(r.rules)),
  // F2 信任规则：allow 通路上的具名视图（^$ 锚定 + Tier3 拒绝 + 默认停用 + 上限 50）。
  trustRules: () =>
    get<{ rules: InterceptRule[]; count: number; max: number }>("/intercept/trust-rules").then((r) => arr(r.rules)),
  createTrustRule: (body: {
    tool: string;
    pattern: string;
    task_id?: number;
    reason?: string;
    operator?: string;
    confirm?: boolean;
  }) =>
    post<{ ok: boolean; rule?: InterceptRule; enabled?: boolean; suggest?: boolean }>("/intercept/trust-rules", body),
  previewTrustRule: (body: { tool: string; pattern: string; task_id?: number }) =>
    post<{ would_allow: number; scanned: number; window_days: number; samples: { id: number; preview: string }[] }>(
      "/intercept/trust-rules/preview",
      body,
    ),
  createInterceptRule: (rule: Omit<InterceptRule, "id" | "created_at" | "updated_at">) =>
    post<InterceptRule>("/intercept/rules", rule),
  updateInterceptRule: (id: number, rule: Omit<InterceptRule, "id" | "created_at" | "updated_at">) =>
    put<InterceptRule>(`/intercept/rules/${id}`, rule),
  deleteInterceptRule: (id: number) => del<{ deleted: number }>(`/intercept/rules/${id}`),
  toggleInterceptRule: (id: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/intercept/rules/${id}/toggle`, { enabled }),

  // ---- asset intercept rules（资产拦截：全局黑名单） ----
  assetInterceptRules: () => get<{ rules: AssetInterceptRule[] }>("/asset-intercept/rules").then((r) => arr(r.rules)),
  createAssetInterceptRule: (rule: Pick<AssetInterceptRule, "enabled" | "kind" | "pattern" | "note">) =>
    post<AssetInterceptRule>("/asset-intercept/rules", rule),
  updateAssetInterceptRule: (id: number, rule: Pick<AssetInterceptRule, "enabled" | "kind" | "pattern" | "note">) =>
    put<AssetInterceptRule>(`/asset-intercept/rules/${id}`, rule),
  deleteAssetInterceptRule: (id: number) => del<{ deleted: number }>(`/asset-intercept/rules/${id}`),
  toggleAssetInterceptRule: (id: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/asset-intercept/rules/${id}/toggle`, { enabled }),

  // ---- intercept pending (ask) ----
  interceptPending: () => get<{ pending: InterceptPending[] }>("/intercept/pending").then((r) => arr(r.pending)),
  interceptGetOne: (id: number) => get<InterceptPending>(`/intercept/pending/${id}`),
  // E1-5（方案 v10 §9）：批准时可选「同时放行本任务后续同类调用」——taskTool=true
  // 时 body 带 task_tool，签发 (task, tool, agent) 级通配凭据，随任务终态过期；
  // 基线类工具后端照旧拒绝签发（响应 task_tool_issued=false）。仅 allowed 有意义。
  interceptDecide: (id: number, decision: "allowed" | "denied", taskTool = false) =>
    post<{ ok: boolean; task_tool_issued?: boolean }>(`/intercept/pending/${id}/decide`, {
      decision,
      ...(taskTool ? { task_tool: true } : {}),
    }),
  interceptExecution: (id: number, conversationId?: number) =>
    get<import("@/lib/types").InterceptExecution>(
      `/intercept/history/${id}/execution${conversationId ? `?conversation=${conversationId}` : ""}`,
    ),
  interceptDetail: (id: number) => get<InterceptDetail>(`/intercept/history/${id}`),
  interceptHistory: () => get<{ items: InterceptApprovalRow[] }>("/intercept/history").then((r) => arr(r.items)),
  interceptHistoryPage: (page = 1, size = 20, filter: InterceptApprovalFilter = {}) =>
    get<{ items: InterceptApprovalRow[]; total?: number }>(
      `/intercept/history?${interceptPageQuery(page, size, filter)}`,
    ).then((r) => ({
      items: arr(r.items),
      total: r.total ?? r.items?.length ?? 0,
    })),
  interceptTask: (taskId: string) =>
    get<{ items: InterceptApprovalRow[] }>(`/intercept/task/${taskId}`).then((r) => arr(r.items)),
  interceptTaskPage: (taskId: string, page = 1, size = 20, filter: InterceptApprovalFilter = {}) =>
    get<{ items: InterceptApprovalRow[]; total?: number }>(
      `/intercept/task/${encodeURIComponent(taskId)}?${interceptPageQuery(page, size, filter)}`,
    ).then((r) => ({
      items: arr(r.items),
      total: r.total ?? r.items?.length ?? 0,
    })),

  // ---- intercept tool-config (全局工具拦截范围) ----
  interceptGetToolConfig: async (): Promise<{ enabled_tools: string[] }> => {
    if (MOCK) return { enabled_tools: ["bash"] };
    const token = getToken();
    const r = await fetch("/api/intercept/tool-config", {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },
  interceptSetToolConfig: async (enabledTools: string[]): Promise<void> => {
    if (MOCK) return;
    const token = getToken();
    const r = await fetch("/api/intercept/tool-config", {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ enabled_tools: enabledTools }),
    });
    if (!r.ok) throw new Error(await r.text());
  },

  // ---- intercept LLM judge (模型兜底审批,全局配置) ----
  interceptGetJudgeConfig: () => get<JudgeConfig>("/intercept/judge"),
  interceptSetJudgeConfig: (cfg: JudgeConfig) => put<{ ok: boolean }>("/intercept/judge", cfg),
  interceptJudgeUsage: (days = 30) => get<JudgeUsage>(`/intercept/judge/usage?days=${days}`),

  // ---- commands (tool execution history, any tool) ----
  commands: (params?: { task?: string; q?: string; status?: "error" | "ok"; page?: number; size?: number }) => {
    const sp = new URLSearchParams();
    if (params?.task) sp.set("task", params.task);
    if (params?.q) sp.set("q", params.q);
    if (params?.status) sp.set("status", params.status);
    sp.set("page", String(params?.page ?? 0));
    sp.set("size", String(params?.size ?? 50));
    return get<{ commands: CommandRecord[]; total: number }>(`/commands?${sp}`);
  },
  // 各工具调用次数；沿用列表的 task/q 筛选，统计的是整个结果集而非当前页。
  commandStats: (params?: { task?: string; q?: string }) => {
    const sp = new URLSearchParams();
    if (params?.task) sp.set("task", params.task);
    if (params?.q) sp.set("q", params.q);
    return get<{ stats: ToolStat[] }>(`/commands/stats?${sp}`);
  },

  // ---- LLM records ----
  llmRecords: (params?: { model?: string; session?: string; task?: string; page?: number; size?: number }) => {
    const sp = new URLSearchParams();
    if (params?.model) sp.set("model", params.model);
    if (params?.session) sp.set("session", params.session);
    if (params?.task) sp.set("task", params.task);
    if (params?.page !== undefined) sp.set("page", String(params.page));
    sp.set("size", String(params?.size ?? 50));
    return get<{ records: LLMRecordItem[]; total: number }>(`/llm/records?${sp}`);
  },
  llmRecordDetail: (id: number) => get<LLMRecordDetail>(`/llm/records/${id}`),
  // M3（方案 v10 §6 二期）：缓存命中率 + 分位延迟聚合（llm_records 派生）。
  llmStats: () =>
    get<{
      total_calls: number;
      errors: number;
      input_tokens: number;
      output_tokens: number;
      cache_read: number;
      cache_write: number;
      cache_hit_rate: number;
      latency_p50_ms: number;
      latency_p95_ms: number;
      by_worker: { worker: string; calls: number; p50_ms: number; p95_ms: number }[];
    }>(`/llm/records/stats`),
  llmTasks: () => get<{ tasks: LLMTask[] }>(`/llm/records/tasks`),
  llmRecordsDeleteTask: (task: string) => del<{ deleted: number }>(`/llm/records?task=${encodeURIComponent(task)}`),
  // 按模型聚合本任务的 token 用量（来自常开的 llm_usage 计量账本，逐次调用精确，
  // per-agent 绑定 / 轮询 / 中断消耗都覆盖）。
  tokensByModel: (task: string) =>
    get<{ models: ModelTokenStat[] }>(`/llm/records/by-model?task=${encodeURIComponent(task)}`),

  // ---- 一键更新 ----
  // 检查以后端为准：下载是后端做的，浏览器能连 GitHub 而服务器连不上的情况很常见
  // （服务器在内网、代理只配在浏览器上），那时点更新必然失败。
  // 后端对 GitHub 的查询结果有 30 分钟缓存（未认证的 GitHub API 是 60 次/小时/IP，
  // 顶栏每次整页加载都会查一次，不缓存会很快耗光配额）。force=true 强制回源，
  // 留给用户显式点「检查更新」时用。
  checkUpdate: (force = false) => get<UpdateCheck>(`/update/check${force ? "?force=1" : ""}`),
  // 202 即返回，实际下载在后台跑，进度走 /api/update/stream。
  applyUpdate: () => post<{ ok: boolean; target: string }>(`/update/apply`),
  rollbackUpdate: () => post<{ ok: boolean }>(`/update/rollback`),
};
