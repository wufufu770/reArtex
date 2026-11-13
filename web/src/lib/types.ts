// ARTEX domain model — types used across the UI.
// Derived from the functional spec (section 7: 关键数据形状).

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle";

export interface Task {
  id: string;
  name?: string; // 可选任务名称;空/缺省=未命名,展示时回退到描述
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 已登记漏洞数(按严重度分档)
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 资产覆盖度功能开关(创建时定,默认开)；false=不计算/不展示覆盖度
  scope_configured?: boolean; // seed 是否配好 scope；false=详情页显示「scope 未配置」横幅（H1）
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // 预设分类；null/缺省=无
  intercept_rules?: AssetInterceptRuleInput[]; // 预设的任务级拦截/允许规则
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- Asset graph (global, shared across tasks) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey
  value?: string;
  company_id?: string; // 归属公司资产 id；空=未归属
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// Task asset view — server-side enriched, paginated.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- New unified asset model (new backend) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 力导向「资产覆盖图」的一个节点。key 唯一：资产="a:<id>"、公司="c:<id>"、
// 无资产行的根域名="r:<domain>"。in_scope=false 的是仅用于连线的灰色上下文节点。
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// 某资产在本任务探索图里关联到的意图/事实/发现（覆盖图节点抽屉用）。
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// 任务测试范围的一条（覆盖度分母 + 授权边界）。
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // 后端 JOIN companies 解析，仅 kind=company 有值
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 新增企业时提交的结构化资产范围规则。
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 资产范围写入的结果。errors 是本次提交里不合法的行；warnings 是与本次提交无关、
// 但会让归属结果不符合预期的既有数据问题（如 ip 字段存了主机名的资产）。
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 公司资产范围规则的一条（归属唯一真值来源）。
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain 时有值
  net?: string; // kind=ip|cidr 时有值
  value?: string; // kind=icp|keyword 时可能由后端直接返回
  raw: string; // 原始用户输入，用于显示和回填
  reason?: string;
}

// 企业：type=company 的资产节点 + 图标 + 资产计数 + 资产范围规则。
export interface Company {
  id: number;
  name: string;
  logo?: string; // 远程图标 URL；空则前端用名称首字母
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 意图假删除(state='deleted')时的删除原因
}

// 播报板一页:按创建顺序分页的节点 + 这一页涉及的边 + 边另一端的节点(refs,按 id 索引),
// 这样每条播报都能说清「从哪来、产出了什么」,而不用把整张图拉下来。
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // 节点 id → 该节点锚定的资产(播报板展开时顺带展示,含本页节点与其邻居)。
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 目标管理卡片用的目标(后端已把 payload 拆成 text/vulnclass)。
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 约束管理卡片用的操作约束(allow=允许 / deny=禁止)。
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
// info = 信息级（E1-7）：未证实候选面 / 侦察线索 / 无直接影响的暴露。
export type Severity = "critical" | "high" | "medium" | "low" | "info";

// 漏洞处置状态:待处理 / 处理中 / 已确认 / 已处理 / 已修复 / 误报 / 忽略 / 重复 / 风险接受。
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset 是一个漏洞绑定的资产(已在后端预渲染 label)。
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 独立 findings 表的行 id,状态更新的句柄(任务内旧节点可能缺失)
  vulnclass: string;
  name?: string; // 漏洞名称;为空时展示回退到 vulnclass
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 详细报告(Markdown);仅详情接口返回,列表为空
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage 是发现列表的服务端分页响应。
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 可选任务名称;空/缺省=未命名
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  info: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats 是发现全表聚合(统计卡 + 漏洞类型下拉),服务端计算,不受分页影响。
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  info: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption 是发现页「按任务」筛选下拉的一项:有漏洞的任务(描述为空表示任务已删除,
// 前端回退展示 id)及其漏洞条数。
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 可选任务名称;空/缺省=未命名
  description: string;
  count: number;
}

// FindingQuery 是发现列表分页/筛选/排序参数。
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // 任务 id;"all"/空 = 不按任务筛选
  query?: string;
  sort?: "severity" | "time";
  // 资产树节点 key;选中一个节点 = 选中它的整棵子树。空 = 不按资产筛选。
  assetScope?: string;
}

// ---- Findings by asset (资产视图) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode 是资产树的一个节点。key 形如 a:<id>(资产)、c:<id>(企业)、
// r:<domain>(库里没有资产行的根域名)、__none__(未关联资产)。
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // 直接挂在该资产上的发现数
  total: number; // 含子孙、按发现去重
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET 与后端 db.FindingUnassignedAsset 对应。
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- Activity / sessions ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_paused" // P2 分层经济：决策槽位（planner/mainagent）按策略暂停，等人工改档
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment 是一次上传的文件:path 相对该会话/任务工作目录(即 agent 的 CWD)。
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 绝对路径(scope=staging 暂存上传时返回;建任务前把它写进描述)
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 调度，仅自定义 agent) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 定时:每 N 秒(0=不定时)
  on_finding: boolean; // 任意任务发现 finding 时触发
  on_goal_met: boolean; // 任意任务达成目标时触发
  on_task_timeout: boolean; // 任意任务超时时触发
  on_tool_call: boolean; // 选中工具被调用(执行完成)时触发
  on_task_create: boolean; // 任意任务被创建时触发
  interval_message: string; // 各触发条件的独立用户消息
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call 选中的工具 key(至少一个)
  last_fire?: string;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- Backend logs (/logs page) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id; present for DB-persisted lines
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 批量改分类的逐任务结果。失败只可能是任务已被删除，分类本身的写入是原子的。
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // main-agent session: which conversation segment (0 = original)
}

// ---- Security ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent 自动绑定流量证据，默认关闭；不影响人工绑定
  llm_record: boolean; // LLM 录制开关（默认关）；关闭时不记录任何 LLM 调用
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 独立出口代理(http/https/socks5)，用于访问搜索端点；与记录流量的 MITM 代理无关。空=直连。
  web_search_proxy?: string;
  // 全局出口代理(http/https/socks5，可带 user:pass)，所有目标流量走它。开启流量捕获时作为
  // MITM 上游；关闭捕获时直接注入 agent 的 bash/WebFetch。空=直连。
  global_proxy?: string;
  python_interpreter?: string; // 自定义脚本工具的 python 解释器路径(空=运行时检测)
  workers?: number; // 并发工作 agent 数(默认3)；对之后启动的任务生效
  // 任务并发上限:同时「运行中」的任务数上限。关闭=不限;开启后新建任务超限则排队,有空位自动启动。
  task_concurrency_enabled?: boolean; // 默认 false
  task_concurrency_limit?: number; // 开启后默认 5
  // LLM 轮询(故障转移)。默认关；开启后「未指定模型」的 agent 在当前配置不可用
  // （余额不足/key 失效/限流/服务异常）时自动切到下一个配置。
  llm_pool_enabled?: boolean; // 默认 false
  // 绑定了指定配置的 agent/任务失败时是否也回落到轮询链。默认 false = 绑定即独占。
  llm_pool_bind_fallback?: boolean;
  // 操作约束注入范围(默认都开):把任务的 allow/deny 约束拼进对应 agent 的系统提示。
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 实验功能:noa 模型驱动上下文压缩(默认关)。开启后平台接入的四类 agent(planner/
  // worker/主 agent/对话)由 noa 接管上下文压缩,取代内置 compaction;每 run 读一次,对
  // 之后启动的 run 生效。
  noa_compaction?: boolean;
  // ---- 漏洞 IM 推送（渠道本身是独立资源，见 /api/notify/*，这里只有三项全局配置）----
  notify_enabled?: boolean; // 推送总开关，默认开；用于维护期一键止血
  notify_public_base_url?: string; // 漏洞详情回链的外部访问地址；空=消息不带回链
  notify_digest_interval_min?: number; // 汇总模式周期（分钟），默认 30
}

// ---- 出网治理状态（GET /api/settings/egress-governance）----

// EgressGovernanceState 是后端给的出网治理契约。
//
// 后端把 governed 由「未覆盖通道清单是否为空」派生（清单非空 ⇒ governed=false），
// 所以 UI 读到的 governed=true 必然意味着清单为空。前端仍然【不单读】 governed：
// 渲染只认清单，见 lib/egress-governance.ts。
export interface EgressGovernanceState {
  governed: boolean;
  // 未覆盖通道清单——逐条照后端给的字符串显示，前端不复制、不改写、不兜底成空数组。
  uncovered_channels: string[];
}

// ---- 漏洞 IM 推送 ----

// NotificationFilter 是渠道的过滤条件，字段全部可选，缺省即不过滤。
// 后端对所有字段都不加校验：配置畸形时按「命中」处理（宁可多推不可漏推）。
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 空=不限；非空则要求与漏洞所属任务有交集
  asset_ids?: number[]; // 空=不限；非空则要求与漏洞锚定资产有交集
  vulnclass_include?: string[]; // 空=全收；非空则要求漏洞类型命中任一关键词（大小写不敏感子串）
  vulnclass_exclude?: string[]; // 命中任一关键词即排除（排除优先于包含）
  on_status_change?: boolean; // 是否也接收漏洞处置状态变更事件
}

// NotificationChannel 是一个渠道实例。config 的字段随 kind 而异，
// 且凭据字段在读取时被替换成 "__masked__" 开头的掩码值——原样回传即表示「不改」。
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys 由后端按渠道类型给出，前端据此渲染密码框与「留空即不改」提示，
  // 不硬编码任何渠道知识。
  secret_keys: string[];
}

// NotificationKind 是 /api/notify/meta 返回的渠道类型元数据。
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery 是一条投递记录，用于投递历史与失败重发。
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  // 网关候选地址：完整候选列表（第一项 = 主地址，其余按序作为故障备选）。
  // null/空 = 回落 base_url 单值（只用主地址）。后端未发版时缺省。
  base_urls?: string[] | null;
  proxy?: string;
  model: string;
  models?: string[]; // E1 多模型：该 profile 可用模型列表（含默认模型）
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 思考开关(thinking.type): ""=不发送(默认) | "disabled"=关闭 | "enabled"=开启
  thinking_type?: string;
  // 思考强度: ""=不发送(默认) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // 轮询顺位：越大越先被选中。激活配置恒为链首，与本值无关。
  priority?: number;
  // true = 不作为故障转移目标（仍可被 agent/任务显式绑定使用）。
  pool_exclude?: boolean;
  // true（默认）= 流式(SSE) | false = 真·非流式(stream:false，一次性返回)。
  streaming?: boolean;
  // 单次回复的输出上限(token)。0 = 不发送该字段，由服务端默认值决定。
  // 注意与 context_window_k 区分：后者是模型总容量，只在本地用于压缩阈值。
  max_tokens?: number;
  // 上限用哪个请求字段名，仅 format="openai" 有意义：
  // ""=max_tokens(默认) | "max_completion_tokens"(OpenAI 推理模型只认它)
  max_tokens_field?: string;
  // 自定义会话头名：非空时每次请求带该 HTTP 头，头值=当前会话/意图的 session id。
  // ""=不发送。用于按 session-id 头做提示缓存/粘性路由的网关。
  session_header_key?: string;
  // 本配置对重试的覆盖（建连/空响应/同 provider 安全窗口）。留空/全 0 = 跟随全局策略。
  retry?: LLMRetryOverride;
}

// ---- LLM 重试策略 ----
// 一层重试的两个旋钮。两者都是「0 = 未配置」：
//   attempts    0=用默认次数 | -1=关闭该层重试 | >0=重试次数
//   interval_ms 0=用默认的指数退避 | >0=改用这个固定毫秒间隔
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 单个 LLM 配置能覆盖的三层（都是「跟着端点走」的重试）。
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 建连重试：连接重置/超时/429/5xx，流开始前
  empty: LLMRetryRule; // 空响应重试：完成但没有任何内容（仅 openai 格式）
  stream: LLMRetryRule; // 同 provider 安全窗口重试：未交付输出前的断流重放
}

// 全局策略 = 上面三层的默认值 + 两层只有全局的：
//   breaker 轮询熔断（attempts=连续几次瞬时失败熔断，interval_ms=固定冷却时长）
//   intent  意图重跑（worker 以 model_error 收场后整条意图重跑）
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM 轮询（故障转移）----
// 一个配置在轮询链中的位置与健康状态。state:
//   ok       正常
//   degraded 有连续失败但未达熔断阈值
//   tripped  已熔断，冷却期内被跳过（cooldown_secs 为剩余秒数）
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 是否为当前激活配置（恒为链首）
  excluded: boolean; // pool_exclude：不参与轮询
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

// ---- LLM 网关候选地址（端点维度的探活与切换）----
// 一个候选端点的探活状态。
export interface LLMEndpointCandidate {
  url: string;
  alive: boolean; // true = 探活通过（绿点）；false = 不可达（红点）
  latency_ms?: number;
}

// 一次端点自动切换的记录。
export interface LLMEndpointSwitch {
  at: string; // 切换时间
  from: string; // 原端点
  to: string; // 新端点
  cause: string; // 切换原因（如 dial tcp 失败）
}

// 一个 profile 的端点维度状态：当前生效端点、候选红绿灯、最近切换（后端最多返回 20 条）。
// candidates/switches 可缺省：Go 侧空列表会序列化成 null，按缺省处理。
export interface LLMEndpointStatus {
  profile_id: string;
  current: string; // 当前生效端点
  main: string; // 主地址（恢复回切目标）
  candidates?: LLMEndpointCandidate[];
  switches?: LLMEndpointSwitch[];
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
  // 各 profile 的端点探活/切换状态（G1）。后端未发版时缺省，UI 优雅降级不渲染。
  endpoints?: LLMEndpointStatus[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 内置为 goals/planner/mainagent/worker；自定义为用户自定 key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 绑定的 LLM 配置；null/absent = 跟随任务/会话/全局
  max_turns?: number; // 0 = 无限制
  run_seconds?: number; // worker 单次运行墙钟上限(秒)；0 = 无限制
  web_search?: boolean; // 是否启用网络搜索(受系统全局开关门控)
  interactive_shell?: boolean; // 是否启用交互式 shell(持久 PTY 会话工具族)
  // P3 触发后处理策略(仅自定义 agent 有意义)
  trigger_run_mode?: "serial" | "parallel"; // 串行排队 / 每次触发各自并发一个会话
  trigger_merge_mode?: "by_task" | "all" | "none"; // 仅 serial：同任务合并 / 全部合并 / 不合并
  trigger_max_parallel?: number; // 仅 parallel：每 agent 并发上限；0=不限
  // agent 级思考档位覆盖(方案 v9.2 批次 B)：""/absent=跟随绑定 profile 默认；
  // "enabled"/"disabled"=强制思考开关(switch 形态模型)；其余=effort 档位
  thinking_effort?: string;
  // 绑定数量(仅列表接口返回)：可见 MCP / 可见 Skill / 绑定工具
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
  // 会话选择器可见性(EDGE E1)：false = 不出现在 chat 页选择器(执行体 agent 用)。
  // absent = 旧后端/默认 true。
  chat_visible?: boolean;
}

// ---------- 业务建模（第三张图）与偏锋探索 ----------

// 业务图节点（BizGraphNode）：七类业务节点 × design/behavior 两层。
export interface BizGraphNode {
  id: number;
  company_id: number;
  task_id: number; // 0 = 无归属任务
  layer: "design" | "behavior";
  node_type: "biz_entity" | "biz_action" | "biz_state" | "biz_role" | "biz_trust" | "biz_dataflow" | "biz_external";
  name: string;
  raw_name: string;
  description: string;
  props: Record<string, unknown>;
  confidence: "observed" | "inferred";
  source: string;
  superseded_by: number; // 0 = 未失效
  asset_ids: number[];
  created_at?: string;
  updated_at?: string;
}

// 业务图边（BizGraphEdge）：九种关系。
export interface BizGraphEdge {
  id: number;
  company_id: number;
  task_id: number;
  layer: "design" | "behavior";
  src_id: number;
  dst_id: number;
  rel:
    | "owns"
    | "invokes"
    | "depends_on"
    | "trusts"
    | "flows_to"
    | "validates"
    | "requires_state"
    | "transitions"
    | "grants";
  props: Record<string, unknown>;
  confidence: "observed" | "inferred";
  superseded_by: number;
}

export interface BizGraphData {
  nodes: BizGraphNode[];
  edges: BizGraphEdge[];
}

// 双层差异（biz_layer_diffs）：八类差异 × 四档 severity × 四态 status。
export interface BizLayerDiff {
  id: number;
  company_id: number;
  task_id: number;
  diff_type:
    | "missing_validation"
    | "extra_permission"
    | "state_bypass"
    | "trust_violation"
    | "unexpected_flow"
    | "timing_anomaly"
    | "unbounded_consumption"
    | "workflow_skip";
  severity: "critical" | "high" | "medium" | "low";
  design_ref: Record<string, unknown>;
  behavior_ref: Record<string, unknown>;
  description: string;
  wstg_ref: string;
  stride_ref: string;
  status: "open" | "pushed" | "confirmed" | "false_positive";
  created_at?: string;
}

// 偏锋格点与覆盖（EDGE）。
export interface EdgeCell {
  id: number;
  task_id: number;
  cell_id: string;
  methodology_id: string;
  state: "open" | "dispatched" | "confirmed" | "disproven" | "folded";
  attempts: number;
  criterion_state: Record<string, unknown>;
}

export interface EdgeRun {
  id: number;
  task_id: number;
  methodology_id: string;
  criterion_trace: unknown[];
  stop_reason: string;
  stop_detail: Record<string, unknown>;
}

export interface EdgeCoverage {
  task_id: number;
  edge_mode: boolean;
  methodology_id: string;
  cells_total: number;
  cells_terminal: number;
  cells_dispatched: number;
  criterion_progress: number;
  cells: EdgeCell[];
  open_run: EdgeRun | null;
}

// 思考档位能力(方案 v9.2 批次 A：注册表三层解法的第一层)。
// shape="effort" → levels 是档位数组(reasoning_effort 参数)；"switch" → 只认
// 开关(thinking.type)。known=false = 未收录模型(前端挂「未验证」注记)。
export interface ThinkingCapability {
  shape: "effort" | "switch";
  levels: string[];
  default?: string;
  note?: string;
  known: boolean;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 可绑定的 LLM 配置候选(供「默认模型」下拉)；当前绑定见 agent.llm_profile_id
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 已保存的收尾提示词(空=用内置默认)
  wrapup_default?: string; // 内置默认收尾提示词(占位/恢复默认)
  wrapup_max_turns?: number; // 已保存的收尾轮数(0=用内置默认)
  wrapup_max_turns_default?: number; // 内置默认收尾轮数(供 "0=默认N" 提示)
  // 任务级超时收尾词(仅 worker/planner，task_timeout_wrapup_supported=true 时才显示该分区)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 调用统计（skill_usage 账本）。从未被调用过的 skill：calls=0、last_used 缺省。
  calls: number;
  tasks: number; // 加载过它的任务数（chat 会话不计入）
  usage_agents: string[]; // 加载过它的 agent key
  last_used?: string;
}

// SkillCall 是一次 Skill() 调用（单个 skill 的最近调用列表）。
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = 非任务场景（对话会话）
  session_id: string;
  args_len: number;
}

// MissingSkill 是被点名但不存在的 skill —— "想用但没有"的缺口。
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (内置工具目录) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // biome-ignore lint/suspicious/noExplicitAny: JSON-Schema 结构开放
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // 自定义工具类型
  // biome-ignore lint/suspicious/noExplicitAny: 自定义工具执行规格结构开放
  exec?: Record<string, any>; // 自定义工具执行规格(kind!=builtin)
  deferred?: boolean; // schema 延迟(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules（资产拦截：全局黑名单） ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr"
  // command_regex 是证据保全策略装的命令级规则（W11②）：安全控制本身，API/UI
  // 均不提供新建与改写入口，列表里只读展示。
  | "command_regex";

// action 仅用于任务级规则：block=拦截(禁止测试) allow=允许(白名单)。
export type AssetInterceptAction = "block" | "allow";

// 任务级资产拦截/允许规则的录入项（创建任务、任务详情编辑使用）。
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // 全局规则不带此字段（恒为拦截）；任务级规则区分 block/allow
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "shortcircuit" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // biome-ignore lint/suspicious/noExplicitAny: 工具入参随工具定义而异
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // 规则 message 或模型判定理由(模型判定带 [模型] 前缀)
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: 模型兜底审批(仅当没有任何拦截规则命中时由模型判断)的全局配置。
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = 跟随激活/默认配置
  prompt: string; // 判定提示词;GET 未设置时后端回填内置模板全文
  timeout_seconds: number; // 模型调用超时
  fail_action: "allow" | "ask" | "deny"; // 模型出错/超时/不可解析时的回退
  ask_timeout_seconds: number; // 模型判 ask 转人工后的审批等待超时
  ask_timeout_action: "allow" | "deny"; // 审批超时后的默认动作
}

// JudgeUsage: 模型兜底审批(judge 通道)的累计 token 用量 + 近 N 天每日序列。
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "shortcircuit" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 资产同步 (ScopeSentry 数据源) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// 单个工具的调用统计（/commands/stats）；errors 为其中失败的次数。
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider 实际收发的 HTTP 原文：请求为 buildBody() 发出的完整 body（含工具
  // schema），响应为原始 SSE 帧。上面的 request_body/response_body 是归一化视图，
  // 丢弃了工具 schema 与 tool_use 块。旧记录为空。
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check —— 当前版本与 GitHub 最新正式版的比较结果。 */
export interface UpdateCheck {
  /** 当前运行的版本；开发构建为 "dev" 或 git describe 的带后缀形式。 */
  current: string;
  /** 运行形态。docker 下换装只作用于容器可写层，重建容器会退回镜像版本。 */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** 是否存在可回滚的上一版本（artex.old）。 */
  has_backup: boolean;
  /** 本次启动时自更新自举的结论（换装失败 / 已回滚等），无事发生时为空。 */
  boot_notice?: string;
  rolled_back?: boolean;
  /** 查询 GitHub 失败时给出原因，此时下面的字段都不会有。 */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 当前平台对应的发布包名，以及该 Release 是否真的带了它。 */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 双方版本号是否可比较；开发构建为 false，此时禁用一键更新。 */
  comparable?: boolean;
  /** comparable 为 false 时的说明。 */
  reason?: string;
}

/** /api/update/stream 推送的一条更新进度。 */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** 仅下载阶段有意义（0-100）；其余阶段为 -1。 */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}

// ---- 任务大厅（群聊） ----
//
// 一个大厅 = 一个 task + 一个大厅会话（agent_key='auto'，分发者）+ N 个成员会话。
// 字段与后端 db.ChatGroup / db.ChatGroupMember / db.ChatGroupMessage / db.GroupQuota 一一对应。

// TaskHallGroup 是群组本体。
export interface TaskHallGroup {
  id: number;
  task_id: number;
  // task_exploration_id 是该群 task 的探索图 id：成员会话注入任务上下文时要用。
  task_exploration_id: number;
  // conversation_id 是大厅会话（分发者在这里接待人）。
  conversation_id: number;
  name: string;
  status: string; // draft | active | draining | parked | resuming | closed | failed
  concurrency_limit: number;
  plan_id?: number;
  budget_tokens?: number;
  drained_at?: string;
  parked_at?: string;
  resumed_at?: string;
  created_at: string;
  updated_at: string;
}

// TaskHallMember 是群成员：agent key + 它自己的会话 + 大厅内的分工。
// role 只表达分工（dispatcher/member/observer），不表达权限。
export interface TaskHallMember {
  group_id: number;
  agent_key: string;
  conversation_id?: number;
  role: string;
  joined_at: string;
}

// TaskHallStatus 是后端纯函数算出的大厅整体状态（db.ComputeTaskHallStatus）：
// level 是档位、summary 是一句话人话，blocked_members 列出卡住的成员。
export interface TaskHallStatus {
  group_id: number;
  task_id: number;
  group_status: string;
  level: string;
  summary: string;
  blocked_members: string[];
  computed_at: string;
}

// TaskHallGroupDetail 是群详情：群本体 + 成员 + 群内禁列表 + 消息箱计数。
export interface TaskHallGroupDetail extends TaskHallGroup {
  members: TaskHallMember[];
  // disabled_tools 是**禁列表**：出现的 key 就是被禁用的（表里没有 enabled 列 ⇒ 只能收紧）。
  disabled_tools: Record<string, boolean>;
  message_count: number;
  // hall_status 是语义化整体状态（键避开嵌入 ChatGroup 的 status=群原始状态字符串）；
  // 后端算不出时缺省（详情不被状态计算拖垮）。
  hall_status?: TaskHallStatus | null;
}

// TaskHallRow 是 /task-hall/tasks 的一行：一个已发布的大厅（带成员与额度上限）。
export interface TaskHallRow {
  task_id: number;
  group_id: number;
  name: string;
  status: string;
  conversation_id: number;
  concurrency_limit: number;
  budget_tokens?: number;
  members: TaskHallMember[];
  created_at: string;
}

// TaskHallMessage 是群消息箱（mailbox）里的一条消息。
// 注意这里**没有** owner/assignee：意图归属的唯一权威是 exploration_nodes.owner。
export interface TaskHallMessage {
  id: number;
  group_id: number;
  from_agent: string; // "human" = 人插话
  to_agent: string; // "" = 广播给全群
  kind: string; // note | request | answer | finding | escalation
  body: string;
  task_id?: number;
  intent_id?: number;
  finding_id?: number;
  status: string; // unread | read | acked | blocked
  // blocked_reason 非空 = 这条消息在投递前被拦下（落库留痕，但不进接收方上下文）。
  blocked_reason?: string;
  claims_authorization?: boolean;
  created_at: string;
  read_at?: string;
}

// TaskHallQuota 是群预算的当前读数（操作面与对账用）。
export interface TaskHallQuota {
  group_id: number;
  state: string; // active | draining | parked
  budget_tokens: number;
  period_kind: string; // 目前只有 daily（自然日，Asia/Shanghai）
  period_key: string; // 形如 2026-10-30
  // consumed = 本周期已结算的 token；reserved = 在途预扣占住的 token。
  consumed_tokens: number;
  reserved_tokens: number;
  // remaining 可能为负（实际用量可能超过估算）。
  remaining_tokens: number;
}

// ---- 策略库（L4）----
//
// 一条策略 = 一份「怎么打这类目标」的可读说明书：激活条件 / 步骤 / 前置条件 /
// 工具需求 / 里程碑 / 已知失败模式。
//
// ⚠️ **策略正文是参考文本，不是可执行产物**：Phase 6 没有自动执行通道，UI 也不
// 提供「执行这条策略」这种入口 —— 策略只被人读、被引用。
// 字段与后端 db.Strategy（db/strategies.go:63-93）一一对应。
export type StrategyState = "quarantined" | "active" | "retired" | "tombstone";

// Strategy 是策略库（L4）的一条策略。
export interface Strategy {
  id: number;
  // company_id 为空 = 平台级策略（库里是 NULL）。
  company_id?: number;
  name: string;
  summary: string;
  // 下面八个 jsonb 列是策略正文。形状随写入方而变（对象 / 数组 / 标量都可能），
  // 所以按「不透明 JSON」处理，页面上按原样展开显示。
  activation?: unknown;
  steps?: unknown;
  preconditions?: unknown;
  tool_requirements?: unknown;
  success_criteria?: unknown;
  failure_modes?: unknown;
  parameters?: unknown;
  applies_to?: unknown;
  state: StrategyState;
  // confidence / utility / run_utility 都是 0..1 的实数。⚠️ utility 的**权威值**
  // 在 strategy_utility 物化视图里，策略表上的这一列只是缓存。
  confidence: number;
  utility: number;
  run_utility: number;
  usage_count: number;
  hit_count: number;
  miss_count: number;
  retrieval_count: number;
  // pinned = 唯一证人标记：被 pin 的策略是通往某个漏洞的唯一已知路径。
  pinned: boolean;
  version: number;
  valid_from: string;
  invalid_at?: string;
  expired_at?: string;
  // retired_at / retired_reason 只在人工退役后有值 —— 退役 ≠ 删除（行与证据链都还在）。
  retired_at?: string;
  retired_reason?: string;
  created_at: string;
  updated_at: string;
}

// StrategyEvidence 是策略的一条证据引用（证据链 = 策略的出处）。
// 没有它，一条策略就只是「凭空提权」的主张。
export interface StrategyEvidence {
  // ref_type ∈ finding | traffic_snapshot | activity | tool_call | run。
  ref_type: string;
  // ref_id 是裸 id（TEXT，不是 BIGINT）。
  ref_id: string;
  // content_digest 是原始证据的摘要 —— 证据本体仍在 findings / activity 等表里。
  content_digest: string;
  // role ∈ created_by | validated_by | contradicted_by。
  role: string;
  created_at: string;
}

// StrategyDetail 是 GET /api/strategies/{id} 的响应：策略本体 + 证据链。
export interface StrategyDetail {
  strategy: Strategy;
  evidence: StrategyEvidence[];
}

export type StrategyProposalStatus = "pending" | "approved" | "rejected" | "superseded";

// StrategyProposal 是「agent 提议、宿主裁决」的那一层。
export interface StrategyProposal {
  id: number;
  // route=create_new 时 strategy_id 可能指向一条还不存在的策略。
  strategy_id: number;
  // route ∈ edit_existing | create_new | retire。
  route: string;
  // payload 是提议的变更内容（形状随 route 而变）。
  payload: unknown;
  status: StrategyProposalStatus;
  // reason = 提议时写的理由；decided_reason = 裁决时写的理由。
  reason: string;
  decided_reason: string;
  created_by: string;
  decided_by: string;
  decided_at?: string;
  created_at: string;
}

// StrategyPinQueue 是 GET /api/strategy-pin-queue 的响应。
//
// ⚠️ **advisory_only = true**：这个队列只提示、**绝不自动解 pin** ——
// 自动解 pin 会砸掉漏洞报告的可复现性。解 pin 只能由人显式做
// （POST /api/strategies/{id}/pin）。
export interface StrategyPinQueue {
  queue: Strategy[];
  pinned_count: number;
  // threshold 是「该让人看一眼了」的堆积阈值（后端与 curator 用同一个常量）。
  threshold: number;
  // review_required 只表示「堆积过线」，不代表任何自动动作。
  review_required: boolean;
  advisory_only: boolean;
}

// StrategyDecision 是人工裁决的载荷：**理由 + 操作人都必填**。
// 后端对缺字段直接 400（"人工退役必须写明理由" / "…必须写明操作人"），
// 且这一笔会进 audit_events 哈希链 —— 事后能查「谁做的、为什么」。
export interface StrategyDecision {
  reason: string;
  actor: string;
}

// StrategyPinDecision 是 pin 置位 / 解除的载荷。
// ⚠️ pinned 是**必填** boolean：后端用指针区分「缺字段」与「显式 false」，
// 少了这个区分，一次漏字段的请求会被当成「解除唯一证人」。
export interface StrategyPinDecision extends StrategyDecision {
  pinned: boolean;
}

// StrategyProposalDecision 是提议裁决的载荷。
// ⚠️ approved 只表示「人批了」，**不等于策略生效** —— 生效还要过 curator 的
// 三条件（重放闸门 + 观测数 + 里程碑命中）。
export interface StrategyProposalDecision extends StrategyDecision {
  status: "approved" | "rejected";
}

// ---- SoD 链（职责分离）----
//
// 三个只读观测面（决策账本 / 对抗复核队列 / 工具熔断）+ 审计链校验。
// 字段与后端 server/sod_api.go 的 DTO 一一对应。
export type SodVerdict = "approve" | "deny" | "needs_review";

// SodTier 是自动权限分层（后端 agent.Tier*）。
export type SodTier = "Tier1" | "Tier2" | "Tier3";

// SodCircuitState 是工具熔断的三态（后端 internal/sodpolicy.Circuit*）。
export type SodCircuitState = "closed" | "open" | "half_open";

// SodDecision 是决策账本的一行 —— 一次裁决的完整留痕。
export interface SodDecision {
  id: number;
  // task_id / conversation_id 缺省 = 这条裁决不挂在某个任务 / 会话上。
  task_id?: number;
  conversation_id?: number;
  source_agent: string;
  tool_name: string;
  verdict: SodVerdict;
  reason_code: string;
  // payload_digest / intent_digest / token_digest / scope_digest 是摘要，
  // 原始载荷**永不入库**。
  payload_digest: string;
  payload_preview: string;
  intent_digest: string;
  token_digest: string;
  scope_digest: string;
  // anomaly_flag = 意图规整器标记了异常（是复核，不是拒绝）。
  anomaly_flag: boolean;
  anomaly_reason: string;
  // checks 是闸门记下的各道检查结论（不透明 JSON，按原样展示）。
  checks: unknown;
  // E1-2：非空 = 这条放行带人工改判标记（checks.manual_override），账本页徽标展示。
  overridden?: string;
  cache_hit: boolean;
  latency_ms: number;
  created_at: string;
  // 分层信息是**读时推导**的（原始载荷不入库 ⇒ 推导值是下界，不是真值）。
  tier: SodTier | "";
  tier_category: string;
  // tier_host_only = 这一层只走宿主侧写面，不经分类器。
  tier_host_only: boolean;
  // tier_scope_in 为 null = 账本里没有范围结论（未判定），与「判定为不在范围」不同。
  tier_scope_in: boolean | null;
  tier_reason: string;
  tier_derived: boolean;
}

// SodDecisionLedger 是 GET /api/sod/decisions 的响应。
export interface SodDecisionLedger {
  decisions: SodDecision[];
  count: number;
  limit: number;
  // tier_note 是后端写明的语义声明：分层是读时推导值，真值只可能更高。
  tier_note: string;
}

// SodReviewItem 是对抗复核队列的一项 —— 交给 redteam 证伪的 flagged 调用。
export interface SodReviewItem {
  id: number;
  task_id?: number;
  conversation_id?: number;
  source_agent: string;
  tool_name: string;
  payload_digest: string;
  payload_preview: string;
  anomaly_flag: boolean;
  anomaly_reason: string;
  reason_code: string;
  structured_intent: unknown;
  // status ∈ pending | claimed | …（表自己的状态机，值来自 db.SoDReview*）。
  status: string;
  claimed_by: string;
  resolution: string;
  created_at: string;
  updated_at: string;
}

// SodReviewQueue 是 GET /api/sod/review-queue 的响应。
//
// ⚠️ **advisory_only = true / human_approval_queue = false**：这张队列交给
// 对抗者（redteam）证伪，**不是**人工审批队列；人工审批走 intercept_pending。
export interface SodReviewQueue {
  queue: SodReviewItem[];
  count: number;
  limit: number;
  status: string;
  advisory_only: boolean;
  human_approval_queue: boolean;
  // consumer = 谁消费这张队列（redteam）；purpose = 消费目的是什么。
  consumer: string;
  purpose: string;
  note: string;
}

// SodCircuit 是一个工具的熔断状态。
export interface SodCircuit {
  tool_key: string;
  // state 是闸门此刻实际执行的**有效状态**；recorded_state 是行里的原始值
  // （开路超期后按 half_open 处理，两个都要给）。
  state: SodCircuitState;
  recorded_state: string;
  // present = 表里真有这一行（false = 这个工具还没出过事 ⇒ closed）。
  present: boolean;
  fail_count: number;
  opened_at?: string;
  reason: string;
  updated_at?: string;
}

// SodCircuitList 是 GET /api/sod/circuit 的响应。
export interface SodCircuitList {
  circuits: SodCircuit[];
  count: number;
  // states 是按有效状态统计的条数。
  states: Record<string, number>;
  note: string;
}

// ---- 拦截种类 / auditor 建议队列 / 影子对照（方案 v4.1 §2/§3/§11-G1）----

// SodCategoryAction 是种类动作：hard_deny=硬拒 / ask=转人工 / auditor=送审计裁决 / pass=放行。
// 快路径就是表里 action=pass 的行——改动作即全链生效（单一裁决面，v4.1 §10-F3）。
export type SodCategoryAction = "hard_deny" | "ask" | "auditor" | "pass";

// SodCategoryFallback 是 auditor 故障/超时/超限时该种类的兜底动作。
// 默认 ask：绝不静默放行、也绝不变墙（§4 故障语义）。
export type SodCategoryFallback = "deny" | "ask" | "pass";

// SodMergedGateMode 是合并闸的三态开关（§11-G1）：
// off=旧链裁决 / shadow=新旧并跑只记差异 / enforce=新链接管裁决。
export type SodMergedGateMode = "off" | "shadow" | "enforce";

// SodFeedRow 是裁决大屏 recent 流的一行（GET /api/sod/feed，方案 v6 主题 A）。
// 字段与 server/sod_feed.go 的 sodFeedRow 对齐：ts 是 HH24:MI:SS；auditor 与
// relevance 取自 checks（auditor=pass/deny，exploit_relevance=high/medium/none）。
export interface SodFeedRow {
  id: number;
  ts: string;
  agent: string;
  tool: string;
  verdict: string;
  reason_code: string;
  category: string;
  auditor: string;
  relevance: string;
  // E1-3：字段名与账本真实列对齐——后端原来借「note」装 anomaly_reason 的内容。
  anomaly_reason: string;
  // E1-2：非空 = 这条放行带人工改判标记（checks.manual_override），大屏徽标展示。
  overridden?: string;
}

// SodFeedPending 是中列「待人工裁决」的一张卡（intercept_pending status=pending，
// input 是脱敏载荷前 400 字）。决策动作复用 POST /api/intercept/pending/{id}/decide。
export interface SodFeedPending {
  id: number;
  tool: string;
  reason: string;
  agent: string;
  ts: string;
  input: string;
}

// SodFeedStats 是右列统计卡：近 1h 自动放行 / auditor 拒 / 转人工 / 硬拦 +
// 人工改判 + blocked 意图数。
export interface SodFeedStats {
  auto_pass_1h: number;
  auditor_deny_1h: number;
  human_1h: number;
  hard_deny_1h: number;
  override_1h: number;
  blocked_intents: number;
}

// SodFeed 是大屏 2s 轮询的聚合响应。
export interface SodFeed {
  recent: SodFeedRow[];
  pending: SodFeedPending[];
  stats: SodFeedStats;
  mode: SodMergedGateMode;
}

// ---- 治理/审计聚合视图（方案 v10 §9 E1-6/E1-8，只读）----

// SodGovernanceDecision 是任务详情「治理」tab 裁决流的一行
// （GET /api/tasks/{id}/governance 的 decisions）。ts 是 HH24:MI:SS；note 后端截 160 字；
// overridden 非空 = 这条放行带人工改判标记。
export interface SodGovernanceDecision {
  id: number;
  ts: string;
  agent: string;
  tool: string;
  verdict: string;
  reason_code: string;
  note: string;
  overridden?: string;
  category: string;
}

// SodGovernanceOverride 是本任务的人工改判凭据一行。scope ∈ once | task | task_tool
// （task_tool = ask 批准「同时放行本任务后续同类调用」签发的通配凭据）。
export interface SodGovernanceOverride {
  id: number;
  tool: string;
  scope: string;
  reason: string;
  created_by: string;
  payload_digest: string;
}

// SodManualOverrideRow 是 GET /api/sod/overrides 的一行（db.SodManualOverride 原样投影，
// 字段名带 _key 后缀，与治理 tab 的聚合投影 SodGovernanceOverride 不同）。
export interface SodManualOverrideRow {
  id: number;
  decision_id: number;
  payload_digest: string;
  tool_key: string;
  task_id: number;
  agent_key: string;
  scope: string;
  reason: string;
  created_by: string;
  created_at: string;
  expires_at?: string | null;
  use_count: number;
  last_used_at?: string | null;
}

// SodSystemEvent 是本任务的系统事件一行（B6 升级 / LLM 故障转移等，worker=system 或
// kind ∈ llm_switch/llm_failover/llm_paused）。
export interface SodSystemEvent {
  ts: string;
  worker: string;
  kind: string;
  summary: string;
}

// TaskGovernance 是任务详情「治理」tab 的聚合响应。
export interface TaskGovernance {
  task_id: string;
  decisions: SodGovernanceDecision[];
  overrides: SodGovernanceOverride[];
  system_events: SodSystemEvent[];
}

// SodHookChainEditable 是生效 hook 链里的可热改面一行：endpoint 指向既有配置页面/工具。
export interface SodHookChainEditable {
  name: string;
  endpoint: string;
  note: string;
}

// SodHookChainReadonly 是代码常量层（基线/tier/五检查）一行：只读展示并写明理由。
export interface SodHookChainReadonly {
  name: string;
  reason: string;
}

// SodHookChain 是 GET /api/sod/hookchain 的响应：可热改面 + 代码常量只读面。
export interface SodHookChain {
  editable: SodHookChainEditable[];
  readonly: SodHookChainReadonly[];
  // categories 是 sod_categories 表的实读快照（E1-8 实况化）：种类→动作/cap/启停。
  categories?: SodHookCategory[] | null;
}

// SodHookCategory 是生效种类表的一行（后端 best-effort 实读；读不出时缺省）。
export interface SodHookCategory {
  name: string;
  action: string;
  cap_per_task: number;
  enabled: boolean;
  locked: boolean;
  priority: number;
  reason: string;
}

// AuditEvent 是 audit_events 哈希链的一条明细（GET /api/audit/events）。
// payload 后端截 240 字；digest 依赖 prev_digest，逐条串链。
export interface AuditEvent {
  id: number;
  seq: number;
  actor: string;
  tool: string;
  action: string;
  reason: string;
  payload: string;
  digest: string;
  prev_digest: string;
  ts: string; // MM-DD HH24:MI:SS
}

// AuditEventsPage 是审计事件明细的分页响应。
export interface AuditEventsPage {
  items: AuditEvent[];
  total: number;
  page: number;
  size: number;
}

// SodCategory 是拦截种类表的一行（种类 = 一类命令的名字 + 匹配器 + 动作 + 上限 + 兜底）。
export interface SodCategory {
  id: number;
  name: string;
  // kind：builtin=内置识别器（确定性分类器，pattern 为识别器名）/ custom=自定义正则。
  kind: "builtin" | "custom";
  pattern: string;
  action: SodCategoryAction;
  // 每任务上限；0 = 不限。超限自动升级为 ask（防橡皮图章）。
  cap_per_task: number;
  fallback: SodCategoryFallback;
  // 锁定基线行（删除/破坏、平台凭据、平台自身配置）：UI 可见但动作不可改、不可删——安全地板。
  locked: boolean;
  enabled: boolean;
  priority: number;
  reason: string;
  // 回链：本种类由哪条 auditor 建议采纳而来（人工新建的行没有此字段）。
  suggestion_id?: number;
  created_by: string;
  created_at: string;
  updated_at: string;
}

// SodCategorySuggestion 是 auditor 的一条种类建议——它观察到同类动作反复出现，
// 建议沉淀成确定性种类（采纳后这类调用连 auditor 都不用再问，§3 建议循环）。
export interface SodCategorySuggestion {
  id: number;
  name: string;
  kind: "builtin" | "custom";
  pattern: string;
  action: SodCategoryAction;
  // 证据链（G3）：最近 N 次同类裁决的 id + 样例预览，采纳时可回放。Go 侧空值序列化为 null。
  evidence: unknown[] | null;
  status: "pending" | "adopted" | "dismissed";
  reason: string;
  created_at: string;
  decided_at?: string | null;
  decided_by?: string;
}

// ShadowDiff 是影子模式的一行对照：旧链实际裁决 vs 新链「假如 enforce 会怎么判」。
// ⚠️ shadow 态最终动作永远按旧链，这张表只是切换 enforce 前核对无误伤的依据。
export interface ShadowDiff {
  id: number;
  task_id?: number;
  tool_name: string;
  payload_digest: string;
  // 旧链词汇：approve | deny | needs_review；新链另可能给 gate（范围检查待跑）。
  old_verdict: string;
  old_reason: string;
  new_verdict: string;
  new_reason: string;
  created_at: string;
}

// AuditChainVerification 是 GET /api/audit/chain 的响应。
//
// ⚠️ 断链不是接口故障：后端照样回 200，用 intact=false + broken_at_seq 表达
// 「链被改过」这个**结论**。
export interface AuditChainVerification {
  intact: boolean;
  // broken_at_seq = 第一处断裂的位置（0 = 没有断裂）。
  broken_at_seq: number;
  error?: string;
}

// P5 传承图：一条跨任务经验（server/legacy.go）。state 词汇刻意与 fact 三态区分。
export interface LegacyNode {
  id: number;
  company_id?: number | null;
  kind: "experience" | "pitfall" | "fingerprint_summary";
  title: string;
  summary: string;
  detail?: string;
  fingerprint?: Record<string, string | number | boolean>;
  state: "draft" | "accepted" | "retired";
  source_task_id?: number | null;
  source_task?: string;
  created_at: string;
  reviewed_at?: string | null;
  reviewed_by?: string;
}

// ---- 报告中心（/system/reports）----

// ReportMeta 是报告列表行（不带正文）。
export interface ReportMeta {
  id: number;
  title: string;
  task_id?: number;
  source_path?: string;
  size: number;
  created_by: string;
  created_at: string;
  updated_at: string;
}

// ReportFull 是整篇（编辑器/查看用）。
export interface ReportFull {
  id: number;
  title: string;
  task_id?: number;
  source_path?: string;
  content: string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

// ReportCandidate 是「从工作空间导入」的候选文件/目录一行。
export interface ReportCandidate {
  path: string;
  name: string;
  size?: number;
  is_dir?: boolean;
}
