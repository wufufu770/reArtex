export const mentionKinds = [
  // 「成员」是**本地**项（会话成员 / 候选 agent），不参与服务端搜索；放在首位，
  // 面板里成员组排在最前。
  { kind: "agent", label: "成员", alias: "agent" },
  { kind: "finding", label: "漏洞", alias: "finding" },
  { kind: "asset", label: "资产", alias: "asset" },
  { kind: "company", label: "企业", alias: "company" },
  { kind: "endpoint", label: "接口", alias: "api" },
  { kind: "ip", label: "IP", alias: "ip" },
  { kind: "app", label: "应用", alias: "app" },
  { kind: "root_domain", label: "域名", alias: "domain" },
  { kind: "subdomain", label: "子域名", alias: "subdomain" },
  { kind: "service", label: "服务", alias: "service" },
] as const;

export type MentionKind = (typeof mentionKinds)[number]["kind"];
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
  // agent 类的稳定 key（= agent_key）。裸 "@<agentKey> " 是后端 internal/sessionwake
  // 判断「定向必醒」的依据，所以它必须原样进正文。
  agentKey?: string;
}

export function activeMention(value: string, caret: number, trigger = "@") {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf(trigger);
  if (start < 0) return null;
  // 触发符前面：@ 允许紧贴单词（邮箱@x），其余触发符要求行首或前面是空白，
  // 否则路径 / 命令里的斜杠会误触。
  if (start > 0 && trigger === "@" && /[\w.+/-]/.test(before[start - 1])) return null;
  if (start > 0 && trigger !== "@" && !/\s/.test(before[start - 1])) return null;
  const query = before.slice(start + trigger.length);
  if (/[[\]\r\n]/.test(query) || query.includes(trigger) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    if (item.kind === "agent") continue;
    for (const alias of [item.label.toLowerCase(), item.alias]) {
      if (text === alias || text.startsWith(`${alias} `) || (/[^a-z]/.test(alias) && text.startsWith(alias))) {
        return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
      }
    }
  }
  const categories = mentionKinds.filter(
    (item) => item.kind !== "agent" && (item.label.toLowerCase().startsWith(text) || item.alias.startsWith(text)),
  );
  return { kind: "" as const, query: query.trim(), categories };
}

export function mentionToken(item: ChatMention) {
  // agent 类是**裸 key**（后端 internal/sessionwake 从正文认 @agentkey 来决定定向必醒），
  // 与记录类的结构化 token 互不干扰。
  if (item.kind === "agent") return `@${item.agentKey ?? item.label} `;
  const kind = mentionKinds.find((entry) => entry.kind === item.kind)?.label ?? "资产";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}

export function selectedMentions(value: string) {
  return [...value.matchAll(/@\[(漏洞|资产|企业|接口|IP|应用|域名|子域名|服务)#([0-9]+)(?: ([^\]\r\n]*))?\]/g)].map(
    (match) => ({
      token: match[0],
      label: `${match[1]} #${match[2]}${match[3] ? ` · ${match[3]}` : ""}`,
      start: match.index,
    }),
  );
}
