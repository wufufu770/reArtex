import type { StrategyState } from "@/lib/types";

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

// fmtScore renders a 0..1 ratio (confidence / utility) with two decimals.
export function fmtScore(n?: number | null): string {
  if (typeof n !== "number" || !Number.isFinite(n)) return "—";
  return n.toFixed(2);
}

// fmtSigned renders run_utility —— 它是 +1 / -1 的累加器，可以是负数。
export function fmtSigned(n?: number | null): string {
  if (typeof n !== "number" || !Number.isFinite(n)) return "—";
  return n > 0 ? `+${n.toFixed(2)}` : n.toFixed(2);
}

// STRATEGY_STATES 是后端 state 白名单里的四个取值（GET /api/strategies?state=）。
// 顺序按「还能用 → 终态」排。
export const STRATEGY_STATES: { value: StrategyState; label: string }[] = [
  { value: "active", label: "在役 · active" },
  { value: "quarantined", label: "隔离中 · quarantined" },
  { value: "retired", label: "已退役 · retired" },
  { value: "tombstone", label: "已墓碑 · tombstone" },
];

// PROPOSAL_STATUSES 是提议状态的取值（GET /api/strategy-proposals?status=）。
// 第一项是默认值：后端不传 status 时只返回 pending。
export const PROPOSAL_STATUSES: { value: string; label: string }[] = [
  { value: "pending", label: "待裁决 · pending" },
  { value: "approved", label: "已批准 · approved" },
  { value: "rejected", label: "已驳回 · rejected" },
  { value: "superseded", label: "已被取代 · superseded" },
];

// REF_TYPE_LABEL 把证据引用的类型白名单翻成中文。
export const REF_TYPE_LABEL: Record<string, string> = {
  finding: "漏洞",
  traffic_snapshot: "流量快照",
  activity: "活动",
  tool_call: "工具调用",
  run: "运行",
};

// fmtJSON 把不透明的 jsonb 列渲染成可读文本。空对象 / 空数组按「没有内容」处理。
export function fmtJSON(value: unknown): string {
  if (value === undefined || value === null) return "—";
  if (typeof value === "string") return value.trim() || "—";
  try {
    const text = JSON.stringify(value, null, 2);
    return text && text !== "{}" && text !== "[]" ? text : "—";
  } catch {
    return String(value);
  }
}

// fmtDigest shortens a content digest to head…tail for table display.
export function fmtDigest(digest: string): string {
  if (!digest) return "—";
  return digest.length <= 20 ? digest : `${digest.slice(0, 10)}…${digest.slice(-8)}`;
}
