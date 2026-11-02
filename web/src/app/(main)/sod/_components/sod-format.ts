// SoD 面的格式化助手与取值常量。
//
// 这一层只做「把后端的机器值翻成人能读的字」：原因码 / 层级 / 熔断态的中文
// 标签都在 lib/status.ts 里（那儿是全 app 共享的语义表），这里只放**本面特有**
// 的排版规则。

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

// fmtDigest shortens a content digest to head…tail for table display.
export function fmtDigest(digest: string): string {
  if (!digest) return "—";
  return digest.length <= 20 ? digest : `${digest.slice(0, 10)}…${digest.slice(-8)}`;
}

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

// fmtLatency renders the gate's decision latency.
export function fmtLatency(ms?: number | null): string {
  if (typeof ms !== "number" || !Number.isFinite(ms)) return "—";
  return `${ms} ms`;
}

// SOD_LIMIT_OPTIONS 是决策账本 / 复核队列单页条数的可选值。
//
// 后端把 limit 截到自己的上限（决策 200 / 队列 200），所以这里只给不越界的值。
export const SOD_LIMIT_OPTIONS = [50, 100, 200];

// SOD_SCOPE_LABEL 把分层里的范围结论翻成三种可读说法。
//
// ⚠️ 三态必须分开：「在范围内」/「不在范围内」/「未判定」在事后排查时的含义
// 完全不同 —— 糊成两个会让人分不清「确凿的越界」和「查不出来」。
export function sodScopeLabel(scopeIn: boolean | null | undefined): string {
  if (scopeIn === true) return "范围判定 · 在范围内";
  if (scopeIn === false) return "范围判定 · 不在范围内";
  return "范围判定 · 未判定";
}
