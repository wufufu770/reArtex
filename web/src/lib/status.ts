// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: 意图 / 覆盖 / 任务 / 严重度 each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose 用作「严重」——实心高强调,视觉上明显高于「高危」的软红描边。
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "待领", tone: "slate" },
  running: { label: "执行中", tone: "blue" },
  paused: { label: "已暂停", tone: "amber" },
  done: { label: "已完成", tone: "green" },
  // blocked = 模型/API/网络故障重试用尽，这条意图基本没真正探成（非目标拦截）。
  blocked: { label: "执行出错", tone: "red" },
  // exhausted = 达到步数/时间预算被中途掐断、只写回部分结果（非方向已探尽）。
  exhausted: { label: "预算耗尽", tone: "violet" },
  // stopped = 历史软删除状态（保留,历史数据）。
  stopped: { label: "已停止", tone: "slate" },
  // deleted = 用户假删除了该意图（保留节点与血缘，删除原因见 delete_reason 字段）。
  deleted: { label: "已删除", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "已创建", tone: "slate" },
  queued: { label: "排队中", tone: "amber" },
  running: { label: "运行中", tone: "blue" },
  paused: { label: "已暂停", tone: "amber" },
  done: { label: "已完成", tone: "green" },
  failed: { label: "失败", tone: "red" },
  timeout: { label: "已超时", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "严重", tone: "rose" },
  high: { label: "高危", tone: "red" },
  medium: { label: "中危", tone: "amber" },
  low: { label: "低危", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "待处理", tone: "amber" },
  in_progress: { label: "处理中", tone: "blue" },
  confirmed: { label: "已确认", tone: "red" },
  resolved: { label: "已处理", tone: "green" },
  fixed: { label: "已修复", tone: "green" },
  false_positive: { label: "误报", tone: "slate" },
  ignored: { label: "忽略", tone: "neutral" },
  duplicate: { label: "重复", tone: "neutral" },
  risk_accepted: { label: "风险接受", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "探索中", tone: "blue" },
  paused: { label: "已暂停", tone: "amber" },
  stalled: { label: "停滞", tone: "red" },
  idle: { label: "空闲", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "进行中", tone: "blue" },
  met: { label: "已达成", tone: "green" },
  abandoned: { label: "已放弃", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "放行", tone: "green" },
  block: { label: "拦截", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "观测", tone: "slate" },
  confirmed: { label: "确认", tone: "green" },
  tombstoned: { label: "废弃", tone: "neutral" },
};

// 推送投递状态。sending 用 blue 而不是 amber：它不是「有问题」，
// 而是「已被领取、正在发」，与 pending 的等待语义要能区分开。
const delivery: Record<string, StatusMeta> = {
  pending: { label: "待发送", tone: "amber" },
  sending: { label: "发送中", tone: "blue" },
  sent: { label: "已送达", tone: "green" },
  failed: { label: "失败", tone: "red" },
  skipped: { label: "已跳过", tone: "neutral" },
};

// 群消息箱状态。blocked = 投递前被拦下（落库留痕，但不进接收方上下文）。
const message: Record<string, StatusMeta> = {
  unread: { label: "未读", tone: "amber" },
  read: { label: "已读", tone: "blue" },
  acked: { label: "已确认", tone: "green" },
  blocked: { label: "被拦截", tone: "red" },
};

// 群组（任务大厅）状态。draining/parked 是拒绝态：不再发起新的模型调用。
const group: Record<string, StatusMeta> = {
  draft: { label: "草稿", tone: "slate" },
  active: { label: "运行中", tone: "blue" },
  draining: { label: "收尾中", tone: "amber" },
  parked: { label: "已冻结", tone: "red" },
  resuming: { label: "恢复中", tone: "blue" },
  closed: { label: "已关闭", tone: "neutral" },
  failed: { label: "失败", tone: "red" },
};

// 群预算状态（后端 db.QuotaState）。draining/parked 都拒绝新的模型调用。
const quota: Record<string, StatusMeta> = {
  active: { label: "正常", tone: "green" },
  draining: { label: "收尾中", tone: "amber" },
  parked: { label: "已冻结", tone: "red" },
};

// 群内分工。role 只表达分工，不表达权限。
const role: Record<string, StatusMeta> = {
  dispatcher: { label: "分发者", tone: "violet" },
  member: { label: "成员", tone: "blue" },
  observer: { label: "旁观", tone: "slate" },
};

// 策略状态机（后端 db.StrategyActive 等四个常量）。
// quarantined 是隔离态：新策略一律先进隔离态，晋级要过 curator 的三条件。
// retired 是人工退役（≠ 删除，行与证据链都还在）；tombstone 是硬 TTL 到了的终态。
const strategy: Record<string, StatusMeta> = {
  quarantined: { label: "隔离中", tone: "amber" },
  active: { label: "在役", tone: "green" },
  retired: { label: "已退役", tone: "slate" },
  tombstone: { label: "已墓碑", tone: "neutral" },
};

// 策略提议状态（后端 db.StrategyProposalStatus）。
// ⚠️ approved 只表示「人批了」，不等于策略生效 —— 生效还要过 curator 的三条件。
const proposal: Record<string, StatusMeta> = {
  pending: { label: "待裁决", tone: "amber" },
  approved: { label: "已批准", tone: "green" },
  rejected: { label: "已驳回", tone: "red" },
  superseded: { label: "已被取代", tone: "slate" },
};

// 提议的变更路由（后端 db.StrategyRoute*）。
const proposalRoute: Record<string, StatusMeta> = {
  edit_existing: { label: "改现有策略", tone: "blue" },
  create_new: { label: "新建策略", tone: "violet" },
  retire: { label: "退役策略", tone: "amber" },
};

// 证据角色（后端 db.strategyEvidenceRoles）：这条证据是支持、还是反驳这条策略。
const evidenceRole: Record<string, StatusMeta> = {
  created_by: { label: "来源", tone: "slate" },
  validated_by: { label: "佐证", tone: "green" },
  contradicted_by: { label: "反证", tone: "red" },
};

// SoD（职责分离）裁决三态（后端 internal/sodpolicy.Verdict*）。
//
// ⚠️ needs_review **不是** approve 的同义词：needs_review 的工具调用同样被
// block，它只是「证据不足以拒，但值得让对抗者（redteam）证伪」。所以这一态
// 用 amber（待办）而不是 green（放行）。
const sodVerdict: Record<string, StatusMeta> = {
  approve: { label: "放行", tone: "green" },
  deny: { label: "拒绝", tone: "red" },
  needs_review: { label: "待证伪", tone: "amber" },
};

// 自动权限分层（后端 agent.Tier*）。Tier3 = 高风险 / scope 外 / 裁决写。
const sodTier: Record<string, StatusMeta> = {
  Tier1: { label: "Tier1 安全只读", tone: "green" },
  Tier2: { label: "Tier2 范围内写", tone: "blue" },
  Tier3: { label: "Tier3 高风险", tone: "red" },
};

// 层类别（后端 agent.TierCategory）。egress 用 amber：出网治理本身不完整，
// 这一类动作的放行判断不能被读成「出网已被管住」。
const sodTierCategory: Record<string, StatusMeta> = {
  safe_read: { label: "安全只读", tone: "slate" },
  scoped_write: { label: "范围内结构化写", tone: "blue" },
  out_of_scope: { label: "范围外", tone: "red" },
  adjudication: { label: "裁决写", tone: "violet" },
  state_change: { label: "状态变更", tone: "amber" },
  egress: { label: "出网", tone: "amber" },
  scope_expansion: { label: "扩权", tone: "rose" },
  proposal: { label: "提议", tone: "blue" },
  unlisted: { label: "未登记工具", tone: "amber" },
};

// 裁决原因码（后端 internal/sodpolicy.Reason*）。
//
// 为什么「查不出来」的那几个用 slate 而不是 red：它们不是「越界」的确定结论，
// 是「查不出来 ⇒ fail closed 拒绝」。两者在事后排查时的含义不同，不能混成一个
// 「红」。
const sodReason: Record<string, StatusMeta> = {
  scope_ok: { label: "范围通过", tone: "green" },
  all_checks_passed: { label: "全检查通过", tone: "green" },
  tier2_not_applicable: { label: "不涉及 Tier2", tone: "green" },
  out_of_scope: { label: "越界 · 目标不在范围", tone: "red" },
  tool_circuit_open: { label: "工具熔断中", tone: "red" },
  quota_exhausted: { label: "群组额度耗尽", tone: "amber" },
  tier2_inconsistent: { label: "Tier2 证据不一致", tone: "amber" },
  anomaly_flagged: { label: "异常标记 · 待证伪", tone: "violet" },
  scope_check_unavailable: { label: "范围检查不可用", tone: "slate" },
  tool_health_unavailable: { label: "工具健康不可查", tone: "slate" },
  quota_check_unavailable: { label: "额度状态不可查", tone: "slate" },
  tier2_check_unavailable: { label: "Tier2 检查不可查", tone: "slate" },
  normalizer_unavailable: { label: "意图规整器不可用", tone: "slate" },
  normalizer_malformed: { label: "规整输出不合法", tone: "slate" },
  execution_token_invalid: { label: "执行凭证无效", tone: "slate" },
};

// 工具熔断状态（后端 internal/sodpolicy.Circuit*）。
// open = 闸门此刻拒绝这个工具的一切调用；half_open = 放行一次试探。
const sodCircuit: Record<string, StatusMeta> = {
  closed: { label: "closed · 放行", tone: "green" },
  open: { label: "open · 拒绝一切调用", tone: "red" },
  half_open: { label: "half_open · 放行一次试探", tone: "amber" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
  message,
  group,
  quota,
  role,
  strategy,
  proposal,
  proposalRoute,
  evidenceRole,
  sodVerdict,
  sodTier,
  sodTierCategory,
  sodReason,
  sodCircuit,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
