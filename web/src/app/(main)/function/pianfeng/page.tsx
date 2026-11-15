"use client";

// 偏锋探索独立页（0.15.1 拆分，按用户裁定还原 0.11.8 的"并页"结构）：
// 方法论注册表 + 偏锋任务判据进度 + stop_reason 统计，数据走既有 API
//（/api/edge/methodologies、/api/edge/tasks）。业务建模在 /function/business-graph，
// 差异面板在 /function/business-graph/diffs——三个 sidebar 入口各自直达。

import { EdgeOverviewPanel } from "@/components/edge-overview-panel";

export default function PianfengPage() {
  return (
    <div className="h-full">
      <EdgeOverviewPanel />
    </div>
  );
}
