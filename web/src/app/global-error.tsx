"use client";

// 根级 error boundary（0.13.5）：根 layout 自身抛错时 error.tsx 接不住（它在
// layout 之内），global-error 接管并替换整个 html——所以必须自带 <html>/<body>，
// 且不能依赖 provider / 全局样式之外的任何组件（内联样式）。

import { useEffect } from "react";

import { reportClientError } from "@/lib/client-error-reporter";

export default function GlobalError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  useEffect(() => {
    console.error("[global-error]", error);
    reportClientError(`global error: ${error.message}`, error.stack);
  }, [error]);

  return (
    <html lang="en">
      <body
        style={{
          margin: 0,
          minHeight: "100vh",
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          gap: 16,
          fontFamily: "system-ui, sans-serif",
          background: "#0b0b0c",
          color: "#e5e5e5",
          padding: 24,
          textAlign: "center",
        }}
      >
        <h2 style={{ fontSize: 18, fontWeight: 600, margin: 0 }}>应用出错</h2>
        <p style={{ maxWidth: 560, fontSize: 14, opacity: 0.7, wordBreak: "break-all", margin: 0 }}>
          {error.message || "未知错误"}
        </p>
        <div style={{ display: "flex", gap: 8 }}>
          <button
            type="button"
            onClick={reset}
            style={{
              padding: "6px 14px",
              borderRadius: 6,
              border: "1px solid #444",
              background: "#1f1f22",
              color: "inherit",
              cursor: "pointer",
            }}
          >
            重试
          </button>
          <button
            type="button"
            onClick={() => {
              window.location.href = "/dashboard";
            }}
            style={{
              padding: "6px 14px",
              borderRadius: 6,
              border: "1px solid #444",
              background: "transparent",
              color: "inherit",
              cursor: "pointer",
            }}
          >
            返回仪表盘
          </button>
        </div>
      </body>
    </html>
  );
}
