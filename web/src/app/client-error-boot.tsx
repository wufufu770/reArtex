"use client";

import { useEffect } from "react";

import { installGlobalErrorHandlers } from "@/lib/client-error-reporter";

// 全局错误上报挂载点（0.13.5）：根 layout 是 server component，window 级
// onerror/unhandledrejection 由这个空渲染的客户端组件兜住。
export function ClientErrorBoot() {
  useEffect(() => {
    installGlobalErrorHandlers();
  }, []);
  return null;
}
