import type { TaskHallMember } from "@/lib/types";

// fmtTokens renders a compact token count (1234 → 1.2k, 2_000_000 → 2M).
export function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

// fmtDateTime renders an RFC3339 timestamp in the app's short zh-CN form.
export function fmtDateTime(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// dispatcherOf returns the group's dispatcher. auto 恒为分发者（后端建群时写入）。
export function dispatcherOf(members: TaskHallMember[]): TaskHallMember | undefined {
  return members.find((m) => m.role === "dispatcher");
}

// workerMembers returns the members that actually take jobs —— 分发者不与成员抢单，
// 所以席位与并发占用只按成员算。
export function workerMembers(members: TaskHallMember[]): TaskHallMember[] {
  return members.filter((m) => m.role !== "dispatcher");
}

// MESSAGE_KIND_LABEL maps the mailbox kind whitelist (db.chatGroupMessageKinds) to labels.
export const MESSAGE_KIND_LABEL: Record<string, string> = {
  note: "备注",
  request: "协商请求",
  answer: "答复",
  finding: "漏洞",
  escalation: "升级",
};
