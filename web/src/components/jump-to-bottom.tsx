"use client";

// 「到顶 / 到底」常驻悬浮按钮（长消息流共用组件）。
//
// 覆盖旧对话 ChatView 与任务详情会话页签（会话流 SessionView 用
// MessageScroller 的按钮，同轮已改为常驻可见）。
// 设计口径（用户明确要求）：**常驻可点**，不做「滚离端点才浮现」的智能
// 显隐——打开会话就停在底部，「到底」按钮若隐藏等于永远看不见。
// viewportOf 返回滚动容器元素（可为 null；未挂载时按钮点不出效果但不报错）。

import * as React from "react";

import { ArrowDownIcon } from "lucide-react";

import { Button } from "@/components/ui/button";

export function JumpToBottom({
  viewportOf,
}: {
  viewportOf: () => HTMLElement | null;
}) {
  const jump = (to: "start" | "end") => {
    const vp = viewportOf();
    if (!vp) return;
    vp.scrollTo({ top: to === "end" ? vp.scrollHeight : 0, behavior: "smooth" });
  };
  return (
    <div className="absolute right-4 bottom-4 z-10 flex flex-col gap-1.5">
      <Button
        type="button"
        size="icon"
        variant="outline"
        aria-label="回到顶部"
        title="回到顶部"
        className="size-8 rounded-full bg-card/95 shadow-md"
        onClick={() => jump("start")}
      >
        <ArrowDownIcon className="size-4 rotate-180" />
      </Button>
      <Button
        type="button"
        size="icon"
        variant="outline"
        aria-label="回到底部"
        title="回到底部（最新消息）"
        className="size-8 rounded-full bg-card/95 shadow-md"
        onClick={() => jump("end")}
      >
        <ArrowDownIcon className="size-4" />
      </Button>
    </div>
  );
}
