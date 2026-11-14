"use client";

// 前端渲染错误上报（0.13.5）：window.onerror / unhandledrejection / error boundary
// 三路合一，走 POST /api/logs/client（服务端限流+截断，tag=webclient）。
// 客户端侧做两道自我约束：同 message 60s 去重、全会话上限 20 条——上报管道
// 自己不能成为风暴源。

let installed = false;
const seenMessages = new Map<string, number>();
let sessionCount = 0;
const MAX_PER_SESSION = 20;
const DEDUP_WINDOW_MS = 60_000;

export function reportClientError(message: string, stack?: string) {
  if (sessionCount >= MAX_PER_SESSION) return;
  const now = Date.now();
  const key = message.slice(0, 120);
  const last = seenMessages.get(key) ?? 0;
  if (now - last < DEDUP_WINDOW_MS) return;
  seenMessages.set(key, now);
  sessionCount += 1;
  try {
    void fetch("/api/logs/client", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        message,
        stack,
        url: typeof window === "undefined" ? "" : window.location.href,
      }),
      keepalive: true,
    }).catch(() => undefined);
  } catch {
    // 上报失败静默——它永远不能比业务错误更吵。
  }
}

/** 幂等挂载 window 级兜底（根 layout 的 ClientErrorBoot 调一次）。 */
export function installGlobalErrorHandlers() {
  if (installed || typeof window === "undefined") return;
  installed = true;
  window.addEventListener("error", (ev) => {
    reportClientError(ev.message || "window.error", ev.error?.stack);
  });
  window.addEventListener("unhandledrejection", (ev) => {
    const reason = ev.reason;
    const msg = reason instanceof Error ? reason.message : String(reason ?? "unhandledrejection");
    reportClientError(`unhandled rejection: ${msg}`, reason instanceof Error ? reason.stack : undefined);
  });
}
