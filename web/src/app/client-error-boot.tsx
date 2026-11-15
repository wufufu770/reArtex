"use client";

import { useEffect } from "react";

import { installGlobalErrorHandlers } from "@/lib/client-error-reporter";

// 全局错误上报挂载点（0.13.5）+ 版本错位提示（0.14.6 F12）：根 layout 是
// server component，window 级 onerror/unhandledrejection 与发版检测由这个
// 空渲染的客户端组件兜住。
//
// F12 语义：页面构建时的版本（构建注入 NEXT_PUBLIC_ARTEX_VERSION）与运行中
// 服务的版本（/api/health）不一致 ⇒ 用户持有旧 HTML、指向的 chunk 可能已
// 404——提示刷新。会话内只提醒一次（不打扰）。
export function ClientErrorBoot() {
  useEffect(() => {
    installGlobalErrorHandlers();
    const buildVersion = process.env.NEXT_PUBLIC_ARTEX_VERSION;
    if (!buildVersion) return;
    let warned = false;
    const check = async () => {
      if (warned || document.hidden) return;
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (!r.ok) return;
        const d = (await r.json()) as { version?: string };
        if (d.version && d.version !== buildVersion) {
          warned = true;
          const { toast } = await import("sonner");
          toast(`平台已更新到 v${d.version}——当前页面是旧版本，建议刷新`, {
            duration: Infinity,
            action: { label: "刷新", onClick: () => window.location.reload() },
          });
        }
      } catch {
        /* 离线/后端未起：静默 */
      }
    };
    void check();
    const t = setInterval(check, 5 * 60_000);
    return () => clearInterval(t);
  }, []);
  return null;
}
