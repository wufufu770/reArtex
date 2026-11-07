// 会话（session）相关的纯函数：显示名、相对时间、presence 语义、列表查询。
//
// 全部是纯函数（不碰 React、不发请求）⇒ 列表行、头部花名册、右侧面板共用同一套
// 口径，不会出现「列表说工作中、头部说空闲」这种自相矛盾。
//
// 依据：方案书 §4.2（三轨命名）、§4.3（presence）、§4.6（前端信息架构）。

import type { Session, SessionAttachmentType, SessionMember, SessionPresence } from "@/lib/session-types";
import { type Tone, toneDot } from "@/lib/status";
import type { Agent, Conversation } from "@/lib/types";

// ---- 名册 ----

export function sessionMembers(session: Session): SessionMember[] {
  return session.members ?? [];
}

// memberDisplayName 取成员的展示名：后端附带的 display_name 优先，其次前端
// 已知的 agent 名，最后退回 agent_key（agent 可能已被下线）。
export function memberDisplayName(member: SessionMember, agentByKey: Map<string, Agent>): string {
  if (member.display_name) return member.display_name;
  if (member.agent_name) return member.agent_name;
  const agent = agentByKey.get(member.agent_key);
  return agent?.name || member.agent_key;
}

// activeMembers 只给「还在席上」的成员（left_at 有值 = 已离席，历史归属不变）。
export function activeMembers(session: Session): SessionMember[] {
  return sessionMembers(session).filter((member) => !member.left_at);
}

// ---- 命名（三轨：用户显式名 > 生成标题 > 默认显示名）----

export function sessionDisplayName(session: Session): string {
  const explicit = session.name.trim();
  if (explicit) return explicit;
  const generated = session.title.trim();
  if (generated) return generated;
  return "新会话";
}

export function sessionNameSourceLabel(session: Session): string {
  switch (session.name_source) {
    case "user":
      return "用户命名";
    case "generated":
      return "生成标题";
    default:
      return "默认名";
  }
}

// ---- 时间 ----

export function relativeTime(iso: string | null | undefined): string {
  if (!iso) return "";
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  const diff = Date.now() - t;
  if (diff < 0) return "刚刚";
  const minutes = Math.floor(diff / 60_000);
  if (minutes < 1) return "刚刚";
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "昨天";
  if (days < 7) return `${days} 天前`;
  return new Date(t).toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
}

export function isToday(iso: string | null | undefined): boolean {
  if (!iso) return false;
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return false;
  const now = new Date();
  return t.getFullYear() === now.getFullYear() && t.getMonth() === now.getMonth() && t.getDate() === now.getDate();
}

// ---- presence（三态 + 在席/已退出）----

export interface PresenceMeta {
  label: string;
  tone: Tone;
  dot: string;
}

export function presenceMeta(presence: SessionPresence | string | undefined): PresenceMeta {
  switch (presence) {
    case "working":
      return { label: "工作中", tone: "blue", dot: toneDot.blue };
    case "listening":
      return { label: "在听", tone: "green", dot: toneDot.green };
    case "blocked":
      return { label: "需关注", tone: "amber", dot: toneDot.amber };
    case "left":
      return { label: "已退出", tone: "slate", dot: toneDot.slate };
    case "idle":
      return { label: "在席", tone: "neutral", dot: toneDot.neutral };
    default:
      return { label: "在席", tone: "neutral", dot: toneDot.neutral };
  }
}

// ---- 列表行状态点 ----

export type SessionStatus = "working" | "attention" | "unread" | "idle";

// sessionStatus 从名册与未读数推出列表行上的那一个状态点：
// 卡住（需你决策）> 工作中 > 有未读 > 空闲。
export function sessionStatus(session: Session): SessionStatus {
  const members = activeMembers(session);
  if (members.some((member) => member.presence === "blocked")) return "attention";
  if (members.some((member) => member.presence === "working")) return "working";
  if ((session.unread_count ?? 0) > 0) return "unread";
  return "idle";
}

export function sessionStatusMeta(status: SessionStatus): PresenceMeta {
  switch (status) {
    case "attention":
      return { label: "需关注", tone: "amber", dot: toneDot.amber };
    case "working":
      return { label: "工作中", tone: "blue", dot: toneDot.blue };
    case "unread":
      return { label: "有未读", tone: "green", dot: toneDot.green };
    default:
      return { label: "", tone: "neutral", dot: toneDot.neutral };
  }
}

// ---- 关联工件 ----

export const ATTACHMENT_TYPE_LABELS: Record<SessionAttachmentType, string> = {
  task: "任务",
  task_hall: "任务大厅",
  strategy: "策略",
  asset: "资产",
  finding: "漏洞",
  pr: "PR",
  external: "外部",
};

export function attachmentTypeLabel(type: string): string {
  return ATTACHMENT_TYPE_LABELS[type as SessionAttachmentType] ?? type;
}

// ---- 列表查询（§4.6 工具条：搜索 / 筛选 / 排序 / 视图）----

export type SessionFilter = "all" | "unread" | "working" | "attention";
export type SessionSort = "updated" | "created";
export type SessionViewMode = "flat" | "by_agent" | "by_attachment";

export interface SessionListQuery {
  query: string;
  filter: SessionFilter;
  sort: SessionSort;
  view: SessionViewMode;
  // agent 是**筛选维度**（一行上的字段），不是分组轴（§2.1 共识 2 / §9 定稿 4）。
  agent: string | null;
}

export const DEFAULT_SESSION_LIST_QUERY: SessionListQuery = {
  query: "",
  filter: "all",
  sort: "updated",
  view: "flat",
  agent: null,
};

function matchesQuery(session: Session, needle: string, agentByKey: Map<string, Agent>): boolean {
  if (!needle) return true;
  const haystack = [
    session.name,
    session.title ?? "",
    ...sessionMembers(session).flatMap((member) => [member.agent_key, memberDisplayName(member, agentByKey)]),
    ...(session.attachments ?? []).flatMap((attachment) => [
      attachment.identity_key,
      attachmentTypeLabel(attachment.attachment_type),
    ]),
  ]
    .join(" ")
    .toLowerCase();
  return haystack.includes(needle.toLowerCase());
}

export function filterSessions(
  sessions: Session[],
  query: SessionListQuery,
  agentByKey: Map<string, Agent>,
): Session[] {
  const needle = query.query.trim();
  const filtered = sessions.filter((session) => {
    if (!matchesQuery(session, needle, agentByKey)) return false;
    if (query.agent && !sessionMembers(session).some((member) => member.agent_key === query.agent)) {
      return false;
    }
    switch (query.filter) {
      case "unread":
        return (session.unread_count ?? 0) > 0;
      case "working":
        return activeMembers(session).some((member) => member.presence === "working");
      case "attention":
        return activeMembers(session).some((member) => member.presence === "blocked");
      default:
        return true;
    }
  });
  const key = query.sort === "created" ? "created_at" : "updated_at";
  return [...filtered].sort((a, b) => {
    const ta = new Date(a[key] ?? 0).getTime();
    const tb = new Date(b[key] ?? 0).getTime();
    if (Number.isFinite(ta) && Number.isFinite(tb) && ta !== tb) return tb - ta;
    return b.id - a.id;
  });
}

export interface SessionSection {
  key: string;
  label: string;
  sessions: Session[];
}

function isPinned(session: Session): boolean {
  return Boolean(session.pinned_at);
}

// groupSessions 切成分区：置顶 / 今天 / 更早。
// 置顶优先（与旧版「置顶块浮在分组上面」一致），置顶项不在下面重复出现。
export function groupSessions(sessions: Session[]): SessionSection[] {
  const pinned = sessions.filter(isPinned);
  const rest = sessions.filter((session) => !isPinned(session));
  const today = rest.filter((session) => isToday(session.updated_at));
  const earlier = rest.filter((session) => !isToday(session.updated_at));
  const sections: SessionSection[] = [];
  if (pinned.length) sections.push({ key: "pinned", label: "置顶", sessions: pinned });
  if (today.length) sections.push({ key: "today", label: "今天", sessions: today });
  if (earlier.length) sections.push({ key: "earlier", label: "更早", sessions: earlier });
  return sections;
}

// groupSessionsByKind 是 A3 的形态四分区：总控 / 群聊 / 大厅 / 旧会话。
// kind 缺省（旧数据）或不认识的值一律归入「旧会话」，不是群聊。这里只给会话分桶：
// 认领了旧对话的会话照常按 kind 进桶（旧对话挂在会话的「旧对话记录」页签）；没被认领
// 的旧对话不归这里管，在列表下方单独的「旧对话」块（chat/page.tsx）。
// 分区本身不带折叠态 —— 分区头的折叠由 SessionList 渲染、ChatPage 持有（按视图前缀存本地）。
export function groupSessionsByKind(sessions: Session[]): SessionSection[] {
  const buckets: Record<string, Session[]> = {
    master: [],
    chat: [],
    task_hall: [],
    legacy: [],
  };
  for (const session of sessions) {
    const k = session.kind;
    if (k === "master") buckets.master.push(session);
    else if (k === "task_hall") buckets.task_hall.push(session);
    else if (k === "chat") buckets.chat.push(session);
    else buckets.legacy.push(session);
  }
  const sections: SessionSection[] = [];
  const labelOf: Record<string, string> = {
    master: "总控",
    chat: "群聊",
    task_hall: "大厅",
    legacy: "旧会话",
  };
  for (const key of ["master", "chat", "task_hall", "legacy"] as const) {
    if (buckets[key].length > 0) {
      sections.push({ key: `kind:${key}`, label: labelOf[key], sessions: buckets[key] });
    }
  }
  return sections;
}

// groupSessionsByAgent 是**可选视图**（§9 定稿 4：保留为可选开关，默认关闭）。
export function groupSessionsByAgent(sessions: Session[], agentByKey: Map<string, Agent>): SessionSection[] {
  const buckets = new Map<string, Session[]>();
  for (const session of sessions) {
    const keys = sessionMembers(session).map((member) => member.agent_key);
    if (keys.length === 0) {
      const list = buckets.get("") ?? [];
      list.push(session);
      buckets.set("", list);
      continue;
    }
    for (const key of keys) {
      const list = buckets.get(key) ?? [];
      list.push(session);
      buckets.set(key, list);
    }
  }
  return [...buckets.entries()]
    .map(([key, list]) => ({
      key: key || "__none__",
      label: key ? agentByKey.get(key)?.name || key : "无成员",
      sessions: list,
    }))
    .sort((a, b) => a.label.localeCompare(b.label, "zh-CN"));
}

// groupSessionsByAttachment 按会话关联的工件类型分组（视图三选一）。
export function groupSessionsByAttachment(sessions: Session[]): SessionSection[] {
  const buckets = new Map<string, Session[]>();
  for (const session of sessions) {
    const type = session.attachments?.[0]?.attachment_type ?? "";
    const list = buckets.get(type) ?? [];
    list.push(session);
    buckets.set(type, list);
  }
  return [...buckets.entries()].map(([type, list]) => ({
    key: type || "__none__",
    label: type ? attachmentTypeLabel(type) : "无关联",
    sessions: list,
  }));
}

// bindLegacyConversations 找出「会话 ↔ 旧对话」的对应关系（迁移兼容，§4.7）。
//
// 两种认法：
//   1) 会话成员上带着 conversation_id —— 后端直接给的，最准；
//   2) 回填出来的会话（每个旧对话自成一个会话 + 一行成员）—— 单一成员、标题与
//      agent 都对得上、且候选唯一时才认；认不准就不认（宁可让它留在旧对话列表里，
//      也不能把 A 的历史挂到 B 的会话上）。
//
// 返回 sessionId → conversationId。
export function bindLegacyConversations(sessions: Session[], conversations: Conversation[]): Map<number, number> {
  const result = new Map<number, number>();
  const used = new Set<number>();
  const byId = new Map(conversations.map((conversation) => [conversation.id, conversation]));

  for (const session of sessions) {
    for (const member of sessionMembers(session)) {
      const convId = member.conversation_id;
      if (convId == null) continue;
      if (!byId.has(convId)) continue;
      result.set(session.id, convId);
      used.add(convId);
      break;
    }
  }

  for (const session of sessions) {
    if (result.has(session.id)) continue;
    const members = sessionMembers(session);
    if (members.length !== 1) continue;
    const agentKey = members[0].agent_key;
    const title = session.title.trim();
    if (!title) continue;
    const candidates = conversations.filter(
      (conversation) =>
        conversation.agent_key === agentKey &&
        (conversation.title ?? "").trim() === title &&
        !used.has(conversation.id),
    );
    if (candidates.length !== 1) continue;
    result.set(session.id, candidates[0].id);
    used.add(candidates[0].id);
  }

  return result;
}
