"use client";

// 发现页统计卡（0.14.8 批次七 G1 温和拆分试点）：从 findings/page.tsx 提取——
// 纯展示组件（stats 进、卡片网格出），零行为变化。页文件（1300+ 行）的后续
// 拆分按同款「独立可读的展示块」粒度分批推进。

import { BugIcon, CircleAlertIcon, ClockIcon, InfoIcon, ShieldAlertIcon, TriangleAlertIcon } from "lucide-react";

import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import type { FindingStats } from "@/lib/types";
import { cn } from "@/lib/utils";

export function FindingStatsCards({ stats }: { stats: FindingStats }) {
  const statCards = [
    { label: "发现总数", value: stats.total, icon: BugIcon },
    { label: "待处理", value: stats.pending, tone: "text-amber-500", icon: ClockIcon },
    { label: "严重", value: stats.critical, tone: "text-rose-600", icon: ShieldAlertIcon },
    { label: "高危", value: stats.high, tone: "text-red-500", icon: TriangleAlertIcon },
    { label: "中危", value: stats.medium, tone: "text-amber-500", icon: TriangleAlertIcon },
    { label: "低危", value: stats.low, tone: "text-slate-500", icon: CircleAlertIcon },
    { label: "信息", value: stats.info, tone: "text-muted-foreground", icon: InfoIcon },
  ];

  return (
    <div className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-7">
      {statCards.map((stat) => {
        const StatIcon = stat.icon;
        return (
          <Card key={stat.label} className="gap-1 py-4">
            <CardHeader className="px-4">
              <CardDescription>{stat.label}</CardDescription>
              <CardTitle className={cn("flex items-center gap-2 text-2xl tabular-nums", stat.tone)}>
                <StatIcon className="size-5" aria-hidden="true" />
                {stat.value}
              </CardTitle>
            </CardHeader>
          </Card>
        );
      })}
    </div>
  );
}
