"use client";

// 路由段级 error boundary（0.13.5，全量审查 P0②）：此前全应用没有任何
// error.tsx——客户端渲染期一旦抛未捕获异常，Next 卸载整棵子树，用户只看到
// 纯白页。现在段内抛错落到这里：显示错误摘要 + 重试 + 返回，并经上报管道
// 写入服务端日志（tag=webclient，日志页可查）。

import { useEffect } from "react";

import { Button } from "@/components/ui/button";
import { reportClientError } from "@/lib/client-error-reporter";

export default function RouteError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  useEffect(() => {
    console.error("[route-error]", error);
    reportClientError(`render error: ${error.message}`, error.stack);
  }, [error]);

  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center gap-4 p-6 text-center">
      <h2 className="font-semibold text-lg">页面渲染出错</h2>
      <p className="max-w-xl break-all text-muted-foreground text-sm">{error.message || "未知错误"}</p>
      <div className="flex gap-2">
        <Button size="sm" onClick={reset}>
          重试
        </Button>
        <Button
          size="sm"
          variant="outline"
          onClick={() => {
            window.location.href = "/dashboard";
          }}
        >
          返回仪表盘
        </Button>
      </div>
    </div>
  );
}
