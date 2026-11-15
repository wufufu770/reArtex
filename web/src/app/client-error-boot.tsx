"use client";

import { useEffect } from "react";

import { installGlobalErrorHandlers } from "@/lib/client-error-reporter";

// 全局错误上报挂载点（0.13.5）：根 layout 是 server component，window 级
// onerror/unhandledrejection 由这个空渲染的客户端组件兜住。
// （0.14.6 F12 的版本错位提示已按用户裁定移除——换装提示是一次性场景，
// 不需要常驻轮询机制。）
export function ClientErrorBoot() {
  useEffect(() => {
    installGlobalErrorHandlers();
  }, []);
  return null;
}
