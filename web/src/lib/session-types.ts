// 会话（session）实体的 API 契约 —— 方案书 §4.1 数据模型 / §4.5 API 变更。
//
// 为什么单独一个文件：`conversations.agent_key` 是单值列 ⇒ 一个旧对话只能跑一个
// agent；会话把「agent」从会话属性升格为**成员属性**，一个 session 下可以有多个
// 成员（花名册），人与 agent 在同一条消息流里。所有会话相关的 JSON 形状集中在这里，
// 与 lib/types.ts（旧领域模型）平行。
//
// 字段名沿用后端 snake_case 列名（与仓库里其它接口一致）：后端把 sessions /
// session_members / session_attachments / session_messages 四张表序列化成 JSON，
// 前端按同一套名字读。

// ---- 会话本体 ----

// kind='master' 是总控会话：群聊全部能力 + 绑定任务（创建即编排或 spawn_task
// 自动绑回）+ 引擎进展播报进会话流。
export type SessionKind = "chat" | "master" | "task_hall" | "side_question";

// 三轨命名（§4.2）：用户显式名 > 生成标题 > 默认显示名。
export type SessionNameSource = "user" | "generated" | "default";

// 成员分工：分发者只派单不抢活（auto 默认是分发者）。
export type SessionRole = "dispatcher" | "member" | "observer";

// presence 三态（§4.3 / §9 约束 3）：在席可被唤醒 / 正在跑 / 卡住；left 表示已退出。
export type SessionPresence = "idle" | "listening" | "working" | "blocked" | "left";

// 会话↔工件的类型化关联（§4.4）。
export type SessionAttachmentType = "task" | "task_hall" | "strategy" | "asset" | "finding" | "pr" | "external";

export interface Session {
  id: number;
  // 用户显式名 —— 一旦设置永不被生成标题覆盖，也是唯一检索句柄。
  name: string;
  // 生成标题（模型摘要）；没有时用默认显示名兜底。
  title: string;
  name_source: SessionNameSource;
  kind: SessionKind;
  task_id: number | null;
  strategy_id: number | null;
  asset_id: number | null;
  pinned_at: string | null;
  created_at: string;
  updated_at: string;
  // ---- 列表/详情接口附带的摘要（后端可省略，前端按空处理） ----
  // 花名册：成员 + presence。
  members?: SessionMember[];
  // 已关联工件。
  attachments?: SessionAttachment[];
  // 未读消息数（会话内 status='unread' 的消息条数）。
  unread_count?: number;
  // 任务大厅型会话（kind='task_hall'）对应的群 id：用来读额度。
  group_id?: number | null;
}

// ---- 成员（花名册席位） ----

export interface SessionMember {
  session_id: number;
  // agent 的唯一 key（对应 agents.key）。
  agent_key: string;
  role: SessionRole;
  presence: SessionPresence;
  // 卡住时的预计剩余分钟数与卡在谁身上。
  eta_minutes?: number | null;
  blocked_on?: string | null;
  joined_at: string;
  // 退出时间：有值 = 已离席，历史消息归属不变。
  left_at?: string | null;
  // 展示名（后端可在名册里附上，省得前端再查 agent 表）。
  agent_name?: string;
  // 后端序列化出来的展示名（db/session.go 的 SessionMember.DisplayName）。
  display_name?: string;
  // 已离席（left_at 有值时后端同时给出这个标志）。
  has_left?: boolean;
  // 该成员对应的旧对话 id（迁移回填后每个成员背后各有一个会话）。
  conversation_id?: number | null;
}

// ---- 会话↔工件 ----

export interface SessionAttachment {
  id: number;
  session_id: number;
  attachment_type: SessionAttachmentType;
  // 稳定标识（PR 用 'owner/repo#12'），反查走
  // GET /api/sessions/attachments/{type}/{key}/sessions。
  identity_key: string;
  payload?: Record<string, unknown>;
  created_at: string;
}

// ---- 会话消息 ----

export type SessionMessageKind = "note" | "request" | "answer" | "finding" | "escalation" | "receipt";

export type SessionMessageStatus = "unread" | "read" | "acked" | "blocked";

// 收据条动作（§4.3 噪音控制）：agent 的「收到 / 好的」只落收据条，不进消息流。
export type SessionReceiptAction = "" | "taken" | "noted" | "deferred";

export interface SessionMessage {
  id: number;
  session_id: number;
  // 线程层：回复挂在谁下面；为空 = 顶层消息。
  parent_message_id: number | null;
  // 发件人：空串 = 人。
  from_agent: string;
  // 收件人：空串 = 广播给全会话。
  to_agent: string;
  kind: SessionMessageKind;
  body: string;
  task_id: number | null;
  intent_id: number | null;
  finding_id: number | null;
  status: SessionMessageStatus;
  // 非空 = 投递前被拦下（已留痕，不进接收方上下文）。
  blocked_reason: string;
  claims_authorization: boolean;
  // 收据条：贴在哪条消息下面 + 收据动作；为空 = 普通消息。
  receipt_of: number | null;
  receipt_action: SessionReceiptAction;
  created_at: string;
  read_at: string | null;
}

// ---- 请求体 ----

export interface SessionCreateInput {
  // 用户显式名；留空则由后端生成标题或用默认显示名。
  name?: string;
  // 会话形态；缺省 chat（群聊）。master = 总控（创建即编排）。
  kind?: SessionKind;
  // 成员（agent key）；默认只带 auto（§9 定稿 2）。
  agents?: string[];
  // 分发者（默认 auto）。
  dispatcher?: string;
  // 只对 kind='master' 有意义：非空 ⇒ 后端按这个目标建任务并绑定会话，
  // 规划/认领/执行进展直接播报进会话流。
  goal?: string;
  // 建会话时顺手关联的工件。
  attachments?: { attachment_type: SessionAttachmentType; identity_key: string }[];
}

export interface SessionPatchInput {
  // 改名：写用户显式名（生成标题永不覆盖它）。
  name?: string;
  // 置顶 / 取消置顶。
  pinned?: boolean;
  // 关联增删。
  add_attachments?: { attachment_type: SessionAttachmentType; identity_key: string }[];
  remove_attachments?: { attachment_type: SessionAttachmentType; identity_key: string }[];
}

export interface SessionMessageInput {
  body: string;
  // 定向收件人；空串 = 广播。
  to_agent?: string;
  kind?: SessionMessageKind;
  attachments?: { name: string; path: string; size: number }[];
}
